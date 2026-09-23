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
)

const responseUsage = `"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":3},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":6},"total_tokens":28}`

func TestResponsesProtocol(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed"} {
		o := textObservation{Protocol: "responses"}
		raw := `{"type":"response.` + status + `","response":{"object":"response","id":"resp_test","model":"test","status":"` + status + `",` + responseUsage + `}}`
		err := o.observe([]byte(raw))
		if (err != nil) != (status == "failed") || !o.HasUsage || o.Usage.Input != 12 || o.Usage.CacheRead != 5 || o.Usage.CacheWrite != 3 || o.Usage.Output != 8 {
			t.Fatal("Responses terminal usage", status, o, err)
		}
		if status != "failed" && (!o.complete() || o.ResponseID != "resp_test") {
			t.Fatal("missing Responses terminal", o)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"type":"error","message":"secret"}`, `{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"type":"response.completed","response":{}}}`,
		`{"type":"response.completed","response":{"object":"response","id":"resp_test","status":"incomplete"}}`,
		`{"object":"response","id":"bad/id","status":"completed"}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":8,"input_tokens_details":{"cached_tokens":19,"cache_write_tokens":3}}}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":8,"output_tokens_details":{"reasoning_tokens":9}}}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":null}}`,
	} {
		o := textObservation{Protocol: "responses"}
		if err := o.observe([]byte(raw)); err == nil {
			t.Fatal("invalid response accepted", raw)
		}
	}
	u, err := parseChatUsage([]byte(`{"prompt_tokens":20,"completion_tokens":8,"cache_creation_input_tokens":15,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":5}}`))
	if err != nil || u.Input != 15 || u.CacheWrite != 0 {
		t.Fatal("explicit zero cache-write precedence", u, err)
	}
}

func testResponses(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-cookie")
		r.Header.Set("X-Api-Key", "client-header-key")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		t.Helper()
		w := call(path, token, body, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	var calls, mode atomic.Int32
	var otherCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer responses-upstream" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.URL.RawQuery != "" {
			t.Error("Responses credentials leaked or incorrect upstream credentials")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["model"]) != `"upstream-responses"` {
			t.Error("Responses model mapping")
		}
		if body["stream_options"] != nil {
			t.Error("Chat options injected into Responses")
		}
		if r.Header.Get("X-Codex-Beta-Features") != "" {
			var items []map[string]json.RawMessage
			if r.Header.Get("X-Codex-Beta-Features") != "remote_compaction_v2" || json.Unmarshal(body["input"], &items) != nil || len(items) != 2 || credentialString(items[0], "role") != "user" || credentialString(items[1], "type") != "compaction_trigger" {
				t.Error("native compaction input or negotiation")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "responses-request")
		if r.URL.Path == "/v1/responses/input_tokens" {
			_, _ = fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":20}`)
			return
		}
		if r.URL.Path == "/v1/responses/compact" {
			_, _ = fmt.Fprint(w, `{"object":"response.compaction","id":"cmp_test","output":[{"type":"compaction","encrypted_content":"opaque-content"}],`+responseUsage+`}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Error("unapproved Responses upstream path", r.URL.Path)
		}
		if mode.Load() == 4 {
			w.WriteHeader(503)
			return
		}
		status := "completed"
		if mode.Load() == 1 {
			status = "incomplete"
		} else if mode.Load() == 2 {
			status = "failed"
		}
		usage := responseUsage
		if mode.Load() == 3 {
			usage = `"usage":null`
		}
		response := fmt.Sprintf(`{"object":"response","id":"resp_%d","model":"response-model","status":%q,"service_tier":"default","output":[{"type":"function_call","call_id":"call_one","name":"weather","arguments":"{\"city\":\"Tokyo\"}"},{"type":"reasoning","encrypted_content":"encrypted-reasoning"}],%s}`, n, status, usage)
		if string(body["stream"]) != "true" {
			_, _ = fmt.Fprint(w, response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"object\":\"response\",\"id\":\"resp_%d\",\"status\":\"in_progress\"}}\n\n", n)
		_, _ = fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"Tokyo\",\"sequence_number\":1}\n\n")
		if mode.Load() == 5 {
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":%s}\n\n", status, status, response)
	}))
	defer up.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer other.Close()
	uid := int64(manage("/api/v1/admin/users", admin, map[string]any{"email": "responses@example.test", "password": "responses-password", "balance": 10})["id"].(float64))
	user := manage("/api/v1/auth/login", "", map[string]any{"email": "responses@example.test", "password": "responses-password"})["access_token"].(string)
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Responses", "platform": "openai"})["id"].(float64))
	manage("/api/v1/admin/channels", admin, map[string]any{"name": "Responses", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"client-response"}, "input_price": json.Number("0.000001"), "output_price": json.Number("0.000002"), "cache_read_price": json.Number("0.0000001"), "cache_write_price": json.Number("0.000003"), "reasoning_effort_multipliers": map[string]any{"high": 2}}}})
	account := func(name, base string, priority int) int64 {
		return int64(manage("/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "openai", "type": "apikey", "priority": priority, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "responses-upstream", "base_url": base, "api_protocol": "responses", "model_mapping": map[string]string{"client-response": "upstream-responses"}}})["id"].(float64))
	}
	aid := account("Responses", up.URL+"/v1", 1)
	k := manage("/api/v1/keys", user, map[string]any{"name": "Responses", "group_id": gid, "quota": 10})
	key, kid := k["key"].(string), int64(k["id"].(float64))
	body := map[string]any{"model": "client-response", "input": []any{map[string]any{"role": "user", "content": "weather"}}, "reasoning": map[string]any{"effort": "high"}, "tools": []any{map[string]any{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}}}}
	first := call("/v1/responses", key, body, "responses-once")
	for _, alias := range []string{"/responses", "/backend-api/codex/responses"} {
		replay := call(alias, key, body, "responses-once")
		if first.Code != 200 || replay.Body.String() != first.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("Responses JSON/alias replay", first.Code, first.Body.String(), replay.Code, replay.Body.String())
		}
	}
	if !strings.Contains(first.Body.String(), "encrypted-reasoning") || !strings.Contains(first.Body.String(), "function_call") {
		t.Fatal("Responses reasoning or tool output lost")
	}
	var input, output, read, write int
	var cost, balance, consumed, effort string
	if err := a.DB.QueryRow(`SELECT l.input_tokens,l.output_tokens,l.cache_read_tokens,l.cache_creation_tokens,l.actual_cost::text,u.balance::text,k.quota_used::text,l.reasoning_effort FROM usage_logs l JOIN users u ON u.id=l.user_id JOIN api_keys k ON k.id=l.api_key_id WHERE k.id=$1`, kid).Scan(&input, &output, &read, &write, &cost, &balance, &consumed, &effort); err != nil || input != 12 || output != 8 || read != 5 || write != 3 || cost != "0.0000750000" || balance != "9.99992500" || consumed != "0.00007500" || effort != "high" {
		t.Fatal("Responses accounting", input, output, read, write, cost, balance, consumed, effort, err)
	}
	for _, path := range []string{"/responses/input_tokens", "/backend-api/codex/responses/input_tokens"} {
		if w := call(path, key, body, "response-count"); w.Code != 200 || !strings.Contains(w.Body.String(), `"input_tokens":20`) {
			t.Fatal("Responses token count", w.Code, w.Body.String())
		}
	}
	var logs int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&logs); err != nil || logs != 1 {
		t.Fatal("Responses count charged consumption", logs, err)
	}
	if w := call("/v1/responses/compact", key, body, "response-compact"); w.Code != 200 || !strings.Contains(w.Body.String(), "opaque-content") {
		t.Fatal("Responses compaction", w.Code, w.Body.String())
	}
	body["stream"] = true
	stream := call("/responses", key, body, "response-stream")
	if stream.Code != 200 || !strings.Contains(stream.Body.String(), "event: response.completed") || strings.Contains(stream.Body.String(), "[DONE]") {
		t.Fatal("Responses stream", stream.Code, stream.Body.String())
	}
	if w := call("/backend-api/codex/responses", key, body, "response-stream"); w.Body.String() != stream.Body.String() || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("Responses stream replay")
	}
	for _, state := range []int32{1, 2, 3, 5} {
		mode.Store(state)
		w := call("/responses", key, body, fmt.Sprint("response-state-", state))
		if state == 1 {
			if !strings.Contains(w.Body.String(), "event: response.incomplete") || strings.Contains(w.Body.String(), "event: error") {
				t.Fatal("Responses output limit must remain native incomplete", w.Body.String())
			}
		} else if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
			t.Fatal("Responses failure falsely completed", state, w.Body.String())
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_responses_settlement CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w := call("/responses", key, body, "response-settlement")
	if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("Responses completed before billing", w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_responses_settlement"); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&logs); err != nil || logs != 6 {
		t.Fatal("Responses failed/partial billing or settlement recovery", logs, err)
	}
	delete(body, "stream")
	before := calls.Load()
	for _, tools := range []any{[]any{map[string]any{"type": "web_search"}}, []any{nil}, "invalid"} {
		hidden := map[string]any{"model": "client-response", "input": []any{map[string]any{"type": "additional_tools", "tools": tools}, map[string]any{"role": "user", "content": "hello"}}}
		if w := call("/responses", key, hidden, ""); w.Code != 400 || calls.Load() != before {
			t.Fatal("additional tools bypassed admission", w.Code, calls.Load())
		}
	}
	for _, field := range []string{"background", "conversation", "input", "tools", "previous_response_id"} {
		saved, exists := body[field]
		body[field] = map[string]any{"background": true, "conversation": "conv_foreign", "input": []any{map[string]any{"type": "item_reference", "id": "msg_foreign"}}, "tools": []any{map[string]any{"type": "web_search"}}, "previous_response_id": "../escape"}[field]
		if w := call("/responses", key, body, ""); w.Code != 400 {
			t.Fatal("unsupported/unsafe Responses request accepted", field, w.Code)
		}
		if exists {
			body[field] = saved
		} else {
			delete(body, field)
		}
	}
	for _, path := range []string{"/responses/other", "/responses/resp_foreign/cancel", "/responses/compact/extra", "/responses/%2e%2e/other"} {
		if w := call(path, key, body, ""); w.Code != 404 {
			t.Fatal("unapproved Responses subpath", path, w.Code)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid Responses request reached upstream")
	}
	inputBefore := body["input"]
	body["input"] = []any{map[string]any{"type": "compaction_trigger"}, map[string]any{"role": "user", "content": "compact"}, map[string]any{"type": "compaction_trigger"}}
	if w := call("/responses", key, body, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("non-streaming native compaction accepted", w.Code)
	}
	body["stream"], body["store"] = true, false
	if w := call("/responses", key, body, "response-native-compact"); !strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("native compaction forwarding", w.Code, w.Body.String())
	}
	var native bool
	if err := a.DB.QueryRow("SELECT native_compaction_v2 FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&native); err != nil || !native {
		t.Fatal("native compaction marker", native, err)
	}
	delete(body, "stream")
	delete(body, "store")
	body["input"] = inputBefore
	body["previous_response_id"] = fmt.Sprintf("resp_%d", calls.Load())
	if w := call("/responses", key, body, ""); w.Code != 404 {
		t.Fatal("store=false response was bound", w.Code)
	}
	delete(body, "previous_response_id")
	// A more preferred account must never receive the first account's response ID.
	account("Other Responses", other.URL, 0)
	body["previous_response_id"] = "resp_1"
	if w := call("/responses", key, body, "response-followup"); w.Code != 200 || otherCalls.Load() != 0 {
		t.Fatal("Responses continuation affinity", w.Code, w.Body.String())
	}
	otherKey := manage("/api/v1/keys", user, map[string]any{"name": "Other Responses", "group_id": gid})["key"].(string)
	if w := call("/responses", otherKey, body, ""); w.Code != 404 || otherCalls.Load() != 0 {
		t.Fatal("cross-key response reference allowed", w.Code)
	}
	var keyInfo gatewayKey
	keyInfo.ID, keyInfo.GroupID = kid, gid
	identity := &gatewayIdentity{Key: keyInfo}
	bound, err := a.previousResponse(t.Context(), identity, "resp_1")
	if err != nil || bound.AccountID != aid {
		t.Fatal("persisted response binding", bound, err)
	}
	if err := a.Redis.Del(t.Context(), responseBindingKey(identity, "resp_1")).Err(); err != nil {
		t.Fatal(err)
	}
	if w := call("/responses", key, body, "response-followup"); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("completed continuation could not replay after affinity expiry", w.Code)
	}
	if w := call("/responses", key, body, ""); w.Code != 404 || otherCalls.Load() != 0 {
		t.Fatal("missing response binding fell back to another account", w.Code)
	}
	original, err := a.loadAccount(t.Context(), aid)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.bindResponse(t.Context(), identity, original, "resp_1"); err != nil {
		t.Fatal(err)
	}
	mode.Store(4)
	if w := call("/responses", key, body, "response-unavailable"); w.Code != 503 || otherCalls.Load() != 0 {
		t.Fatal("affinity fell back to another upstream", w.Code)
	}
	if _, err := a.DB.Exec(`UPDATE accounts SET overload_until=NULL,credentials=jsonb_set(credentials,'{api_key}','"rotated"') WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if w := call("/responses", key, body, "response-rotated"); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("continuation survived upstream key rotation", w.Code)
	}
	var reconciled bool
	if err := a.DB.QueryRow("SELECT (10-balance)=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&reconciled); err != nil || !reconciled {
		t.Fatal("Responses balance reconciliation", err)
	}
}
