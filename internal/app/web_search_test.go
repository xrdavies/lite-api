package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestWebSearchContracts(t *testing.T) {
	for _, raw := range []string{`{"providers":[{"type":"other"}]}`, `{"enabled":true,"providers":[{"type":"brave"}]}`, `{"providers":[{"type":"brave","quota_limit":-1}]}`, `{"providers":[{"type":"brave","api_key":"x\ny"}]}`, `{"providers":[{"type":"brave"},{"type":"brave"}]}`, `{"providers":[{"type":"tavily","expires_at":-1}]}`} {
		var cfg webSearchConfig
		if json.Unmarshal([]byte(raw), &cfg) != nil || cfg.validate() == nil {
			t.Fatal("accepted invalid search config", raw)
		}
	}
	for _, tc := range []struct{ anchor, now, next string }{
		{"2024-01-31", "2024-02-10", "2024-02-29"}, {"2023-01-31", "2023-02-28", "2023-03-31"},
		{"2025-12-15", "2026-01-20", "2026-02-15"}, {"2026-12-15", "2026-01-20", "2026-12-15"},
	} {
		anchor, _ := time.Parse(time.DateOnly, tc.anchor)
		now, _ := time.Parse(time.DateOnly, tc.now)
		stamp := anchor.Unix()
		if got := now.Add(webSearchQuotaTTL(&stamp, now)).Format(time.DateOnly); got != tc.next {
			t.Fatal("monthly reset", tc, got)
		}
	}
	for raw, want := range map[string]string{`true`: "enabled", `false`: "default", `null`: "default", `"disabled"`: "disabled", `"default"`: "default"} {
		if got := webSearchMode(json.RawMessage(raw)); got != want {
			t.Fatal("legacy account mode", got, want)
		}
	}
	var request map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"text","text":"query"}]}]}`), &request)
	if !onlyWebSearch(request) || webSearchQuery(request) != "query" {
		t.Fatal("search classification")
	}
	request["tools"] = json.RawMessage(`[{"name":"web_search"},{"name":"other"}]`)
	if onlyWebSearch(request) {
		t.Fatal("mixed tool request intercepted")
	}
	request["messages"] = json.RawMessage(`[{"role":"assistant","content":"query"}]`)
	if webSearchQuery(request) != "" {
		t.Fatal("assistant used as search query")
	}
}

func testWebSearch(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const path = "/api/v1/admin/settings/web-search-emulation"
	ctx := context.Background()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.190:1200"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	var braveCalls, tavilyCalls, modelCalls atomic.Int64
	var failure atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("client or model credentials sent to search")
		}
		if r.URL.Path == "/brave" {
			braveCalls.Add(1)
			if r.Method != "GET" || r.Header.Get("X-Subscription-Token") != "brave-secret" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("q") == "" || r.URL.Query().Get("count") != "5" {
				t.Error("Brave request contract")
			}
			if failure.Load() {
				w.WriteHeader(429)
				fmt.Fprint(w, `{"error":"brave-secret"}`)
				return
			}
			fmt.Fprint(w, `{"web":{"results":[{"url":"https://example.test/brave","title":"Brave result","description":"Found something","age":"1 day ago"}]}}`)
			return
		}
		tavilyCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer tavily-secret" || r.Header.Get("X-Subscription-Token") != "" || body["max_results"] != float64(5) || body["search_depth"] != "basic" || body["api_key"] != nil {
			t.Error("Tavily request contract")
		}
		fmt.Fprint(w, `{"results":[{"url":"https://example.test/tavily","title":"Tavily result","content":"Search fallback"}]}`)
	}))
	defer provider.Close()
	oldEndpoints := webSearchEndpoints
	webSearchEndpoints = map[string]string{"brave": provider.URL + "/brave", "tavily": provider.URL + "/tavily"}
	defer func() { webSearchEndpoints = oldEndpoints }()
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", webSearchSetting)
	defer a.Redis.Del(ctx, webSearchQuotaKey("brave"), webSearchQuotaKey("tavily"))
	for _, route := range []struct{ method, suffix string }{{"GET", ""}, {"PUT", ""}, {"POST", "/test"}, {"POST", "/reset-usage"}} {
		if w := call(route.method, path+route.suffix, ordinary, map[string]any{}, ""); w.Code != 403 {
			t.Fatal("search admin boundary", route, w.Code)
		}
	}
	if got := must("GET", path, admin, nil); got["enabled"] != false || len(got["providers"].([]any)) != 0 {
		t.Fatal("search first boot", got)
	}
	for _, raw := range []any{nil, map[string]any{"providers": []any{map[string]any{"type": "other"}}}, map[string]any{"enabled": true, "providers": []any{map[string]any{"type": "brave"}}}, map[string]any{"providers": []any{map[string]any{"type": "brave", "proxy_id": 9999999}}}, map[string]any{"unknown": true}} {
		if w := call("PUT", path, admin, raw, ""); w.Code != 400 {
			t.Fatal("bad search configuration", w.Code, w.Body.String())
		}
	}
	limit := int64(100)
	cfg := webSearchConfig{Enabled: true, Providers: []webSearchProvider{{Type: "brave", APIKey: "brave-secret", QuotaLimit: &limit}, {Type: "tavily", APIKey: "tavily-secret"}}}
	for _, method := range []string{"PUT", "GET"} {
		w := call(method, path, admin, cfg, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "-secret") || !strings.Contains(w.Body.String(), `"api_key_configured":true`) {
			t.Fatal("secret returned", w.Code, w.Body.String())
		}
	}
	merge := cfg
	merge.Providers = append([]webSearchProvider(nil), cfg.Providers...)
	for i := range merge.Providers {
		merge.Providers[i].APIKey, merge.Providers[i].QuotaUsed = "", 999999
	}
	must("PUT", path, admin, merge)
	stored, err := loadWebSearch(ctx, a.DB)
	if err != nil || stored.Providers[0].APIKey != "brave-secret" || stored.Providers[0].QuotaUsed != 0 {
		t.Fatal("empty key merge or writable usage", err)
	}
	if got := must("POST", path+"/test", admin, map[string]string{"query": "private search"}); got["provider"] != "brave" {
		t.Fatal("admin test", got)
	}
	if n, err := a.webSearchUsage(ctx, "brave"); err != nil || n != 0 {
		t.Fatal("admin test consumed local quota", n, err)
	}
	modelProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		if r.Header.Get("X-Api-Key") != "model-secret" {
			t.Error("model credentials lost")
		}
		if strings.HasSuffix(r.URL.Path, "count_tokens") {
			fmt.Fprint(w, `{"input_tokens":5}`)
			return
		}
		fmt.Fprint(w, `{"id":"msg_native","type":"message","role":"assistant","model":"claude-search-test","content":[{"type":"text","text":"native"}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"end_turn"}`)
	}))
	defer modelProvider.Close()
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Search emulation", "platform": "anthropic", "rate_multiplier": 2}))
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "search-emulation@example.test", "password": "search-password", "balance": 100, "concurrency": 10}))
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "search-emulation@example.test", "password": "search-password"})["access_token"].(string)
	key := must("POST", "/api/v1/keys", token, map[string]any{"name": "search", "group_id": gid, "quota": 100})["key"].(string)
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "search-account", "platform": "anthropic", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "model-secret", "base_url": modelProvider.URL}, "extra": map[string]any{webSearchFeature: "enabled"}}))
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	prices := []any{map[string]any{"platform": "anthropic", "models": []string{"claude-search-test"}, "billing_mode": "token", "input_price": "0.01", "output_price": "0.02", "cache_read_price": "0", "cache_write_price": "0"}}
	cid := id(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "search-channel", "group_ids": []int64{gid}, "model_pricing": prices}))
	cpath := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	body := map[string]any{"model": "claude-search-test", "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": "private search"}}, "tools": []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}}
	w := call("POST", "/v1/messages", key, body, "search-one")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "web_search_tool_result") || modelCalls.Load() != 0 {
		t.Fatal("search intercept", w.Code, w.Body.String())
	}
	var tokens, n int64
	var cost string
	if err = a.DB.QueryRow("SELECT input_tokens+output_tokens,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&tokens, &cost); err != nil || tokens != 0 || cost != "0.0000000000" {
		t.Fatal("display tokens billed", tokens, cost, err)
	}
	before := braveCalls.Load()
	if w = call("POST", "/v1/messages", key, body, "search-one"); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || braveCalls.Load() != before {
		t.Fatal("search replay dispatched", w.Code)
	}
	body["stream"] = true
	w = call("POST", "/v1/messages", key, body, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: message_stop") || !strings.Contains(w.Body.String(), "srvtoolu_ws_") {
		t.Fatal("search SSE", w.Code, w.Body.String())
	}
	delete(body, "stream")
	// Count-only and mixed-tool requests follow the ordinary model branch.
	if w = call("POST", "/messages/count_tokens", key, body, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "input_tokens") {
		t.Fatal("count bypass", w.Code, w.Body.String())
	}
	originalTools := body["tools"]
	body["tools"] = append(originalTools.([]any), map[string]any{"name": "other", "input_schema": map[string]any{"type": "object"}})
	if w = call("POST", "/v1/messages", key, body, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "msg_native") {
		t.Fatal("mixed tools bypass", w.Code, w.Body.String())
	}
	body["tools"] = originalTools
	// Channel inheritance preserves other feature keys, and explicit disabled wins.
	if _, err = a.DB.Exec(`UPDATE channels SET features_config='{"future":{"keep":true}}' WHERE id=$1`, cid); err != nil {
		t.Fatal(err)
	}
	must("PUT", cpath, admin, map[string]any{"features_config": map[string]any{webSearchFeature: map[string]bool{"anthropic": true}}})
	must("PUT", apath, admin, map[string]any{"extra": map[string]any{webSearchFeature: false}})
	if w = call("POST", "/v1/messages", key, body, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "srvtoolu_ws_") {
		t.Fatal("legacy false channel inheritance", w.Code, w.Body.String())
	}
	if got := must("GET", cpath, admin, nil); got["features_config"].(map[string]any)["future"] == nil {
		t.Fatal("feature patch discarded unknown keys")
	}
	must("PUT", apath, admin, map[string]any{"extra": map[string]any{webSearchFeature: "disabled"}})
	if w = call("POST", "/v1/messages", key, body, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "msg_native") {
		t.Fatal("account disabled ignored", w.Code)
	}
	must("PUT", apath, admin, map[string]any{"extra": map[string]any{webSearchFeature: "enabled"}})
	cfg.Enabled = false
	must("PUT", path, admin, cfg)
	if w = call("POST", "/v1/messages", key, body, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "msg_native") {
		t.Fatal("global switch ignored", w.Code)
	}
	cfg.Enabled = true
	must("PUT", path, admin, cfg)
	// Explicit per-request pricing still charges once and replays without charge.
	must("PUT", cpath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "anthropic", "models": []string{"claude-search-test"}, "billing_mode": "per_request", "per_request_price": "0.02"}}})
	w = call("POST", "/v1/messages", key, body, "paid-search")
	if w.Code != 200 {
		t.Fatal("per-request search", w.Code, w.Body.String())
	}
	if err = a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost); err != nil || cost != "0.0400000000" {
		t.Fatal("search per-request billing", cost, err)
	}
	// Quota rejection/failure falls through to the other search provider only.
	before = modelCalls.Load()
	failure.Store(true)
	usedBefore, _ := a.webSearchUsage(ctx, "brave")
	if w = call("POST", "/v1/messages", key, body, "fallback-search"); w.Code != 200 || !strings.Contains(w.Body.String(), "Tavily result") || modelCalls.Load() != before {
		t.Fatal("provider fallback", w.Code, w.Body.String())
	}
	if used, _ := a.webSearchUsage(ctx, "brave"); used != usedBefore {
		t.Fatal("failed provider quota not rolled back", used, usedBefore)
	}
	cfg.Providers = cfg.Providers[:1]
	must("PUT", path, admin, cfg)
	w = call("POST", "/v1/messages", key, body, "failed-search")
	if w.Code != 503 || strings.Contains(w.Body.String(), "-secret") || modelCalls.Load() != before {
		t.Fatal("search failure exposed or fell back to LLM", w.Code, w.Body.String())
	}
	if err = a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed search billed", err)
	}
	var healthy bool
	if err = a.DB.QueryRow("SELECT status='active' AND schedulable AND rate_limit_reset_at IS NULL FROM accounts WHERE id=$1", aid).Scan(&healthy); err != nil || !healthy {
		t.Fatal("search failure marked model unhealthy", err)
	}
	failure.Store(false)
	// No-balance and forbidden model requests do not reach search.
	before = braveCalls.Load()
	if _, err = a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w = call("POST", "/v1/messages", key, body, "empty-search"); w.Code != 402 || braveCalls.Load() != before {
		t.Fatal("wallet gate", w.Code)
	}
	if _, err = a.DB.Exec("UPDATE users SET balance=100 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other-model"}}})
	if w = call("POST", "/v1/messages", key, body, "forbidden-search"); w.Code != 403 || braveCalls.Load() != before {
		t.Fatal("model gate", w.Code, w.Body.String())
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	// Atomic quota: only two searches may dispatch, even from concurrent requests.
	limit = 2
	must("PUT", path, admin, cfg)
	must("POST", path+"/reset-usage", admin, map[string]string{"provider_type": "brave"})
	var successes atomic.Int64
	var wg sync.WaitGroup
	before = braveCalls.Load()
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.runWebSearch(ctx, cfg, "quota race", nil, false); err == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if used, _ := a.webSearchUsage(ctx, "brave"); used != 2 || successes.Load() != 2 || braveCalls.Load()-before != 2 {
		t.Fatal("search quota race", used, successes.Load(), braveCalls.Load()-before)
	}
	if got := must("POST", path+"/test", admin, map[string]string{"query": "quota bypass probe"}); got["provider"] != "brave" {
		t.Fatal("admin probe should bypass quota", got)
	}
	// A rollback from before reset/expiry must never decrement the new cycle.
	epoch, _ := a.Redis.HGet(ctx, webSearchQuotaKey("brave"), "epoch").Result()
	must("POST", path+"/reset-usage", admin, map[string]string{"provider_type": "brave"})
	if _, err = a.runWebSearch(ctx, cfg, "new cycle", nil, false); err != nil {
		t.Fatal(err)
	}
	if err = webSearchRollback.Run(ctx, a.Redis, []string{webSearchQuotaKey("brave")}, epoch).Err(); err != nil {
		t.Fatal(err)
	}
	if used, _ := a.webSearchUsage(ctx, "brave"); used != 1 {
		t.Fatal("old rollback changed new cycle", used)
	}
	before = braveCalls.Load()
	expired := time.Now().Add(-time.Second).Unix()
	cfg.Providers[0].ExpiresAt = &expired
	if _, err = a.runWebSearch(ctx, cfg, "expired", nil, false); err == nil || braveCalls.Load() != before {
		t.Fatal("expired search dispatched")
	}
	cfg.Providers[0].ExpiresAt = nil
	// A configured provider proxy is honored and cannot be deleted while referenced.
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.Header.Get("X-Subscription-Token") != "brave-secret" {
			t.Error("proxy search key")
		}
		fmt.Fprint(w, `{"web":{"results":[]}}`)
	}))
	defer proxy.Close()
	hostPort := strings.TrimPrefix(proxy.URL, "http://")
	addr := netip.MustParseAddrPort(hostPort)
	pid := id(must("POST", "/api/v1/admin/proxies", admin, map[string]any{"name": "search-proxy", "protocol": "http", "host": addr.Addr().String(), "port": addr.Port()}))
	cfg.Providers[0].ProxyID = &pid
	must("PUT", path, admin, cfg)
	if w = call("DELETE", fmt.Sprintf("/api/v1/admin/proxies/%d", pid), admin, nil, ""); w.Code != 409 {
		t.Fatal("referenced search proxy deleted", w.Code)
	}
	must("POST", path+"/test", admin, map[string]string{"query": "proxy probe"})
	if proxyCalls.Load() != 1 || braveCalls.Load() != before {
		t.Fatal("search proxy bypass")
	}
	// An explicit account proxy takes precedence over the provider proxy.
	badID := int64(99999999)
	if _, err = a.runWebSearch(ctx, cfg, "unavailable account proxy", &badID, true); err == nil || proxyCalls.Load() != 1 || braveCalls.Load() != before {
		t.Fatal("failed account proxy silently became provider proxy/direct")
	}
	// Redis loss cannot silently bypass a configured quota.
	unavailable := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	defer unavailable.Close()
	fresh := &App{DB: a.DB, Redis: unavailable, privateUpstreams: a.privateUpstreams}
	if _, err = fresh.runWebSearch(ctx, cfg, "redis failure", nil, false); err == nil || braveCalls.Load() != before {
		t.Fatal("quota storage failure dispatched")
	}
	cfg.Providers[0].ProxyID = nil
	must("PUT", path, admin, cfg)
	must("DELETE", fmt.Sprintf("/api/v1/admin/proxies/%d", pid), admin, nil)
	if err = a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE path=$1 AND method='PUT'", path).Scan(&n); err != nil || n == 0 {
		t.Fatal("search config audit missing", err)
	}
	// Results and upstream errors never enter quota keys or model usage records.
	var leaked bool
	if err = a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM usage_logs WHERE user_id=$1 AND to_jsonb(usage_logs)::text LIKE '%private search%')", uid).Scan(&leaked); err != nil || leaked {
		t.Fatal("search query persisted in usage", err)
	}
}

func TestWebSearchTransport(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Query().Get("q") {
		case "redirect":
			http.Redirect(w, r, "/stolen", http.StatusFound)
		case "oversized":
			fmt.Fprint(w, strings.Repeat(" ", (2<<20)+1))
		case "invalid-url":
			fmt.Fprint(w, `{"web":{"results":[{"url":"javascript:bad"}]}}`)
		case "missing":
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, `{"web":{"results":[]}}`)
		}
	}))
	defer server.Close()
	old := webSearchEndpoints
	webSearchEndpoints = map[string]string{"brave": server.URL}
	defer func() { webSearchEndpoints = old }()
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	p := webSearchProvider{Type: "brave", APIKey: "test-secret"}
	for _, query := range []string{"redirect", "oversized", "invalid-url", "missing"} {
		before := calls.Load()
		if _, err := a.searchProvider(context.Background(), p, query, nil); err == nil || calls.Load() != before+1 {
			t.Fatal("bad provider response or followed redirect", query, err)
		}
	}
	if _, err := (&App{}).searchProvider(context.Background(), p, "private", nil); err == nil {
		t.Fatal("private provider allowed")
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS accepted") }))
	defer tls.Close()
	webSearchEndpoints["brave"] = tls.URL
	if _, err := a.searchProvider(context.Background(), p, "tls", nil); err == nil {
		t.Fatal("untrusted TLS accepted")
	}
	resp := webSearchMessages(&webSearchResponse{Query: "q", Results: []webSearchResult{}}, "claude-test", true)
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "No search results") || !strings.Contains(string(raw), "event: message_stop") {
		t.Fatal("empty search results response")
	}
}
