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

func TestChatResponsesConversion(t *testing.T) {
	request := `{"model":"example","stream":true,"max_tokens":99,"max_completion_tokens":101,"reasoning_effort":"high","service_tier":"priority","temperature":0.5,"parallel_tool_calls":true,"store":true,"messages":[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"https://example.test/p.png","detail":"high"}},{"type":"file","file":{"filename":"doc.pdf","file_data":"data:application/pdf;base64,YQ=="}}]},{"role":"assistant","content":"checking","reasoning_content":"think","tool_calls":[{"id":"call_one","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_two","type":"custom","custom":{"name":"patch","input":"raw patch"}}]},{"role":"tool","tool_call_id":"call_one","content":"result"},{"role":"tool","tool_call_id":"call_two","content":"ok"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}},{"type":"custom","custom":{"name":"patch","format":{"type":"text"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}},"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}}`
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(request), &body)
	raw, err := chatToResponses(body)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if err != nil || got["store"] != false || got["stream"] != true || got["max_output_tokens"] != float64(101) || got["messages"] != nil || got["response_format"] != nil {
		t.Fatal("converted request", string(raw), err)
	}
	input := got["input"].([]any)
	if len(input) != 7 || input[0].(map[string]any)["role"] != "developer" || input[3].(map[string]any)["call_id"] != "call_one" || input[6].(map[string]any)["type"] != "custom_tool_call_output" {
		t.Fatal("converted input order/tools", input)
	}
	if got["tools"].([]any)[0].(map[string]any)["strict"] != false || got["tool_choice"].(map[string]any)["name"] != "lookup" || got["text"].(map[string]any)["format"].(map[string]any)["name"] != "answer" {
		t.Fatal("tool schema or text format", got)
	}
	for _, field := range []string{"n", "stop", "logprobs", "audio"} {
		copy := map[string]json.RawMessage{}
		for k, v := range body {
			copy[k] = v
		}
		copy[field] = json.RawMessage("true")
		if _, err := chatToResponses(copy); err == nil {
			t.Fatal("unsupported control lost", field)
		}
	}
	for _, message := range []string{`{"role":"admin","content":"x"}`, `{"role":"user","content":42}`, `{"role":"tool","content":"x"}`, `{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}`} {
		badBody := map[string]json.RawMessage{"model": json.RawMessage(`"m"`), "messages": json.RawMessage("[" + message + "]")}
		if _, err := chatToResponses(badBody); err == nil {
			t.Fatal("invalid message accepted", message)
		}
	}
	legacy := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"assistant","function_call":{"name":"get","arguments":"{}"}},{"role":"function","name":"get","content":"answer"}],"functions":[{"name":"get","parameters":{"type":"object"}}],"function_call":{"name":"get"}}`), &legacy)
	if raw, err := chatToResponses(legacy); err != nil || !strings.Contains(string(raw), `"call_id":"get"`) || !strings.Contains(string(raw), `"strict":false`) {
		t.Fatal("legacy function conversion", string(raw), err)
	}
	for _, tc := range []struct{ status, reason, finish string }{{"completed", "", "tool_calls"}, {"incomplete", "max_output_tokens", "length"}, {"incomplete", "content_filter", "content_filter"}} {
		raw := `{"object":"response","id":"resp_convert","created_at":12,"status":"` + tc.status + `","incomplete_details":{"reason":"` + tc.reason + `"},"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"message","content":[{"type":"output_text","text":"answer"},{"type":"refusal","refusal":"no"}]},{"type":"function_call","call_id":"call_one","name":"get","arguments":"{}"}],` + responseUsage + `}`
		converted, err := responsesToChat([]byte(raw), "public-model")
		var result map[string]any
		_ = json.Unmarshal(converted, &result)
		if err != nil || result["model"] != "public-model" || result["object"] != "chat.completion" || result["choices"].([]any)[0].(map[string]any)["finish_reason"] != tc.finish || result["usage"].(map[string]any)["prompt_tokens"] != float64(20) {
			t.Fatal("converted output", string(converted), err)
		}
	}
	s := newResponseChatStream("public-model", true)
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_stream","created_at":12,"status":"in_progress","output":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}`,
		`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"Hello!"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_a","name":"get","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{"}`,
		`{"type":"response.function_call_arguments.done","output_index":1,"arguments":"{}"}`,
		`{"type":"response.completed","response":{"id":"resp_stream","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello!"}]},{"type":"function_call","call_id":"call_a","name":"get","arguments":"{}"}],` + responseUsage + `}}`,
	}
	var result strings.Builder
	for _, event := range frames {
		wire, err := s.event([]byte(event))
		if err != nil {
			t.Fatal(err)
		}
		result.WriteString(wire)
	}
	if strings.Count(result.String(), `"content":"Hello"`) != 1 || strings.Contains(result.String(), `"content":"Hello!"`) || strings.Count(result.String(), `"id":"call_a"`) != 1 || !strings.Contains(result.String(), `"completion_tokens":8`) || !strings.Contains(result.String(), `"finish_reason":"tool_calls"`) || !strings.HasSuffix(result.String(), "data: [DONE]\n\n") {
		t.Fatal("stream conversion/duplicates", result.String())
	}
	if _, err := s.event([]byte(`{"type":"response.output_text.done","text":"changed"}`)); err == nil {
		t.Fatal("changed text accepted")
	}
	s = newResponseChatStream("m", false)
	if _, err := s.event([]byte(frames[1])); err == nil {
		t.Fatal("missing creation accepted")
	}
	if _, err := s.event([]byte(`{"type":"response.output_text.delta","output_index":-1}`)); err == nil {
		t.Fatal("invalid event index accepted")
	}
	terminal, err := s.event([]byte(frames[len(frames)-1]))
	if err != nil || strings.Contains(terminal, `"usage"`) || !strings.Contains(terminal, `"content":"Hello!"`) {
		t.Fatal("terminal-only fallback", terminal, err)
	}
	s = newResponseChatStream("m", false)
	if _, err := s.event([]byte(frames[0])); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"raw thought"}`,
		`{"type":"response.reasoning_text.done","output_index":0,"text":"raw thought"}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"summary"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"summary"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","call_id":"custom_one","name":"patch","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":1,"delta":"patch body"}`,
		`{"type":"response.custom_tool_call_input.done","output_index":1,"input":"patch body"}`,
	} {
		if _, err := s.event([]byte(event)); err != nil {
			t.Fatal("reasoning/custom conversion", err)
		}
	}
	if s.Text["raw_reasoning:0:0"] != "raw thought" || s.Text["reasoning_content:0:0"] != "summary" || s.Text["tool:1"] != "patch body" {
		t.Fatal("reasoning/custom accumulation", s.Text)
	}
}

func testChatResponses(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
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
			t.Fatalf("%s %s %d %s", method, path, w.Code, w.Body.String())
		}
		var v struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "chat-responses@example.test", "password": "chat-responses-password", "balance": 50}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "chat-responses@example.test", "password": "chat-responses-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Chat Responses", "platform": "openai", "rate_multiplier": 2}))
	group := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Bridge", "group_id": gid, "quota": 50})
	key, kid := k["key"].(string), id(k)
	price := map[string]any{"platform": "openai", "models": []string{"public-bridge"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004}
	must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Bridge price", "group_ids": []int64{gid}, "model_pricing": []any{price}})
	var calls, mode, changePrice atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"mapped-bridge"}]}`)
			return
		}
		n := calls.Add(1)
		var b map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&b) != nil || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer bridge-upstream" || credentialString(b, "model") != "mapped-bridge" || string(b["store"]) != "false" || b["messages"] != nil || b["stream_options"] != nil || b["input"] == nil {
			t.Error("bridge upstream request", r.URL.Path, b)
		}
		if changePrice.Swap(0) == 1 {
			if _, err := a.DB.Exec("UPDATE groups SET rate_multiplier=3 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		status, details := "completed", `"incomplete_details":null`
		if mode.Load() == 1 {
			status, details = "incomplete", `"incomplete_details":{"reason":"max_output_tokens"}`
		}
		if mode.Load() == 2 {
			status = "failed"
		}
		usage := responseUsage
		if mode.Load() == 3 {
			usage = `"usage":null`
		}
		output := `[{"type":"message","content":[{"type":"output_text","text":"hello bridge"}]},{"type":"function_call","call_id":"call_bridge","name":"get","arguments":"{}"}]`
		result := fmt.Sprintf(`{"object":"response","id":"resp_bridge_%d","created_at":12,"model":"upstream-answer","status":"%s",%s,"output":%s,%s}`, n, status, details, output, usage)
		w.Header().Set("X-Request-ID", "bridge-upstream-request")
		if string(b["stream"]) != "true" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, result)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(raw string) { fmt.Fprintf(w, "data: %s\n\n", raw); http.NewResponseController(w).Flush() }
		emit(fmt.Sprintf(`{"type":"response.created","response":{"object":"response","id":"resp_bridge_%d","created_at":12,"model":"upstream-answer","status":"in_progress","output":[]}}`, n))
		emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello "}`)
		if mode.Load() == 4 {
			return
		}
		emit(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"bridge"}`)
		emit(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"call_bridge","name":"get","arguments":""}}`)
		emit(`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`)
		emit(`{"type":"response.` + status + `","response":` + result + `}`)
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Responses bridge", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "rate_multiplier": 3, "extra": map[string]any{"quota_limit": 100}, "credentials": map[string]any{"api_key": "bridge-upstream", "api_protocol": "responses", "base_url": up.URL, "model_mapping": map[string]string{"public-bridge": "mapped-bridge"}}}))
	body := func(stream, usage bool) map[string]any {
		return map[string]any{"model": "public-bridge", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": stream, "stream_options": map[string]bool{"include_usage": usage}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get", "parameters": map[string]any{"type": "object"}}}}}
	}
	check := func(w *httptest.ResponseRecorder, actual string) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal("bridge failed", w.Code, w.Body.String())
		}
		var total, cost, inbound, upstream, requested, mapped, response string
		var logged int64
		err := a.DB.QueryRow(`SELECT total_cost::text,actual_cost::text,inbound_endpoint,upstream_endpoint,requested_model,upstream_model,upstream_response_model,account_id FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&total, &cost, &inbound, &upstream, &requested, &mapped, &response, &logged)
		if err != nil || total != "0.3070000000" || cost != actual || upstream != "/v1/responses" || !strings.HasSuffix(inbound, "/chat/completions") || requested != "public-bridge" || mapped != "mapped-bridge" || response != "upstream-answer" || logged != aid {
			t.Fatal("bridge billing", total, cost, inbound, upstream, requested, mapped, response, logged, err)
		}
	}
	first := call("POST", "/v1/chat/completions", key, body(false, false), "bridge-json")
	check(first, "0.6140000000")
	if !strings.Contains(first.Body.String(), `"object":"chat.completion"`) || !strings.Contains(first.Body.String(), `"model":"public-bridge"`) || !strings.Contains(first.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatal("converted JSON", first.Body.String())
	}
	var balance, used, accountUsed string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text,a.extra->>'quota_used' FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts a ON a.id=$3 WHERE u.id=$1 AND k.id=$2`, uid, kid, aid).Scan(&balance, &used, &accountUsed); err != nil || balance != "49.38600000" || used != "0.61400000" || rat(json.Number(accountUsed)).Cmp(rat("0.921")) != 0 {
		t.Fatal("bridge counters", balance, used, accountUsed, err)
	}
	for _, path := range []string{"/chat/completions", "/backend-api/codex/chat/completions"} {
		w := call("POST", path, key, body(false, false), "bridge-json")
		if w.Code != 200 || w.Body.String() != first.Body.String() || calls.Load() != 1 || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("bridge aliases", w.Code, calls.Load())
		}
	}
	for _, include := range []bool{false, true} {
		w := call("POST", "/v1/chat/completions", key, body(true, include), "")
		check(w, "0.6140000000")
		if strings.Contains(w.Body.String(), `response.completed`) || strings.Count(w.Body.String(), `"id":"call_bridge"`) != 1 || strings.Contains(w.Body.String(), `"usage"`) != include || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal("converted SSE", w.Body.String())
		}
	}
	changePrice.Store(1)
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.6140000000")
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.9210000000")
	must("PUT", group, admin, map[string]any{"rate_multiplier": 2})
	mode.Store(1)
	for _, stream := range []bool{false, true} {
		w := call("POST", "/v1/chat/completions", key, body(stream, true), "")
		check(w, "0.6140000000")
		if !strings.Contains(w.Body.String(), `"finish_reason":"length"`) {
			t.Fatal("incomplete status lost", w.Body.String())
		}
	}
	for _, m := range []int32{2, 3, 4} {
		mode.Store(m)
		w := call("POST", "/v1/chat/completions", key, body(true, true), "")
		if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal("failed conversion signaled success", m, w.Body.String())
		}
		var count int
		err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count)
		want := 0
		if m == 2 {
			want = 1
		}
		if err != nil || count != want {
			t.Fatal("failed conversion billing", m, count, err)
		}
	}
	mode.Store(0)
	before := calls.Load()
	invalid := body(false, false)
	invalid["n"] = 2
	if w := call("POST", "/v1/chat/completions", key, invalid, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("unsupported choice sent", w.Code)
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_bridge_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/chat/completions", key, body(true, true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_bridge_receipt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "[DONE]") || strings.Contains(failed.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal("terminal escaped settlement", failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.614 AND upstream_endpoint='/v1/responses'", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("bridge settlement recovery", count, err)
	}
	var nativeCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Error("native retry path", r.URL.Path)
		}
		w.WriteHeader(503)
	}))
	defer native.Close()
	nativeID := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Native fallback test", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "native-upstream", "base_url": native.URL}}))
	must("PUT", group, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"public-bridge": {nativeID}}})
	before = calls.Load()
	check(call("POST", "/v1/chat/completions", key, body(true, true), ""), "0.6140000000")
	if nativeCalls.Load() != 1 || calls.Load() != before+1 {
		t.Fatal("native to converted retry", nativeCalls.Load(), calls.Load()-before)
	}
	must("PUT", group, admin, map[string]any{"model_routing_enabled": false})
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", nativeID), admin, map[string]any{"schedulable": false})
	// Conversion participates in the same account preference and composite catalog.
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite bridge", "platform": "composite", "rate_multiplier": 2, "model_pricing": []any{price}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	routes := fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite)
	must("POST", routes, admin, map[string]any{"public_model": "public-bridge", "match_type": "exact", "target_platform": "openai", "upstream_model": "public-bridge", "endpoint": "chat_completions", "enabled": true})
	compositeKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "Composite bridge", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/chat/completions", compositeKey, body(false, false), ""), "0.6140000000")
	w := call("GET", "/v1/models", compositeKey, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"public-bridge"`) {
		t.Fatal("converted composite model missing", w.Code, w.Body.String())
	}
	for _, platform := range []string{"kimi", "zhipu", "deepseek", "minimax"} {
		groupID := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Bridge " + platform, "platform": platform, "rate_multiplier": 2, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"public-bridge"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004}}}))
		accountID := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Bridge " + platform, "platform": platform, "type": "apikey", "group_ids": []int64{groupID}, "credentials": map[string]any{"api_key": "bridge-upstream", "api_protocol": "responses", "base_url": up.URL, "model_mapping": map[string]string{"public-bridge": "mapped-bridge"}}}))
		platformKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "Bridge " + platform, "group_id": groupID})["key"].(string)
		w := call("POST", "/v1/chat/completions", platformKey, body(true, true), "")
		var logged int64
		var cost string
		if err := a.DB.QueryRow("SELECT account_id,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&logged, &cost); err != nil || logged != accountID || cost != "0.6140000000" || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal("platform conversion", platform, w.Code, w.Body.String(), err)
		}
	}
}
