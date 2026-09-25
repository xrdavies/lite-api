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

func TestMessagesCountRequest(t *testing.T) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","system":"rules","max_tokens":90,"stream":true,"service_tier":"priority","stop_sequences":["stop"],"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"lookup","input":{"n":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"found"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup"}}`), &body)
	raw, err := messagesCountRequest(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(raw, &result)
	if len(result) != 4 || result["input"] == nil || result["tools"] == nil || result["tool_choice"] == nil || credentialString(result, "model") != "m" || !bytes.Contains(raw, []byte(`9007199254740993`)) || !bytes.Contains(raw, []byte(`"role":"developer"`)) || !bytes.Contains(raw, []byte(`"type":"function_call_output"`)) {
		t.Fatal("count conversion lost inputs or kept generation options", string(raw))
	}
	if string(body["max_tokens"]) != "90" {
		t.Fatal("counting mutated caller body")
	}
	for _, messages := range []string{`null`, `[]`, `"wrong"`, `[{"role":"unknown","content":"hi"}]`} {
		body["messages"] = json.RawMessage(messages)
		if _, err := messagesCountRequest(body, nil); err == nil {
			t.Fatal("invalid count input accepted", messages)
		}
	}
}

func testMessagesCounting(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(path, key string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.212:1234"
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Anthropic-Beta", "client-only")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.212:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "counting@example.test", "password": "counting-password", "balance": 100}))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "counting@example.test", "password": "counting-password"})["access_token"].(string)
	var calls, mode atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/prefix/v1/responses/input_tokens" || r.Header.Get("Authorization") != "Bearer counting-upstream" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Beta") != "" || credentialString(body, "model") != "count-upstream" || body["input"] == nil {
			t.Error("wrong counting endpoint, credentials, model or inputs", r.URL.Path)
		}
		for _, name := range []string{"stream", "store", "max_output_tokens", "max_tokens", "messages", "service_tier"} {
			if body[name] != nil {
				t.Error("generation control in counting body", name)
			}
		}
		switch mode.Load() {
		case 1:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"input_tokens not found; counting-upstream"}}`)
		case 2:
			fmt.Fprint(w, `{"input_tokens":-1}`)
		case 3:
			fmt.Fprint(w, `{"usage":{"input_tokens":9}}`)
		default:
			fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":17,"internal":"private"}`)
		}
	}))
	defer up.Close()
	for _, wire := range []string{"chat_completions", "responses", "anthropic"} {
		gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Counting " + wire, "platform": "openai", "allow_messages_dispatch": true, "force_openai_fast": true, "profit_control_enabled": true, "profit_min_margin": "0.99", "messages_dispatch_model_config": map[string]any{"exact_model_mappings": map[string]string{"public-count": "count-admission"}}}))
		gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Counting " + wire, "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "counting-upstream", "base_url": up.URL + "/prefix/v1", "api_protocol": wire, "model_mapping": map[string]string{"public-count": "count-upstream", "count-admission": "not-forwarded"}}}))
		keyObject := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Counting", "group_id": gid})
		key := keyObject["key"].(string)
		body := map[string]any{"model": "public-count", "messages": []any{map[string]any{"role": "user", "content": "count me"}}}
		if wire != "anthropic" {
			for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
				before := calls.Load()
				w := call(path, key, body, "count-replay"+path)
				if w.Code != 200 || calls.Load() != before+1 || strings.TrimSpace(w.Body.String()) != `{"input_tokens":17}` {
					t.Fatal("Messages count bridge", wire, w.Code, w.Body.String())
				}
				w = call(path, key, body, "count-replay"+path)
				if w.Code != 200 || calls.Load() != before+1 || w.Header().Get("Idempotency-Replayed") != "true" {
					t.Fatal("count replay dispatched", w.Code)
				}
			}
			for _, tc := range []struct {
				mode   int64
				status int
			}{{1, 404}, {2, 502}, {3, 502}} {
				mode.Store(tc.mode)
				w := call("/messages/count_tokens", key, body, "")
				if w.Code != tc.status || strings.Contains(w.Body.String(), "counting-upstream") {
					t.Fatal("count failure", wire, tc, w.Code, w.Body.String())
				}
			}
			mode.Store(0)
			manage("PUT", gp, admin, map[string]any{"allow_messages_dispatch": false})
			before := calls.Load()
			if w := call("/messages/count_tokens", key, body, ""); w.Code != 403 || calls.Load() != before {
				t.Fatal("count bypassed Messages gate", w.Code)
			}
		}
		// The native counting endpoint is independent of the account's text wire.
		for _, path := range []string{"/v1/responses/input_tokens", "/responses/input_tokens", "/backend-api/codex/responses/input_tokens"} {
			before := calls.Load()
			w := call(path, key, map[string]any{"model": "public-count", "input": "count me"}, "")
			if w.Code != 200 || calls.Load() != before || !strings.Contains(w.Body.String(), `"input_tokens":2`) {
				t.Fatal("native count endpoint", wire, w.Code, w.Body.String())
			}
		}
		if wire == "responses" {
			before := calls.Load()
			input := map[string]any{"model": "public-count", "input": "count me"}
			first := call("/responses/input_tokens", key, input, "native-replay")
			replay := call("/v1/responses/input_tokens", key, input, "native-replay")
			if first.Code != 200 || replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || first.Body.String() != replay.Body.String() || calls.Load() != before {
				t.Fatal("native count replay", first.Code, replay.Code)
			}
			input["input"] = "changed"
			if w := call("/responses/input_tokens", key, input, "native-replay"); w.Code != 409 {
				t.Fatal("native replay conflict", w.Code)
			}
			if w := call("/responses/input_tokens", user, input, ""); w.Code != 401 {
				t.Fatal("JWT local count", w.Code)
			}
			input["input"] = "count me"
			for _, tools := range []string{nativeCodeTools, nativeHostedShellTools, `[{"type":"web_search"}]`, `[{"type":"programmatic_tool_calling"}]`} {
				input["tools"] = json.RawMessage(tools)
				if w := call("/responses/input_tokens", key, input, ""); w.Code != 200 || calls.Load() != before {
					t.Fatal("hosted count dispatched", w.Code, w.Body.String())
				}
			}
			input["tools"] = json.RawMessage(`[{"type":"file_search","vector_store_ids":["vs_count"]}]`)
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 503 {
				t.Fatal("ungranted store counted", w.Code)
			}
			ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
			manage("PUT", ap, admin, map[string]any{"extra": map[string]any{"response_vector_stores": map[string]any{fmt.Sprint(gid): []string{"vs_count"}}}})
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 200 {
				t.Fatal("granted store count", w.Code, w.Body.String())
			}
			delete(input, "tools")
			input["input"] = json.RawMessage(nativeCodeCalls)
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 404 {
				t.Fatal("unowned tool history counted", w.Code)
			}
			u, err := a.loadAccount(context.Background(), aid)
			if err != nil {
				t.Fatal(err)
			}
			identity := &gatewayIdentity{UserID: uid, Key: gatewayKey{ID: id(keyObject), GroupID: gid}}
			binding := responseBinding{AccountID: aid, Target: responseTarget(u), Items: []string{"ci_team"}, CodeTool: true, Containers: []string{"cntr_team"}}
			if err := a.storeResponseBinding(context.Background(), identity, "resp_counting", binding); err != nil {
				t.Fatal(err)
			}
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 200 || calls.Load() != before {
				t.Fatal("owned full tool history count", w.Code, w.Body.String())
			}
			other := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Other counting key", "group_id": gid})["key"].(string)
			if w := call("/responses/input_tokens", other, input, ""); w.Code != 404 {
				t.Fatal("cross Key tool history", w.Code)
			}
			input["input"] = "count me"
			input["previous_response_id"] = "resp_counting"
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 200 || calls.Load() != before+1 || !strings.Contains(w.Body.String(), `"input_tokens":17`) {
				t.Fatal("remote history wasn't resolved upstream", w.Code, w.Body.String())
			}
			mode.Store(1)
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 404 {
				t.Fatal("missing remote history silently estimated", w.Code, w.Body.String())
			}
			mode.Store(0)
			delete(input, "previous_response_id")
			input["model"] = "not-allowed"
			before = calls.Load()
			if w := call("/responses/input_tokens", key, input, ""); w.Code != 503 || calls.Load() != before {
				t.Fatal("local account model admission", w.Code)
			}
		}
		var status string
		if err := a.DB.QueryRow("SELECT status FROM accounts WHERE id=$1", aid).Scan(&status); err != nil || status != "active" {
			t.Fatal("unsupported count disabled account", status, err)
		}
		before := calls.Load()
		if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
			t.Fatal(err)
		}
		if w := call("/responses/input_tokens", key, map[string]any{"model": "public-count", "input": "hi"}, ""); w.Code != 402 || calls.Load() != before {
			t.Fatal("count bypassed balance", w.Code)
		}
		if _, err := a.DB.Exec("UPDATE users SET balance=100 WHERE id=$1", uid); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	var used string
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&count); err != nil || count != 0 {
		t.Fatal("count created consumption", count, err)
	}
	if err := a.DB.QueryRow("SELECT sum(quota_used)::text FROM api_keys WHERE user_id=$1", uid).Scan(&used); err != nil || rat(json.Number(used)).Sign() != 0 {
		t.Fatal("count debited keys", used, err)
	}
}
