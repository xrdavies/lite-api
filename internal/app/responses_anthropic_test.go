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

func TestResponsesAnthropicConversion(t *testing.T) {
	decode := func(raw string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	request := decode(`{"model":"m","instructions":"top","reasoning":{"effort":"xhigh"},"max_output_tokens":2048,"parallel_tool_calls":false,"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"get","parameters":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"type":"custom","name":"patch"},{"type":"tool_search","execution":"client"}],"tool_choice":{"type":"function","namespace":"ns","name":"get"},"input":[{"role":"developer","content":[{"type":"input_text","text":"input rules","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":[{"type":"input_text","text":"question"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="}]},{"type":"reasoning","encrypted_content":"foreign-do-not-forward","summary":[]},{"type":"function_call","call_id":"a","namespace":"ns","name":"get","arguments":"{\"x\":9007199254740993}"},{"type":"custom_tool_call","call_id":"b","name":"patch","input":"raw patch"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"result"},{"type":"input_image","image_url":"https://example.test/a.png"}]},{"type":"custom_tool_call_output","call_id":"b","output":"ok"}]}`)
	in, effort, err := responsesToAnthropicRequest(request, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in.Body)
	if effort != "max" || !strings.Contains(string(raw), `"name":"ns__get"`) || !strings.Contains(string(raw), `"disable_parallel_tool_use":true`) || !strings.Contains(string(raw), `"budget_tokens":2047`) || !strings.Contains(string(raw), `"cache_control":{"type":"ephemeral"}`) || !strings.Contains(string(raw), "9007199254740993") || strings.Contains(string(raw), "foreign") || !in.Custom["patch"] || !strings.Contains(string(raw), `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Fatal(effort, string(raw))
	}
	var wire struct {
		Messages []struct {
			Role    string
			Content []map[string]json.RawMessage
		}
		System []any
		Stream bool
	}
	_ = json.Unmarshal(raw, &wire)
	if len(wire.Messages) != 3 || len(wire.Messages[2].Content) != 2 || len(wire.System) != 2 || wire.Stream {
		t.Fatal(string(raw))
	}
	// Only saved native content carries a provider signature, and namespaces stay original in storage.
	bridge := newAnthropicResponsesStream("public", in)
	result, err := bridge.response([]byte(`{"type":"message","id":"native-one","stop_reason":"tool_use","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"thought","signature":"native-sig"},{"type":"redacted_thinking","data":"opaque-data"},{"type":"text","text":"last"},{"type":"tool_use","id":"call_ns","name":"ns__get","input":{"n":9007199254740993}},{"type":"tool_use","id":"call_custom","name":"patch","input":{"input":"patch text"}},{"type":"tool_use","id":"call_search","name":"tool_search","input":{"query":"find"}}]}`), priceUsage{Input: 10, CacheRead: 5, CacheWrite: 6, Output: 8})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []map[string]json.RawMessage
		Usage  map[string]any
	}
	_ = json.Unmarshal(result, &response)
	if len(response.Output) != 6 || credentialString(response.Output[0], "type") != "message" || credentialString(response.Output[1], "type") != "reasoning" || credentialString(response.Output[2], "type") != "message" || credentialString(response.Output[3], "namespace") != "ns" || credentialString(response.Output[4], "input") != "patch text" || credentialString(response.Output[5], "type") != "tool_search_call" || response.Usage["input_tokens"] != float64(21) || strings.Contains(string(result), "native-sig") || strings.Contains(string(result), "opaque-data") {
		t.Fatal(string(result))
	}
	history := append(in.History, bridge.assistant())
	next := decode(`{"model":"m","instructions":"replace","input":[{"type":"function_call_output","call_id":"call_ns","output":"ok"},{"type":"custom_tool_call_output","call_id":"call_custom","output":"applied"},{"type":"tool_search_output","call_id":"call_search","tools":[{"type":"function","name":"found","parameters":{"type":"object"}}]}]}`)
	again, _, err := responsesToAnthropicRequest(next, history)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(again.Body)
	if !strings.Contains(string(raw), `"signature":"native-sig"`) || !strings.Contains(string(raw), `"data":"opaque-data"`) || !strings.Contains(string(raw), `"name":"found"`) || !strings.Contains(string(raw), `"text":"input rules"`) || strings.Contains(string(raw), `"text":"top"`) || strings.Contains(string(raw), `"namespace"`) || strings.Contains(string(raw), `"response_input"`) || strings.Contains(string(raw), `"tool_search":`) {
		t.Fatal("native continuation", string(raw))
	}
	// Tool discovery remains available in another continuation without a new tools declaration.
	third, _, err := responsesToAnthropicRequest(decode(`{"model":"m","input":"next"}`), again.History)
	raw, _ = json.Marshal(third.Body)
	if err != nil || !strings.Contains(string(raw), `"name":"found"`) {
		t.Fatal("saved discovery", string(raw), err)
	}
	for _, patch := range []string{`{"instructions":5}`, `{"max_output_tokens":0}`, `{"text":5}`, `{"text":{"format":{"type":"unknown"}}}`, `{"context_management":[]}`, `{"truncation":"auto"}`, `{"input":[{"type":"native_assistant","content":[]}]}`, `{"input":[{"role":"system","content":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]}`, `{"input":[{"type":"function_call","name":"ns__get","call_id":"bad","arguments":"{}"}]}`} {
		copy := decode(`{"model":"m","input":"ok","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"get"}]}]}`)
		_ = json.Unmarshal([]byte(patch), &copy)
		if _, _, err := responsesToAnthropicRequest(copy, nil); err == nil {
			t.Fatal("invalid conversion accepted", patch)
		}
	}
	// Signatures arrive after thinking text and must be retained for authenticated history.
	events := []string{
		`{"type":"message_start","message":{"id":"native-sse","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"first"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"tail"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"last"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"call_search","name":"tool_search","input":{}}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"find\"}"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"}}`,
		`{"type":"message_stop"}`,
	}
	bridge = newAnthropicResponsesStream("public", in)
	var output strings.Builder
	for i, event := range events {
		wire, err := bridge.event([]byte(event), priceUsage{Input: 10, Output: 8})
		if err != nil {
			t.Fatal(i, err)
		}
		if i < len(events)-1 && strings.Contains(wire, "response.output_item.done") {
			t.Fatal("early terminal")
		}
		output.WriteString(wire)
	}
	if !strings.Contains(output.String(), "response.incomplete") || strings.Contains(output.String(), "sig-tail") || strings.Contains(output.String(), "[DONE]") {
		t.Fatal(output.String())
	}
	saved := bridge.assistant()
	if credentialString(saved.Anthropic[1], "signature") != "sig-tail" || credentialString(saved.Anthropic[1], "thinking") != "thought" || credentialString(saved.Anthropic[2], "text") != "last" {
		t.Fatal("captured native content", saved)
	}
	sequence := 0
	for _, line := range strings.Split(output.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Sequence int `json:"sequence_number"`
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
		if event.Sequence != sequence {
			t.Fatal("sequence", event.Sequence, sequence)
		}
		sequence++
	}
}

func testResponsesAnthropic(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.71:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
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
		var v struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "responses-anthropic@example.test", "password": "responses-anthropic-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "responses-anthropic@example.test", "password": "responses-anthropic-password"})["access_token"].(string)
	var calls, mode, continuations atomic.Int32
	var captured atomic.Value
	const usage = `"usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"native-up"}]}`)
			return
		}
		n := calls.Add(1)
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		raw, _ := json.Marshal(b)
		captured.Store(string(raw))
		if r.URL.Path != "/v1/messages" || credentialString(b, "model") != "native-up" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "native-secret" || b["previous_response_id"] != nil {
			t.Error("upstream request", r.URL.Path, string(raw))
		}
		if strings.Contains(string(raw), `"signature":"native-signature"`) {
			continuations.Add(1)
			if strings.Contains(string(raw), `"text":"top rules"`) || !strings.Contains(string(raw), `"text":"input rules"`) {
				t.Error("continuation instructions", string(raw))
			}
		}
		if mode.Load() == 6 {
			w.WriteHeader(503)
			return
		}
		reason := "tool_use"
		if mode.Load() == 1 {
			reason = "max_tokens"
		}
		if mode.Load() == 2 {
			reason = "refusal"
		}
		u := usage
		if mode.Load() == 3 {
			u = `"usage":null`
		}
		tool := "ns__get"
		args := `{"x":9007199254740993}`
		if mode.Load() == 7 {
			tool = "tool_search"
			args = `{"query":"lookup"}`
		}
		if mode.Load() == 8 {
			tool = "patch"
			args = `{"input":"custom result"}`
		}
		callID := fmt.Sprintf("tool_%d", n)
		if string(b["stream"]) != "true" {
			fmt.Fprintf(w, `{"id":"native_%d","type":"message","model":"native-answer","stop_reason":"%s","content":[{"type":"thinking","thinking":"secret thought","signature":"native-signature"},{"type":"text","text":"answer"},{"type":"tool_use","id":"%s","name":"%s","input":%s}],%s}`, n, reason, callID, tool, args, u)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(value string) { fmt.Fprintf(w, "data: %s\n\n", value); _ = http.NewResponseController(w).Flush() }
		emit(fmt.Sprintf(`{"type":"message_start","message":{"id":"native_%d","type":"message","model":"native-answer","content":[],%s}}`, n, u))
		emit(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		emit(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"secret thought"}}`)
		emit(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"native-signature"}}`)
		emit(`{"type":"content_block_stop","index":0}`)
		emit(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		emit(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`)
		if mode.Load() == 4 {
			return
		}
		emit(`{"type":"content_block_stop","index":1}`)
		emit(fmt.Sprintf(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"%s","name":"%s","input":{}}}`, callID, tool))
		encoded, _ := json.Marshal(args)
		emit(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":` + string(encoded) + `}}`)
		emit(`{"type":"content_block_stop","index":2}`)
		if mode.Load() == 5 {
			emit(`{"type":"error","error":{"message":"private-error"}}`)
			return
		}
		emit(`{"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}`)
		emit(`{"type":"message_stop"}`)
	}))
	defer up.Close()
	price := func(platform string) map[string]any {
		return map[string]any{"platform": platform, "models": []string{"public-native"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004, "cache_write_1h_price": 0.006, "reasoning_effort_multipliers": map[string]any{"xhigh": 9, "max": 1.5}}
	}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses Anthropic", "platform": "anthropic", "rate_multiplier": 2, "model_pricing": []any{price("anthropic")}}))
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Native response", "group_id": gid, "quota": 50})
	key, kid := k["key"].(string), id(k)
	account := func(platform string, groups []int64) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Responses " + platform, "platform": platform, "type": "apikey", "rate_multiplier": 3, "group_ids": groups, "credentials": map[string]any{"api_key": "native-secret", "base_url": up.URL, "api_protocol": "anthropic", "model_mapping": map[string]string{"public-native": "native-up"}}}))
	}
	aid := account("anthropic", []int64{gid})
	tools := []any{map[string]any{"type": "namespace", "name": "ns", "tools": []any{map[string]any{"type": "function", "name": "get", "parameters": map[string]any{"type": "object"}}}}, map[string]any{"type": "custom", "name": "patch"}, map[string]any{"type": "tool_search", "execution": "client"}}
	body := func(stream bool) map[string]any {
		return map[string]any{"model": "public-native", "stream": stream, "instructions": "top rules", "input": []any{map[string]any{"role": "developer", "content": "input rules"}, map[string]any{"role": "user", "content": "question"}}, "tools": tools}
	}
	check := func(w *httptest.ResponseRecorder, cost string, account int64) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), "gateway_error") {
			t.Fatalf("conversion %d %s", w.Code, w.Body.String())
		}
		var actual, upstream, requested, upmodel string
		var logged int64
		err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,requested_model,upstream_model,account_id FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&actual, &upstream, &requested, &upmodel, &logged)
		if err != nil || actual != cost || upstream != "/v1/messages" || requested != "public-native" || upmodel != "native-up" || logged != account {
			t.Fatal("billing", actual, upstream, requested, upmodel, logged, err)
		}
	}
	first := call("POST", "/v1/responses", key, body(false), "native-json")
	check(first, "0.6140000000", aid)
	var result struct {
		ID     string
		Output []map[string]json.RawMessage
	}
	_ = json.Unmarshal(first.Body.Bytes(), &result)
	if !strings.HasPrefix(result.ID, "resp_lite_") || credentialString(result.Output[2], "namespace") != "ns" || strings.Contains(first.Body.String(), "native-signature") {
		t.Fatal(first.Body.String())
	}
	for _, path := range []string{"/responses", "/backend-api/codex/responses"} {
		w := call("POST", path, key, body(false), "native-json")
		if w.Body.String() != first.Body.String() || calls.Load() != 1 || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("alias replay", w.Body.String())
		}
	}
	var balance, used string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE u.id=$1 AND k.id=$2`, uid, kid).Scan(&balance, &used); err != nil || balance != "99.38600000" || used != "0.61400000" {
		t.Fatal(balance, used, err)
	}
	bindingRaw, err := a.Redis.Get(t.Context(), responseBindingKey(&gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}, result.ID)).Result()
	if err != nil || strings.Contains(bindingRaw, "native-signature") || strings.Contains(bindingRaw, "secret thought") {
		t.Fatal("unencrypted history", err)
	}
	next := map[string]any{"model": "public-native", "previous_response_id": result.ID, "instructions": "replacement", "input": []any{map[string]any{"type": "function_call_output", "call_id": credentialString(result.Output[2], "call_id"), "output": "tool result"}}, "tools": tools}
	check(call("POST", "/responses", key, next, ""), "0.6140000000", aid)
	if continuations.Load() != 1 {
		t.Fatal("native signature not replayed")
	}
	for _, stream := range []bool{false, true} {
		w := call("POST", "/v1/responses", key, body(stream), "")
		check(w, "0.6140000000", aid)
		if stream && (!strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "[DONE]")) {
			t.Fatal(w.Body.String())
		}
	}
	// Continue from a streaming response with the captured native signature.
	streamed := call("POST", "/v1/responses", key, body(true), "")
	var streamedID, streamedCall string
	for _, line := range strings.Split(streamed.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type     string
			Response struct {
				ID     string
				Output []map[string]json.RawMessage
			}
		}
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
		if event.Type == "response.completed" {
			streamedID = event.Response.ID
			streamedCall = credentialString(event.Response.Output[2], "call_id")
		}
	}
	if streamedID == "" {
		t.Fatal("missing streaming response ID")
	}
	check(call("POST", "/v1/responses", key, map[string]any{"model": "public-native", "previous_response_id": streamedID, "input": []any{map[string]any{"type": "function_call_output", "call_id": streamedCall, "output": "continued"}}}, ""), "0.6140000000", aid)
	if continuations.Load() != 2 {
		t.Fatal("streaming signature not restored")
	}
	unstored := body(false)
	unstored["store"] = false
	unbound := call("POST", "/v1/responses", key, unstored, "")
	var unboundResult struct{ ID string }
	_ = json.Unmarshal(unbound.Body.Bytes(), &unboundResult)
	if n, err := a.Redis.Exists(t.Context(), responseBindingKey(&gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}, unboundResult.ID)).Result(); err != nil || n != 0 {
		t.Fatal("store=false retained history", n, err)
	}
	// Discover a namespace, then retain that definition through a further continuation.
	mode.Store(7)
	search := call("POST", "/v1/responses", key, body(false), "")
	var searchResult struct {
		ID     string
		Output []map[string]json.RawMessage
	}
	_ = json.Unmarshal(search.Body.Bytes(), &searchResult)
	mode.Store(0)
	discovery := map[string]any{"model": "public-native", "previous_response_id": searchResult.ID, "input": []any{map[string]any{"type": "tool_search_output", "call_id": credentialString(searchResult.Output[2], "call_id"), "tools": []any{tools[0]}}}}
	discovered := call("POST", "/v1/responses", key, discovery, "")
	check(discovered, "0.6140000000", aid)
	if !strings.Contains(captured.Load().(string), `"name":"ns__get"`) {
		t.Fatal("discovery not activated")
	}
	var discoveredResult struct {
		ID     string
		Output []map[string]json.RawMessage
	}
	_ = json.Unmarshal(discovered.Body.Bytes(), &discoveredResult)
	follow := map[string]any{"model": "public-native", "previous_response_id": discoveredResult.ID, "input": []any{map[string]any{"type": "function_call_output", "call_id": credentialString(discoveredResult.Output[2], "call_id"), "output": "found result"}}}
	check(call("POST", "/v1/responses", key, follow, ""), "0.6140000000", aid)
	if !strings.Contains(captured.Load().(string), `"name":"ns__get"`) {
		t.Fatal("saved discovery lost")
	}
	before := calls.Load()
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "Other native", "group_id": gid})["key"].(string)
	if w := call("POST", "/v1/responses", other, next, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("cross-key continuation", w.Code)
	}
	for _, suffix := range []string{"/compact", "/input_tokens"} {
		w := call("POST", "/v1/responses"+suffix, key, body(false), "")
		if w.Code != 503 || calls.Load() != before {
			t.Fatal("native-only operation", suffix, w.Code)
		}
	}
	for _, m := range []int32{1, 2, 3, 4, 5, 7, 8} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if (m == 4 || m == 5) && !stream {
				continue
			}
			w := call("POST", "/v1/responses", key, body(stream), "")
			if m == 3 || m == 4 || m == 5 {
				if !strings.Contains(w.Body.String(), "error") || strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "private-error") {
					t.Fatal("failed response", w.Body.String())
				}
				var count int
				want := 1
				if m == 3 {
					want = 0
				}
				if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != want {
					t.Fatal(count, want, err)
				}
			} else {
				check(w, "0.6140000000", aid)
				if m <= 2 && !strings.Contains(w.Body.String(), `"status":"incomplete"`) {
					t.Fatal(w.Body.String())
				}
				if m == 7 && !strings.Contains(w.Body.String(), `"type":"tool_search_call"`) {
					t.Fatal(w.Body.String())
				}
				if m == 8 && !strings.Contains(w.Body.String(), `"input":"custom result"`) {
					t.Fatal(w.Body.String())
				}
			}
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_native_response CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/responses", key, body(true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_native_response"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "response.completed") || strings.Contains(failed.Body.String(), "response.output_item.done") || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal("terminal before settlement", failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.614", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("receipt recovery", count, err)
	}
	effortBody := body(false)
	effortBody["reasoning"] = map[string]string{"effort": "xhigh"}
	check(call("POST", "/v1/responses", key, effortBody, ""), "0.9210000000", aid)
	// Continuation cannot switch even when another healthy account is available.
	backup := account("anthropic", []int64{gid})
	mode.Store(6)
	before = calls.Load()
	w := call("POST", "/v1/responses", key, next, "")
	if w.Code != 503 || calls.Load() != before+1 {
		t.Fatal("continuation switched accounts", w.Code, calls.Load()-before)
	}
	mode.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-secret"}})
	before = calls.Load()
	if w := call("POST", "/v1/responses", key, next, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("credential rotation", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "native-secret"}})
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", backup), admin, map[string]any{"schedulable": false})
	for _, platform := range []string{"openai", "kimi", "zhipu", "deepseek", "minimax"} {
		group := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses native " + platform, "platform": platform, "rate_multiplier": 2, "model_pricing": []any{price(platform)}}))
		k := must("POST", "/api/v1/keys", user, map[string]any{"name": platform, "group_id": group})["key"].(string)
		upstream := account(platform, []int64{group})
		for _, stream := range []bool{false, true} {
			check(call("POST", "/v1/responses", k, body(stream), ""), "0.6140000000", upstream)
		}
	}
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses native composite", "platform": "composite", "rate_multiplier": 2, "model_pricing": []any{price("anthropic")}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-native", "target_platform": "anthropic", "upstream_model": "public-native", "endpoint": "responses", "match_type": "exact", "enabled": true})
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite native", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/responses", ck, body(true), ""), "0.6140000000", aid)
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-native") {
		t.Fatal("composite catalog", w.Code, w.Body.String())
	}
}
