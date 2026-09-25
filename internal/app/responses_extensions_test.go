package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResponseExtensionPaths(t *testing.T) {
	for _, paths := range [][]string{
		{"summarize", "v2/analyze", "{response_id}/revise", "compare/{response_id}/{response_id}"},
		{}, {"literal.v2/run"},
	} {
		if err := validateResponseExtensions(paths); err != nil {
			t.Fatal(paths, err)
		}
	}
	for _, paths := range [][]string{
		{""}, {"/summarize"}, {"summarize/"}, {"a//b"}, {"a/.."}, {"a/%2e"}, {"中文"},
		{"compact"}, {"{response_id}/cancel"}, {"input_tokens/v2"}, {"a/input_items"},
		{"x{response_id}"}, {"{id}/run"}, {"a/b/c/d/e/f/g/h/i"}, {strings.Repeat("a", 102)},
		{"run", "run"}, {"run", "{response_id}"}, {"{response_id}/run", "owned/run"},
		{"left/{response_id}", "{response_id}/right"}, make([]string, 65),
	} {
		if err := validateResponseExtensions(paths); err == nil {
			t.Fatal("invalid extension configuration accepted", paths)
		}
	}
	ctx := context.WithValue(context.Background(), responseExtensionsKey{}, []string{"summarize", "compare/{response_id}/{response_id}"})
	for _, action := range []string{"summarize", "compare/resp_one/resp_two"} {
		for _, stream := range []bool{false, true} {
			r := httptest.NewRequest("POST", "/responses/"+action, nil).WithContext(ctx)
			r.SetPathValue("action", action+"/")
			body := map[string]json.RawMessage{"input": json.RawMessage(`"request"`), "background": json.RawMessage(`true`), "tools": json.RawMessage(`[{"type":"tool_search"}]`)}
			in, err := parseResponsesRequest(r, textRequest{Protocol: "responses", Stream: stream}, body)
			if err != nil || !in.ResponseExtension || !in.Background || in.Action != "/"+action || len("gateway."+in.Scope+".9223372036854775807") > 128 || !in.HostedToolSearch {
				t.Fatal("registered extension lost normal request semantics", action, in, err)
			}
			if action != "summarize" && (len(in.ResponseResources) != 2 || in.ResponseResources[0] != "resp_one" || in.ResponseResources[1] != "resp_two") {
				t.Fatal("path resources not captured", in)
			}
		}
	}
	for _, action := range []string{"Summarize", "other", "compare/foreign.with.dot/resp_two", "compare/resp_one", "compare/resp_one/../resp_two"} {
		if _, ok := matchResponseExtension(ctx, action); ok {
			t.Fatal("unregistered extension accepted", action)
		}
	}
	for _, action := range []string{"compare/" + strings.Repeat("a", 80) + "/" + strings.Repeat("b", 30), "summarize//nested"} {
		r := httptest.NewRequest("POST", "/responses/"+action, nil).WithContext(ctx)
		r.SetPathValue("action", action)
		if _, err := parseResponsesRequest(r, textRequest{Protocol: "responses"}, map[string]json.RawMessage{"input": json.RawMessage(`"request"`)}); err == nil {
			t.Fatal("unsafe or oversized expanded path accepted", action)
		}
	}
	// Registering extensions never changes the built-in compact/count contracts.
	for _, action := range []string{"compact", "input_tokens", "resp_owned/compact"} {
		r := httptest.NewRequest("POST", "/responses/"+action, nil).WithContext(ctx)
		r.SetPathValue("action", action)
		if _, err := parseResponsesRequest(r, textRequest{Protocol: "responses", Stream: true}, map[string]json.RawMessage{"input": json.RawMessage(`"request"`)}); err == nil {
			t.Fatal("extension settings enabled streaming on built-in operation", action)
		}
	}
}

func testResponseExtensions(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", responseExtensionsSetting)
	call := func(method, path, token, idem string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.211:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-cookie")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, "", body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Response extensions", "platform": "openai", "is_exclusive": true}))
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "extensions@example.test", "password": "extensions-password", "balance": 10, "allowed_groups": []int64{gid}}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "extensions@example.test", "password": "extensions-password"})["access_token"].(string)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "extensions", "group_id": gid})
	key, kid := k["key"].(string), id(k)
	otherKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "other extensions", "group_id": gid})["key"].(string)
	price := map[string]any{"platform": "openai", "models": []string{"extension-model"}, "input_price": "0.000001", "output_price": "0.000002", "cache_read_price": "0.0000001"}
	must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Response extension prices", "group_ids": []int64{gid}, "model_pricing": []any{price}})
	var calls, otherCalls, mode atomic.Int32
	var mu sync.Mutex
	results := map[string]json.RawMessage{}
	paths := []string{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer extension-provider" || r.Header.Get("Cookie") != "" {
			t.Error("extension credentials leaked")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			mu.Lock()
			raw := results[strings.TrimPrefix(r.URL.Path, "/v1/responses/")]
			mu.Unlock()
			if raw == nil {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(raw)
			return
		}
		n := calls.Add(1)
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "native-extension-model" {
			t.Error("extension model not mapped")
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if mode.Load() == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"private extension error"}`)
			return
		}
		responseID := fmt.Sprintf("resp_ext_%d", n)
		usage := `{"input_tokens":2,"output_tokens":3}`
		if mode.Load() == 2 {
			usage = "null"
		}
		raw := json.RawMessage(fmt.Sprintf(`{"id":%q,"object":"response","status":"completed","model":"native-extension-model","output":[{"type":"message","id":"msg_ext_%d","role":"assistant","content":[{"type":"output_text","text":"9007199254740993"}]}],"usage":%s}`, responseID, n, usage))
		mu.Lock()
		results[responseID] = raw
		mu.Unlock()
		if string(body["background"]) == "true" {
			if string(body["stream"]) == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":%q,\"object\":\"response\",\"status\":\"queued\"}}\n\n", responseID)
			} else {
				fmt.Fprintf(w, `{"id":%q,"object":"response","status":"queued"}`, responseID)
			}
			return
		}
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"object\":\"response\",\"status\":\"in_progress\"}}\n\n", responseID)
			fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", raw)
			return
		}
		_, _ = w.Write(raw)
	}))
	defer provider.Close()
	account := func(name, url string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "priority": priority, "credentials": map[string]any{"base_url": url, "api_key": "extension-provider", "api_protocol": "responses", "model_mapping": map[string]string{"extension-model": "native-extension-model"}}}))
	}
	aid := account("Extensions", provider.URL+"/v1", 1)
	body := map[string]any{"model": "extension-model", "input": "extension prompt"}
	const settingPath = "/api/v1/admin/settings"
	configured := []string{"summarize", "v2/analyze", "{response_id}/revise", "compare/{response_id}/{response_id}"}
	if w := call("POST", "/responses/summarize", key, "", body); w.Code != 404 || calls.Load() != 0 {
		t.Fatal("unregistered path dispatched", w.Code)
	}
	if w := call("PUT", settingPath, user, "", map[string]any{responseExtensionsSetting: configured}); w.Code != 403 {
		t.Fatal("user configured extensions", w.Code)
	}
	must("PUT", settingPath, admin, map[string]any{responseExtensionsSetting: configured})
	for _, patch := range []map[string]any{{}, {responseExtensionsSetting: nil}} {
		if got := must("PUT", settingPath, admin, patch)[responseExtensionsSetting]; string(mustJSON(got)) != string(mustJSON(configured)) {
			t.Fatal("omitted/null extension settings changed", got)
		}
	}
	if w := call("GET", "/api/v1/settings/public", "", "", nil); w.Code != 200 || strings.Contains(w.Body.String(), responseExtensionsSetting) {
		t.Fatal("extension configuration leaked publicly", w.Code)
	}
	site := must("GET", settingPath, admin, nil)["site_name"]
	if w := call("PUT", settingPath, admin, "", map[string]any{"site_name": "should roll back", responseExtensionsSetting: []string{"{response_id}/revise", "owned/revise"}}); w.Code != 400 || must("GET", settingPath, admin, nil)["site_name"] != site {
		t.Fatal("invalid extension settings partially applied", w.Code)
	}
	exec("ALTER TABLE settings ADD CONSTRAINT test_extensions_failure CHECK(key<>'responses_extension_paths') NOT VALID")
	defer a.DB.Exec("ALTER TABLE settings DROP CONSTRAINT IF EXISTS test_extensions_failure")
	if w := call("PUT", settingPath, admin, "", map[string]any{"site_name": "should roll back", responseExtensionsSetting: []string{"alternate"}}); w.Code != 500 {
		t.Fatal("settings SQL failure not reported", w.Code)
	}
	exec("ALTER TABLE settings DROP CONSTRAINT test_extensions_failure")
	if must("GET", settingPath, admin, nil)["site_name"] != site {
		t.Fatal("settings SQL failure partially committed")
	}
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, suffix := range []string{"/summarize", "/summarize/"} {
			w := call("POST", prefix+suffix, key, "extension-once", body)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"resp_ext_1"`) || calls.Load() != 1 {
				t.Fatal("extension alias replay", w.Code, w.Body.String())
			}
		}
	}
	checkUsage := func(requestID, endpoint, want string) {
		t.Helper()
		var n int
		var cost, upstream string
		if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text,min(upstream_endpoint) FROM usage_logs WHERE request_id=$1", requestID).Scan(&n, &cost, &upstream); err != nil || n != 1 || cost != want || upstream != endpoint {
			t.Fatal("extension receipt", n, cost, upstream, err)
		}
	}
	w := call("POST", "/responses/v2/analyze", key, "extension-once", body)
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatal("different extension replayed", w.Code)
	}
	checkUsage(w.Header().Get("X-Request-ID"), "/v1/responses/v2/analyze", "0.0000080000")
	if w := call("POST", "/responses", key, "", map[string]any{"model": "extension-model", "previous_response_id": "resp_ext_1", "input": []any{map[string]any{"type": "item_reference", "id": "msg_ext_1"}}}); w.Code != 200 {
		t.Fatal("extension output affinity not bound", w.Code, w.Body.String())
	}
	// Extensions retain normal auth/model admission and cannot fall back to a
	// conversion that silently discards the requested action.
	before := calls.Load()
	for _, token := range []string{"", user} {
		if w := call("POST", "/responses/summarize", token, "", body); w.Code != 401 || calls.Load() != before {
			t.Fatal("extension accepted non-Key identity", w.Code)
		}
	}
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	for _, protocol := range []string{"chat_completions", "anthropic"} {
		must("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_protocol": protocol}})
		if w := call("POST", "/responses/summarize", key, "", body); w.Code != 503 || calls.Load() != before {
			t.Fatal("extension dispatched through protocol conversion", protocol, w.Code)
		}
	}
	must("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	must("PUT", gpath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other-model"}}})
	if w := call("POST", "/responses/summarize", key, "", body); w.Code != 403 || calls.Load() != before {
		t.Fatal("extension bypassed model allowlist", w.Code)
	}
	must("PUT", gpath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherCalls.Add(1); w.WriteHeader(503) }))
	defer other.Close()
	otherID := account("Preferred extension source", other.URL, 0)
	for _, path := range []string{"/responses/resp_ext_1/revise", "/responses/compare/resp_ext_1/resp_ext_2"} {
		w := call("POST", path, key, "", map[string]any{"model": "extension-model"})
		if w.Code != 200 || otherCalls.Load() != 0 {
			t.Fatal("resource extension affinity", w.Code, w.Body.String())
		}
	}
	before = calls.Load()
	for _, path := range []string{"/responses/resp_ext_1/revise", "/responses/compare/resp_ext_1/resp_unknown"} {
		if w := call("POST", path, otherKey, "", body); w.Code != 404 || calls.Load() != before || otherCalls.Load() != 0 {
			t.Fatal("foreign resource extension", w.Code)
		}
	}
	if w := call("POST", "/responses/compare/resp_ext_1/resp_unknown", key, "", body); w.Code != 404 || calls.Load() != before {
		t.Fatal("second resource escaped authorization", w.Code)
	}
	identity := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	u, err := a.loadAccount(t.Context(), aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []responseBinding{{AccountID: otherID, Target: "other"}, {AccountID: aid, Target: responseTarget(u), History: "converted"}, {AccountID: aid, Target: responseTarget(u), FileIDs: []string{"file_revoked"}}} {
		raw, _ := json.Marshal(source)
		if err := a.Redis.Set(t.Context(), responseBindingKey(identity, "resp_restricted"), raw, 0).Err(); err != nil {
			t.Fatal(err)
		}
		if w := call("POST", "/responses/compare/resp_ext_1/resp_restricted", key, "", body); w.Code < 400 || calls.Load() != before || otherCalls.Load() != 0 {
			t.Fatal("extension source restrictions lost", w.Code)
		}
	}
	// Path resources carry the same tool/container restrictions as body history.
	if err := a.storeResponseBinding(t.Context(), identity, "resp_container", responseBinding{AccountID: aid, Target: responseTarget(u), CodeTool: true, Containers: []string{"cntr_extension"}}); err != nil {
		t.Fatal(err)
	}
	codeBody := map[string]any{"model": "extension-model", "tools": []any{map[string]any{"type": "code_interpreter", "container": "cntr_extension"}}}
	for _, path := range []string{"/responses/resp_ext_1/revise", "/responses/resp_container/revise"} {
		w := call("POST", path, key, "", codeBody)
		if path == "/responses/resp_ext_1/revise" {
			if w.Code != 404 || calls.Load() != before {
				t.Fatal("extension borrowed an unrelated container", w.Code)
			}
			continue
		}
		var result struct{ ID string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("owned container extension failed", w.Code, w.Body.String())
		}
		binding, err := a.previousResponse(t.Context(), identity, result.ID)
		if err != nil || binding == nil || !binding.CodeTool || !slices.Equal(binding.Containers, []string{"cntr_extension"}) {
			t.Fatal("extension dropped inherited tool restrictions", binding, err)
		}
	}
	body["previous_response_id"] = "resp_ext_1"
	body["stream"] = true
	w = call("POST", "/responses/summarize", key, "extension-stream", body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.completed") || strings.Contains(w.Body.String(), "event: error") {
		t.Fatal("stream extension", w.Code, w.Body.String())
	}
	checkUsage(w.Header().Get("X-Request-ID"), "/v1/responses/summarize", "0.0000080000")
	delete(body, "stream")
	mode.Store(1)
	before = calls.Load()
	if w := call("POST", "/responses/summarize", key, "extension-rejected", body); w.Code != 502 || calls.Load() != before+1 || otherCalls.Load() != 0 {
		t.Fatal("extension rejection retried", w.Code, w.Body.String())
	}
	mode.Store(0)
	exec("UPDATE accounts SET overload_until=NULL,rate_limit_reset_at=NULL WHERE id=$1", aid)
	// No resource binding: even here a rejection must not retry another account.
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", otherID), admin, map[string]any{"priority": 2})
	exec("UPDATE account_groups SET priority=2 WHERE account_id=$1 AND group_id=$2", otherID, gid)
	mode.Store(1)
	before = calls.Load()
	if w := call("POST", "/responses/summarize", key, "unbound-rejection", map[string]any{"model": "extension-model", "input": "request"}); w.Code != 502 || calls.Load() != before+1 || otherCalls.Load() != 0 {
		t.Fatal("unbound extension retried another source", w.Code, calls.Load(), before, otherCalls.Load())
	}
	mode.Store(0)
	exec("UPDATE accounts SET overload_until=NULL,rate_limit_reset_at=NULL WHERE id=$1", aid)
	mode.Store(2)
	if w := call("POST", "/responses/summarize", key, "", body); w.Code != 502 {
		t.Fatal("extension without usage accepted", w.Code)
	}
	mode.Store(0)
	exec("ALTER TABLE usage_logs ADD CONSTRAINT test_extensions_usage CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID")
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_extensions_usage")
	w = call("POST", "/responses/summarize", key, "extension-settlement", body)
	if w.Code != 503 || strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal("extension result before settlement", w.Code)
	}
	exec("ALTER TABLE usage_logs DROP CONSTRAINT test_extensions_usage")
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	checkUsage(w.Header().Get("X-Request-ID"), "/v1/responses/summarize", "0.0000080000")
	before = calls.Load()
	if w := call("POST", "/responses/summarize/", key, "extension-settlement", body); w.Code != 503 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
		t.Fatal("extension settlement retried upstream", w.Code)
	}
	body["background"], body["store"] = true, true
	w = call("POST", "/responses/v2/analyze", key, "extension-background", body)
	var accepted struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" {
		t.Fatal("background extension acceptance", w.Code, w.Body.String())
	}
	backgroundRequestID := w.Header().Get("X-Request-ID")
	body["stream"] = true
	streamed := call("POST", "/responses/summarize", key, "extension-background-stream", body)
	// The provider closes after acceptance; report the interruption and retain
	// the accepted task for recovery instead of sending another POST.
	if streamed.Code != 200 || !strings.Contains(streamed.Body.String(), "event: response.created") || !strings.Contains(streamed.Body.String(), "poll the response for recovery") {
		t.Fatal("background extension interruption lost recovery", streamed.Code, streamed.Body.String())
	}
	delete(body, "stream")
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"rate_multiplier": 9})
	must("PUT", settingPath, admin, map[string]any{responseExtensionsSetting: []string{}})
	exec("ALTER TABLE usage_logs ADD CONSTRAINT test_extensions_usage CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID")
	if w := call("GET", "/responses/"+accepted.ID, key, "", nil); w.Code != 503 || strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal("background extension published before settlement", w.Code)
	}
	exec("ALTER TABLE usage_logs DROP CONSTRAINT test_extensions_usage")
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	for i := 0; i < 2; i++ {
		if err := fresh.runBackgroundResponses(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if w := call("GET", "/responses/"+accepted.ID, key, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) {
		t.Fatal("background extension recovery", w.Code, w.Body.String())
	}
	checkUsage(backgroundRequestID, "/v1/responses/v2/analyze", "0.0000080000")
	checkUsage(streamed.Header().Get("X-Request-ID"), "/v1/responses/summarize", "0.0000080000")
	delete(body, "background")
	delete(body, "store")
	before = calls.Load()
	if w := call("POST", "/responses/summarize", key, "", body); w.Code != 404 || calls.Load() != before {
		t.Fatal("revoked extension dispatched", w.Code)
	}
	must("PUT", settingPath, admin, map[string]any{responseExtensionsSetting: configured})
	exec("UPDATE accounts SET credentials=jsonb_set(credentials,'{api_key}','\"rotated\"') WHERE id=$1", aid)
	if w := call("POST", "/responses/resp_ext_1/revise", key, "", body); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("rotated extension source used", w.Code)
	}
	exec("UPDATE settings SET value='bad json' WHERE key=$1", responseExtensionsSetting)
	if w := call("POST", "/responses/summarize", key, "", body); w.Code != 503 || calls.Load() != before {
		t.Fatal("corrupt extension settings permitted request", w.Code)
	}
	var reconciled bool
	if err := a.DB.QueryRow("SELECT (10-balance)=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&reconciled); err != nil || !reconciled {
		t.Fatal("extension balance reconciliation", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 2 || paths[0] != "/v1/responses/summarize" || paths[1] != "/v1/responses/v2/analyze" {
		t.Fatal("extension upstream paths lost", paths)
	}
}
