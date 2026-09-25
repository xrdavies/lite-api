package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlphaSearchContract(t *testing.T) {
	for _, tc := range []struct {
		price                      *json.Number
		rate, total, actual, debit string
	}{
		{nil, "2", "0.0100000000", "0.0200000000", "0.02000000"},
		{number("0"), "2", "0.0000000000", "0.0000000000", "0.00000000"},
		{number("0.12345678"), "0.3333", "0.1234567800", "0.0411481448", "0.04114814"},
	} {
		cost, err := alphaSearchCost(tc.price, json.Number(tc.rate))
		if err != nil || cost.Total != tc.total || cost.Actual != tc.actual || cost.Debit != tc.debit {
			t.Fatal("search decimal billing", cost, err)
		}
	}
	for _, raw := range []string{`{"model":"m","stream":true}`, `{"model":"m","stream":null}`, `{"model":4}`, `{"model":"*"}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if _, err := parseTextRequest(httptest.NewRequest("POST", "/alpha/search", nil), "alpha_search", body); err == nil {
			t.Fatal("invalid search accepted", raw)
		}
	}
	o := textObservation{Protocol: "alpha_search"}
	for _, raw := range []string{`null`, `[]`, `{"error":{"message":"no"}}`, `broken`} {
		if o.observe([]byte(raw)) == nil || o.HasUsage {
			t.Fatal("invalid search charged", raw)
		}
	}
	if err := o.observe([]byte(`{"results":[]}`)); err != nil || !o.HasUsage || o.Usage.Requests != 1 || o.Usage.Input != 0 {
		t.Fatal("successful search missing call count", o, err)
	}
}

func testAlphaSearch(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-private-cookie")
		r.Header.Set("X-Codex-Beta-Features", "do-not-forward")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var envelope struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "search@example.test", "password": "search-password", "balance": 10}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "search@example.test", "password": "search-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Search", "platform": "openai", "rate_multiplier": 2}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Search", "group_id": gid, "quota": 10, "rate_limit_5h": 10})
	key, kid := k["key"].(string), id(k)
	var calls, primaryStatus, changePrice atomic.Int32
	const output = `{"id":"search-result","results":[{"url":"https://example.test/result","title":"Result"}],"future_field":{"a":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/alpha/search" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Codex-Beta-Features") != "" {
			t.Error("search header/path isolation", r.URL.Path)
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "upstream-search" || body["commands"] == nil || body["prompt_cache_key"] != nil || body["store"] != nil || body["prompt_cache_retention"] != nil {
			t.Error("search body changed", body)
		}
		if r.Header.Get("Authorization") != "Bearer search-upstream" && r.Header.Get("Authorization") != "Bearer search-backup" {
			t.Error("upstream credential mismatch")
		}
		if r.Header.Get("Authorization") == "Bearer search-upstream" && primaryStatus.Load() != 0 {
			w.WriteHeader(int(primaryStatus.Load()))
			_, _ = fmt.Fprint(w, `{"error":"secret upstream error"}`)
			return
		}
		if changePrice.Swap(0) == 1 {
			if _, err := a.DB.Exec("UPDATE groups SET web_search_price_per_call=0.2 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "search-upstream-id")
		_, _ = fmt.Fprint(w, output)
	}))
	defer upstream.Close()
	account := func(name, protocol, secret string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "openai", "type": "apikey", "priority": priority, "group_ids": []int64{gid}, "rate_multiplier": 3, "extra": map[string]any{"upstream_request_id_header": "X-Request-ID", "quota_limit": 100}, "credentials": map[string]any{"api_key": secret, "base_url": upstream.URL + "/v1", "api_protocol": protocol, "model_mapping": map[string]string{"public-search": "upstream-search"}}}))
	}
	aid := account("Search", "chat_completions", "search-upstream", 1)
	body := map[string]any{"model": "public-search", "id": "search-session", "commands": []any{map[string]any{"type": "search", "query": "team query"}}, "prompt_cache_key": "private-cache", "store": true, "prompt_cache_retention": "24h"}
	check := func(w *httptest.ResponseRecorder, total, actual string) {
		t.Helper()
		if w.Code != 200 || w.Body.String() != output {
			t.Fatal("search forwarding", w.Code, w.Body.String())
		}
		var gotTotal, gotActual, mode, endpoint, requestID string
		var tokens int64
		err := a.DB.QueryRow(`SELECT total_cost::text,actual_cost::text,billing_mode,upstream_endpoint,upstream_request_id,input_tokens+output_tokens+cache_read_tokens+cache_creation_tokens FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&gotTotal, &gotActual, &mode, &endpoint, &requestID, &tokens)
		if err != nil || gotTotal != total || gotActual != actual || mode != "per_request" || endpoint != "/v1/alpha/search" || requestID != "search-upstream-id" || tokens != 0 {
			t.Fatal("search accounting", gotTotal, gotActual, mode, endpoint, tokens, err)
		}
	}
	first := call("POST", "/v1/alpha/search", key, body, "one-search")
	check(first, "0.0100000000", "0.0200000000")
	for _, path := range []string{"/alpha/search", "/backend-api/codex/alpha/search"} {
		w := call("POST", path, key, body, "one-search")
		if w.Code != 200 || w.Body.String() != output || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("search alias replay", w.Code, w.Body.String(), calls.Load())
		}
	}
	var balance, used, window, accountUsed string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text,k.usage_5h::text,a.extra->>'quota_used' FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts a ON a.id=$3 WHERE u.id=$1 AND k.id=$2`, uid, kid, aid).Scan(&balance, &used, &window, &accountUsed); err != nil || balance != "9.98000000" || used != "0.02000000" || window != "0.02000000" || rat(json.Number(accountUsed)).Cmp(rat("0.03")) != 0 {
		t.Fatal("search counters", balance, used, window, accountUsed, err)
	}
	for _, price := range []any{"bad", 0.123456789} {
		if w := call("PUT", gp, admin, map[string]any{"web_search_price_per_call": price}, ""); w.Code != 400 {
			t.Fatal("invalid price accepted", w.Code)
		}
	}
	if w := call("PUT", gp, user, map[string]any{"web_search_price_per_call": 0}, ""); w.Code != 403 {
		t.Fatal("user set price", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"web_search_price_per_call": 0.12345678})
	must("PUT", gp+"/rate-multipliers", admin, map[string]any{"entries": []any{map[string]any{"user_id": uid, "rate_multiplier": 0.3333}}})
	changePrice.Store(1)
	check(call("POST", "/alpha/search", key, body, ""), "0.1234567800", "0.0411481448")
	check(call("POST", "/alpha/search", key, body, ""), "0.2000000000", "0.0666600000")
	must("PUT", gp, admin, map[string]any{"web_search_price_per_call": 0})
	must("PUT", gp, admin, map[string]any{"web_search_price_per_call": nil})
	check(call("POST", "/alpha/search", key, body, ""), "0.0000000000", "0.0000000000")
	must("PUT", gp, admin, map[string]any{"web_search_price_per_call": -1})
	check(call("POST", "/alpha/search", key, body, ""), "0.0100000000", "0.0033330000")
	// A text price is not needed for standalone search, and cannot override it.
	cid := id(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Search prices", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"public-search"}, "billing_mode": "per_request", "per_request_price": 9}}}))
	check(call("POST", "/alpha/search", key, body, ""), "0.0100000000", "0.0033330000")
	before := calls.Load()
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other"}}})
	if w := call("POST", "/alpha/search", key, body, ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed allowlist", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}, "claude_code_only": true})
	if w := call("POST", "/alpha/search", key, body, ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed client restriction", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"claude_code_only": false})
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"restrict_models": true, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"other"}, "input_price": 0}}})
	if w := call("POST", "/alpha/search", key, body, ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed channel restriction", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"restrict_models": false})
	backup := account("Search backup", "responses", "search-backup", 2)
	for _, status := range []int32{401, 404, 405, 429, 503} {
		primaryStatus.Store(status)
		must("PUT", gp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string]any{"public-search": []int64{aid}}})
		check(call("POST", "/alpha/search", key, body, ""), "0.0100000000", "0.0033330000")
		var logged int64
		var state string
		if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE user_id=$1 ORDER BY id DESC LIMIT 1", uid).Scan(&logged); err != nil || logged != backup {
			t.Fatal("search failover", logged, err)
		}
		if err := a.DB.QueryRow("SELECT status FROM accounts WHERE id=$1", aid).Scan(&state); err != nil || state != "active" {
			t.Fatal("endpoint failure disabled whole account", state, err)
		}
		must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	}
	primaryStatus.Store(0)
	// Settlement retries never send the third-party request again.
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_search_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/alpha/search", key, body, "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_search_receipt"); err != nil {
		t.Fatal(err)
	}
	if failed.Code != 503 {
		t.Fatal("settlement failure not surfaced", failed.Code, failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND billing_mode='per_request' AND actual_cost=0.003333", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("search recovery replayed/duplicated", count, err)
	}
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite search", "platform": "composite", "web_search_price_per_call": 0.05}))
	key2 := must("POST", "/api/v1/keys", user, map[string]any{"name": "Composite search", "group_id": composite})["key"].(string)
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	route := must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-search", "target_platform": "openai", "match_type": "exact", "endpoint": "responses", "enabled": true})
	check(call("POST", "/alpha/search", key2, body, ""), "0.0500000000", "0.0500000000")
	quotaPath := fmt.Sprintf("/api/v1/admin/users/%d/platform-quotas", uid)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": 0}}})
	before = calls.Load()
	if w := call("POST", "/alpha/search", key2, body, ""); w.Code != 429 || calls.Load() != before {
		t.Fatal("composite search bypassed platform quota", w.Code, w.Body.String())
	}
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": 10}}})
	check(call("POST", "/alpha/search", key2, body, ""), "0.0500000000", "0.0500000000")
	var platformUsage string
	if err := a.DB.QueryRow("SELECT daily_usage_usd::text FROM user_platform_quotas WHERE user_id=$1 AND platform='openai'", uid).Scan(&platformUsage); err != nil || rat(json.Number(platformUsage)).Cmp(rat("0.05")) != 0 {
		t.Fatal("search platform accounting", platformUsage, err)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes/%d", composite, id(route)), admin, map[string]any{"public_model": "public-search", "target_platform": "grok", "match_type": "exact", "endpoint": "responses", "enabled": true})
	before = calls.Load()
	if w := call("POST", "/alpha/search", key2, body, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("search dispatched non-OpenAI composite", w.Code)
	}
	if w := call("POST", "/alpha/search", user, body, ""); w.Code != 401 || calls.Load() != before {
		t.Fatal("JWT accepted as gateway key", w.Code)
	}
	// Queued search observes the same policy snapshot as the other gateway paths.
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", backup), admin, map[string]any{"schedulable": false})
	if !a.takeSlot("account", aid, 1) || !a.takeSlot("account", aid, 2) || !a.takeSlot("account", aid, 3) {
		t.Fatal("occupy search account")
	}
	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() { queued <- call("POST", "/alpha/search", key, body, "") }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, waiting := a.concurrencySnapshot()
		if waiting[fmt.Sprintf("account:%d", aid)] > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("search did not queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must("PUT", gp, admin, map[string]any{"web_search_price_per_call": 0.02})
	for i := 0; i < 3; i++ {
		a.releaseSlot("account", aid)
	}
	select {
	case w := <-queued:
		if w.Code != 409 || calls.Load() != before {
			t.Fatal("search queued through price change", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search queue did not wake")
	}
	if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/alpha/search", key, body, ""); w.Code != 402 || calls.Load() != before {
		t.Fatal("search bypassed balance", w.Code)
	}
	if w := call("POST", "/alpha/search", key, body, "one-search"); w.Code != 200 || w.Body.String() != output || calls.Load() != before {
		t.Fatal("completed replay recharged exhausted user", w.Code)
	}
	if strings.Contains(first.Body.String(), "search-upstream") {
		t.Fatal("credential leak")
	}
}
