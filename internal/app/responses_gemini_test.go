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
)

func TestResponsesGeminiBridge(t *testing.T) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"model":"gemini-3.1-pro","stream":true,"max_output_tokens":32,"input":"hello"}`), &body); err != nil {
		t.Fatal(err)
	}
	wire, _, _, err := responsesGeminiRequest(map[string]any{
		"model":             body["model"],
		"stream":            body["stream"],
		"max_output_tokens": body["max_output_tokens"],
		"messages":          []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil || !strings.Contains(string(wire), `"contents"`) || strings.Contains(string(wire), `"messages"`) {
		t.Fatalf("request conversion: %s: %v", wire, err)
	}
	s := newResponsesGeminiStream("public", nil)
	for _, raw := range []string{
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}]}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
	} {
		if _, err := s.observe([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	terminal, result, err := s.finish(priceUsage{Input: 3, Output: 2})
	if err != nil || !strings.Contains(terminal, `response.completed`) || !strings.Contains(string(result), `"status":"completed"`) || !strings.Contains(string(result), `hello`) {
		t.Fatalf("response conversion: terminal=%s result=%s err=%v", terminal, result, err)
	}
}

const geminiResponseTools = `[{"type":"namespace","name":"team","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},{"type":"custom","name":"patch"},{"type":"tool_search","execution":"client"}]`
const geminiResponseCalls = `{"modelVersion":"native-gemini","candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_lookup","name":"team__lookup","args":{"n":9007199254740993}},"thoughtSignature":"lookup-signature"},{"functionCall":{"id":"call_patch","name":"patch","args":{"input":"apply this"}},"thoughtSignature":"patch-signature"},{"functionCall":{"id":"call_search","name":"tool_search","args":{"query":"find tools"}},"thoughtSignature":"search-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2,"totalTokenCount":23}}`

func TestResponsesGeminiTools(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	in, err := responsesToChatRequest(parse(`{"model":"gemini-3.1-pro","input":"hello","tools":`+geminiResponseTools+`}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		s := newResponsesGeminiStream("public", in)
		var result []byte
		if stream {
			if _, err = s.observe([]byte(geminiResponseCalls)); err == nil {
				_, result, err = s.finish(priceUsage{Input: 10, CacheRead: 5, Output: 8})
			}
		} else {
			result, err = s.response([]byte(geminiResponseCalls), priceUsage{Input: 10, CacheRead: 5, Output: 8})
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`"namespace":"team"`, `"name":"lookup"`, `"type":"custom_tool_call"`, `"input":"apply this"`, `"type":"tool_search_call"`, `"execution":"client"`, `9007199254740993`} {
			if !strings.Contains(string(result), want) {
				t.Fatal("lost Responses tool contract", stream, want, string(result))
			}
		}
		if strings.Contains(string(result), "signature") || strings.Contains(string(result), "team__lookup") {
			t.Fatal("native metadata exposed", string(result))
		}
		history := append(in.History, s.assistant())
		next, err := responsesToChatRequest(parse(`{"model":"gemini-3.1-pro","tools":`+geminiResponseTools+`,"input":[{"type":"function_call_output","call_id":"call_lookup","output":"found"},{"type":"custom_tool_call_output","call_id":"call_patch","output":"applied"},{"type":"tool_search_output","call_id":"call_search","tools":[{"type":"function","name":"discovered","parameters":{"type":"object"}}]}]}`), history)
		if err != nil {
			t.Fatal(err)
		}
		wire, _, _, err := responsesGeminiRequest(next.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"lookup-signature", "patch-signature", "search-signature", `"name":"team__lookup"`, `"name":"discovered"`, `9007199254740993`} {
			if !strings.Contains(string(wire), want) {
				t.Fatal("lost continuation metadata", want, string(wire))
			}
		}
		if strings.Contains(string(wire), "skip_thought_signature_validator") {
			t.Fatal("signature replaced by sentinel")
		}
	}
}

func testResponsesGemini(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.245:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-cookie")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	var calls atomic.Int32
	var continued atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-Goog-Api-Key") != "response-gemini-secret" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || body["contents"] == nil || body["model"] != nil {
			t.Error("Gemini dispatch or credential isolation")
		}
		stream := r.URL.Path == "/v1beta/models/gemini-3.1-pro:streamGenerateContent"
		if !stream && r.URL.Path != "/v1beta/models/gemini-3.1-pro:generateContent" || stream && r.URL.Query().Get("alt") != "sse" {
			t.Error("Gemini dispatch path", r.URL)
		}
		response := geminiResponseCalls
		if strings.Contains(string(body["contents"]), "functionResponse") {
			continued.Store(true)
			for _, signature := range []string{"lookup-signature", "patch-signature", "search-signature"} {
				if !strings.Contains(string(body["contents"]), signature) {
					t.Error("lost authenticated tool signature", signature)
				}
			}
			if !strings.Contains(string(body["tools"]), `"name":"discovered"`) {
				t.Error("lost discovered tool")
			}
			response = `{"modelVersion":"native-gemini","candidates":[{"content":{"parts":[{"text":"continued"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":15,"cachedContentTokenCount":5,"candidatesTokenCount":6,"thoughtsTokenCount":2,"totalTokenCount":23}}`
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", response)
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, response)
		}
	}))
	defer provider.Close()
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "responses-gemini@example.test", "password": "gemini-password", "balance": 10}))
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "responses-gemini@example.test", "password": "gemini-password"})["access_token"].(string)
	price := map[string]any{"platform": "gemini", "models": []string{"public-gemini"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses Gemini", "platform": "gemini", "rate_multiplier": 2, "model_pricing": []any{price}}))
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Responses Gemini", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"base_url": provider.URL, "api_key": "response-gemini-secret", "model_mapping": map[string]string{"public-gemini": "gemini-3.1-pro"}}})
	key := must("POST", "/api/v1/keys", token, map[string]any{"name": "Gemini", "group_id": gid, "quota": 10})["key"].(string)
	other := must("POST", "/api/v1/keys", token, map[string]any{"name": "Other Gemini", "group_id": gid})["key"].(string)
	for i, stream := range []bool{false, true} {
		body := map[string]any{"model": "public-gemini", "stream": stream, "store": true, "input": "find tools", "tools": json.RawMessage(geminiResponseTools)}
		idem := fmt.Sprint("responses-gemini-", i)
		w := call("POST", "/v1/responses", key, body, idem)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"namespace":"team"`) || !strings.Contains(w.Body.String(), `"type":"custom_tool_call"`) || !strings.Contains(w.Body.String(), `"type":"tool_search_call"`) || strings.Contains(w.Body.String(), "signature") {
			t.Fatal("Responses Gemini tools", w.Code, w.Body.String())
		}
		var result struct{ ID string }
		if stream {
			for _, line := range strings.Split(w.Body.String(), "\n") {
				if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"type":"response.completed"`) {
					var event struct{ Response struct{ ID string } }
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event)
					result.ID = event.Response.ID
				}
			}
		} else {
			_ = json.Unmarshal(w.Body.Bytes(), &result)
		}
		if result.ID == "" {
			t.Fatal("missing response ID")
		}
		before := calls.Load()
		if replay := call("POST", "/responses", key, body, idem); replay.Body.String() != w.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
			t.Fatal("Gemini Responses replay", replay.Code, replay.Body.String())
		}
		body["previous_response_id"] = result.ID
		body["input"] = json.RawMessage(`[{"type":"function_call_output","call_id":"call_lookup","output":"found"},{"type":"custom_tool_call_output","call_id":"call_patch","output":"applied"},{"type":"tool_search_output","call_id":"call_search","tools":[{"type":"function","name":"discovered","parameters":{"type":"object"}}]}]`)
		if foreign := call("POST", "/responses", other, body, ""); foreign.Code != 404 || calls.Load() != before {
			t.Fatal("foreign Gemini continuation", foreign.Code)
		}
		w = call("POST", "/backend-api/codex/responses", key, body, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "continued") || !continued.Swap(false) {
			t.Fatal("Gemini continuation", w.Code, w.Body.String())
		}
	}
	var count int
	var cost, balance, used string
	if err := a.DB.QueryRow(`SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE user_id=$1 AND input_tokens=10 AND output_tokens=8 AND cache_read_tokens=5 AND upstream_endpoint LIKE '/v1beta/models/gemini-3.1-pro:%'`, uid).Scan(&count, &cost); err != nil || count != 4 || cost != "2.2000000000" {
		t.Fatal("Gemini usage", count, cost, err)
	}
	if err := a.DB.QueryRow("SELECT balance::text,(SELECT quota_used::text FROM api_keys WHERE key=$2) FROM users WHERE id=$1", uid, key).Scan(&balance, &used); err != nil || balance != "7.80000000" || used != "2.20000000" {
		t.Fatal("Gemini balances", balance, used, err)
	}
	// A failed settlement must withhold tool completion events and remain recoverable.
	if _, err := a.DB.Exec(fmt.Sprintf("ALTER TABLE usage_logs ADD CONSTRAINT test_response_gemini CHECK(user_id<>%d) NOT VALID", uid)); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_response_gemini")
	w := call("POST", "/v1/responses", key, map[string]any{"model": "public-gemini", "input": "hello", "stream": true, "tools": json.RawMessage(geminiResponseTools)}, "")
	if strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "response.output_item.done") || !strings.Contains(w.Body.String(), "gateway_error") {
		t.Fatal("unsettled tool completion", w.Code, w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_response_gemini"); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE user_id=$1", uid).Scan(&count, &cost); err != nil || count != 5 || cost != "2.7500000000" || calls.Load() != before {
		t.Fatal("Gemini settlement recovery", count, cost, err)
	}
}
