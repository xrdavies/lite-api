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

func TestNativeUsage(t *testing.T) {
	a := textObservation{Protocol: "anthropic"}
	for _, event := range []string{
		`{"type":"message_start","message":{"type":"message","model":"claude-test","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}}}`,
		`{"type":"message_delta","usage":{"input_tokens":0,"output_tokens":8,"cache_read_input_tokens":0}}`,
		`{"type":"message_delta","usage":{"output_tokens":8}}`,
		`{"type":"message_stop"}`,
	} {
		if err := a.observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if !a.HasUsage || !a.complete() || a.Usage.Input != 10 || a.Usage.Output != 8 || a.Usage.CacheRead != 5 || a.Usage.CacheWrite5m != 2 || a.Usage.CacheWrite1h != 4 {
		t.Fatal("Anthropic cumulative usage lost or double counted", a)
	}
	g := textObservation{Protocol: "gemini"}
	for _, event := range []string{
		`{"candidates":[{"index":0},{"index":1}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":2,"thoughtsTokenCount":3}}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":7}}`,
	} {
		if err := g.observe([]byte(event)); err != nil {
			t.Fatal(err)
		}
	}
	if g.complete() {
		t.Fatal("unfinished second candidate lost")
	}
	if err := g.observe([]byte(`{"candidates":[{"index":1,"finishReason":"MAX_TOKENS"}]}`)); err != nil || !g.complete() || g.Usage.Input != 6 || g.Usage.Output != 12 || g.Usage.CacheRead != 4 {
		t.Fatal(g, err)
	}
	image := textObservation{Protocol: "gemini"}
	if err := image.observe([]byte(`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":30,"cachedContentTokenCount":5,"candidatesTokenCount":20,"promptTokensDetails":[{"modality":"TEXT","tokenCount":20},{"modality":"IMAGE","tokenCount":5}],"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":8},{"modality":"IMAGE","tokenCount":12}]}}`)); err != nil || image.Usage.Input != 25 || image.Usage.Output != 20 || image.Usage.CacheRead != 5 || image.Usage.ImageOutput != 12 {
		t.Fatal("Gemini image usage", image, err)
	}
	req := httptest.NewRequest("POST", "/v1beta/models/gemini-image:generateContent", nil)
	req.SetPathValue("action", "gemini-image:generateContent")
	if _, err := parseTextRequest(req, "gemini", map[string]json.RawMessage{
		"contents":         json.RawMessage(`[{"parts":[{"text":"draw a cat"}]}]`),
		"generationConfig": json.RawMessage(`{"responseModalities":["TEXT","IMAGE"]}`),
	}); err != nil {
		t.Fatal("Gemini IMAGE modality rejected", err)
	}
	for protocol, events := range map[string][]string{
		"anthropic": {`{"type":"message_stop"}`, `{"usage":{"input_tokens":-1,"output_tokens":0}}`, `{"usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":2147483648}}`},
		"gemini":    {`{"usageMetadata":{"promptTokenCount":1,"cachedContentTokenCount":2}}`, `{"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2147483647,"thoughtsTokenCount":1}}`, `{"usageMetadata":{"promptTokenCount":null}}`},
	} {
		for _, event := range events {
			o := textObservation{Protocol: protocol}
			if err := o.observe([]byte(event)); err == nil {
				t.Fatal("invalid upstream event accepted", protocol, event)
			}
		}
	}
	url, err := upstreamURL("https://example.test/v1beta", "/v1beta/models/gemini-test:streamGenerateContent?alt=sse")
	if err != nil || url != "https://example.test/v1beta/models/gemini-test:streamGenerateContent?alt=sse" {
		t.Fatal("Gemini SSE URL", url, err)
	}
}

func testNativeGateway(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, token, header string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		if header == "Authorization" {
			token = "Bearer " + token
		}
		r.Header.Set(header, token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Anthropic-Version", "2023-06-01")
		r.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
		r.Header.Set("Cookie", "never-forward")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		t.Helper()
		w := call(path, token, "Authorization", body, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	u := manage("/api/v1/admin/users", admin, map[string]any{"email": "native@example.test", "password": "native-test-password", "balance": 100})
	uid := int64(u["id"].(float64))
	user := manage("/api/v1/auth/login", "", map[string]any{"email": "native@example.test", "password": "native-test-password"})["access_token"].(string)
	var calls, mode atomic.Int32
	const anthroUsage = `"usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}`
	const geminiUsage = `"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":5,"thoughtsTokenCount":7}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.URL.Query().Get("key") != "" {
			t.Error("native transport forwarded client credentials")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid upstream native request")
		}
		if mode.Load() == 2 {
			w.WriteHeader(401)
			_, _ = fmt.Fprint(w, "upstream-native-secret")
			return
		}
		anthropic := strings.HasPrefix(r.URL.Path, "/v1/messages")
		if anthropic {
			if r.Header.Get("X-Api-Key") != "upstream-native-secret" || r.Header.Get("X-Goog-Api-Key") != "" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != "prompt-caching-2024-07-31" {
				t.Error("Anthropic header isolation")
			}
			if string(body["model"]) != `"upstream-model"` {
				t.Error("Anthropic mapping missing")
			}
			if strings.HasSuffix(r.URL.Path, "/count_tokens") {
				_, _ = fmt.Fprint(w, `{"input_tokens":21}`)
				return
			}
			if string(body["stream"]) != "true" {
				_, _ = fmt.Fprint(w, `{"type":"message","model":"response-native","content":[{"type":"tool_use","id":"call-1","name":"weather","input":{"city":"Tokyo"}}],`+anthroUsage+`}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"response-native\","+anthroUsage+"}}\n\n")
			_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"signed-value\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":8}}\n\n")
			if mode.Load() != 1 {
				_, _ = fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}
			return
		}
		if r.Header.Get("X-Goog-Api-Key") != "upstream-native-secret" || r.Header.Get("X-Api-Key") != "" || !strings.HasPrefix(r.URL.Path, "/v1beta/models/upstream-model:") {
			t.Error("Gemini mapping or header isolation")
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			if raw := body["generateContentRequest"]; raw != nil {
				var nested struct{ Model string }
				if json.Unmarshal(raw, &nested) != nil || nested.Model != "models/upstream-model" {
					t.Error("nested count request escaped model mapping")
				}
			}
			_, _ = fmt.Fprint(w, `{"totalTokens":10}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":generateContent") {
			_, _ = fmt.Fprint(w, `{"modelVersion":"response-native","candidates":[{"content":{"parts":[{"functionCall":{"name":"weather","args":{"city":"Tokyo"}},"thoughtSignature":"signed-value"}]},"finishReason":"STOP"}],`+geminiUsage+`}`)
			return
		}
		if r.URL.Query().Get("alt") != "sse" {
			t.Error("Gemini stream query missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"modelVersion\":\"response-native\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"OK\",\"thoughtSignature\":\"signed-value\"}]}}],"+geminiUsage+"}\n\n")
		if mode.Load() != 1 {
			_, _ = fmt.Fprint(w, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}],"+geminiUsage+"}\n\n")
			_, _ = fmt.Fprint(w, "data: {"+geminiUsage+"}\n\n")
		}
	}))
	defer up.Close()
	for _, protocol := range []string{"anthropic", "gemini"} {
		mode.Store(0)
		gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Native " + protocol, "platform": protocol, "model_allowlist": map[string]any{"enabled": true, "models": []string{"client-model"}}})["id"].(float64))
		manage("/api/v1/admin/channels", admin, map[string]any{"name": "Native " + protocol, "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": protocol, "models": []string{"client-model"}, "input_price": json.Number("0.000001"), "output_price": json.Number("0.000002"), "cache_read_price": json.Number("0.0000005"), "cache_write_price": json.Number("0.000003"), "cache_write_1h_price": json.Number("0.000004")}}})
		manage("/api/v1/admin/accounts", admin, map[string]any{"name": "Native " + protocol, "platform": protocol, "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "upstream-native-secret", "base_url": up.URL, "model_mapping": map[string]string{"client-model": "upstream-model"}}})
		k := manage("/api/v1/keys", user, map[string]any{"name": "Native " + protocol, "group_id": gid, "quota": 10})
		key, kid := k["key"].(string), int64(k["id"].(float64))
		header, path, countPath := "X-Api-Key", "/v1/messages", "/v1/messages/count_tokens"
		body := map[string]any{"model": "client-model", "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "OK"}}}
		if protocol == "gemini" {
			header, path, countPath = "X-Goog-Api-Key", "/v1beta/models/client-model:generateContent", "/v1beta/models/client-model:countTokens"
			body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "OK"}}}}}
		}
		for _, stream := range []bool{false, true} {
			if protocol == "gemini" {
				if stream {
					path = "/v1beta/models/client-model:streamGenerateContent?alt=sse"
				}
			} else {
				body["stream"] = stream
			}
			idem := fmt.Sprintf("native-%v", stream)
			before := calls.Load()
			w := call(path, key, header, body, idem)
			if w.Code != 200 || strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "signed-value") && protocol == "gemini" {
				t.Fatalf("%s stream=%v: %d %s", protocol, stream, w.Code, w.Body.String())
			}
			replay := call(path, key, header, body, idem)
			if replay.Code != 200 || replay.Body.String() != w.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before+1 {
				t.Fatal("native replay failed", protocol)
			}
			var input, output, cached, write, hour int64
			var cost, endpoint string
			err := a.DB.QueryRow("SELECT input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,cache_creation_1h_tokens,actual_cost::text,upstream_endpoint FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&input, &output, &cached, &write, &hour, &cost, &endpoint)
			if err != nil {
				t.Fatal(err)
			}
			if protocol == "anthropic" {
				if input != 10 || output != 8 || cached != 5 || write != 6 || hour != 4 || cost != "0.0000505000" || endpoint != "/v1/messages" {
					t.Fatal("Anthropic settlement mismatch", input, output, cached, write, hour, cost, endpoint)
				}
			} else if input != 6 || output != 12 || cached != 4 || cost != "0.0000320000" || !strings.HasPrefix(endpoint, "/v1beta/models/upstream-model:") {
				t.Fatal("Gemini settlement mismatch", input, output, cached, cost, endpoint)
			}
		}
		if protocol == "anthropic" {
			body["stream"] = false
		}
		w := call(countPath, key, header, body, "count")
		if w.Code != 200 {
			t.Fatal("count endpoint", protocol, w.Code, w.Body.String())
		}
		if protocol == "anthropic" {
			before := calls.Load()
			alias := call("/messages/count_tokens", key, header, body, "count")
			if alias.Code != 200 || alias.Body.String() != w.Body.String() || alias.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
				t.Fatal("count alias replay", alias.Code, alias.Body.String())
			}
			alias = call("/messages/count_tokens", key, header, body, "count-alias")
			if alias.Code != 200 || alias.Body.String() != w.Body.String() || calls.Load() != before+1 {
				t.Fatal("count alias dispatch", alias.Code, alias.Body.String())
			}
			for _, credential := range []string{"", user} {
				if w := call("/messages/count_tokens", credential, header, body, ""); w.Code != 401 {
					t.Fatal("count alias authentication", w.Code)
				}
			}
			badModel := map[string]any{"model": "not-allowed", "messages": body["messages"]}
			if w := call("/messages/count_tokens", key, header, badModel, ""); w.Code != 403 || calls.Load() != before+1 {
				t.Fatal("count alias model access", w.Code)
			}
		}
		if protocol == "gemini" {
			nested := map[string]any{"generateContentRequest": map[string]any{"model": "models/client-model", "contents": body["contents"]}}
			w = call(countPath+"?key="+key, "", "X-Goog-Api-Key", nested, "")
			if w.Code != 200 {
				t.Fatal("Gemini nested count request", w.Code, w.Body.String())
			}
			if w := call("/v1beta/models/client-model:delete", key, header, body, ""); w.Code != 404 {
				t.Fatal("uncontrolled Gemini action", w.Code)
			}
		}
		var count int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != 2 {
			t.Fatal("token counting charged consumption", count, err)
		}
		mode.Store(1)
		if protocol == "anthropic" {
			body["stream"] = true
		}
		w = call(path, key, header, body, "broken")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatal("incomplete native stream succeeded", protocol, w.Code, w.Body.String())
		}
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != 3 {
			t.Fatal("partial native stream consumption lost", count, err)
		}
		mode.Store(0)
		if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_native_settlement CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
			t.Fatal(err)
		}
		w = call(path, key, header, body, "settlement-failure")
		if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "finishReason") {
			t.Fatal("native completion preceded settlement", protocol, w.Body.String())
		}
		if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_native_settlement"); err != nil {
			t.Fatal(err)
		}
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal("native receipt recovery", err)
		}
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != 4 {
			t.Fatal("native pending settlement was not recovered", count, err)
		}
		unauth := call(path, "bad-key", header, body, "")
		marker := "authentication_error"
		if protocol == "gemini" {
			marker = "UNAUTHENTICATED"
		}
		if unauth.Code != 401 || !strings.Contains(unauth.Body.String(), marker) {
			t.Fatal("native authentication error shape", unauth.Code, unauth.Body.String())
		}
		mode.Store(2)
		w = call(path, key, header, body, "upstream-error")
		if w.Code != 502 || strings.Contains(w.Body.String(), "upstream-native-secret") {
			t.Fatal("native upstream error handling", w.Code)
		}
	}
	var reconciled bool
	if err := a.DB.QueryRow("SELECT (100-balance)=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&reconciled); err != nil || !reconciled {
		t.Fatal("native balance reconciliation", err)
	}
}
