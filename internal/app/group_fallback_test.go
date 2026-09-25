package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClaudeClientPolicy(t *testing.T) {
	metadata, _ := json.Marshal(map[string]string{"user_id": `{"device_id":"device","session_id":"session"}`})
	body := map[string]json.RawMessage{"model": json.RawMessage(`"claude-sonnet"`), "max_tokens": json.RawMessage(`64`), "metadata": metadata}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	for _, field := range []string{"X-App", "Anthropic-Beta", "Anthropic-Version"} {
		r.Header.Set(field, "present")
	}
	for _, prompt := range []string{"You are Claude Code, Anthropic's official CLI for Claude.", "x-anthropic-billing-header: cc_version=2.1.220; cc_entrypoint=jetbrains;", "You are a Claude agent, built on Anthropic's Claude Agent SDK."} {
		body["system"], _ = json.Marshal([]any{map[string]string{"type": "text", "text": prompt}})
		if !claudeCodeClient(r, body) {
			t.Fatal("valid client denied", prompt)
		}
	}
	for _, header := range []string{"X-App", "Anthropic-Beta", "Anthropic-Version"} {
		r.Header.Del(header)
		if claudeCodeClient(r, body) {
			t.Fatal("missing client header accepted", header)
		}
		r.Header.Set(header, "present")
	}
	body["metadata"], _ = json.Marshal(map[string]string{"user_id": "user_" + strings.Repeat("a", 64) + "_account__session_12345678-1234-1234-1234-123456789012"})
	if !claudeCodeClient(r, body) {
		t.Fatal("legacy metadata rejected")
	}
	body["metadata"] = json.RawMessage(`{"user_id":"invalid"}`)
	if claudeCodeClient(r, body) {
		t.Fatal("invalid metadata accepted")
	}
	body["metadata"] = metadata
	security := "You are a security monitor for autonomous AI coding agents." + strings.Repeat("x", 10000) + "## Threat Model - `<transcript>`: ## HARD BLOCK ## SOFT BLOCK ## Classification Process ## Output Format <block>yes</block> <block>no</block>"
	body["system"], _ = json.Marshal([]any{map[string]string{"type": "text", "text": "context"}, map[string]string{"type": "text", "text": security}})
	if !claudeCodeClient(r, body) {
		t.Fatal("security monitor not recognized")
	}
	delete(body, "system")
	if claudeCodeClient(r, body) {
		t.Fatal("UA alone accepted Messages")
	}
	body["max_tokens"] = json.RawMessage(`1`)
	if !claudeCodeClient(r, body) {
		t.Fatal("probe rejected")
	}
	r.Header.Set("User-Agent", "forged claude-cli/2.1.220")
	if claudeCodeClient(r, body) {
		t.Fatal("invalid user agent accepted")
	}
	r.Header.Set("User-Agent", "CLAUDE-CLI/2.1.220")
	r.URL.Path = "/v1/messages/count_tokens"
	if !claudeCodeClient(r, nil) {
		t.Fatal("count helper rejected")
	}
	if promptSimilarity("aaa", "aaaa") != 0.8 || promptSimilarity("A  B", "a b") != 1 || promptSimilarity("", "x") != 0 {
		t.Fatal("bigram similarity changed")
	}
}

func testGroupFallback(t *testing.T, a *App, admin string) {
	call := func(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.100:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	check := func(w *httptest.ResponseRecorder, code int) {
		t.Helper()
		if w.Code != code {
			t.Fatalf("got %d want %d: %s", w.Code, code, w.Body.String())
		}
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, nil)
		check(w, 200)
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "fallback@example.test", "password": "fallback-password", "balance": 100, "concurrency": 1}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "fallback@example.test", "password": "fallback-password"})["access_token"].(string)
	price := func(input string) []any {
		return []any{map[string]any{"platform": "anthropic", "models": []string{"client-model"}, "input_price": input, "output_price": "0.002", "cache_write_price": "0", "cache_read_price": "0"}}
	}
	target := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Fallback target", "platform": "anthropic", "is_exclusive": true, "rate_multiplier": 9, "model_pricing": price("0.9")}))
	source := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Client source", "platform": "anthropic", "rate_multiplier": 2, "claude_code_only": true, "fallback_group_id": target, "model_pricing": price("0.001")}))
	sp, tp := fmt.Sprintf("/api/v1/admin/groups/%d", source), fmt.Sprintf("/api/v1/admin/groups/%d", target)
	keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "Fallback", "group_id": source, "quota": 100})
	key, kid := keyData["key"].(string), id(keyData)
	check(call("POST", "/api/v1/keys", user, map[string]any{"name": "Private", "group_id": target}, nil), 403)
	check(call("PUT", sp, user, map[string]any{"claude_code_only": false}, nil), 403)
	check(call("PUT", sp, admin, map[string]any{"fallback_group_id": source}, nil), 400)
	check(call("PUT", sp, admin, map[string]any{"fallback_group_id": 999999999}, nil), 400)
	check(call("PUT", tp, admin, map[string]any{"claude_code_only": true}, nil), 400)
	check(call("PUT", tp, admin, map[string]any{"fallback_group_id": source}, nil), 400)
	must("PUT", sp, admin, map[string]any{"fallback_group_id": nil, "description": "preserved"})
	if v := must("GET", sp, admin, nil); int64(v["fallback_group_id"].(float64)) != target {
		t.Fatal("null cleared fallback")
	}
	var calls atomic.Int32
	var failStatus atomic.Int32
	var pause atomic.Bool
	started, release := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct {
			Model  string
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "client-model" {
			t.Error("upstream request", body, err)
		}
		if pause.Swap(false) {
			started <- struct{}{}
			<-release
		}
		if status := failStatus.Load(); status != 0 {
			w.WriteHeader(int(status))
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			fmt.Fprint(w, `{"input_tokens":10}`)
		} else if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"model\":\"client-model\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", n)
		} else {
			fmt.Fprintf(w, `{"id":"msg_%d","type":"message","role":"assistant","model":"client-model","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, n)
		}
	}))
	defer func() { close(release); up.Close() }()
	account := func(gid int64, label string) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": label, "platform": "anthropic", "type": "apikey", "concurrency": 1, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "fallback-" + label, "base_url": up.URL}}))
	}
	sa, ta := account(source, "source"), account(target, "target")
	body := map[string]any{"model": "client-model", "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "ok"}}}
	request := func(headers map[string]string) *httptest.ResponseRecorder {
		return call("POST", "/v1/messages", key, body, headers)
	}
	assertUsage := func(w *httptest.ResponseRecorder, aid int64) {
		t.Helper()
		check(w, 200)
		var gotAccount, gotGroup int64
		var cost, rate string
		if err := a.DB.QueryRow(`SELECT account_id,group_id,actual_cost::text,rate_multiplier::text FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&gotAccount, &gotGroup, &cost, &rate); err != nil || gotAccount != aid || gotGroup != source || cost != "0.0400000000" || rate != "2.0000" {
			t.Fatal("fallback changed billing or account", gotAccount, gotGroup, cost, rate, err)
		}
	}
	assertUsage(request(nil), ta)
	// The target owns admission margins; the source owns the customer's rate.
	// A target default of 9 must not admit cost 1 when source 2 * (1-.6)=.8.
	must("PUT", tp, admin, map[string]any{"profit_control_enabled": true, "profit_min_margin": "0.6"})
	profitCalls := calls.Load()
	check(request(nil), 503)
	if calls.Load() != profitCalls {
		t.Fatal("fallback used target billing rate for profit")
	}
	must("PUT", sp, admin, map[string]any{"profit_control_enabled": true, "profit_min_margin": "0.9"})
	must("PUT", tp, admin, map[string]any{"profit_control_enabled": false})
	assertUsage(request(nil), ta)
	must("PUT", sp, admin, map[string]any{"profit_control_enabled": false})
	// The configured private target delegates account selection only.
	var storedGroup int64
	if err := a.DB.QueryRow("SELECT group_id FROM api_keys WHERE id=$1", kid).Scan(&storedGroup); err != nil || storedGroup != source {
		t.Fatal("fallback reassigned client key", err)
	}
	cli := map[string]string{"User-Agent": "claude-cli/2.1.220", "X-App": "cli", "Anthropic-Beta": "test-beta", "Anthropic-Version": "2023-06-01"}
	assertUsage(request(cli), ta) // UA without prompt/metadata is insufficient.
	body["system"] = []any{map[string]string{"type": "text", "text": "x-anthropic-billing-header: cc_entrypoint=cli;"}}
	body["metadata"] = map[string]string{"user_id": `{"device_id":"d","session_id":"s"}`}
	assertUsage(request(cli), sa)
	body["stream"] = true
	w := request(map[string]string{"Idempotency-Key": "fallback-stream"})
	assertUsage(w, ta)
	before := calls.Load()
	replay := request(map[string]string{"Idempotency-Key": "fallback-stream"})
	check(replay, 200)
	if calls.Load() != before || replay.Body.String() != w.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("fallback SSE replay invoked upstream")
	}
	delete(body, "stream")
	var usageBefore, usageAfter int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&usageBefore); err != nil {
		t.Fatal(err)
	}
	check(call("POST", "/v1/messages/count_tokens", key, body, cli), 200)
	check(call("POST", "/v1/messages/count_tokens", key, body, nil), 200)
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&usageAfter); err != nil || usageBefore != usageAfter {
		t.Fatal("count helper billed", usageBefore, usageAfter, err)
	}
	for _, path := range []string{"/v1/chat/completions", "/chat/completions", "/backend-api/codex/chat/completions", "/v1/responses", "/responses", "/backend-api/codex/responses", "/v1/embeddings"} {
		b := map[string]any{"model": "client-model", "input": "ok", "messages": body["messages"]}
		check(call("POST", path, key, b, cli), 403)
	}
	must("PUT", sp, admin, map[string]any{"fallback_group_id": 0})
	before = calls.Load()
	check(request(nil), 403)
	if calls.Load() != before {
		t.Fatal("restricted request dispatched")
	}
	assertUsage(request(cli), sa)
	must("PUT", sp, admin, map[string]any{"fallback_group_id": target})
	must("PUT", tp, admin, map[string]any{"status": "inactive"})
	check(request(nil), 503)
	must("PUT", tp, admin, map[string]any{"status": "active"})
	must("PUT", sp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other"}}})
	check(request(nil), 403)
	must("PUT", sp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	cid := id(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Fallback restriction", "group_ids": []int64{target}, "restrict_models": true, "model_pricing": []any{map[string]any{"platform": "anthropic", "models": []string{"other"}, "input_price": "0", "output_price": "0"}}}))
	check(request(nil), 403)
	cp := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	must("PUT", cp, admin, map[string]any{"model_pricing": price("0.8")})
	assertUsage(request(nil), ta) // target prices never replace source prices
	must("PUT", cp, admin, map[string]any{"billing_model_source": "upstream", "model_pricing": []any{map[string]any{"platform": "anthropic", "models": []string{"other"}, "input_price": "0", "output_price": "0"}}})
	check(request(nil), 503)
	must("PUT", cp, admin, map[string]any{"restrict_models": false})
	quotaPath := fmt.Sprintf("/api/v1/admin/users/%d/platform-quotas", uid)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "anthropic", "daily_limit_usd": 0}}})
	check(request(nil), 429)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "anthropic", "daily_limit_usd": 100}}})
	assertUsage(request(nil), ta)
	var platformUsed string
	if err := a.DB.QueryRow("SELECT daily_usage_usd::text FROM user_platform_quotas WHERE user_id=$1 AND platform='anthropic' AND deleted_at IS NULL", uid).Scan(&platformUsed); err != nil || platformUsed != "0.0400000000" {
		t.Fatal(platformUsed, err)
	}
	failStatus.Store(400)
	before = calls.Load()
	check(request(nil), 502)
	if calls.Load() != before+1 {
		t.Fatal("400 caused cross-group retry")
	}
	failStatus.Store(0)
	failStatus.Store(503)
	before = calls.Load()
	check(request(nil), 503)
	if calls.Load() != before+1 {
		t.Fatal("503 escaped the fallback target")
	}
	failStatus.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", ta), admin, map[string]any{})
	// Account waits must recheck the source relationship and target state.
	for _, tc := range []struct {
		path            string
		change, restore map[string]any
		want            int
	}{
		{tp, map[string]any{"status": "inactive"}, map[string]any{"status": "active"}, 503},
		{sp, map[string]any{"claude_code_only": false}, map[string]any{"claude_code_only": true}, 409},
	} {
		if !a.takeSlot("account", ta, 1) {
			t.Fatal("could not occupy target")
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- request(nil) }()
		waitForQueue(t, a, "account", ta, 1)
		before = calls.Load()
		must("PUT", tc.path, admin, tc.change)
		a.releaseSlot("account", ta)
		check(<-done, tc.want)
		if calls.Load() != before {
			t.Fatal("queued request ignored changed policy")
		}
		must("PUT", tc.path, admin, tc.restore)
	}
	if !a.takeSlot("user", uid, 1) {
		t.Fatal("could not occupy user")
	}
	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() { queued <- request(nil) }()
	waitForQueue(t, a, "user", uid, 1)
	must("PUT", sp, admin, map[string]any{"claude_code_only": false})
	a.releaseSlot("user", uid)
	check(<-queued, 409)
	must("PUT", sp, admin, map[string]any{"claude_code_only": true})
	// A dispatched response retains its source price snapshot across admin edits.
	pause.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- request(nil) }()
	<-started
	must("PUT", sp, admin, map[string]any{"rate_multiplier": 7})
	release <- struct{}{}
	assertUsage(<-done, ta)
	must("PUT", sp, admin, map[string]any{"rate_multiplier": 2})
	// A composite target resolves its own route, while quotas stay with the
	// original concrete platform. A zero quota on the target platform is unrelated.
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Fallback composite", "platform": "composite"}))
	ca := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Fallback kimi", "platform": "kimi", "type": "apikey", "group_ids": []int64{composite}, "credentials": map[string]any{"api_key": "fallback-kimi", "base_url": up.URL, "api_protocol": "anthropic"}}))
	routePath := fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite)
	check(call("POST", routePath, admin, map[string]any{"public_model": "client-model", "match_type": "exact", "target_platform": "kimi", "upstream_model": "client-model", "endpoint": "messages"}, nil), 201)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "anthropic", "daily_limit_usd": 100}, map[string]any{"platform": "kimi", "daily_limit_usd": 0}}})
	must("PUT", sp, admin, map[string]any{"fallback_group_id": composite})
	assertUsage(request(nil), ca)
	var kimiUsed string
	if err := a.DB.QueryRow("SELECT daily_usage_usd::text FROM user_platform_quotas WHERE user_id=$1 AND platform='kimi' AND deleted_at IS NULL", uid).Scan(&kimiUsed); err != nil || kimiUsed != "0.0000000000" {
		t.Fatal("fallback changed quota attribution", kimiUsed, err)
	}
	// Model routing belongs to the scheduling target, not the source group.
	must("PUT", sp, admin, map[string]any{"fallback_group_id": target, "model_routing_enabled": true, "model_routing": map[string][]int64{"client-model": {sa}}})
	tb := account(target, "target-preferred")
	must("PUT", tp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"client-model": {tb}}})
	assertUsage(request(nil), tb)
	must("PUT", tp, admin, map[string]any{"model_routing_enabled": false})
	// Concurrent graph writes cannot create a cycle.
	x := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Graph x"}))
	y := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Graph y"}))
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for from, to := range map[int64]int64{x: y, y: x} {
		wg.Add(1)
		go func(from, to int64) {
			defer wg.Done()
			codes <- call("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", from), admin, map[string]any{"fallback_group_id": to}, nil).Code
		}(from, to)
	}
	wg.Wait()
	c1, c2 := <-codes, <-codes
	if c1+c2 != 600 {
		t.Fatal("concurrent cycle", c1, c2)
	}
	must("DELETE", tp, admin, nil)
	check(request(nil), 503)
	// A dangling target must not prevent an administrator from disabling or
	// editing the source while repairing the relationship.
	must("PUT", sp, admin, map[string]any{"claude_code_only": false})
	assertUsage(request(nil), sa)
	must("PUT", sp, admin, map[string]any{"claude_code_only": true})
	must("PUT", sp, admin, map[string]any{"fallback_group_id": -1})
	check(request(nil), 403)
}
