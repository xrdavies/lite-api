package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"
)

func TestResponsesChatRequest(t *testing.T) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","instructions":"rules","stream":true,"reasoning":{"effort":"high"},"max_output_tokens":42,"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"custom","name":"patch"}],"tool_choice":{"type":"custom","name":"patch"},"input":[{"role":"developer","content":"extra rules"},{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"https://example.test/a.png","detail":"high"},{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,YQ=="}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},{"type":"function_call","call_id":"one","name":"lookup","arguments":"{}"},{"type":"custom_tool_call","call_id":"two","name":"patch","input":"raw patch"},{"type":"function_call_output","call_id":"one","output":"result"},{"type":"custom_tool_call_output","call_id":"two","output":"ok"},{"role":"developer","content":"later rules"}]}`), &body)
	in, err := responsesToChatRequest(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Messages) != 6 || in.Messages[0].Content != "rules\n\nextra rules" || in.Messages[5].Role != "user" || in.Messages[2].Reasoning != "thinking" || len(in.Messages[2].Calls) != 2 || in.Messages[2].Calls[1].Function.Arguments != `{"input":"raw patch"}` || !in.Custom["patch"] {
		t.Fatal("history translation", in.Messages)
	}
	raw, _ := json.Marshal(in.Body)
	if !strings.Contains(string(raw), `"max_completion_tokens":42`) || !strings.Contains(string(raw), `"include_usage":true`) || !strings.Contains(string(raw), `"response_format":{"json_schema"`) || !strings.Contains(string(raw), `"tool_choice":{"function":{"name":"patch"}`) || in.Body["previous_response_id"] != nil || in.Body["store"] != false {
		t.Fatal("request translation", string(raw))
	}
	for _, input := range []string{`[{"type":"reasoning","encrypted_content":"opaque"}]`, `[{"type":"compaction_trigger"}]`, `[{"role":"user","content":[{"type":"input_audio"}]}]`, `[{"type":"function_call","name":"a","call_id":"c","arguments":"{"}]`, `[{"type":"function_call_output","output":"a"}]`} {
		copy := map[string]json.RawMessage{}
		for k, v := range body {
			copy[k] = v
		}
		copy["input"] = json.RawMessage(input)
		if _, err := responsesToChatRequest(copy, nil); err == nil {
			t.Fatal("invalid input accepted", input)
		}
	}
	for _, raw := range []string{`{"model":"m","input":"hello","prompt":{}}`, `{"model":"m","input":"hello","truncation":"auto"}`, `{"model":"m","input":"hello","tools":[{"type":"web_search","name":"search"}]}`} {
		var input map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &input)
		if _, err := responsesToChatRequest(input, nil); err == nil {
			t.Fatal("unsupported request accepted", raw)
		}
	}
	var extra map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","input":[{"type":"additional_tools","tools":[{"type":"function","name":"extra","parameters":{"type":"object"}}]},{"role":"user","content":"hello"}]}`), &extra)
	if in, err := responsesToChatRequest(extra, nil); err != nil || in.Body["tools"] == nil {
		t.Fatal("additional tools lost", err)
	}
	var original map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","instructions":"replace me","input":[{"role":"system","content":"persistent rule"},{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"type":"reasoning","summary":[{"type":"summary_text","text":"later reasoning"}]},{"type":"function_call","call_id":"one","name":"lookup","arguments":"{}"}]}`), &original)
	first, err := responsesToChatRequest(original, nil)
	if err != nil || len(first.History) != 3 || first.History[0].Content != "persistent rule" || first.History[2].Reasoning != "later reasoning" {
		t.Fatal("input instructions or later reasoning lost", first, err)
	}
	next, err := responsesToChatRequest(map[string]json.RawMessage{"model": json.RawMessage(`"m"`), "instructions": json.RawMessage(`"new rule"`), "input": json.RawMessage(`"next"`)}, first.History)
	if err != nil || next.Messages[0].Content != "new rule\n\npersistent rule" || next.History[0].Content != "persistent rule" {
		t.Fatal("continuation instruction semantics", next, err)
	}
}

func TestResponsesChatNamespaces(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := parse(`{"model":"m","input":[{"role":"user","content":"hello"},{"type":"function_call","namespace":"files","name":"read","call_id":"one","arguments":"{}"}],"tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}],"tool_choice":{"type":"function","namespace":"files","name":"read"}}`)
	r := httptest.NewRequest("POST", "/responses", nil)
	if _, err := parseTextRequest(r, "responses", body); err != nil {
		t.Fatal("native namespace admission", err)
	}
	converted, err := responsesToChatRequest(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(converted.Body)
	if strings.Contains(string(wire), `"namespace"`) || !strings.Contains(string(wire), `"name":"files__read"`) || converted.History[1].Calls[0].Namespace != "files" || converted.Messages[1].Calls[0].Namespace != "" {
		t.Fatal("namespace lowering/history", string(wire))
	}
	s := newChatResponsesStream("m", converted.Custom)
	s.Namespaces = converted.Namespaces
	stream, err := s.observe([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_read","type":"function","function":{"name":"files__read","arguments":"{}"}}]},"finish_reason":"tool_calls"}],`+chatBridgeUsage+`}`), true)
	if err != nil {
		t.Fatal(err)
	}
	terminal, raw, err := s.finish()
	if err != nil || !strings.Contains(stream+terminal, `"namespace":"files"`) || !strings.Contains(string(raw), `"name":"read"`) || strings.Contains(string(raw), "files__read") {
		t.Fatal("namespace restoration", string(raw), err)
	}
	continued, err := responsesToChatRequest(parse(`{"model":"m","input":[{"type":"function_call_output","call_id":"call_read","output":"ok"}]}`), []convertedChatMessage{s.assistant()})
	if err != nil || continued.Messages[0].Calls[0].Function.Name != "files__read" || continued.Namespaces["files__read"].Namespace != "files" {
		t.Fatal("namespace history without redeclaration", continued, err)
	}
	for _, raw := range []string{
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"one"}]},{"type":"function","name":"ns__one"}]}`,
		`{"tools":[{"type":"function","name":"ns__one"},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"one"}]}]}`,
		`{"tools":[{"type":"namespace","name":"a__b","tools":[{"type":"function","name":"c"}]},{"type":"namespace","name":"a","tools":[{"type":"function","name":"b__c"}]}]}`,
		`{"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"one"},{"type":"function","name":"one","strict":true}]}]}`,
		`{"input":[{"type":"function_call","name":"ns__one","call_id":"a","arguments":"{}"},{"type":"function_call","namespace":"ns","name":"one","call_id":"b","arguments":"{}"}]}`,
	} {
		b := parse(raw)
		b["model"] = json.RawMessage(`"m"`)
		if b["input"] == nil {
			b["input"] = json.RawMessage(`"hello"`)
		}
		if _, err := responsesToChatRequest(b, nil); err == nil {
			t.Fatal("namespace collision accepted", raw)
		}
	}
	for _, raw := range []string{
		`{"type":"namespace","name":"ns","tools":[{"type":"web_search"}]}`,
		`{"type":"namespace","name":"ns","tools":[{"type":"namespace","name":"nested","tools":[{"type":"function","name":"f"}]}]}`,
		`{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}],"children":[{"type":"web_search"}]}`,
	} {
		b := parse(`{"model":"m","input":"hello","tools":[` + raw + `]}`)
		if _, err := parseTextRequest(r, "responses", b); err == nil {
			t.Fatal("invalid namespace admitted", raw)
		}
	}
	duplicate := parse(`{"model":"m","input":"hello","tools":[{"type":"namespace","name":"ns","children":[{"type":"function","name":"one"},{"type":"function","name":"one"}]}]}`)
	if got, err := responsesToChatRequest(duplicate, nil); err != nil || len(got.Body["tools"].([]any)) != 1 {
		t.Fatal("namespace duplicate deduplication", err)
	}
	additional := parse(`{"model":"m","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read"}]}]},{"role":"user","content":"hello"}]}`)
	if _, err := parseTextRequest(r, "responses", additional); err != nil {
		t.Fatal("additional namespace admission", err)
	}
	if got, err := responsesToChatRequest(additional, nil); err != nil || got.Namespaces["files__read"] != (responseToolName{"files", "read"}) {
		t.Fatal("additional namespace lowering", err)
	}
	left := flattenedToolName(strings.Repeat("工具", 30), "read")
	right := flattenedToolName(strings.Repeat("工具", 30), "write")
	if len(left) > 64 || !utf8.ValidString(left) || left == right {
		t.Fatal("long namespace identity", left, right)
	}
}

const chatBridgeUsage = `"usage":{"prompt_tokens":20,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":3},"completion_tokens_details":{"reasoning_tokens":6},"total_tokens":28}`

func TestChatResponsesEvents(t *testing.T) {
	s := newChatResponsesStream("public", map[string]bool{"patch": true})
	events := []string{
		`{"id":"chat_one","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`{"id":"chat_one","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
		`{"id":"chat_one","choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"id":"call_one","type":"function","function":{"name":"get","arguments":"{\"x\":"}},{"index":4,"id":"call_two","type":"function","function":{"name":"patch","arguments":"{\"input\":\""}}]}}]}`,
		`{"id":"chat_one","choices":[{"index":0,"delta":{"tool_calls":[{"index":3,"function":{"arguments":"1}"}},{"index":4,"function":{"arguments":"patch\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"id":"chat_one","choices":[],` + chatBridgeUsage + `}`,
	}
	var wire strings.Builder
	for _, e := range events {
		out, err := s.observe([]byte(e), true)
		if err != nil {
			t.Fatal(err)
		}
		wire.WriteString(out)
	}
	if strings.Contains(wire.String(), "response.completed") || strings.Contains(wire.String(), "function_call_arguments.done") {
		t.Fatal("terminal output before finish")
	}
	terminal, raw, err := s.finish()
	if err != nil {
		t.Fatal(err)
	}
	wire.WriteString(terminal)
	var result struct {
		ID, Model, Status string
		Output            []map[string]any
		Usage             map[string]any
	}
	_ = json.Unmarshal(raw, &result)
	if result.Model != "public" || result.Status != "completed" || len(result.Output) != 4 || result.Output[2]["arguments"] != `{"x":1}` || result.Output[3]["type"] != "custom_tool_call" || result.Output[3]["input"] != "patch" || result.Usage["input_tokens"] != float64(20) {
		t.Fatal("terminal conversion", string(raw))
	}
	sequence := 0
	added := map[string]bool{}
	for _, line := range strings.Split(wire.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e map[string]any
		_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
		if e["sequence_number"] != float64(sequence) {
			t.Fatal("event sequence", e)
		}
		sequence++
		if e["type"] == "response.output_item.added" {
			added[e["item"].(map[string]any)["id"].(string)] = true
		}
		if id, ok := e["item_id"].(string); ok && !added[id] {
			t.Fatal("event before output item", e)
		}
	}
	if len(added) != 4 || strings.Contains(wire.String(), "[DONE]") {
		t.Fatal("invalid Responses lifecycle", wire.String())
	}
	for _, finish := range []string{"length", "content_filter"} {
		s := newChatResponsesStream("m", nil)
		_, err := s.observe([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"`+finish+`"}],`+chatBridgeUsage+`}`), false)
		if err != nil {
			t.Fatal(err)
		}
		wire, raw, err := s.finish()
		if err != nil || !strings.Contains(wire, "response.incomplete") || !strings.Contains(string(raw), `"status":"incomplete"`) {
			t.Fatal("incomplete lost", err)
		}
	}
	for _, raw := range []string{`{"choices":[{"index":1,"delta":{}}]}`, `{"choices":[{"delta":{"tool_calls":[{"index":-1}]}}]}`, `{"choices":[{"delta":{},"finish_reason":"unknown"}]}`} {
		if _, err := newChatResponsesStream("m", nil).observe([]byte(raw), true); err == nil {
			t.Fatal("bad chat event accepted", raw)
		}
	}
	s = newChatResponsesStream("m", nil)
	if _, _, err := s.finish(); err == nil {
		t.Fatal("missing finish accepted")
	}
	_, _ = s.observe([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"bad","type":"function","function":{"name":"get","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`), false)
	if _, _, err := s.finish(); err == nil {
		t.Fatal("truncated tool emitted as complete")
	}
	s = newChatResponsesStream("m", nil)
	_, _ = s.observe([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`), true)
	if _, err := s.observe([]byte(`{"choices":[{"delta":{"reasoning":"late reasoning"}}]}`), true); err == nil {
		t.Fatal("reasoning alias accepted after finish")
	}
	s = newChatResponsesStream("m", nil)
	if _, err := s.observe([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"same","type":"function","function":{"name":"one","arguments":"{}"}},{"id":"same","type":"function","function":{"name":"two","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), false); err == nil {
		t.Fatal("ambiguous tool IDs accepted")
	}
}

func testResponsesChat(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-client")
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
		var e struct{ Data map[string]any }
		if json.Unmarshal(w.Body.Bytes(), &e) != nil {
			t.Fatal("management response")
		}
		return e.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "responses-chat@example.test", "password": "responses-chat-password", "balance": 30}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "responses-chat@example.test", "password": "responses-chat-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses Chat", "platform": "openai", "rate_multiplier": 2}))
	keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "Responses Chat", "group_id": gid, "quota": 30})
	key, kid := keyData["key"].(string), id(keyData)
	price := map[string]any{"platform": "openai", "models": []string{"public-chat"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004}
	must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Responses Chat", "group_ids": []int64{gid}, "model_pricing": []any{price}})
	var calls, mode atomic.Int32
	var gotMessages atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"mapped-chat"}]}`)
			return
		}
		calls.Add(1)
		if r.URL.Path == "/v1/responses/input_tokens" {
			fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":12}`)
			return
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "mapped-chat" || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer chat-upstream" || r.Header.Get("Cookie") != "" || body["previous_response_id"] != nil || body["input"] != nil || string(body["store"]) != "false" {
			t.Error("upstream Chat request", r.URL.Path, body)
		}
		if mode.Load() == 6 {
			w.WriteHeader(503)
			return
		}
		gotMessages.Store(string(body["messages"]))
		w.Header().Set("X-Request-ID", "chat-upstream-request")
		finish := "tool_calls"
		if mode.Load() == 1 {
			finish = "length"
		}
		if mode.Load() == 2 {
			finish = "content_filter"
		}
		usage := chatBridgeUsage
		if mode.Load() == 3 {
			usage = `"usage":null`
		}
		message := `{"role":"assistant","content":"converted answer","reasoning_content":"converted thought","tool_calls":[{"id":"call_convert","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`
		if mode.Load() == 7 {
			message = strings.ReplaceAll(message, `"name":"lookup"`, `"name":"files__lookup"`)
			if !strings.Contains(string(body["tools"]), `"name":"files__lookup"`) || strings.Contains(string(body["tools"]), `"namespace"`) {
				t.Error("namespaced tool not lowered")
			}
		}
		if string(body["stream"]) != "true" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"chat_result","model":"real-chat","choices":[{"index":0,"message":`+message+`,"finish_reason":"`+finish+`"}],`+usage+`}`)
			return
		}
		if !strings.Contains(string(body["stream_options"]), `"include_usage":true`) {
			t.Error("missing metering request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); http.NewResponseController(w).Flush() }
		emit(`{"id":"chat_stream","model":"real-chat","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"converted thought"}}]}`)
		toolChunk := `{"id":"chat_stream","choices":[{"index":0,"delta":{"content":"converted answer","tool_calls":[{"index":0,"id":"call_convert","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}`
		if mode.Load() == 7 {
			toolChunk = strings.ReplaceAll(toolChunk, `"name":"lookup"`, `"name":"files__lookup"`)
		}
		emit(toolChunk)
		if mode.Load() != 4 {
			emit(`{"id":"chat_stream","choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}]}`)
		}
		emit(`{"id":"chat_stream","choices":[],` + usage + `}`)
		if mode.Load() != 5 {
			emit("[DONE]")
		}
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Chat bridge", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "chat-upstream", "api_protocol": "chat_completions", "base_url": up.URL, "model_mapping": map[string]string{"public-chat": "mapped-chat"}}}))
	body := func(stream bool) map[string]any {
		return map[string]any{"model": "public-chat", "instructions": "current rules", "input": "private query", "stream": stream, "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	}
	check := func(w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal("converted response", w.Code, w.Body.String())
		}
		var cost, endpoint, requested, mapped, response string
		if err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,requested_model,upstream_model,upstream_response_model FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&cost, &endpoint, &requested, &mapped, &response); err != nil || cost != "0.6140000000" || endpoint != "/v1/chat/completions" || requested != "public-chat" || mapped != "mapped-chat" || response != "real-chat" {
			t.Fatal("converted billing", cost, endpoint, err)
		}
	}
	first := call("POST", "/v1/responses", key, body(false), "convert-json")
	check(first)
	var result struct {
		ID, Status string
		Output     []map[string]any
	}
	_ = json.Unmarshal(first.Body.Bytes(), &result)
	if result.Status != "completed" || len(result.Output) != 3 || result.Output[0]["type"] != "reasoning" || result.Output[2]["call_id"] != "call_convert" {
		t.Fatal("converted output", first.Body.String())
	}
	var balance, used string
	if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE u.id=$1 AND k.id=$2", uid, kid).Scan(&balance, &used); err != nil || balance != "29.38600000" || used != "0.61400000" {
		t.Fatal("converted balance", balance, used, err)
	}
	for _, path := range []string{"/responses", "/backend-api/codex/responses"} {
		w := call("POST", path, key, body(false), "convert-json")
		if w.Code != 200 || w.Body.String() != first.Body.String() || calls.Load() != 1 {
			t.Fatal("converted replay", w.Code)
		}
	}
	g := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	saved, err := a.Redis.Get(t.Context(), responseBindingKey(g, result.ID)).Result()
	if err != nil || strings.Contains(saved, "private query") || strings.Contains(saved, "converted answer") || strings.Contains(saved, "current rules") {
		t.Fatal("history plaintext leaked", err)
	}
	var binding responseBinding
	_ = json.Unmarshal([]byte(saved), &binding)
	if binding.History == "" {
		t.Fatal("missing saved history")
	}
	wrong := &gatewayIdentity{Key: gatewayKey{ID: kid + 1, GroupID: gid}}
	if _, err := a.chatHistory(wrong, result.ID, &binding); err == nil {
		t.Fatal("history decrypted for other Key")
	}
	if _, err := a.chatHistory(g, result.ID+"tampered", &binding); err == nil {
		t.Fatal("history decrypted for other response ID")
	}
	changed := binding
	changed.Target += "tampered"
	if _, err := a.chatHistory(g, result.ID, &changed); err == nil {
		t.Fatal("history decrypted for other upstream source")
	}
	continued := body(false)
	continued["previous_response_id"] = result.ID
	continued["instructions"] = "replacement rules"
	continued["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_convert", "output": "tool result"}}
	check(call("POST", "/responses", key, continued, ""))
	messages := gotMessages.Load().(string)
	if !strings.Contains(messages, "private query") || !strings.Contains(messages, "converted thought") || !strings.Contains(messages, "tool result") || !strings.Contains(messages, "replacement rules") || strings.Contains(messages, "current rules") {
		t.Fatal("continuation history", messages)
	}
	withSystem := body(false)
	withSystem["input"] = []any{map[string]any{"role": "system", "content": "persistent rule"}, map[string]any{"role": "user", "content": "remember me"}}
	w := call("POST", "/responses", key, withSystem, "")
	check(w)
	var systemResponse struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &systemResponse)
	withSystem["previous_response_id"], withSystem["instructions"], withSystem["input"] = systemResponse.ID, "replacement rules", "continue"
	check(call("POST", "/responses", key, withSystem, ""))
	messages = gotMessages.Load().(string)
	if !strings.Contains(messages, "persistent rule") || !strings.Contains(messages, "replacement rules") || strings.Contains(messages, "current rules") {
		t.Fatal("input system instruction lost on continuation", messages)
	}
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "other", "group_id": gid})["key"].(string)
	before := calls.Load()
	itemRequest := body(false)
	itemRequest["previous_response_id"] = result.ID
	itemRequest["input"] = []any{map[string]string{"type": "item_reference", "id": "msg_convert"}}
	if w := call("POST", "/responses", key, itemRequest, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("item reference entered Chat conversion", w.Code)
	}
	if w := call("POST", "/responses", other, continued, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("cross-Key continuation", w.Code)
	}
	if w := call("POST", "/responses/input_tokens", key, continued, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("local converted response ID sent to native counting", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	if w := call("POST", "/responses", key, continued, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("continued after credential rotation", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "chat-upstream"}})
	noStore := body(false)
	noStore["store"] = false
	w = call("POST", "/responses", key, noStore, "")
	check(w)
	var unbound struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &unbound)
	if n, err := a.Redis.Exists(t.Context(), responseBindingKey(g, unbound.ID)).Result(); err != nil || n != 0 {
		t.Fatal("store=false retained history", n, err)
	}
	for _, m := range []int32{0, 1, 2} {
		mode.Store(m)
		idem := fmt.Sprintf("converted-stream-%d", m)
		w := call("POST", "/responses", key, body(true), idem)
		check(w)
		want := "response.completed"
		if m != 0 {
			want = "response.incomplete"
		}
		if !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), "[DONE]") {
			t.Fatal("converted SSE", w.Body.String())
		}
		before := calls.Load()
		replay := call("POST", "/v1/responses", key, body(true), idem)
		if replay.Code != 200 || replay.Body.String() != w.Body.String() || calls.Load() != before {
			t.Fatal("converted SSE replay", replay.Code)
		}
	}
	for _, m := range []int32{3, 4, 5} {
		mode.Store(m)
		w := call("POST", "/responses", key, body(true), "")
		if strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "event: error") {
			t.Fatal("bad stream completed", m, w.Body.String())
		}
		var count int
		err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count)
		want := 1
		if m == 3 {
			want = 0
		}
		if err != nil || count != want {
			t.Fatal("bad stream usage", m, count, err)
		}
	}
	mode.Store(0)
	before = calls.Load()
	if w := call("POST", "/responses/compact", key, body(false), ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("native compaction converted", w.Code)
	}
	if w := call("POST", "/responses/input_tokens", key, body(false), ""); w.Code != 200 || calls.Load() != before+1 || !strings.Contains(w.Body.String(), `"input_tokens":12`) {
		t.Fatal("native count endpoint", w.Code, w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_reverse_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/responses", key, body(true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_reverse_receipt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "response.completed") || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal("success escaped failed settlement", failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.614", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("recovery charged twice", count, err)
	}

	mode.Store(7)
	for _, stream := range []bool{false, true} {
		namespaced := body(stream)
		namespaced["tools"] = []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}}
		namespaced["tool_choice"] = map[string]string{"type": "function", "namespace": "files", "name": "lookup"}
		w := call("POST", "/responses", key, namespaced, "")
		check(w)
		if !strings.Contains(w.Body.String(), `"namespace":"files"`) || !strings.Contains(w.Body.String(), `"name":"lookup"`) || strings.Contains(w.Body.String(), "files__lookup") {
			t.Fatal("namespaced output", w.Body.String())
		}
		if !stream {
			var response struct{ ID string }
			_ = json.Unmarshal(w.Body.Bytes(), &response)
			namespaced["previous_response_id"] = response.ID
			namespaced["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_convert", "output": "found"}}
			check(call("POST", "/responses", key, namespaced, ""))
			if got := gotMessages.Load().(string); !strings.Contains(got, `"name":"files__lookup"`) || strings.Contains(got, `"namespace"`) {
				t.Fatal("namespaced continuation", got)
			}
		}
	}
	mode.Store(0)
	// A continuation remains bound even when a healthy fallback is available.
	var spareCalls atomic.Int32
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spareCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"spare","model":"real-chat","choices":[{"message":{"content":"spare"},"finish_reason":"stop"}],`+chatBridgeUsage+`}`)
	}))
	defer spare.Close()
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "spare Chat bridge", "platform": "openai", "type": "apikey", "priority": 99, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "chat-upstream", "base_url": spare.URL, "api_protocol": "chat_completions", "model_mapping": map[string]string{"public-chat": "mapped-chat"}}})
	mode.Store(6)
	if w := call("POST", "/responses", key, continued, ""); w.Code != 503 || spareCalls.Load() != 0 {
		t.Fatal("continuation switched accounts", w.Code, spareCalls.Load())
	}
	mode.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)

	// Same-platform conversion and discovery work in each supported CN group
	// and in explicitly routed composite groups.
	for _, platform := range []string{"kimi", "zhipu", "deepseek", "minimax", "composite"} {
		group := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "reverse " + platform, "platform": platform}))
		accountPlatform := platform
		if platform == "composite" {
			accountPlatform = "openai"
			must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", group), admin, map[string]any{"public_model": "public-chat", "match_type": "exact", "target_platform": "openai", "upstream_model": "public-chat", "endpoint": "responses", "enabled": true})
		}
		must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "reverse " + platform, "platform": accountPlatform, "type": "apikey", "group_ids": []int64{group}, "credentials": map[string]any{"api_key": "chat-upstream", "api_protocol": "chat_completions", "base_url": up.URL, "model_mapping": map[string]string{"public-chat": "mapped-chat"}}})
		must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "reverse " + platform, "group_ids": []int64{group}, "model_pricing": []any{map[string]any{"platform": accountPlatform, "models": []string{"public-chat"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004}}})
		key := must("POST", "/api/v1/keys", user, map[string]any{"name": "reverse " + platform, "group_id": group})["key"].(string)
		for _, stream := range []bool{false, true} {
			w := call("POST", "/responses", key, body(stream), "")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "converted answer") {
				t.Fatal("reverse platform conversion", platform, stream, w.Code, w.Body.String())
			}
			var cost, endpoint string
			if err := a.DB.QueryRow("SELECT actual_cost::text,upstream_endpoint FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost, &endpoint); err != nil || cost != "0.3070000000" || endpoint != "/v1/chat/completions" {
				t.Fatal("reverse platform billing", platform, cost, endpoint, err)
			}
		}
		if w := call("GET", "/v1/models", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-chat") {
			t.Fatal("reverse model discovery", platform, w.Code, w.Body.String())
		}
	}
}
