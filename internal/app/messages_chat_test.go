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

func TestMessagesChat(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var b map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	body := parse(`{"model":"test-model","max_tokens":120,"stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: private"},{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}}},"stop_sequences":["END"],"tools":[{"name":"lookup","input_schema":{"type":"object"},"strict":true}],"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YQ=="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"foreign-signature"},{"type":"redacted_thinking","data":"foreign-data"},{"type":"tool_use","id":"call_one","name":"lookup","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_one","is_error":true,"content":[{"type":"text","text":"missing"},{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]},{"type":"text","text":"try again"}]}]}`)
	raw, effort, err := messagesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if effort != "xhigh" || !strings.Contains(string(raw), `"reasoning_effort":"xhigh"`) || !strings.Contains(string(raw), `"reasoning_content":"thought"`) || !strings.Contains(string(raw), `"include_usage":true`) || !strings.Contains(string(raw), `"max_completion_tokens":120`) || !strings.Contains(string(raw), `"parallel_tool_calls":false`) || !strings.Contains(string(raw), `"file_data":"data:application/pdf;base64,YQ=="`) || !strings.Contains(string(raw), "9007199254740993") || strings.Contains(string(raw), "foreign") || strings.Contains(string(raw), "billing-header") || strings.Contains(string(raw), "cache_control") {
		t.Fatal(effort, string(raw))
	}
	var wire struct{ Messages []convertedChatMessage }
	_ = json.Unmarshal(raw, &wire)
	if len(wire.Messages) != 6 || wire.Messages[3].Role != "tool" || wire.Messages[3].Content != "Tool error: missing" || wire.Messages[4].Role != "user" {
		t.Fatal(string(raw))
	}
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-astra", "deepseek-v4", "glm-5", "kimi-k2.5", "k3"} {
		body["model"], _ = json.Marshal(model)
		_, effort, err := messagesToChat(body)
		if err != nil || effort != "max" {
			t.Fatal(model, effort, err)
		}
	}
	for _, replacement := range []string{
		`{"messages":[{"role":"system","content":"bad"}]}`,
		`{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"INVALID"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result"}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","id":"a","input":[] }]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"lookup","id":"a","input":{}},{"type":"tool_use","name":"lookup","id":"a","input":{}}]}]}`,
		`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
		`{"tool_choice":{"type":"tool","name":"missing"}}`,
		`{"stop_sequences":[""]}`, `{"top_k":3}`, `{"context_management":{}}`,
	} {
		copy := map[string]json.RawMessage{}
		for k, v := range body {
			copy[k] = v
		}
		for k, v := range parse(replacement) {
			copy[k] = v
		}
		if _, _, err := messagesToChat(copy); err == nil {
			t.Fatal("accepted invalid request", replacement)
		}
	}
	s := newChatMessagesStream("public")
	chunks := []string{
		`{"id":"one","choices":[{"index":0,"delta":{"reasoning_content":"thought"}}]}`,
		`{"id":"one","choices":[{"index":0,"delta":{"content":"answer"}}]}`,
		`{"id":"one","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{\"n\":"}}]}}]}`,
		`{"id":"one","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"9007199254740993}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	var stream strings.Builder
	for _, chunk := range chunks {
		w, err := s.observe([]byte(chunk), true)
		if err != nil {
			t.Fatal(err)
		}
		stream.WriteString(w)
	}
	if strings.Contains(stream.String(), "message_stop") || strings.Contains(stream.String(), `"type":"tool_use"`) {
		t.Fatal("premature terminal", stream.String())
	}
	terminal, result, err := s.finish(priceUsage{Input: 10, Output: 5, CacheRead: 3})
	if err != nil || !strings.Contains(string(result), `"input":{"n":9007199254740993}`) || !strings.Contains(string(result), `"stop_reason":"tool_use"`) || strings.Contains(stream.String()+terminal, "response.") || !strings.Contains(terminal, `"cache_read_input_tokens":3`) {
		t.Fatal(string(result), terminal, err)
	}
	// The generated stream must also satisfy the native Messages lifecycle.
	native := newAnthropicChatStream("public", true, nil)
	observation := textObservation{Protocol: "anthropic"}
	for _, line := range strings.Split(stream.String()+terminal, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := []byte(strings.TrimPrefix(line, "data: "))
		if err := observation.observe(data); err != nil {
			t.Fatal(err)
		}
		if _, err := native.event(data, observation.Usage); err != nil {
			t.Fatal("invalid generated lifecycle", err, line)
		}
	}
	if !observation.complete() || observation.Usage.Input != 10 || observation.Usage.CacheRead != 3 {
		t.Fatal("generated metering", observation)
	}
	parallel := parse(`{"model":"m","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"first","input":{}},{"type":"tool_use","id":"b","name":"second","input":{}}]},{"role":"user","content":[{"type":"text","text":"look"},{"type":"tool_result","tool_use_id":"a","content":[{"type":"image","source":{"type":"url","url":"https://example.test/one.png"}}]},{"type":"tool_result","tool_use_id":"b","content":"two"}]}]}`)
	converted, _, err := messagesToChat(parallel)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(converted, &wire)
	if len(wire.Messages) != 5 || wire.Messages[1].Role != "tool" || wire.Messages[2].Role != "tool" || wire.Messages[2].CallID != "b" || wire.Messages[3].Role != "user" {
		t.Fatal("parallel results interrupted", string(converted))
	}
	for _, reason := range []string{"stop", "length", "content_filter"} {
		bridge := newChatMessagesStream("public")
		_, err := bridge.observe([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"`+reason+`"}]}`), false)
		_, raw, e := bridge.finish(priceUsage{})
		want := map[string]string{"stop": "end_turn", "length": "max_tokens", "content_filter": "refusal"}[reason]
		if err != nil || e != nil || !strings.Contains(string(raw), `"stop_reason":"`+want+`"`) {
			t.Fatal(string(raw), err, e)
		}
	}
	for _, raw := range []string{
		`{"choices":[{"message":{"tool_calls":[{"id":"a","type":"function","function":{"name":"lookup","arguments":"[]"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"content":"ok"}}]}`,
		`{"choices":[{"index":2,"message":{"content":"ok"},"finish_reason":"stop"}]}`,
	} {
		bridge := newChatMessagesStream("public")
		_, err := bridge.observe([]byte(raw), false)
		if err == nil {
			_, _, err = bridge.finish(priceUsage{})
		}
		if err == nil {
			t.Fatal("accepted malformed response", raw)
		}
	}
}

func testMessagesChat(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.72:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("Anthropic-Beta", "test-beta")
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
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "messages-chat@example.test", "password": "messages-chat-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "messages-chat@example.test", "password": "messages-chat-password"})["access_token"].(string)
	var calls, mode atomic.Int32
	var captured atomic.Value
	var groupID atomic.Int64
	const usage = `"usage":{"prompt_tokens":20,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"chat-up"}]}`)
			return
		}
		n := calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		captured.Store(string(raw))
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Beta") != "" || credentialString(body, "model") != "chat-up" {
			t.Error("invalid upstream request", r.URL.Path, string(raw))
		}
		if mode.Load() == 8 {
			if _, err := a.DB.Exec("UPDATE groups SET rate_multiplier=7 WHERE id=$1", groupID.Load()); err != nil {
				t.Error(err)
			}
		}
		m := mode.Load()
		if m == 6 {
			w.WriteHeader(503)
			return
		}
		finish := "tool_calls"
		if m == 1 {
			finish = "length"
		}
		if m == 2 {
			finish = "content_filter"
		}
		u := usage
		if m == 3 {
			u = `"usage":null`
		}
		message := `{"role":"assistant","reasoning_content":"thought","content":"answer","tool_calls":[{"index":0,"id":"call_one","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993}"}}]}`
		if string(body["stream"]) != "true" {
			fmt.Fprintf(w, `{"id":"chat_%d","model":"chat-result","choices":[{"index":0,"message":%s,"finish_reason":"%s"}],%s}`, n, message, finish, u)
			return
		}
		if !strings.Contains(string(raw), `"include_usage":true`) {
			t.Error("missing stream metering")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(raw string) { fmt.Fprintf(w, "data: %s\n\n", raw); _ = http.NewResponseController(w).Flush() }
		emit(fmt.Sprintf(`{"id":"chat_%d","model":"chat-result","choices":[{"index":0,"delta":%s}]}`, n, message))
		if m == 4 {
			return
		}
		if m == 7 {
			emit(`{"error":{"message":"private upstream error"}}`)
			return
		}
		emit(fmt.Sprintf(`{"id":"chat_%d","choices":[{"index":0,"delta":{},"finish_reason":"%s"}],%s}`, n, finish, u))
		if m == 5 {
			return
		}
		emit("[DONE]")
	}))
	defer up.Close()
	price := func(platform string) map[string]any {
		if platform == "composite" {
			platform = "openai"
		}
		return map[string]any{"platform": platform, "models": []string{"public-chat"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004, "reasoning_effort_multipliers": map[string]any{"max": 9, "xhigh": 1.5}}
	}
	group := func(platform string) int64 {
		return id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"allow_messages_dispatch": true, "name": "Messages Chat " + platform, "platform": platform, "rate_multiplier": 2, "model_pricing": []any{price(platform)}}))
	}
	account := func(platform string, groups []int64, base string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Messages " + platform, "platform": platform, "type": "apikey", "priority": priority, "rate_multiplier": 3, "group_ids": groups, "credentials": map[string]any{"api_key": "upstream-secret", "base_url": base, "api_protocol": "chat_completions", "model_mapping": map[string]string{"public-chat": "chat-up"}}}))
	}
	gid := group("openai")
	groupID.Store(gid)
	aid := account("openai", []int64{gid}, up.URL, 1)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Messages Chat", "group_id": gid, "quota": 50})
	key, kid := k["key"].(string), id(k)
	body := func(stream bool) map[string]any {
		return map[string]any{"model": "public-chat", "max_tokens": 80, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "question"}}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}}
	}
	check := func(w *httptest.ResponseRecorder, cost string, account int64) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatal(w.Code, w.Body.String())
		}
		var actual, endpoint, requested, upstream string
		var got int64
		err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,requested_model,upstream_model,account_id FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&actual, &endpoint, &requested, &upstream, &got)
		if err != nil || actual != cost || endpoint != "/v1/chat/completions" || requested != "public-chat" || upstream != "chat-up" || got != account {
			t.Fatal(actual, endpoint, requested, upstream, got, err)
		}
	}
	first := call("POST", "/v1/messages", key, body(false), "messages-json")
	check(first, "0.5440000000", aid)
	if !strings.Contains(first.Body.String(), `"type":"message"`) || !strings.Contains(first.Body.String(), `"input":{"n":9007199254740993}`) || strings.Contains(first.Body.String(), "chat-result") {
		t.Fatal(first.Body.String())
	}
	replay := call("POST", "/v1/messages", key, body(false), "messages-json")
	if replay.Body.String() != first.Body.String() || calls.Load() != 1 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("idempotency", replay.Body.String())
	}
	streamed := call("POST", "/v1/messages", key, body(true), "messages-stream")
	check(streamed, "0.5440000000", aid)
	if !strings.Contains(streamed.Body.String(), "message_stop") || strings.Contains(streamed.Body.String(), "[DONE]") || !strings.Contains(streamed.Body.String(), `"cache_read_input_tokens":4`) {
		t.Fatal(streamed.Body.String())
	}
	before := calls.Load()
	again := call("POST", "/v1/messages", key, body(true), "messages-stream")
	if again.Body.String() != streamed.Body.String() || calls.Load() != before {
		t.Fatal("stream replay")
	}
	var balance, quota string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1`, kid).Scan(&balance, &quota); err != nil || balance != "98.91200000" || quota != "1.08800000" {
		t.Fatal(balance, quota, err)
	}
	effort := body(false)
	effort["output_config"] = map[string]string{"effort": "max"}
	check(call("POST", "/v1/messages", key, effort, ""), "0.8160000000", aid)
	if !strings.Contains(captured.Load().(string), `"reasoning_effort":"xhigh"`) {
		t.Fatal(captured.Load())
	}
	next := body(false)
	var result struct{ Content json.RawMessage }
	_ = json.Unmarshal(first.Body.Bytes(), &result)
	next["messages"] = []any{map[string]any{"role": "assistant", "content": result.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_one", "content": "found"}}}}
	check(call("POST", "/v1/messages", key, next, ""), "0.5440000000", aid)
	if !strings.Contains(captured.Load().(string), `"tool_call_id":"call_one"`) || !strings.Contains(captured.Load().(string), `"reasoning_content":"thought"`) {
		t.Fatal(captured.Load())
	}
	before = calls.Load()
	if w := call("POST", "/v1/messages/count_tokens", key, body(false), ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("count_tokens fabricated", w.Code)
	}
	for _, m := range []int32{1, 2, 3, 4, 5, 7} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if m >= 4 && !stream {
				continue
			}
			w := call("POST", "/v1/messages", key, body(stream), "")
			if m >= 3 {
				if !strings.Contains(w.Body.String(), `"type":"error"`) || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "private upstream") {
					t.Fatal(m, w.Body.String())
				}
				var count int
				want := 0
				if m == 5 {
					want = 1
				}
				if err := a.DB.QueryRow(`SELECT count(*) FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != want {
					t.Fatal(m, count, want, err)
				}
			} else {
				check(w, "0.5440000000", aid)
				stop := map[int32]string{1: "max_tokens", 2: "refusal"}[m]
				if !strings.Contains(w.Body.String(), `"stop_reason":"`+stop+`"`) {
					t.Fatal(w.Body.String())
				}
			}
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_messages_chat CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/messages", key, body(true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_messages_chat"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "message_stop") || strings.Contains(failed.Body.String(), `"type":"tool_use"`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal("premature completion", failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow(`SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.544`, failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("recovery", count, err)
	}
	mode.Store(8)
	check(call("POST", "/v1/messages", key, body(false), ""), "0.5440000000", aid)
	mode.Store(0)
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"rate_multiplier": 2})
	before = calls.Load()
	unsupported := body(false)
	unsupported["tools"] = []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}
	if w := call("POST", "/v1/messages", key, unsupported, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("hosted tool bypass", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"max_reasoning_effort": "high"})
	check(call("POST", "/v1/messages", key, effort, ""), "0.5440000000", aid)
	if !strings.Contains(captured.Load().(string), `"reasoning_effort":"high"`) {
		t.Fatal("effort policy bypass", captured.Load())
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"max_reasoning_effort": ""})
	for _, platform := range []string{"kimi", "zhipu", "deepseek", "minimax", "grok"} {
		g := group(platform)
		k := must("POST", "/api/v1/keys", user, map[string]any{"name": platform, "group_id": g})["key"].(string)
		acct := account(platform, []int64{g}, up.URL, 1)
		for _, stream := range []bool{false, true} {
			check(call("POST", "/v1/messages", k, body(stream), ""), "0.5440000000", acct)
		}
	}
	composite := group("composite")
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-chat", "target_platform": "openai", "upstream_model": "public-chat", "endpoint": "messages", "match_type": "exact", "enabled": true})
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite messages", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/messages", ck, body(true), ""), "0.5440000000", aid)
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-chat") {
		t.Fatal("catalog", w.Code, w.Body.String())
	}
	var rejected atomic.Int32
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rejected.Add(1); w.WriteHeader(503) }))
	defer badUp.Close()
	account("openai", []int64{gid}, badUp.URL, 0)
	// A new Key has no sticky account binding from the earlier turns.
	retryKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "retry messages", "group_id": gid})["key"].(string)
	check(call("POST", "/v1/messages", retryKey, body(false), ""), "0.5440000000", aid)
	if rejected.Load() != 1 {
		t.Fatal("retry not exercised", rejected.Load())
	}
	before = calls.Load()
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"claude_code_only": true})
	if w := call("POST", "/v1/messages", key, body(false), ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("client restriction", w.Code)
	}
}
