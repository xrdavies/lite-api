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

func TestCompositeResolution(t *testing.T) {
	route := func(id int64, public, match, endpoint, platform, upstream string, priority int) compositeRoute {
		return compositeRoute{ID: id, compositeRouteConfig: compositeRouteConfig{PublicModel: public, MatchType: match, Endpoint: endpoint, TargetPlatform: platform, UpstreamModel: upstream, Priority: priority, Enabled: true}}
	}
	c := &compositeConfig{Owners: map[string]string{"alias": "deepseek", "gpt-ambiguous": "", "gpt-relay": "grok"}, Routes: []compositeRoute{
		route(7, "alias", "exact", "any", "openai", "routed", 100),
		route(6, "alias", "exact", "messages", "anthropic", "claude-routed", 200),
		route(1, "al", "prefix", "messages", "grok", "prefix", 1),
		route(3, "family-", "prefix", "any", "openai", "", 100),
		route(4, "family-long-", "prefix", "any", "deepseek", "", 200),
		route(8, "family-", "prefix", "responses", "kimi", "fixed", 300),
		route(11, "gpt-disabled", "exact", "any", "grok", "disabled", 100),
		route(12, "gpt-excluded", "exact", "any", "bedrock", "excluded", 100),
	}}
	c.Routes[6].Enabled = false
	for _, tc := range []struct{ model, endpoint, platform, upstream, source string }{
		{"alias", "chat_completions", "openai", "routed", "route"},
		{"alias", "messages", "anthropic", "claude-routed", "route"},
		{"family-long-test", "chat_completions", "deepseek", "family-long-test", "route"},
		{"family-long-test", "responses", "kimi", "fixed", "route"},
		{"gpt-relay", "chat_completions", "grok", "gpt-relay", "account_model"},
		{"gpt-disabled", "chat_completions", "openai", "gpt-disabled", "detector"},
		{"models/gemini-model", "gemini", "gemini", "models/gemini-model", "detector"},
		{"moonshot/k3", "responses", "kimi", "moonshot/k3", "detector"},
	} {
		d := c.resolve(12, tc.model, tc.endpoint)
		if !d.Matched || d.TargetPlatform != tc.platform || d.UpstreamModel != tc.upstream || d.Source != tc.source || d.PublicModel != tc.model || d.GroupID != 12 {
			t.Fatal("composite resolution", tc, d)
		}
	}
	for _, name := range []string{"unknown", "gpt-ambiguous", "gpt-excluded", "bedrock/claude-test", "antigravity/gpt-test", "opencode_go/gpt-test"} {
		if d := c.resolve(12, name, "chat_completions"); d.Matched {
			t.Fatal("unsafe fallback", name, d)
		}
	}
	a, b := route(2, "a", "prefix", "any", "openai", "", 10), route(3, "a", "prefix", "any", "openai", "", 10)
	if !betterCompositeRoute(&a, &b, "messages") {
		t.Fatal("ID tie-break")
	}
	b.Priority = 1
	if betterCompositeRoute(&a, &b, "messages") {
		t.Fatal("priority ordering")
	}
	for name, platform := range map[string]string{"claude-test": "anthropic", "gpt-test": "openai", "o3-mini": "openai", "gemini-test": "gemini", "grok": "grok", "k3-256k": "kimi", "glm-test": "zhipu", "deepseek-test": "deepseek", "MiniMax-M2": "minimax"} {
		if got := detectModelPlatform(name); got != platform {
			t.Fatal(name, got, platform)
		}
	}
}

func testCompositeGateway(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.94:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
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
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	expect := func(status int, method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		w := call(method, path, token, body, "")
		if w.Code != status {
			t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	uid := int64(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "composite@example.test", "password": "composite-password", "balance": "10"})["id"].(float64))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "composite@example.test", "password": "composite-password"})["access_token"].(string)
	gid := int64(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite", "platform": "composite", "rate_multiplier": "0.5"})["id"].(float64))
	group := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	routes := group + "/composite-routes"
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Composite", "group_id": gid})["key"].(string)
	key2 := must("POST", "/api/v1/keys", user, map[string]any{"name": "Other composite key", "group_id": gid})["key"].(string)
	var calls atomic.Int64
	var mode atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if mode.CompareAndSwap(2, 3) {
				started <- struct{}{}
				<-release
			}
			// Explicit mappings support relays without model discovery.
			w.WriteHeader(403)
			return
		}
		n := calls.Add(1)
		platform := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer upstream-")
		if r.Header.Get("X-Api-Key") != "" {
			platform = strings.TrimPrefix(r.Header.Get("X-Api-Key"), "upstream-")
		}
		if r.Header.Get("X-Goog-Api-Key") != "" {
			platform = strings.TrimPrefix(r.Header.Get("X-Goog-Api-Key"), "upstream-")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		model := credentialString(body, "model")
		if platform == "gemini" {
			if !strings.Contains(r.URL.Path, "/models/gemini-final:") {
				t.Error("Gemini route rewrite", r.URL.Path)
			}
		} else if model != platform+"-final" && model != "passthrough-one" {
			t.Error("route/channel/account rewrite", platform, model)
		}
		if !supportedPlatform(platform) || r.Header.Get("Authorization") == "Bearer "+key {
			t.Error("composite upstream credential isolation", platform)
		}
		w.Header().Set("Content-Type", "application/json")
		if mode.Load() == 1 {
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			fmt.Fprint(w, `{"input_tokens":10}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			fmt.Fprint(w, `{"totalTokens":10}`)
			return
		}
		if r.URL.Path == "/v1/embeddings" {
			fmt.Fprintf(w, `{"object":"list","data":[{"embedding":[0.1,0.2],"index":0}],"model":%q,"usage":{"prompt_tokens":10,"total_tokens":10}}`, model)
			return
		}
		if r.URL.Path == "/v1/responses" {
			fmt.Fprintf(w, `{"id":"resp_composite_%d","object":"response","status":"completed","model":%q,"output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n, model)
			return
		}
		if platform == "anthropic" {
			fmt.Fprintf(w, `{"id":"msg_composite","type":"message","model":%q,"content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, model)
			return
		}
		if platform == "gemini" {
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
			return
		}
		response := fmt.Sprintf(`{"id":"chat","model":%q,"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, model)
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", response)
		} else {
			fmt.Fprint(w, response)
		}
	}))
	defer func() { close(release); up.Close() }()
	platforms := []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"}
	accountIDs := map[string]int64{}
	prices := []any{}
	aliases := []string{"alias-*", "shared", "shared-fixed", "passthrough-*", "count-alias", "resp-alias", "gpt-direct", "gpt-ambiguous", "unknown", "excluded"}
	for _, platform := range platforms {
		protocol := "chat_completions"
		if platform == "anthropic" || platform == "gemini" {
			protocol = platform
		}
		mapping := map[string]string{platform + "-internal": platform + "-final"}
		if platform == "openai" {
			mapping["openai-channel"] = "openai-final"
			mapping["passthrough-*"] = ""
			mapping["gpt-direct"] = "openai-final"
			mapping["resp-internal"] = "openai-final"
		}
		if platform == "deepseek" || platform == "openai" {
			mapping["shared"], mapping["gpt-ambiguous"] = platform+"-final", platform+"-final"
		}
		credentials := map[string]any{"api_key": "upstream-" + platform, "base_url": up.URL, "api_protocol": protocol, "model_mapping": mapping}
		if platform == "gemini" {
			delete(credentials, "api_protocol")
		}
		accountIDs[platform] = int64(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Composite " + platform, "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": credentials})["id"].(float64))
		prices = append(prices, map[string]any{"platform": platform, "models": []string{"*"}, "billing_mode": "per_request", "per_request_price": "0.1"})
	}
	cid := int64(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Composite", "group_ids": []int64{gid}, "model_mapping": map[string]any{"openai": map[string]string{"openai-internal": "openai-channel"}}, "model_pricing": prices})["id"].(float64))
	must("PUT", group, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": aliases}})
	route := func(public, platform, upstream, endpoint, match string) map[string]any {
		return map[string]any{"public_model": public, "target_platform": platform, "upstream_model": upstream, "endpoint": endpoint, "match_type": match}
	}
	routeIDs := map[string]int64{}
	for _, platform := range platforms {
		value := route("alias-"+platform, platform, platform+"-internal", "any", "exact")
		routeIDs[platform] = int64(must("POST", routes, admin, value)["id"].(float64))
	}
	var listed struct{ Data []compositeRoute }
	if w := expect(200, "GET", routes, admin, nil); json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Data) != len(platforms) {
		t.Fatal("composite route list", w.Body.String())
	}
	for i, route := range listed.Data {
		if route.ID != routeIDs[route.TargetPlatform] || route.GroupID != gid || route.PublicModel != "alias-"+route.TargetPlatform || i > 0 && route.ID <= listed.Data[i-1].ID {
			t.Fatal("composite list scope or order", route)
		}
	}
	expect(401, "GET", routes, "", nil)
	expect(403, "GET", routes, user, nil)
	expect(403, "POST", routes, user, route("forbidden", "openai", "", "any", "exact"))
	expect(409, "POST", routes, admin, route("alias-openai", "openai", "", "any", "exact"))
	for _, platform := range []string{"bedrock", "composite", "unknown"} {
		expect(400, "POST", routes, admin, route("excluded", platform, "", "any", "exact"))
	}
	expect(400, "POST", routes, admin, route("invalid*", "openai", "", "any", "exact"))
	expect(400, "POST", routes, admin, route("invalid", "openai", "", "typo", "exact"))
	foreign := int64(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Other composite", "platform": "composite"})["id"].(float64))
	foreignPath := fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes/%d", foreign, routeIDs["openai"])
	expect(404, "DELETE", foreignPath, admin, nil)
	expect(404, "PUT", foreignPath, admin, route("alias-openai", "openai", "", "any", "exact"))
	plain := int64(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Not composite", "platform": "openai"})["id"].(float64))
	expect(400, "POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", plain), admin, route("no", "openai", "", "any", "exact"))
	// Preview follows real account ownership and makes no upstream call.
	for _, model := range []string{"shared", "gpt-ambiguous", "unknown"} {
		d := must("POST", routes+"/preview", admin, map[string]any{"model": model})
		if d["matched"] != false {
			t.Fatal("ambiguous/unknown preview", model, d)
		}
	}
	if d := must("POST", routes+"/preview", admin, map[string]any{"model": "gpt-direct"}); d["source"] != "account_model" || d["target_platform"] != "openai" {
		t.Fatal("account ownership preview", d)
	}
	if calls.Load() != 0 {
		t.Fatal("preview called upstream")
	}
	chat := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "ok"}}, "stream": stream}
	}
	for _, platform := range platforms {
		path, body := "/v1/chat/completions", chat("alias-"+platform, false)
		if platform == "anthropic" {
			path, body["max_tokens"] = "/v1/messages", 10
		}
		if platform == "gemini" {
			path = "/v1beta/models/alias-gemini:generateContent"
			body = map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]any{"text": "ok"}}}}}
		}
		w := expect(200, "POST", path, key, body)
		var groupID, accountID int64
		var requested, upstream, cost, billed string
		wantBilled := platform + "-internal"
		if platform == "openai" {
			wantBilled = "openai-channel"
		}
		if err := a.DB.QueryRow("SELECT group_id,account_id,requested_model,upstream_model,actual_cost::text,model FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&groupID, &accountID, &requested, &upstream, &cost, &billed); err != nil || groupID != gid || accountID != accountIDs[platform] || requested != "alias-"+platform || upstream != platform+"-final" || cost != "0.0500000000" || billed != wantBilled {
			t.Fatal("composite billing/routing", platform, groupID, accountID, requested, upstream, cost, billed, err)
		}
	}
	before := calls.Load()
	for _, model := range []string{"shared", "gpt-ambiguous", "unknown"} {
		expect(404, "POST", "/v1/chat/completions", key, chat(model, false))
	}
	expect(403, "POST", "/v1/chat/completions", key, chat("openai-internal", false))
	if calls.Load() != before {
		t.Fatal("invalid composite model reached upstream")
	}
	// A database enum still includes retired platforms. Such a route must block
	// dispatch rather than fall back to a recognizable model name.
	if _, err := a.DB.Exec("INSERT INTO composite_model_routes(group_id,public_model,target_platform) VALUES($1,'gpt-direct','bedrock')", gid); err == nil {
		t.Fatal("unexpected schema enum")
	}
	if _, err := a.DB.Exec("INSERT INTO composite_model_routes(group_id,public_model,target_platform) VALUES($1,'gpt-direct','antigravity')", gid); err != nil {
		t.Fatal(err)
	}
	expect(404, "POST", "/v1/chat/completions", key, chat("gpt-direct", false))
	if _, err := a.DB.Exec("DELETE FROM composite_model_routes WHERE group_id=$1 AND public_model='gpt-direct'", gid); err != nil {
		t.Fatal(err)
	}
	// An explicit route disambiguates account ownership; endpoint-specific exact
	// routes beat an any-endpoint route without changing its other protocols.
	must("POST", routes, admin, route("shared-fixed", "deepseek", "shared", "any", "exact"))
	w := expect(200, "POST", "/v1/chat/completions", key, chat("shared-fixed", false))
	var selected int64
	if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&selected); err != nil || selected != accountIDs["deepseek"] {
		t.Fatal("explicit route lost to ownership", selected, err)
	}
	endpointRoute := int64(must("POST", routes, admin, route("alias-openai", "anthropic", "anthropic-internal", "messages", "exact"))["id"].(float64))
	message := chat("alias-openai", false)
	message["max_tokens"] = 10
	w = expect(200, "POST", "/v1/messages", key, message)
	if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&selected); err != nil || selected != accountIDs["anthropic"] {
		t.Fatal("endpoint route ignored", selected, err)
	}
	must("DELETE", fmt.Sprintf("%s/%d", routes, endpointRoute), admin, nil)
	// Empty prefix target preserves the concrete client model; requested price remains public.
	must("POST", routes, admin, route("passthrough-", "openai", "", "any", "prefix"))
	w = expect(200, "POST", "/v1/chat/completions", key, chat("passthrough-one", false))
	var billing string
	if err := a.DB.QueryRow("SELECT model FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&billing); err != nil || billing != "passthrough-one" {
		t.Fatal("prefix collapsed", billing, err)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"billing_model_source": "requested"})
	w = expect(200, "POST", "/v1/chat/completions", key, chat("alias-openai", false))
	if err := a.DB.QueryRow("SELECT model FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&billing); err != nil || billing != "alias-openai" {
		t.Fatal("public requested price lost", billing, err)
	}
	// Explicit platform quota is applied after resolving a composite group.
	quotaPath := fmt.Sprintf("/api/v1/admin/users/%d/platform-quotas", uid)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "deepseek", "daily_limit_usd": 0}}})
	before = calls.Load()
	expect(429, "POST", "/v1/chat/completions", key, chat("alias-deepseek", false))
	if calls.Load() != before {
		t.Fatal("resolved quota bypassed")
	}
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "deepseek", "daily_limit_usd": "1"}}})
	expect(200, "POST", "/v1/chat/completions", key, chat("alias-deepseek", false))
	var spent string
	if err := a.DB.QueryRow("SELECT daily_usage_usd::text FROM user_platform_quotas WHERE user_id=$1 AND platform='deepseek' AND deleted_at IS NULL", uid).Scan(&spent); err != nil || spent != "0.0500000000" {
		t.Fatal("concrete quota charge", spent, err)
	}
	// Token counting resolves its own endpoint without charging.
	must("POST", routes, admin, route("count-alias", "anthropic", "anthropic-internal", "count_tokens", "exact"))
	countBody := chat("count-alias", false)
	w = expect(200, "POST", "/v1/messages/count_tokens", key, countBody)
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 0 {
		t.Fatal("count-only request charged", count, err)
	}
	expect(404, "POST", "/v1/messages", key, map[string]any{"model": "count-alias", "messages": countBody["messages"], "max_tokens": 10})
	expect(200, "POST", "/embeddings", key, map[string]any{"model": "alias-openai", "input": "hello"})
	expect(400, "POST", "/embeddings", key, map[string]any{"model": "alias-deepseek", "input": "hello"})
	// Same idempotent SSE result remains replayable after the route changes.
	streamBody := chat("alias-openai", true)
	w = call("POST", "/v1/chat/completions", key, streamBody, "composite-stream")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("composite SSE", w.Code, w.Body.String())
	}
	first := w.Body.String()
	openaiRoutePath := fmt.Sprintf("%s/%d", routes, routeIDs["openai"])
	must("PUT", openaiRoutePath, admin, route("alias-openai", "deepseek", "deepseek-internal", "any", "exact"))
	before = calls.Load()
	w = call("POST", "/chat/completions", key, streamBody, "composite-stream")
	if w.Header().Get("Idempotency-Replayed") != "true" || w.Body.String() != first || calls.Load() != before {
		t.Fatal("route edit invalidated safe replay", w.Code, w.Body.String())
	}
	must("PUT", openaiRoutePath, admin, route("alias-openai", "openai", "openai-internal", "any", "exact"))
	// Existing response affinity cannot switch platform after a route edit.
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", accountIDs["openai"]), admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	respRoute := int64(must("POST", routes, admin, route("resp-alias", "openai", "resp-internal", "responses", "exact"))["id"].(float64))
	responseBody := map[string]any{"model": "resp-alias", "input": "ok"}
	w = expect(200, "POST", "/responses", key, responseBody)
	var response struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	responseBody["previous_response_id"] = response.ID
	expect(404, "POST", "/responses", key2, responseBody)
	expect(200, "POST", "/responses", key, responseBody)
	before = calls.Load()
	must("PUT", fmt.Sprintf("%s/%d", routes, respRoute), admin, route("resp-alias", "deepseek", "deepseek-internal", "responses", "exact"))
	expect(503, "POST", "/responses", key, responseBody)
	if calls.Load() != before {
		t.Fatal("continuation switched platform")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", accountIDs["openai"]), admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
	// Platform errors never fall through to another provider, even for a known alias.
	mode.Store(1)
	before = calls.Load()
	expect(503, "POST", "/v1/chat/completions", key, chat("alias-openai", false))
	if calls.Load() != before+1 {
		t.Fatal("cross-platform retry", calls.Load(), before)
	}
	mode.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", accountIDs["openai"]), admin, nil)
	// Model lists expose public route identities, not private mapped targets or ambiguous aliases.
	w = expect(200, "GET", "/v1/models", key, nil)
	for _, model := range []string{"alias-openai", "alias-anthropic", "alias-gemini", "alias-deepseek", "gpt-direct"} {
		if !strings.Contains(w.Body.String(), `"id":"`+model+`"`) {
			t.Fatal("missing composite catalog model", model, w.Body.String())
		}
	}
	for _, hidden := range []string{`"id":"shared"`, `"id":"gpt-ambiguous"`, "openai-final", "openai-internal", "upstream-openai"} {
		if strings.Contains(w.Body.String(), hidden) {
			t.Fatal("composite model disclosure", hidden, w.Body.String())
		}
	}
	w = expect(200, "GET", "/v1beta/models", key, nil)
	if !strings.Contains(w.Body.String(), "models/alias-gemini") || strings.Contains(w.Body.String(), "alias-openai") {
		t.Fatal("native composite catalog", w.Body.String())
	}
	mode.Store(2)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("GET", "/v1/models", key, nil, "") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model query did not start")
	}
	must("PUT", openaiRoutePath, admin, route("alias-openai", "deepseek", "deepseek-internal", "any", "exact"))
	release <- struct{}{}
	select {
	case result := <-done:
		if result.Code != 409 {
			t.Fatal("mixed routing snapshot returned", result.Code, result.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("model query did not finish")
	}
	mode.Store(0)
	// Deletion removes a route immediately and keeps another group's routes inaccessible.
	must("DELETE", openaiRoutePath, admin, nil)
	expect(404, "POST", "/v1/chat/completions", key, chat("alias-openai", false))
	expect(201, "POST", routes, admin, route("alias-openai", "openai", "openai-internal", "any", "exact"))
	must("PUT", group, admin, map[string]any{"status": "inactive"})
	expect(403, "POST", "/v1/chat/completions", key, chat("alias-openai", false))
}
