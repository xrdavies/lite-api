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

func TestMessagesGemini(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var b map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	body := parse(`{"model":"gemini-3.1-pro","max_tokens":10240,"stream":true,"top_k":20,"stop_sequences":["END"],"output_config":{"effort":"max","format":{"type":"json_schema","schema":{"type":"object","additionalProperties":false}}},"system":[{"type":"text","text":"x-anthropic-billing-header: private"},{"type":"text","text":"rules"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}}],"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YQ=="}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"foreign thought","signature":"foreign"},{"type":"tool_use","id":"c1","name":"lookup","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","is_error":true,"content":[{"type":"text","text":"missing"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}}]},{"type":"text","text":"again"}]}]}`)
	raw, effort, err := messagesToGemini(body, nil, false)
	if err != nil || effort != "high" {
		t.Fatal(string(raw), effort, err)
	}
	for _, want := range []string{`"thinkingLevel":"HIGH"`, `"maxOutputTokens":10240`, `"topK":20`, `"stopSequences":["END"]`, `"allowedFunctionNames":["lookup"]`, `"parametersJsonSchema"`, `"responseJsonSchema"`, `"additionalProperties":false`, `9007199254740993`, `"mimeType":"application/pdf"`, `"error":"missing"`, `"thoughtSignature":"skip_thought_signature_validator"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("lost input", want, string(raw))
		}
	}
	if strings.Contains(string(raw), "foreign") || strings.Contains(string(raw), "private") || strings.Contains(string(raw), `"max_tokens"`) || strings.Contains(string(raw), `"model":`) {
		t.Fatal("wrong protocol data", string(raw))
	}
	var parsed struct {
		Contents []struct{ Parts []map[string]json.RawMessage }
	}
	_ = json.Unmarshal(raw, &parsed)
	if len(parsed.Contents) != 3 || len(parsed.Contents[2].Parts) != 2 || !strings.Contains(string(parsed.Contents[2].Parts[0]["functionResponse"]), `"parts":[{"inlineData"`) {
		t.Fatal("tool media not attached", string(raw))
	}
	countRaw, _, err := messagesToGemini(body, nil, true)
	if err != nil || !strings.Contains(string(countRaw), `"generateContentRequest"`) || !strings.Contains(string(countRaw), `"model":"models/gemini-3.1-pro"`) || !strings.Contains(string(countRaw), `"systemInstruction"`) {
		t.Fatal("count conversion", string(countRaw), err)
	}
	for _, patch := range []string{
		`{"top_k":0}`, `{"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`,
		`{"thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"output_config":{},"thinking":{"type":"enabled","budget_tokens":100}}`,
		`{"output_config":{},"thinking":{"type":"enabled","budget_tokens":12000}}`,
		`{"thinking":{"type":"disabled"}}`, `{"thinking":{"type":"bad"}}`,
		`{"messages":[{"role":"user","content":[{"type":"thinking","thinking":"bad"}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"unknown","content":"x"}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"lookup","input":[]}]}]}`,
		`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c","name":"lookup","input":{}},{"type":"tool_use","id":"c","name":"lookup","input":{}}]}]}`,
		`{"tools":[{"name":"search","type":"web_search_20250305"}]}`, `{"context_management":{}}`,
	} {
		b := parse(string(mustJSON(body)))
		_ = json.Unmarshal([]byte(patch), &b)
		if _, _, err := messagesToGemini(b, nil, false); err == nil {
			t.Fatal("invalid request accepted", patch)
		}
	}
	for _, tc := range []struct{ thinking, effort, wire string }{
		{`{"type":"enabled","budget_tokens":4096}`, "", `"thinkingBudget":4096`},
		{`{"type":"adaptive"}`, "", `"includeThoughts":true`},
	} {
		b := parse(string(mustJSON(body)))
		delete(b, "output_config")
		b["thinking"] = json.RawMessage(tc.thinking)
		raw, effort, err := messagesToGemini(b, nil, false)
		if err != nil || effort != tc.effort || !strings.Contains(string(raw), tc.wire) {
			t.Fatal(string(raw), effort, err)
		}
	}
	a := &App{secret: []byte(strings.Repeat("s", 32))}
	g := &gatewayIdentity{}
	g.Key.ID = 10
	g.Key.GroupID = 20
	up := &upstreamAccount{ID: 30, Platform: "gemini", Credentials: parse(`{"api_key":"one","base_url":"https://example.test"}`)}
	seal := func(raw json.RawMessage) (string, error) { return a.sealMessagesReasoning(g, up, raw) }
	bridge := newGeminiMessagesStream("public", seal)
	events := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"first"},{"text":"thought","thought":true,"thoughtSignature":"thought-opaque"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"thoughtSignature":"standalone-opaque"},{"text":"signed text","thoughtSignature":"text-opaque"}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"tool-opaque"},{"text":"after tool"}]},"finishReason":"STOP"}]}`,
	}
	var wire strings.Builder
	for _, e := range events {
		chunk, err := bridge.observe([]byte(e))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(chunk, "tool_use") || strings.Contains(chunk, "after tool") || strings.Contains(chunk, "message_stop") || strings.Contains(chunk, "opaque") {
			t.Fatal("premature/unencrypted output", chunk)
		}
		wire.WriteString(chunk)
	}
	end, raw, err := bridge.finish(priceUsage{Input: 10, CacheRead: 5, Output: 8})
	wire.WriteString(end)
	if err != nil || !strings.Contains(string(raw), `"stop_reason":"tool_use"`) || !strings.Contains(string(raw), `"cache_read_input_tokens":5`) || strings.Contains(string(raw), "opaque") {
		t.Fatal(string(raw), err)
	}
	// Validate the generated wire with the independent native Messages state machine.
	native := newAnthropicChatStream("public", true, nil)
	for _, frame := range strings.Split(wire.String(), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if _, err := native.event([]byte(strings.TrimPrefix(line, "data: ")), priceUsage{}); err != nil {
					t.Fatal("invalid Messages stream", err, frame)
				}
			}
		}
	}
	if !native.Stopped {
		t.Fatal("missing stop")
	}
	var response struct{ Content []map[string]json.RawMessage }
	_ = json.Unmarshal(raw, &response)
	if len(response.Content) != 6 || credentialString(response.Content[0], "text") != "first" || credentialString(response.Content[5], "text") != "after tool" {
		t.Fatal("lost block order", string(raw))
	}
	next := parse(`{"model":"gemini-3.1-pro","max_tokens":100,"messages":[]}`)
	next["messages"] = mustJSON([]any{map[string]any{"role": "assistant", "content": response.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "found"}}}})
	saved, binding, err := a.messagesReasoningInput(g, next)
	if err != nil || binding == nil || binding.AccountID != up.ID || binding.Target != responseTarget(up) {
		t.Fatal(binding, err)
	}
	converted, _, err := messagesToGemini(next, saved, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, sig := range []string{"thought-opaque", "standalone-opaque", "text-opaque", "tool-opaque"} {
		if !strings.Contains(string(converted), sig) {
			t.Fatal("lost signature", sig, string(converted))
		}
	}
	if strings.Contains(string(converted), messagesSignaturePrefix) || strings.Contains(string(converted), "skip_thought") {
		t.Fatal("lost exact replay", string(converted))
	}
	other := *g
	other.Key.ID++
	if _, _, err := a.messagesReasoningInput(&other, next); err == nil {
		t.Fatal("cross-key signature accepted")
	}
	for _, field := range []string{"thinking", "text", "input", "name", "id"} {
		changed := parse(string(mustJSON(next)))
		before, after := "", ""
		switch field {
		case "thinking":
			before = `"thinking":"thought"`
			after = `"thinking":"changed"`
		case "text":
			before = `"text":"signed text"`
			after = `"text":"changed"`
		case "input":
			before = `9007199254740993`
			after = `2`
		case "name":
			before = `"name":"lookup"`
			after = `"name":"other"`
		case "id":
			before = `"id":"c1"`
			after = `"id":"other"`
		}
		changed["messages"] = json.RawMessage(strings.Replace(string(changed["messages"]), before, after, 1))
		if _, _, err := messagesToGemini(changed, saved, false); err == nil {
			t.Fatal("changed signed content accepted", field)
		}
	}
	response.Content = append(response.Content, response.Content[1])
	next["messages"] = mustJSON([]any{map[string]any{"role": "assistant", "content": response.Content}})
	if _, _, err := messagesToGemini(next, saved, false); err == nil {
		t.Fatal("duplicate signed part accepted")
	}
}

func testMessagesGemini(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.132:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("Anthropic-Beta", "client-secret")
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
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "messages-gemini@example.test", "password": "messages-gemini-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "messages-gemini@example.test", "password": "messages-gemini-password"})["access_token"].(string)
	var mode, calls, counts, priceChange, continuation atomic.Int32
	var gid int64
	const metadata = `"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2,"totalTokenCount":23}`
	parts := `[{"text":"thought","thought":true,"thoughtSignature":"private-thought"},{"text":"answer"},{"functionCall":{"id":"c1","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"private-tool"}]`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-3.1-pro","supportedGenerationMethods":["generateContent","countTokens"]}]}`)
			return
		}
		calls.Add(1)
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.Header.Get("X-Goog-Api-Key") != "messages-gemini-secret" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Version") != "" || r.Header.Get("Anthropic-Beta") != "" || !strings.HasPrefix(r.URL.Path, "/v1beta/models/gemini-3.1-pro:") {
			t.Error("upstream isolation", r.URL.Path)
		}
		if mode.Load() == 6 {
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			counts.Add(1)
			var nested map[string]json.RawMessage
			_ = json.Unmarshal(b["generateContentRequest"], &nested)
			if nested["contents"] == nil || nested["tools"] == nil || nested["systemInstruction"] == nil || credentialString(nested, "model") != "models/gemini-3.1-pro" || b["contents"] != nil {
				t.Error("incomplete count request", b)
			}
			var config map[string]json.RawMessage
			_ = json.Unmarshal(nested["generationConfig"], &config)
			if config["maxOutputTokens"] != nil {
				t.Error("invented count output limit", config)
			}
			if mode.Load() == 7 {
				fmt.Fprint(w, `{"totalTokens":-1}`)
				return
			}
			fmt.Fprint(w, `{"totalTokens":17,"cachedContentTokenCount":2}`)
			return
		}
		if b["contents"] == nil || b["messages"] != nil || b["model"] != nil {
			t.Error("invalid conversion", b)
		}
		if continuation.Swap(0) == 1 {
			wire := string(b["contents"])
			if !strings.Contains(wire, `"thoughtSignature":"private-thought"`) || !strings.Contains(wire, `"thoughtSignature":"private-tool"`) || !strings.Contains(wire, `"functionResponse":{"id":"c1"`) || strings.Contains(wire, messagesSignaturePrefix) {
				t.Error("lost continuation", wire)
			}
		}
		if priceChange.Swap(0) == 1 {
			if _, err := a.DB.Exec("UPDATE groups SET rate_multiplier=3 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		usage := metadata
		if mode.Load() == 3 {
			usage = `"usageMetadata":null`
		}
		reason := "STOP"
		if mode.Load() == 1 {
			reason = "MAX_TOKENS"
		}
		if mode.Load() == 2 {
			reason = "SAFETY"
		}
		stream := strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
		if !stream {
			fmt.Fprintf(w, `{"modelVersion":"actual-gemini","candidates":[{"content":{"role":"model","parts":%s},"finishReason":"%s"}],%s}`, parts, reason, usage)
			return
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Error("missing SSE query")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); _ = http.NewResponseController(w).Flush() }
		emit(`{"modelVersion":"actual-gemini","candidates":[{"content":{"role":"model","parts":[{"text":"thought","thought":true,"thoughtSignature":"private-thought"},{"text":"answer"}]}}],` + usage + `}`)
		if mode.Load() == 4 {
			return
		}
		if mode.Load() == 5 {
			emit(`{"error":{"message":"private-error"}}`)
			return
		}
		emit(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c1","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"private-tool"}]},"finishReason":"` + reason + `"}]}`)
		emit(`{` + usage + `}`)
	}))
	defer up.Close()
	price := map[string]any{"platform": "gemini", "models": []string{"public-messages-gemini"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "reasoning_effort_multipliers": map[string]any{"max": 9, "high": 1.5}}
	gid = id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Messages Gemini", "platform": "gemini", "rate_multiplier": 2, "model_pricing": []any{price}}))
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Messages Gemini", "group_id": gid, "quota": 50})["key"].(string)
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Messages Gemini", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "messages-gemini-secret", "base_url": up.URL, "model_mapping": map[string]string{"public-messages-gemini": "gemini-3.1-pro"}}}))
	body := func(stream bool) map[string]any {
		return map[string]any{"model": "public-messages-gemini", "max_tokens": 100, "stream": stream, "system": "rules", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}}
	}
	check := func(w *httptest.ResponseRecorder, cost string) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatalf("gateway %d %s", w.Code, w.Body.String())
		}
		var actual, endpoint string
		var input, output, cache int64
		err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,input_tokens,output_tokens,cache_read_tokens FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&actual, &endpoint, &input, &output, &cache)
		if err != nil || actual != cost || !strings.HasPrefix(endpoint, "/v1beta/models/gemini-3.1-pro:") || input != 10 || output != 8 || cache != 5 {
			t.Fatal("billing", actual, endpoint, input, output, cache, err)
		}
	}
	first := call("POST", "/v1/messages", key, body(false), "messages-gemini-json")
	check(first, "0.5500000000")
	replay := call("POST", "/v1/messages", key, body(false), "messages-gemini-json")
	if replay.Body.String() != first.Body.String() || calls.Load() != 1 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("idempotency replay", replay.Body.String())
	}
	var balance, used string
	if err := a.DB.QueryRow(`SELECT balance::text,(SELECT quota_used::text FROM api_keys WHERE key=$2) FROM users WHERE id=$1`, uid, key).Scan(&balance, &used); err != nil || balance != "99.45000000" || used != "0.55000000" {
		t.Fatal(balance, used, err)
	}
	counted := body(false)
	delete(counted, "max_tokens")
	countResult := call("POST", "/v1/messages/count_tokens", key, counted, "messages-gemini-count")
	if countResult.Code != 200 || countResult.Body.String() != `{"input_tokens":17}` || counts.Load() != 1 {
		t.Fatal("count", countResult.Code, countResult.Body.String())
	}
	countReplay := call("POST", "/v1/messages/count_tokens", key, counted, "messages-gemini-count")
	if countReplay.Body.String() != countResult.Body.String() || counts.Load() != 1 {
		t.Fatal("count replay")
	}
	var logs int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", countResult.Header().Get("X-Request-ID")).Scan(&logs); err != nil || logs != 0 {
		t.Fatal("count charged", logs, err)
	}
	var result struct{ Content []map[string]json.RawMessage }
	_ = json.Unmarshal(first.Body.Bytes(), &result)
	next := body(false)
	next["messages"] = []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": result.Content}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "found"}}}}
	continuation.Store(1)
	check(call("POST", "/v1/messages", key, next, ""), "0.5500000000")
	if continuation.Load() != 0 {
		t.Fatal("continuation not dispatched")
	}
	beforeInvalid := calls.Load()
	invalid := body(false)
	invalid["tool_choice"] = map[string]any{"type": "auto", "disable_parallel_tool_use": true}
	if w := call("POST", "/v1/messages", key, invalid, ""); w.Code != 400 || calls.Load() != beforeInvalid {
		t.Fatal("invalid controls dispatched", w.Code, w.Body.String())
	}
	secondKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "other Gemini key", "group_id": gid})["key"].(string)
	before := calls.Load()
	if w := call("POST", "/v1/messages", secondKey, next, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("cross-key continuation", w.Code, w.Body.String())
	}
	for _, stream := range []bool{false, true} {
		check(call("POST", "/v1/messages", key, body(stream), ""), "0.5500000000")
	}
	streamed := call("POST", "/v1/messages", key, body(true), "")
	check(streamed, "0.5500000000")
	decoder := newAnthropicChatStream("public", false, nil)
	decoder.Capture = true
	for _, frame := range strings.Split(streamed.Body.String(), "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if _, err := decoder.event([]byte(strings.TrimPrefix(line, "data: ")), priceUsage{}); err != nil {
					t.Fatal("invalid Messages SSE", err)
				}
			}
		}
	}
	blocks := []any{}
	for _, b := range decoder.Blocks {
		switch b.Type {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text + b.TextDelta.String()})
		case "thinking":
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": b.Thinking + b.ThinkingDelta.String(), "signature": b.Signature + b.SignatureDelta.String()})
		case "tool_use":
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": b.Input, "signature": b.Signature})
		}
	}
	streamNext := body(false)
	streamNext["messages"] = []any{map[string]any{"role": "assistant", "content": blocks}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "found"}}}}
	continuation.Store(1)
	check(call("POST", "/v1/messages", key, streamNext, ""), "0.5500000000")
	priceChange.Store(1)
	check(call("POST", "/v1/messages", key, body(false), ""), "0.5500000000")
	check(call("POST", "/v1/messages", key, body(false), ""), "0.8250000000")
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"rate_multiplier": 2})
	for _, m := range []int32{1, 2, 3, 4, 5} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if m >= 4 && !stream {
				continue
			}
			w := call("POST", "/v1/messages", key, body(stream), "")
			if m <= 2 {
				check(w, "0.5500000000")
				want := "max_tokens"
				if m == 2 {
					want = "refusal"
				}
				if !strings.Contains(w.Body.String(), `"stop_reason":"`+want+`"`) {
					t.Fatal(w.Body.String())
				}
			} else {
				if strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), `"type":"tool_use"`) || !strings.Contains(w.Body.String(), `"type":"error"`) || strings.Contains(w.Body.String(), "private-error") {
					t.Fatal("false success", w.Code, w.Body.String())
				}
				want := 1
				if m == 3 {
					want = 0
				}
				var n int
				if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n); err != nil || n != want {
					t.Fatal("failure usage", n, want, err)
				}
			}
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_messages_gemini_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/messages", key, body(true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_messages_gemini_receipt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "message_stop") || strings.Contains(failed.Body.String(), `"type":"tool_use"`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal(failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.55", failed.Header().Get("X-Request-ID")).Scan(&n); err != nil || n != 1 || calls.Load() != before {
		t.Fatal("recovery", n, err)
	}
	reasoning := body(false)
	reasoning["output_config"] = map[string]any{"effort": "max"}
	w := call("POST", "/v1/messages", key, reasoning, "")
	check(w, "0.8250000000")
	var requested, effective string
	if err := a.DB.QueryRow("SELECT requested_reasoning_effort,reasoning_effort FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&requested, &effective); err != nil || requested != "max" || effective != "high" {
		t.Fatal(requested, effective, err)
	}
	budget := body(false)
	budget["max_tokens"] = 8192
	budget["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 4096}
	w = call("POST", "/v1/messages", key, budget, "")
	check(w, "0.5500000000")
	if err := a.DB.QueryRow("SELECT COALESCE(reasoning_effort,'') FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&effective); err != nil || effective != "" {
		t.Fatal("budget invented billable level", effective, err)
	}
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Messages Gemini composite", "platform": "composite", "rate_multiplier": 2, "model_pricing": []any{price}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	for _, endpoint := range []string{"messages", "count_tokens"} {
		must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-messages-gemini", "target_platform": "gemini", "upstream_model": "public-messages-gemini", "endpoint": endpoint, "match_type": "exact", "enabled": true})
	}
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/messages", ck, body(true), ""), "0.5500000000")
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-messages-gemini") {
		t.Fatal("discovery", w.Code, w.Body.String())
	}
	if w := call("POST", "/v1/messages/count_tokens", ck, counted, ""); w.Code != 200 || w.Body.String() != `{"input_tokens":17}` {
		t.Fatal("composite count", w.Code, w.Body.String())
	}
	// Continuation never switches source on credentials rotation or upstream failure.
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-secret"}})
	before = calls.Load()
	if w := call("POST", "/v1/messages", key, next, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("rotated source dispatched", w.Code, w.Body.String())
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "messages-gemini-secret"}})
	var alternatives atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		alternatives.Add(1)
		fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":"fallback"}]},"finishReason":"STOP"}],%s}`, metadata)
	}))
	defer fallback.Close()
	fallbackID := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Messages Gemini fallback", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "fallback-secret", "base_url": fallback.URL}}))
	mode.Store(6)
	before = calls.Load()
	if w := call("POST", "/v1/messages", key, next, ""); w.Code != 503 || calls.Load() != before+1 || alternatives.Load() != 0 {
		t.Fatal("continuation switched account", w.Code, calls.Load()-before, alternatives.Load())
	}
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, map[string]any{})
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", fallbackID), admin, map[string]any{"priority": 100})
	fresh := must("POST", "/api/v1/keys", user, map[string]any{"name": "Gemini normal retry", "group_id": gid})["key"].(string)
	before = calls.Load()
	normal := call("POST", "/v1/messages", fresh, body(false), "")
	if normal.Code != 200 || !strings.Contains(normal.Body.String(), "fallback") || calls.Load() != before+1 || alternatives.Load() != 1 {
		t.Fatal("unbound failover", normal.Code, normal.Body.String(), calls.Load()-before, alternatives.Load())
	}
	mode.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, map[string]any{})
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", fallbackID), admin, map[string]any{"schedulable": false})
}
