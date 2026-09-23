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

func TestChatGemini(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	request := parse(`{"model":"gemini-3.1-pro","stream":true,"max_tokens":20,"max_completion_tokens":42,"reasoning_effort":"minimal","stop":["END"],"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","additionalProperties":false,"properties":{"n":{"type":"integer"}}}}},"messages":[{"role":"system","content":"rules"},{"role":"developer","content":"more rules"},{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}},{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,YQ=="}}]},{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"n\":9007199254740993}"},"extra_content":{"google":{"thought_signature":"opaque-signature"}}},{"id":"c2","type":"custom","custom":{"name":"patch","input":"raw patch"}}]},{"role":"tool","tool_call_id":"c1","content":"found"},{"role":"tool","tool_call_id":"c2","content":"done"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","additionalProperties":false},"strict":true}},{"type":"custom","custom":{"name":"patch"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	raw, custom, effort, err := chatToGemini(request)
	if err != nil || effort != "low" || !custom["patch"] {
		t.Fatal(string(raw), effort, custom, err)
	}
	for _, want := range []string{`"maxOutputTokens":42`, `"thinkingLevel":"LOW"`, `"stopSequences":["END"]`, `"parametersJsonSchema"`, `"responseJsonSchema"`, `"additionalProperties":false`, `"allowedFunctionNames":["lookup"]`, `"thoughtSignature":"opaque-signature"`, `"thoughtSignature":"skip_thought_signature_validator"`, `9007199254740993`, `"mimeType":"application/pdf"`, `"functionResponse"`} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("lost input", want, string(raw))
		}
	}
	for _, absent := range []string{`"model":`, `"stream":`, `"store":`, `"max_tokens":`, `"strict":`, `"tool_call_id":`, `"id":"c`} {
		if strings.Contains(string(raw), absent) {
			t.Fatal("wire field", absent, string(raw))
		}
	}
	var output struct {
		Contents []struct {
			Role  string
			Parts []json.RawMessage
		}
		System struct{ Parts []json.RawMessage } `json:"systemInstruction"`
	}
	_ = json.Unmarshal(raw, &output)
	if len(output.Contents) != 3 || len(output.Contents[0].Parts) != 3 || len(output.Contents[1].Parts) != 2 || len(output.Contents[2].Parts) != 2 || len(output.System.Parts) != 2 {
		t.Fatal(string(raw))
	}
	for _, patch := range []string{
		`{"tools":[],"tool_choice":"required"}`, `{"n":2}`, `{"stop":[""]}`, `{"parallel_tool_calls":false}`, `{"service_tier":"priority"}`, `{"reasoning_effort":"invalid"}`, `{"reasoning_effort":"none"}`, `{"tool_choice":{"type":"function","function":{"name":"unknown"}}}`,
		`{"tools":[{"type":"function","function":{"name":"duplicate"}},{"type":"function","function":{"name":"duplicate"}}]}`,
		`{"tools":[{"type":"function","function":{"name":"invalid name"}}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"unknown","content":"hello"}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"one","type":"function","function":{"name":"lookup","arguments":"[]"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"foreign"}}]}]}`,
		`{"tools":[{"type":"web_search"}]}`, `{"response_format":{"type":"json_schema","json_schema":{"schema":null}}}`,
	} {
		v := map[string]json.RawMessage{}
		for k, x := range request {
			v[k] = x
		}
		_ = json.Unmarshal([]byte(patch), &v)
		if _, _, _, err := chatToGemini(v); err == nil {
			t.Fatal("accepted invalid request", patch)
		}
	}
	for _, tc := range []struct{ model, effort, effective, wire string }{
		{"gemini-2.5-flash", "minimal", "", `"thinkingBudget":1024`},
		{"gemini-2.5-flash", "none", "", `"thinkingBudget":0`},
		{"gemini-2.5-pro", "medium", "", `"thinkingBudget":8192`},
		{"gemini-3-flash", "minimal", "minimal", `"thinkingLevel":"MINIMAL"`},
		{"gemini-3.1-pro", "xhigh", "high", `"thinkingLevel":"HIGH"`},
	} {
		v := parse(`{"messages":[{"role":"user","content":"hello"}]}`)
		v["model"], _ = json.Marshal(tc.model)
		v["reasoning_effort"], _ = json.Marshal(tc.effort)
		raw, _, effort, err := chatToGemini(v)
		if err != nil || effort != tc.effective || !strings.Contains(string(raw), tc.wire) {
			t.Fatal(tc, string(raw), effort, err)
		}
	}
	v := parse(`{"model":"gemini-2.5-pro","reasoning_effort":"none","messages":[{"role":"user","content":"hello"}]}`)
	if _, _, _, err := chatToGemini(v); err == nil {
		t.Fatal("disabled mandatory thinking")
	}
	delete(v, "reasoning_effort")
	if raw, _, effort, err := chatToGemini(v); err != nil || effort != "" || strings.Contains(string(raw), "thinkingConfig") {
		t.Fatal("invented effort", string(raw), err)
	}
	s := newGeminiChatStream("public", true, map[string]bool{"patch": true})
	events := []string{
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"thought","thought":true}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"hello "}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"text":"world"},{"functionCall":{"id":"c1","name":"lookup","args":{"n":9007199254740993}},"thoughtSignature":"opaque"},{"functionCall":{"name":"patch","args":{"input":"patch text"}}}]},"finishReason":"STOP"}]}`,
		`{"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2}}`,
	}
	for _, event := range events {
		wire, err := s.observe([]byte(event))
		if err != nil || strings.Contains(wire, "tool_calls") || strings.Contains(wire, "[DONE]") {
			t.Fatal(wire, err)
		}
	}
	end, raw, err := s.finish(priceUsage{Input: 10, CacheRead: 5, Output: 8})
	if err != nil || !strings.Contains(string(raw), `"content":"hello world"`) || !strings.Contains(string(raw), `"reasoning_content":"thought"`) || !strings.Contains(string(raw), `"reasoning_tokens":2`) || !strings.Contains(string(raw), `"prompt_tokens":15`) || !strings.Contains(end, `"thought_signature":"opaque"`) || !strings.Contains(end, `"input":"patch text"`) || !strings.HasSuffix(end, "data: [DONE]\n\n") {
		t.Fatal(string(raw), end, err)
	}
	if _, err := s.observe([]byte(events[0])); err == nil {
		t.Fatal("accepted content after finish")
	}
	for _, bad := range []string{
		`{"candidates":[{"index":1}]}`, `{"candidates":[{},{}]}`, `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"x"}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":[]}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"c","name":"lookup","args":{}}},{"functionCall":{"id":"c","name":"lookup","args":{}}}]}}]}`,
		`{"candidates":[{"finishReason":"MALFORMED_FUNCTION_CALL"}]}`,
	} {
		if _, err := newGeminiChatStream("p", false, nil).observe([]byte(bad)); err == nil {
			t.Fatal("accepted invalid output", bad)
		}
	}
	for _, tc := range []struct{ raw, finish string }{
		{`{"candidates":[{"finishReason":"MAX_TOKENS"}]}`, "length"},
		{`{"candidates":[{"finishReason":"SAFETY"}]}`, "content_filter"},
		{`{"promptFeedback":{"blockReason":"SAFETY"}}`, "content_filter"},
	} {
		s := newGeminiChatStream("p", false, nil)
		_, err := s.observe([]byte(tc.raw))
		end, _, finishErr := s.finish(priceUsage{})
		if err != nil || finishErr != nil || !strings.Contains(end, `"finish_reason":"`+tc.finish+`"`) || strings.Contains(end, `"usage"`) {
			t.Fatal(end, err, finishErr)
		}
	}
	if _, _, err := newGeminiChatStream("p", false, nil).finish(priceUsage{}); err == nil {
		t.Fatal("accepted incomplete output")
	}
	s = newGeminiChatStream("p", false, nil)
	if _, err := s.observe([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup"}}]},"finishReason":"STOP"}]}`)); err != nil {
		t.Fatal("no-argument tool", err)
	}
	if _, raw, err := s.finish(priceUsage{}); err != nil || !strings.Contains(string(raw), `"arguments":"{}"`) {
		t.Fatal("no-argument tool result", string(raw), err)
	}
	s = newGeminiChatStream("p", false, nil)
	s.Size = 16 << 20
	if _, err := s.observe([]byte(events[0])); err == nil {
		t.Fatal("unbounded stream")
	}
}

func testChatGemini(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.131:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
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
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "chat-gemini@example.test", "password": "chat-gemini-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "chat-gemini@example.test", "password": "chat-gemini-password"})["access_token"].(string)
	var calls, mode, priceChange, continuation atomic.Int32
	var gid int64
	const metadata = `"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2,"totalTokenCount":23}`
	parts := `[{"text":"thought","thought":true},{"text":"answer"},{"functionCall":{"id":"call_a","name":"lookup","args":{"n":1}},"thoughtSignature":"opaque"}]`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"models":[{"name":"models/gemini-3.1-pro","supportedGenerationMethods":["generateContent"]}]}`)
			return
		}
		calls.Add(1)
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		stream := strings.HasSuffix(r.URL.Path, ":streamGenerateContent")
		if r.URL.Path != "/v1beta/models/gemini-3.1-pro:generateContent" && !stream || r.Header.Get("X-Goog-Api-Key") != "gemini-secret" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Beta") != "" || b["contents"] == nil || b["messages"] != nil || b["model"] != nil {
			t.Error("invalid upstream routing/isolation", r.URL.Path, b)
		}
		if stream && r.URL.Query().Get("alt") != "sse" {
			t.Error("missing SSE query")
		}
		if continuation.Swap(0) == 1 {
			wire := string(b["contents"])
			if !strings.Contains(wire, `"thoughtSignature":"opaque"`) || !strings.Contains(wire, `"functionResponse"`) || !strings.Contains(wire, `"content":"tool result"`) {
				t.Error("lost tool continuation", wire)
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
		finish := "STOP"
		if mode.Load() == 1 {
			finish = "MAX_TOKENS"
		}
		if mode.Load() == 2 {
			finish = "SAFETY"
		}
		if !stream {
			fmt.Fprintf(w, `{"modelVersion":"actual-gemini","candidates":[{"index":0,"content":{"role":"model","parts":%s},"finishReason":"%s"}],%s}`, parts, finish, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v string) { fmt.Fprintf(w, "data: %s\n\n", v); _ = http.NewResponseController(w).Flush() }
		emit(`{"modelVersion":"actual-gemini","candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}],` + usage + `}`)
		if mode.Load() == 4 {
			return
		}
		if mode.Load() == 5 {
			emit(`{"error":{"message":"private-upstream-error"}}`)
			return
		}
		emit(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_a","name":"lookup","args":{"n":1}},"thoughtSignature":"opaque"}]},"finishReason":"` + finish + `"}]}`)
		// Usage may arrive separately after the final candidate.
		emit(`{` + usage + `}`)
	}))
	defer up.Close()
	price := map[string]any{"platform": "gemini", "models": []string{"public-gemini"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "reasoning_effort_multipliers": map[string]any{"xhigh": 9, "high": 1.5}}
	gid = id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Chat Gemini", "platform": "gemini", "rate_multiplier": 2, "model_pricing": []any{price}}))
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Gemini bridge", "group_id": gid, "quota": 50})["key"].(string)
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Chat Gemini", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "gemini-secret", "base_url": up.URL, "model_mapping": map[string]string{"public-gemini": "gemini-3.1-pro"}}}))
	body := func(stream, include bool) map[string]any {
		return map[string]any{"model": "public-gemini", "stream": stream, "stream_options": map[string]bool{"include_usage": include}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}}
	}
	check := func(w *httptest.ResponseRecorder, cost string, account int64) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), "gateway_error") {
			t.Fatalf("gateway %d %s", w.Code, w.Body.String())
		}
		var actual, upstream, model, upmodel string
		var logged, input, output, cache int64
		err := a.DB.QueryRow(`SELECT actual_cost::text,upstream_endpoint,requested_model,upstream_model,account_id,input_tokens,output_tokens,cache_read_tokens FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&actual, &upstream, &model, &upmodel, &logged, &input, &output, &cache)
		if err != nil || actual != cost || !strings.HasPrefix(upstream, "/v1beta/models/gemini-3.1-pro:") || strings.Contains(upstream, "?") || model != "public-gemini" || upmodel != "gemini-3.1-pro" || logged != account || input != 10 || output != 8 || cache != 5 {
			t.Fatal("billing", actual, upstream, model, upmodel, logged, input, output, cache, err)
		}
	}
	first := call("POST", "/v1/chat/completions", key, body(false, false), "gemini-json")
	check(first, "0.5500000000", aid)
	if !strings.Contains(first.Body.String(), `"model":"public-gemini"`) || !strings.Contains(first.Body.String(), `"reasoning_tokens":2`) || !strings.Contains(first.Body.String(), `"thought_signature":"opaque"`) {
		t.Fatal(first.Body.String())
	}
	for _, path := range []string{"/chat/completions", "/backend-api/codex/chat/completions"} {
		w := call("POST", path, key, body(false, false), "gemini-json")
		if w.Body.String() != first.Body.String() || calls.Load() != 1 || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("alias replay", w.Code, w.Body.String())
		}
	}
	var balance, used string
	if err := a.DB.QueryRow(`SELECT balance::text,(SELECT quota_used::text FROM api_keys WHERE key=$2) FROM users WHERE id=$1`, uid, key).Scan(&balance, &used); err != nil || balance != "99.45000000" || used != "0.55000000" {
		t.Fatal(balance, used, err)
	}
	for _, include := range []bool{true, false} {
		w := call("POST", "/v1/chat/completions", key, body(true, include), "")
		check(w, "0.5500000000", aid)
		if strings.Contains(w.Body.String(), `"usage"`) != include || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") || strings.Contains(w.Body.String(), "finishReason") || strings.Count(w.Body.String(), `"id":"call_a"`) != 1 {
			t.Fatal(w.Body.String())
		}
	}
	var prior struct {
		Choices []struct{ Message map[string]any }
	}
	if err := json.Unmarshal(first.Body.Bytes(), &prior); err != nil {
		t.Fatal(err)
	}
	continued := body(false, false)
	continued["messages"] = []any{map[string]any{"role": "user", "content": "hello"}, prior.Choices[0].Message, map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "tool result"}}
	continuation.Store(1)
	check(call("POST", "/v1/chat/completions", key, continued, ""), "0.5500000000", aid)
	if continuation.Load() != 0 {
		t.Fatal("continuation was not dispatched")
	}
	priceChange.Store(1)
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.5500000000", aid)
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.8250000000", aid)
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"rate_multiplier": 2})
	for _, m := range []int32{1, 2, 3, 4, 5} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if m >= 4 && !stream {
				continue
			}
			w := call("POST", "/v1/chat/completions", key, body(stream, true), "")
			if m <= 2 {
				check(w, "0.5500000000", aid)
				want := "length"
				if m == 2 {
					want = "content_filter"
				}
				if !strings.Contains(w.Body.String(), `"finish_reason":"`+want+`"`) {
					t.Fatal(w.Body.String())
				}
			} else {
				if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"tool_calls":[`) || !strings.Contains(w.Body.String(), "gateway_error") || strings.Contains(w.Body.String(), "private-upstream-error") {
					t.Fatal("false success", w.Code, w.Body.String())
				}
				count, want := 0, 1
				if m == 3 {
					want = 0
				}
				if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != want {
					t.Fatal(count, want, err)
				}
			}
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_gemini_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/chat/completions", key, body(true, true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_gemini_receipt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "[DONE]") || strings.Contains(failed.Body.String(), `"tool_calls":[`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal(failed.Body.String())
	}
	before := calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.55", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("recovery", count, err)
	}
	invalid := body(false, false)
	invalid["n"] = 2
	if w := call("POST", "/v1/chat/completions", key, invalid, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("invalid dispatched", w.Code)
	}
	reasoning := body(false, false)
	reasoning["reasoning_effort"] = "xhigh"
	w := call("POST", "/v1/chat/completions", key, reasoning, "")
	check(w, "0.8250000000", aid)
	var requested, effective string
	if err := a.DB.QueryRow("SELECT requested_reasoning_effort,reasoning_effort FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&requested, &effective); err != nil || requested != "xhigh" || effective != "high" {
		t.Fatal(requested, effective, err)
	}
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Gemini composite", "platform": "composite", "rate_multiplier": 2, "model_pricing": []any{price}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-gemini", "target_platform": "gemini", "upstream_model": "public-gemini", "endpoint": "chat_completions", "match_type": "exact", "enabled": true})
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/chat/completions", ck, body(true, true), ""), "0.5500000000", aid)
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-gemini") {
		t.Fatal("discovery", w.Code, w.Body.String())
	}
	var retries atomic.Int32
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { retries.Add(1); w.WriteHeader(503) }))
	defer unavailable.Close()
	failedID := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Gemini failover", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "retry-secret", "base_url": unavailable.URL}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"public-gemini": {failedID}}})
	fresh := must("POST", "/api/v1/keys", user, map[string]any{"name": "Gemini retry", "group_id": gid})["key"].(string)
	before = calls.Load()
	check(call("POST", "/v1/chat/completions", fresh, body(true, true), ""), "0.5500000000", aid)
	if retries.Load() != 1 || calls.Load() != before+1 {
		t.Fatal("failover", retries.Load(), calls.Load()-before)
	}
}
