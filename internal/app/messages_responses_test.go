package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessagesResponses(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var b map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	a := &App{secret: []byte(strings.Repeat("s", 32))}
	g := &gatewayIdentity{}
	g.Key.ID = 10
	g.Key.GroupID = 20
	up := &upstreamAccount{ID: 30, Platform: "openai", Credentials: parse(`{"api_key":"one","base_url":"https://example.test","api_protocol":"responses"}`)}
	reasoning := json.RawMessage(`{"id":"rs_a","type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"opaque-provider-state"}`)
	sig, err := a.sealMessagesReasoning(g, up, reasoning)
	if err != nil {
		t.Fatal(err)
	}
	body := parse(`{"model":"m","max_tokens":100,"stream":true,"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object"}}},"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true},"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YQ=="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"SIGNATURE"},{"type":"tool_use","id":"call_a","name":"lookup","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_a","content":[{"type":"text","text":"found"},{"type":"image","source":{"type":"url","url":"https://example.test/a.png"}}]}]}]}`)
	body["messages"] = json.RawMessage(strings.ReplaceAll(string(body["messages"]), "SIGNATURE", sig))
	saved, binding, err := a.messagesReasoningInput(g, body)
	if err != nil || binding.AccountID != up.ID || binding.Target != responseTarget(up) {
		t.Fatal(binding, err)
	}
	raw, effort, err := messagesToResponses(body, saved)
	if err != nil || effort != "xhigh" {
		t.Fatal(string(raw), effort, err)
	}
	for _, want := range []string{`"max_output_tokens":100`, `"effort":"xhigh"`, `"strict":false`, `"parallel_tool_calls":false`, `"type":"input_file"`, `"type":"input_image"`, `"role":"developer"`, `"encrypted_content":"opaque-provider-state"`, `"summary":[{"type":"summary_text","text":"thought"}]`, `"n\":9007199254740993`} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("missing", want, string(raw))
		}
	}
	if strings.Contains(string(raw), messagesSignaturePrefix) || strings.Contains(string(raw), "cache_control") || strings.Contains(string(raw), "messages") || strings.Contains(string(raw), "stream_options") {
		t.Fatal("internal/wrong protocol data", string(raw))
	}
	// Expired authenticated state is rejected without sleeping or tampering.
	state := messagesReasoning{AccountID: up.ID, Target: responseTarget(up), Expires: time.Now().Unix() - 1, Item: reasoning}
	cipher := a.chatHistoryCipher()
	nonce := make([]byte, cipher.NonceSize())
	expired := messagesSignaturePrefix + base64.RawURLEncoding.EncodeToString(cipher.Seal(nonce, nonce, mustJSON(state), messagesSignatureAAD(g)))
	expiredBody := parse(string(mustJSON(body)))
	expiredBody["messages"] = json.RawMessage(strings.ReplaceAll(string(body["messages"]), sig, expired))
	if _, _, err := a.messagesReasoningInput(g, expiredBody); err == nil {
		t.Fatal("expired reasoning accepted")
	}
	other := *g
	other.Key.ID++
	if _, _, err := a.messagesReasoningInput(&other, body); err == nil {
		t.Fatal("cross-key signature accepted")
	}
	other = *g
	other.Key.GroupID++
	if _, _, err := a.messagesReasoningInput(&other, body); err == nil {
		t.Fatal("cross-group signature accepted")
	}
	changed := parse(string(mustJSON(body)))
	changed["messages"] = json.RawMessage(strings.ReplaceAll(string(body["messages"]), sig, sig[:len(sig)-10]+"AAAAAAAAAA"))
	if _, _, err := a.messagesReasoningInput(g, changed); err == nil {
		t.Fatal("tampered signature accepted")
	}
	rotation := &App{secret: []byte(strings.Repeat("z", 32))}
	if _, _, err := rotation.messagesReasoningInput(g, body); err == nil {
		t.Fatal("deployment secret rotation accepted")
	}
	foreign := parse(`{"model":"m","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"foreign thought","signature":"foreign"},{"type":"text","text":"answer"}]}]}`)
	raw, _, err = messagesToResponses(foreign, nil)
	if err != nil || strings.Contains(string(raw), "foreign") {
		t.Fatal("foreign state forwarded", string(raw), err)
	}
	for _, patch := range []string{`{"stop_sequences":["END"]}`, `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, `{"messages":[{"role":"user","content":[{"type":"tool_use","id":"a","name":"n","input":{}}]}]}`, `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"n","input":[]}]}]}`} {
		cp := parse(string(mustJSON(body)))
		for k, v := range parse(patch) {
			cp[k] = v
		}
		if _, _, err := messagesToResponses(cp, saved); err == nil {
			t.Fatal("unsupported input accepted", patch)
		}
	}
	// One response stream with a partial text and a complete reasoning signature.
	seal := func(raw json.RawMessage) (string, error) { return a.sealMessagesReasoning(g, up, raw) }
	bridge := newResponsesMessagesStream("public", seal)
	events := []string{
		`{"type":"response.created","response":{"id":"resp_a","status":"in_progress","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_a","type":"message","content":[]}}`,
		`{"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_a","delta":"first"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_a","type":"message","content":[{"type":"output_text","text":"first!"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"rs_a","type":"reasoning","summary":[]}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":1,"summary_index":0,"delta":"thought"}`,
		`{"type":"response.output_item.done","output_index":1,"item":` + string(reasoning) + `}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"n\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"9007199254740993}"}`,
	}
	var stream strings.Builder
	for _, event := range events {
		wire, err := bridge.event([]byte(event), priceUsage{})
		if err != nil {
			t.Fatal(event, err)
		}
		stream.WriteString(wire)
	}
	if !strings.Contains(stream.String(), "first") || !strings.Contains(stream.String(), "thinking_delta") || strings.Contains(stream.String(), `"type":"tool_use"`) {
		t.Fatal(stream.String())
	}
	output := `[{"id":"msg_a","type":"message","content":[{"type":"output_text","text":"first!"}]},` + string(reasoning) + `,{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"n\":9007199254740993}"},{"id":"msg_b","type":"message","content":[{"type":"output_text","text":"last"}]}]`
	terminal, err := bridge.event([]byte(`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":`+output+`}}`), priceUsage{Input: 12, Output: 7, CacheRead: 5})
	if err != nil || !strings.Contains(terminal, "message_stop") || !strings.Contains(terminal, `"stop_reason":"tool_use"`) {
		t.Fatal(terminal, err)
	}
	native := newAnthropicChatStream("public", true, nil)
	native.Capture = true
	observation := textObservation{Protocol: "anthropic"}
	for _, line := range strings.Split(stream.String()+terminal, "\n") {
		if strings.HasPrefix(line, "data: ") {
			data := []byte(strings.TrimPrefix(line, "data: "))
			if err := observation.observe(data); err != nil {
				t.Fatal(err)
			}
			if _, err := native.event(data, observation.Usage); err != nil {
				t.Fatal("generated lifecycle", line, err)
			}
		}
	}
	if !observation.complete() || observation.Usage.CacheRead != 5 {
		t.Fatal(observation)
	}
	streamSignature := native.Blocks[1].Signature + native.Blocks[1].SignatureDelta.String()
	streamInput := parse(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"SIGNATURE"}]}]}`)
	streamInput["messages"] = json.RawMessage(strings.ReplaceAll(string(streamInput["messages"]), "SIGNATURE", streamSignature))
	recovered, _, err := a.messagesReasoningInput(g, streamInput)
	if err != nil || !strings.Contains(string(recovered[streamSignature]), `"type":"summary_text"`) || strings.Contains(string(recovered[streamSignature]), `"Type"`) {
		t.Fatal("SSE signature replay", err, recovered)
	}

	result, err := newResponsesMessagesStream("public", seal).response([]byte(`{"id":"resp_a","status":"completed","output":`+output+`}`), priceUsage{Input: 12, Output: 7, CacheRead: 5})
	if err != nil || !strings.Contains(string(result), `"input":{"n":9007199254740993}`) || strings.Contains(string(result), "opaque-provider-state") {
		t.Fatal(string(result), err)
	}
	for _, suffix := range []string{
		`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_a","type":"message","content":[{"type":"output_text","text":"different"}]}}`,
		`{"type":"response.output_text.delta","output_index":0,"delta":"after done"}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_a","type":"message","content":[]}}`,
		`{"type":"response.completed","response":{"id":"resp_a","status":"completed","output":[]}}`,
	} {
		s := newResponsesMessagesStream("m", seal)
		for _, event := range events[:4] {
			if _, err := s.event([]byte(event), priceUsage{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.event([]byte(suffix), priceUsage{}); err == nil {
			t.Fatal("invalid terminal/change accepted", suffix)
		}
	}
	for _, payload := range []string{
		`{"id":"only","type":"reasoning","summary":[],"encrypted_content":"cipher"}`,
		`{"id":"raw","type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"raw thought"}]}`,
	} {
		s := newResponsesMessagesStream("m", seal)
		raw, err := s.response([]byte(`{"status":"completed","output":[`+payload+`]}`), priceUsage{})
		if err != nil || !strings.Contains(string(raw), `"type":"thinking"`) {
			t.Fatal(string(raw), err)
		}
	}
	// JSON-only ciphertext must also count toward memory limits.
	oversized := messagesResponseItem{ID: "rs_big", Type: "reasoning", Encrypted: strings.Repeat("x", (16<<20)+1)}
	if err := newResponsesMessagesStream("m", seal).put(0, oversized, true); err == nil {
		t.Fatal("oversize signature accepted")
	}
	for _, event := range []string{
		`{"type":"response.output_text.delta","output_index":0,"delta":"orphan"}`,
		`{"type":"response.output_item.added","output_index":4096,"item":{"id":"m","type":"message"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"m","type":"web_search_call"}}`,
	} {
		s := newResponsesMessagesStream("m", seal)
		_, _ = s.event([]byte(events[0]), priceUsage{})
		if _, err := s.event([]byte(event), priceUsage{}); err == nil {
			t.Fatal("invalid stream accepted", event)
		}
	}
	for _, data := range []string{
		`{"status":"completed","output":[{"id":"a","type":"function_call","call_id":"a","name":"n","arguments":"[]"}]}`,
		`{"status":"completed","output":[{"id":"a","type":"function_call","call_id":"a","name":"n","arguments":"{}"},{"id":"b","type":"function_call","call_id":"a","name":"n","arguments":"{}"}]}`,
		`{"status":"completed"}`, `{"status":"failed","output":[]}`,
	} {
		if _, err := newResponsesMessagesStream("m", seal).response([]byte(data), priceUsage{}); err == nil {
			t.Fatal("invalid output accepted", data)
		}
	}
}

func testMessagesResponses(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.73:1234"
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
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "messages-responses@example.test", "password": "messages-responses-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "messages-responses@example.test", "password": "messages-responses-password"})["access_token"].(string)
	var calls, mode atomic.Int32
	var captured atomic.Value
	var groupID atomic.Int64
	const usage = `"usage":{"input_tokens":20,"output_tokens":5,"input_tokens_details":{"cached_tokens":4}}`
	const reasoning = `{"id":"rs_a","type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"upstream-opaque"}`
	const output = `[` + reasoning + `,{"id":"msg_a","type":"message","content":[{"type":"output_text","text":"answer"}]},{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":"{\"n\":9007199254740993}"}]`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"responses-up"}]}`)
			return
		}
		n := calls.Add(1)
		if r.URL.Path == "/v1/responses/input_tokens" {
			fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":12}`)
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw := mustJSON(body)
		captured.Store(string(raw))
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Beta") != "" || credentialString(body, "model") != "responses-up" || string(body["store"]) != "false" || body["messages"] != nil {
			t.Error("upstream request", r.URL.Path, string(raw))
		}
		m := mode.Load()
		if m == 6 {
			w.WriteHeader(503)
			return
		}
		if m == 8 {
			if _, err := a.DB.Exec("UPDATE groups SET rate_multiplier=7 WHERE id=$1", groupID.Load()); err != nil {
				t.Error(err)
			}
		}
		status, reason := "completed", ""
		if m == 1 {
			status, reason = "incomplete", "max_output_tokens"
		}
		if m == 2 {
			status, reason = "incomplete", "content_filter"
		}
		if m == 5 {
			status = "failed"
		}
		u := usage
		if m == 3 {
			u = `"usage":null`
		}
		response := fmt.Sprintf(`{"id":"resp_%d","object":"response","model":"response-result","status":"%s","incomplete_details":{"reason":"%s"},"output":%s,%s}`, n, status, reason, output, u)
		if string(body["stream"]) != "true" {
			fmt.Fprint(w, response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); _ = http.NewResponseController(w).Flush() }
		emit(fmt.Sprintf(`{"type":"response.created","response":{"id":"resp_%d","object":"response","status":"in_progress","output":[]}}`, n))
		emit(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_a","type":"reasoning","summary":[]}}`)
		emit(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"thought"}`)
		emit(`{"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`)
		emit(`{"type":"response.output_item.added","output_index":1,"item":{"id":"msg_a","type":"message","content":[]}}`)
		emit(`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"ans"}`)
		if m == 4 {
			return
		}
		if m == 7 {
			emit(`{"type":"error","message":"private upstream error"}`)
			return
		}
		emit(`{"type":"response.output_item.done","output_index":1,"item":{"id":"msg_a","type":"message","content":[{"type":"output_text","text":"answer"}]}}`)
		emit(`{"type":"response.output_item.added","output_index":2,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"lookup","arguments":""}}`)
		emit(`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"n\":9007199254740993}"}`)
		emit(`{"type":"response.` + status + `","response":` + response + `}`)
	}))
	defer up.Close()
	price := func(platform string) map[string]any {
		if platform == "composite" {
			platform = "openai"
		}
		return map[string]any{"platform": platform, "models": []string{"public-responses"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004, "reasoning_effort_multipliers": map[string]any{"max": 9, "xhigh": 1.5}}
	}
	group := func(platform string) int64 {
		return id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"allow_messages_dispatch": true, "name": "Messages Responses " + platform, "platform": platform, "rate_multiplier": 2, "model_pricing": []any{price(platform)}}))
	}
	account := func(platform string, groups []int64, base string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Messages Responses " + platform, "platform": platform, "type": "apikey", "priority": priority, "rate_multiplier": 3, "group_ids": groups, "credentials": map[string]any{"api_key": "upstream-secret", "base_url": base, "api_protocol": "responses", "model_mapping": map[string]string{"public-responses": "responses-up"}}}))
	}
	gid := group("openai")
	groupID.Store(gid)
	aid := account("openai", []int64{gid}, up.URL, 1)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Messages Responses", "group_id": gid, "quota": 50})
	key, kid := k["key"].(string), id(k)
	body := func(stream bool) map[string]any {
		return map[string]any{"model": "public-responses", "max_tokens": 80, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "question"}}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}}
	}
	check := func(w *httptest.ResponseRecorder, cost string, account int64) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatal(w.Code, w.Body.String())
		}
		var actual, endpoint, requested, upstream string
		var got int64
		err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,requested_model,upstream_model,account_id FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&actual, &endpoint, &requested, &upstream, &got)
		if err != nil || actual != cost || endpoint != "/v1/responses" || requested != "public-responses" || upstream != "responses-up" || got != account {
			t.Fatal(actual, endpoint, requested, upstream, got, err)
		}
	}
	first := call("POST", "/v1/messages", key, body(false), "messages-response-json")
	check(first, "0.5440000000", aid)
	if !strings.Contains(first.Body.String(), messagesSignaturePrefix) || strings.Contains(first.Body.String(), "upstream-opaque") || !strings.Contains(first.Body.String(), `"input":{"n":9007199254740993}`) {
		t.Fatal(first.Body.String())
	}
	replay := call("POST", "/v1/messages", key, body(false), "messages-response-json")
	if replay.Body.String() != first.Body.String() || calls.Load() != 1 {
		t.Fatal("JSON replay")
	}
	streamed := call("POST", "/v1/messages", key, body(true), "messages-response-stream")
	check(streamed, "0.5440000000", aid)
	if !strings.Contains(streamed.Body.String(), "message_stop") || !strings.Contains(streamed.Body.String(), "signature_delta") || strings.Contains(streamed.Body.String(), "response.completed") {
		t.Fatal(streamed.Body.String())
	}
	before := calls.Load()
	replay = call("POST", "/v1/messages", key, body(true), "messages-response-stream")
	if replay.Body.String() != streamed.Body.String() || calls.Load() != before {
		t.Fatal("SSE replay")
	}
	var balance, quota string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1`, kid).Scan(&balance, &quota); err != nil || balance != "98.91200000" || quota != "1.08800000" {
		t.Fatal(balance, quota, err)
	}
	var streamSig string
	for _, line := range strings.Split(streamed.Body.String(), "\n") {
		if strings.HasPrefix(line, "data: ") {
			var e struct {
				Delta struct{ Type, Signature string }
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e)
			if e.Delta.Type == "signature_delta" {
				streamSig = e.Delta.Signature
			}
		}
	}
	fromStream := body(false)
	fromStream["messages"] = []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "thought", "signature": streamSig}, map[string]any{"type": "text", "text": "answer"}}}, map[string]any{"role": "user", "content": "continue"}}
	check(call("POST", "/v1/messages", key, fromStream, ""), "0.5440000000", aid)
	if !strings.Contains(captured.Load().(string), `"encrypted_content":"upstream-opaque"`) {
		t.Fatal("SSE continuation", captured.Load())
	}
	var result struct{ Content json.RawMessage }
	_ = json.Unmarshal(first.Body.Bytes(), &result)
	next := body(false)
	next["messages"] = []any{map[string]any{"role": "assistant", "content": result.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_a", "content": "found"}}}}
	check(call("POST", "/v1/messages", key, next, ""), "0.5440000000", aid)
	if !strings.Contains(captured.Load().(string), `"encrypted_content":"upstream-opaque"`) || !strings.Contains(captured.Load().(string), `"type":"function_call_output"`) {
		t.Fatal("reasoning/tool continuation", captured.Load())
	}
	before = calls.Load()
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "other reasoning", "group_id": gid})["key"].(string)
	if w := call("POST", "/v1/messages", other, next, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("cross-key reasoning", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	if w := call("POST", "/v1/messages", key, next, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("source rotation accepted", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "upstream-secret"}})
	backup := account("openai", []int64{gid}, up.URL, 2)
	mode.Store(6)
	if w := call("POST", "/v1/messages", key, next, ""); w.Code != 503 || calls.Load() != before+1 {
		t.Fatal("signed history failed over", w.Code, calls.Load()-before)
	}
	mode.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", backup), admin, map[string]any{"schedulable": false})
	for _, m := range []int32{1, 2, 3, 4, 5, 7} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if (m == 4 || m == 7) && !stream {
				continue
			}
			w := call("POST", "/v1/messages", key, body(stream), "")
			if m >= 3 {
				if !strings.Contains(w.Body.String(), `"type":"error"`) || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "private upstream") {
					t.Fatal(m, w.Body.String())
				}
				want := 0
				if m == 5 {
					want = 1
				}
				var count int
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
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_messages_responses CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/messages", key, body(true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_messages_responses"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "message_stop") || strings.Contains(failed.Body.String(), `"type":"tool_use"`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal("premature tool completion", failed.Body.String())
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
	effort := body(false)
	effort["output_config"] = map[string]string{"effort": "max"}
	check(call("POST", "/v1/messages", key, effort, ""), "0.8160000000", aid)
	var requested, actual string
	if err := a.DB.QueryRow("SELECT requested_reasoning_effort,reasoning_effort FROM usage_logs WHERE user_id=$1 ORDER BY id DESC LIMIT 1", uid).Scan(&requested, &actual); err != nil || requested != "max" || actual != "xhigh" {
		t.Fatal(requested, actual, err)
	}
	before = calls.Load()
	if w := call("POST", "/v1/messages/count_tokens", key, body(false), ""); w.Code != 200 || calls.Load() != before+1 || !strings.Contains(w.Body.String(), `"input_tokens":12`) {
		t.Fatal("count_tokens bridge", w.Code, w.Body.String())
	}
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
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-responses", "target_platform": "openai", "upstream_model": "public-responses", "endpoint": "messages", "match_type": "exact", "enabled": true})
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite messages responses", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/messages", ck, body(true), ""), "0.5440000000", aid)
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-responses") {
		t.Fatal("catalog", w.Code, w.Body.String())
	}
	var rejected atomic.Int32
	badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rejected.Add(1); w.WriteHeader(503) }))
	defer badUp.Close()
	account("openai", []int64{gid}, badUp.URL, 0)
	retryKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "responses retry", "group_id": gid})["key"].(string)
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
