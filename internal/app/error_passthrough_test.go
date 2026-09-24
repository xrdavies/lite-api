package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestErrorRuleContracts(t *testing.T) {
	rule := errorRule{Name: "errors", Enabled: true, Mode: "any", Codes: []int{429}, Keywords: []string{"capacity", "OVERLOAD"}, Platforms: []string{"openai"}, PassthroughCode: true, PassthroughBody: true}
	if err := rule.validate(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		Mode, Platform, Body string
		Status               int
		Want                 bool
	}{
		{"any", "openai", "nothing", 429, true}, {"any", "openai", "Capacity", 400, true},
		{"any", "grok", "capacity", 429, false}, {"all", "openai", "OVERLOAD", 429, true},
		{"all", "openai", "nothing", 429, false}, {"all", "openai", "capacity", 400, false},
		{"any", "openai", strings.Repeat("x", 8192) + "capacity", 400, false},
	} {
		rule.Mode = test.Mode
		if rule.matches(test.Platform, test.Status, []byte(test.Body)) != test.Want {
			t.Fatal("matching", test)
		}
	}
	rule.Platforms, rule.Codes = nil, nil
	if !rule.matches("grok", 400, []byte("capacity")) {
		t.Fatal("single condition all")
	}
	rule.Keywords = nil
	if rule.matches("grok", 429, nil) || rule.validate() == nil {
		t.Fatal("empty rule")
	}
	for _, raw := range []string{
		`{"error":{"message":"bad upstream-key? https://example.test?api_key=hidden&x=1 bearer private-token"}}`,
		`{"detail":"bad upstream-key? password=hidden"}`, `{"message":"bad upstream-key%3F sk-private123456"}`,
		`{"error":{"message":"{\"error\":{\"message\":\"bad upstream-key? access_token=hidden\"}}"}}`,
	} {
		msg := upstreamErrorMessage([]byte(raw), "upstream-key?")
		if !strings.Contains(msg, "bad") || strings.Contains(msg, "upstream-key") || strings.Contains(msg, "hidden") || strings.Contains(msg, "private") {
			t.Fatal("message sanitization", msg)
		}
	}
	for _, raw := range []string{`<html>private error</html>`, `{"error":"private"}`, `null`, strings.Repeat("x", maxBalanceBody+1)} {
		if upstreamErrorMessage([]byte(raw), "") != "" {
			t.Fatal("non-message error exposed")
		}
	}
	wrapped := &passthroughError{&apiError{422, "mapped error"}, 400, true}
	for _, protocol := range []string{"anthropic", "gemini", "chat_completions", "responses"} {
		w := httptest.NewRecorder()
		textGatewayError(w, protocol, wrapped)
		if w.Code != 422 || !strings.Contains(w.Body.String(), "mapped error") || protocol != "gemini" && !strings.Contains(w.Body.String(), "upstream_error") {
			t.Fatal("protocol error", protocol, w.Code, w.Body.String())
		}
	}
	for _, status := range []int{200, 302, 600} {
		if (&App{}).upstreamError(context.Background(), nil, status, nil, wrapped) != wrapped {
			t.Fatal("non-error altered")
		}
	}
	// Config failure cannot authorize exposing a supplier body.
	db, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if (&App{DB: db}).upstreamError(context.Background(), &upstreamAccount{Platform: "openai"}, 400, []byte(`{"message":"private"}`), wrapped) != wrapped {
		t.Fatal("DB failure changed fallback")
	}
}

func TestBoundedUpstreamError(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.(http.Flusher).Flush()
		switch r.URL.Path {
		case "/slow":
			<-r.Context().Done()
		case "/large":
			fmt.Fprint(w, strings.Repeat("x", maxBalanceBody+100))
		default:
			fmt.Fprint(w, `{"message":"error"}`)
		}
	}))
	defer provider.Close()
	for _, path := range []string{"/slow", "/large", "/normal"} {
		resp, err := http.Get(provider.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		body := readUpstreamError(resp)
		resp.Body.Close()
		if time.Since(started) > 5*time.Second || len(body) > maxBalanceBody+1 || path == "/slow" && len(body) != 0 || path == "/normal" && !json.Valid(body) {
			t.Fatal("bounded read", path, len(body), time.Since(started))
		}
	}
	if readUpstreamError(&http.Response{Body: nil}) != nil {
		t.Fatal("empty body")
	}
}

func testErrorPassthrough(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	const root = "/api/v1/admin/error-passthrough-rules"
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var result struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return result.Data
	}
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	rule := manage("POST", root, admin, map[string]any{"name": "passthrough", "error_codes": []int{400}, "keywords": []string{"capacity"}, "match_mode": "all", "platforms": []string{"OPENAI"}, "passthrough_code": false, "response_code": 422, "passthrough_body": false, "custom_message": "Contact your team admin"})
	rid := id(rule)
	rpath := fmt.Sprintf("%s/%d", root, rid)
	defer a.DB.Exec("DELETE FROM error_passthrough_rules WHERE name LIKE 'passthrough%'")
	if rule["enabled"] != true || rule["skip_monitoring"] != false || len(rule["platforms"].([]any)) != 1 {
		t.Fatal("rule defaults", rule)
	}
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if w := call(m, rpath, ordinary, map[string]any{}, ""); w.Code != 403 {
			t.Fatal("ordinary rule access", m, w.Code)
		}
	}
	for _, m := range []string{"GET", "POST"} {
		if w := call(m, root, "", map[string]any{}, ""); w.Code != 401 {
			t.Fatal("anonymous rule access", w.Code)
		}
	}
	for _, patch := range []any{nil, map[string]any{"id": 3}, map[string]any{"error_codes": []int{200}}, map[string]any{"response_code": 200}, map[string]any{"name": ""}, map[string]any{"keywords": []string{" "}}, map[string]any{"platforms": []string{"antigravity"}}, map[string]any{"match_mode": "regex"}, map[string]any{"priority": 2147483648}} {
		if w := call("PUT", rpath, admin, patch, ""); w.Code != 400 {
			t.Fatal("invalid patch", patch, w.Code)
		}
	}
	changed := manage("PUT", rpath, admin, map[string]any{"keywords": []string{}, "custom_message": nil})
	if changed["custom_message"] != "Contact your team admin" || len(changed["keywords"].([]any)) != 0 {
		t.Fatal("partial update semantics", changed)
	}
	// Separate partial edits must not overwrite each other's fields.
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, patch := range []any{map[string]any{"description": "kept"}, map[string]any{"priority": -2}} {
		wg.Add(1)
		go func(v any) { defer wg.Done(); statuses <- call("PUT", rpath, admin, v, "").Code }(patch)
	}
	wg.Wait()
	for range 2 {
		if <-statuses != 200 {
			t.Fatal("concurrent patch failed")
		}
	}
	changed = manage("GET", rpath, admin, nil)
	if changed["description"] != "kept" || changed["priority"] != float64(-2) {
		t.Fatal("partial edit lost", changed)
	}
	firstMatch := id(manage("POST", root, admin, map[string]any{"name": "passthrough-priority", "priority": -3, "error_codes": []int{400}, "passthrough_code": false, "response_code": 418, "passthrough_body": false, "custom_message": "first matching rule"}))
	probe := &upstreamAccount{Platform: "openai"}
	fallback := bad("unchanged")
	for _, want := range []string{"first matching rule", "Contact your team admin"} {
		if got := a.upstreamError(ctx, probe, 400, nil, fallback); safeGatewayError(got) != want {
			t.Fatal("rule priority", got)
		}
		manage("PUT", fmt.Sprintf("%s/%d", root, firstMatch), admin, map[string]any{"priority": -2})
	}
	manage("DELETE", fmt.Sprintf("%s/%d", root, firstMatch), admin, nil)
	if a.upstreamError(ctx, nil, 400, nil, fallback) != fallback {
		t.Fatal("missing upstream matched a rule")
	}
	var calls, mode atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer passthrough-upstream-secret" && r.Header.Get("X-Api-Key") != "passthrough-upstream-secret" && r.Header.Get("X-Goog-Api-Key") != "passthrough-upstream-secret" {
			t.Error("provider credential")
		}
		m := mode.Load()
		if m == 5 && r.Method == "POST" {
			fmt.Fprint(w, `{"id":"error-task"}`)
			return
		}
		if (m == 5 || m == 6) && r.Method == "GET" {
			status := "queued"
			if m == 6 {
				status = "failed"
			}
			fmt.Fprintf(w, `{"id":"error-task","status":%q}`, status)
			return
		}
		if m == 6 && r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		if m == 2 {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"ok","model":"error-model","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
			return
		}
		status := 400
		if m == 1 || m == 3 {
			status = 503
		}
		if m == 4 {
			status = 401
		}
		w.WriteHeader(status)
		fmt.Fprint(w, `{"error":{"message":"capacity exhausted passthrough-upstream-secret api_key=provider-private"},"extra":"hidden body field"}`)
	}))
	defer provider.Close()
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "passthrough@example.test", "password": "passthrough-password", "balance": 10}))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "passthrough@example.test", "password": "passthrough-password"})["access_token"].(string)
	group := manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "passthrough", "platform": "openai", "allow_image_generation": true})
	key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "passthrough", "group_id": id(group), "quota": 10})["key"].(string)
	account := manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "passthrough", "platform": "openai", "type": "apikey", "group_ids": []int64{id(group)}, "credentials": map[string]any{"api_key": "passthrough-upstream-secret", "base_url": provider.URL}})
	aid := id(account)
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	prices := []any{}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok"} {
		prices = append(prices, map[string]any{"platform": platform, "models": []string{"error-model"}, "input_price": "0.001", "output_price": "0.001", "cache_read_price": "0.001", "cache_write_price": "0.001"})
	}
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "passthrough", "group_ids": []int64{id(group)}, "model_pricing": prices})
	body := map[string]any{"model": "error-model", "messages": []any{map[string]string{"role": "user", "content": "hi"}}}
	assertError := func(w *httptest.ResponseRecorder, status int, message string) {
		t.Helper()
		if w.Code != status || !strings.Contains(w.Body.String(), message) || strings.Contains(w.Body.String(), "passthrough-upstream-secret") || strings.Contains(w.Body.String(), "provider-private") || strings.Contains(w.Body.String(), "hidden body field") {
			t.Fatalf("error response %d want %d: %s", w.Code, status, w.Body.String())
		}
	}
	assertError(call("POST", "/v1/chat/completions", key, body, "mapped-replay"), 422, "Contact your team admin")
	before := calls.Load()
	w := call("POST", "/chat/completions", key, body, "mapped-replay")
	assertError(w, 422, "upstream_error")
	if calls.Load() != before || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("error replay dispatched")
	}
	var logs, usage int
	if err := a.DB.QueryRow("SELECT count(*) FROM ops_error_logs WHERE user_id=$1", uid).Scan(&logs); err != nil || logs != 2 {
		t.Fatal("error records", logs, err)
	}
	manage("PUT", rpath, admin, map[string]any{"passthrough_code": true, "passthrough_body": true, "skip_monitoring": true})
	assertError(call("POST", "/v1/chat/completions", key, body, ""), 400, "capacity exhausted")
	if err := a.DB.QueryRow("SELECT count(*) FROM ops_error_logs WHERE user_id=$1", uid).Scan(&logs); err != nil || logs != 2 {
		t.Fatal("skip_monitoring", logs, err)
	}
	// Same original 503 retry path, with the last supplier error applied only
	// after no more accounts remain. A mapped 4xx must not terminate retry early.
	manage("PUT", rpath, admin, map[string]any{"error_codes": []int{503}, "keywords": []string{"capacity"}, "match_mode": "all", "passthrough_code": false, "response_code": 409, "passthrough_body": false, "custom_message": "Please retry later"})
	mode.Store(1)
	before = calls.Load()
	assertError(call("POST", "/v1/chat/completions", key, body, ""), 409, "Please retry later")
	if calls.Load() != before+1 {
		t.Fatal("single account resent")
	}
	var cooling bool
	if err := a.DB.QueryRow("SELECT overload_until>now() FROM accounts WHERE id=$1", aid).Scan(&cooling); err != nil || !cooling {
		t.Fatal("original status did not cool account", err)
	}
	manage("POST", apath+"/recover-state", admin, nil)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if mode.Load() == 3 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"unmatched private body"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"ok","model":"error-model","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer second.Close()
	secondID := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "passthrough-success", "platform": "openai", "type": "apikey", "priority": 99, "group_ids": []int64{id(group)}, "credentials": map[string]string{"api_key": "second-secret", "base_url": second.URL}}))
	secondPath := fmt.Sprintf("/api/v1/admin/accounts/%d", secondID)
	before = calls.Load()
	if w := call("POST", "/v1/chat/completions", key, body, ""); w.Code != 200 || calls.Load() != before+2 {
		t.Fatal("mapped status stopped failover", w.Code, w.Body.String(), calls.Load()-before)
	}
	manage("POST", apath+"/recover-state", admin, nil)
	mode.Store(3)
	assertError(call("POST", "/v1/chat/completions", key, body, ""), 502, "upstream rejected")
	manage("PUT", secondPath, admin, map[string]any{"status": "inactive"})
	manage("POST", apath+"/recover-state", admin, nil)
	manage("PUT", rpath, admin, map[string]any{"error_codes": []int{400, 401}, "keywords": []string{}, "skip_monitoring": false, "platforms": []string{}})
	mode.Store(0)
	// Native, converted, count, media and SSE-admission paths share the rule.
	for _, test := range []struct {
		Platform, Path string
		Body           any
	}{
		{"openai", "/v1/responses", map[string]any{"model": "error-model", "input": "hi"}},
		{"openai", "/v1/chat/completions", map[string]any{"model": "error-model", "messages": body["messages"], "stream": true}},
		{"openai", "/v1/images/generations", map[string]any{"model": "gpt-image-1", "prompt": "hi"}},
		{"anthropic", "/v1/messages", map[string]any{"model": "error-model", "messages": body["messages"], "max_tokens": 20}},
		{"anthropic", "/v1/messages/count_tokens", map[string]any{"model": "error-model", "messages": body["messages"]}},
		{"gemini", "/v1beta/models/error-model:generateContent", map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}}},
		{"grok", "/v1/videos/generations", map[string]any{"model": "grok-imagine-video", "prompt": "hi"}},
	} {
		if _, err := a.DB.Exec("UPDATE accounts SET platform=$2 WHERE id=$1", aid, test.Platform); err != nil {
			t.Fatal(err)
		}
		if _, err := a.DB.Exec("UPDATE groups SET platform=$2 WHERE id=$1", id(group), test.Platform); err != nil {
			t.Fatal(err)
		}
		assertError(call("POST", test.Path, key, test.Body, ""), 409, "Please retry later")
	}
	// Realtime handshake rejection remains eligible without accepting a downstream socket.
	mode.Store(4)
	front := httptest.NewServer(a.Handler())
	defer front.Close()
	_, response, err := websocket.Dial(ctx, front.URL+"/v1/realtime", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}}})
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatal("realtime mapped handshake", response, err)
	}
	manage("POST", apath+"/recover-state", admin, nil)
	if _, err := a.DB.Exec("UPDATE accounts SET platform='openai' WHERE id=$1", aid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec("UPDATE groups SET platform='openai' WHERE id=$1", id(group)); err != nil {
		t.Fatal(err)
	}
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]string{"api_protocol": "responses"}, "extra": map[string]bool{"openai_apikey_responses_websockets_v2_enabled": true}})
	mode.Store(0)
	conn, _, err := websocket.Dial(ctx, front.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = conn.Write(readCtx, websocket.MessageText, []byte(`{"type":"response.create","model":"error-model","input":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	_, event, err := conn.Read(readCtx)
	if err != nil || !bytes.Contains(event, []byte("Please retry later")) || !bytes.Contains(event, []byte("upstream_error")) {
		t.Fatal("Responses error frame", string(event), err)
	}
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions", "openai_capabilities": []string{"seedance"}}})
	seedance := map[string]any{"model": "error-model", "content": []any{map[string]string{"type": "text", "text": "hi"}}}
	const tasks = "/api/v3/contents/generations/tasks"
	assertError(call("POST", tasks, key, seedance, ""), 409, "Please retry later")
	mode.Store(5)
	w = call("POST", tasks, key, seedance, "")
	var created struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.ID == "" {
		t.Fatal("Seedance accepted", w.Code, w.Body.String())
	}
	mode.Store(0)
	assertError(call("GET", tasks+"/"+created.ID, key, nil, ""), 409, "Please retry later")
	saved, err := a.loadVideoTask(ctx, created.ID)
	if err != nil || saved.Stage != "pending" || saved.Receipt != nil {
		t.Fatal("mapped query failure completed task", saved, err)
	}
	mode.Store(5)
	assertError(call("DELETE", tasks+"/"+created.ID, key, nil, ""), 409, "Please retry later")
	mode.Store(6)
	if w = call("DELETE", tasks+"/"+created.ID, key, nil, ""); w.Code != 200 {
		t.Fatal("task recovery after mapped rejection", w.Code, w.Body.String())
	}
	var balance string
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE id=$1", uid).Scan(&balance); err != nil || balance != "9.99700000" {
		t.Fatal("rejected calls charged", balance, err)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&usage); err != nil || usage != 1 {
		t.Fatal("rejected usage", usage, err)
	}
	var leaked bool
	if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM ops_error_logs WHERE user_id=$1 AND error_message LIKE '%capacity%')", uid).Scan(&leaked); err != nil || leaked {
		t.Fatal("provider body persisted", err)
	}
	// Delete and disable are immediately reflected without a cache invalidation race.
	manage("PUT", rpath, admin, map[string]any{"enabled": false})
	u, err := a.loadAccount(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	if a.upstreamError(ctx, u, 400, nil, fallback) != fallback {
		t.Fatal("disabled rule used")
	}
	manage("DELETE", rpath, admin, nil)
	if w := call("GET", rpath, admin, nil, ""); w.Code != 404 {
		t.Fatal("deleted rule", w.Code)
	}
	if w := call("GET", root, admin, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"data":[]`) {
		t.Fatal("empty list", w.Body.String())
	}
	var audited bool
	if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM audit_logs WHERE path=$1 AND method='PUT' AND status_code=200)", rpath).Scan(&audited); err != nil || !audited {
		t.Fatal("error rule mutation was not audited", err)
	}
}
