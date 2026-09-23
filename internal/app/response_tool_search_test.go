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

const clientSearchDeclaration = `{"type":"tool_search","execution":"client","description":"Find a tool","parameters":{"type":"object","properties":{"goal":{"type":"string"}},"required":["goal"]}}`
const discoveredClientTools = `[{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"},"defer_loading":true}]},{"type":"custom","name":"patch","description":"Edit a file"}]`

func TestClientToolSearch(t *testing.T) {
	parse := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := parse(`{"model":"m","input":"hello","tools":[` + clientSearchDeclaration + `],"tool_choice":{"type":"tool_search"}}`)
	request := httptest.NewRequest("POST", "/responses", nil)
	if _, err := parseTextRequest(request, "responses", body); err != nil {
		t.Fatal(err)
	}
	converted, err := responsesToChatRequest(body, nil)
	if err != nil || !converted.ToolSearch {
		t.Fatal("search conversion", err)
	}
	raw, _ := json.Marshal(converted.Body)
	if !strings.Contains(string(raw), `"name":"tool_search"`) || !strings.Contains(string(raw), `"goal":{"type":"string"}`) || strings.Contains(string(raw), `"execution"`) {
		t.Fatal("search schema changed", string(raw))
	}

	stream := newChatResponsesStream("m", nil)
	stream.ToolSearch = true
	first, err := stream.observe([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"search_one","type":"function","function":{"name":"tool_search","arguments":"{\"goal\":"}}]}}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	next, err := stream.observe([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"files\"}"}}]},"finish_reason":"tool_calls"}],`+chatBridgeUsage+`}`), true)
	if err != nil {
		t.Fatal(err)
	}
	terminal, result, err := stream.finish()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first+next+terminal, "function_call_arguments") || strings.Contains(first+next, "output_item.done") || !strings.Contains(string(result), `"arguments":{"goal":"files"}`) || !strings.Contains(string(result), `"execution":"client"`) || !strings.Contains(string(result), `"id":"tsc_`) {
		t.Fatal("search output lifecycle", string(result))
	}
	var response struct{ Output []map[string]json.RawMessage }
	_ = json.Unmarshal(result, &response)
	if response.Output[0]["name"] != nil {
		t.Fatal("function name leaked into tool search")
	}

	history := []convertedChatMessage{stream.assistant()}
	discovery := parse(`{"model":"m","input":[{"type":"tool_search_output","execution":"client","call_id":"search_one","status":"completed","tools":` + discoveredClientTools + `}]}`)
	loaded, err := responsesToChatRequest(discovery, history)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(loaded.Body)
	if !strings.Contains(string(raw), `"name":"files__read"`) || !loaded.Custom["patch"] || strings.Contains(string(raw), `"tool_search":true`) || strings.Contains(string(raw), `"discovered_tools"`) || len(loaded.History[1].Discoveries) != 2 {
		t.Fatal("discovery promotion/history", string(raw))
	}
	if !history[0].Calls[0].Search {
		t.Fatal("saved history was mutated")
	}
	later, err := responsesToChatRequest(parse(`{"model":"m","input":"continue"}`), loaded.History)
	if err != nil || later.Namespaces["files__read"].Namespace != "files" || !later.Custom["patch"] {
		t.Fatal("discovery lost on continuation", err)
	}
	// A failed lookup preserves its output but cannot promote tools.
	failed := parse(`{"model":"m","input":[{"type":"tool_search_output","call_id":"search_one","status":"failed","tools":` + discoveredClientTools + `,"output":{"error":"lookup failed"}}]}`)
	if got, err := responsesToChatRequest(failed, history); err != nil || got.Body["tools"] != nil || len(got.History[1].Discoveries) != 0 {
		t.Fatal("failed discovery promoted", err)
	}
	// Reordered JSON schemas are identical without float64 precision loss.
	duplicate := parse(`{"model":"m","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740993}}}}],"input":[{"type":"tool_search_output","call_id":"c","tools":[{"name":"read","type":"function","parameters":{"properties":{"n":{"maximum":9007199254740993,"type":"integer"}},"type":"object"}}]}]}`)
	if got, err := responsesToChatRequest(duplicate, nil); err != nil || len(got.Body["tools"].([]any)) != 1 {
		t.Fatal("discovery deduplication", err)
	}
	for _, invalid := range []string{
		`{"tools":[{"type":"tool_search"}]}`,
		`{"tools":[{"type":"tool_search","execution":"server"}]}`,
		`{"tools":[{"type":"tool_search","execution":"client","parameters":[]} ]}`,
		`{"input":[{"type":"tool_search_call","call_id":"c","execution":"server","arguments":{}}]}`,
		`{"input":[{"type":"tool_search_call","call_id":"c","arguments":[]}]}`,
		`{"input":[{"type":"tool_search_output","tools":[]}]}`,
		`{"input":[{"type":"tool_search_output","call_id":"c","tools":[{"type":"web_search"}]}]}`,
		`{"input":[{"type":"tool_search_output","call_id":"c","status":"failed","tools":[{"type":"web_search"}]}]}`,
		`{"input":[{"type":"tool_search_output","call_id":"c","tools":[{"type":"namespace","name":"x","tools":[{"type":"mcp"}]}]}]}`,
		`{"tools":[{"type":"function","name":"read","strict":true}],"input":[{"type":"tool_search_output","call_id":"c","tools":[{"type":"function","name":"read","strict":false}]}]}`,
		`{"tools":[{"type":"function","name":"files__read"}],"input":[{"type":"tool_search_output","call_id":"c","tools":` + discoveredClientTools + `}]}`,
		`{"tools":[{"type":"function","name":"n","parameters":{"maximum":9007199254740993}}],"input":[{"type":"tool_search_output","call_id":"c","tools":[{"type":"function","name":"n","parameters":{"maximum":9007199254740992}}]}]}`,
	} {
		b := parse(invalid)
		b["model"] = json.RawMessage(`"m"`)
		if b["input"] == nil {
			b["input"] = json.RawMessage(`"hello"`)
		}
		if _, err := parseTextRequest(request, "responses", b); err == nil {
			t.Fatal("invalid discovery admitted", invalid)
		}
	}
	for _, invalid := range []string{
		`{"model":"m","input":"hello","tools":[` + clientSearchDeclaration + `,{"type":"function","name":"tool_search"}]}`,
		`{"model":"m","input":"hello","tools":[` + clientSearchDeclaration + `,{"type":"tool_search","execution":"client","description":"different"}]}`,
		`{"model":"m","input":[{"type":"function_call","name":"tool_search","call_id":"c","arguments":"{}"}],"tools":[` + clientSearchDeclaration + `]}`,
	} {
		if _, err := responsesToChatRequest(parse(invalid), nil); err == nil {
			t.Fatal("search identity collision accepted", invalid)
		}
	}
	if _, err := responsesToChatRequest(parse(`{"model":"m","input":"hello","tools":[{"type":"function","name":"tool_search"}]}`), history); err == nil {
		t.Fatal("search history hijacked by ordinary function")
	}
	same := parse(`{"model":"m","input":[{"type":"additional_tools","tools":[` + clientSearchDeclaration + `]},{"role":"user","content":"hello"}],"tools":[` + clientSearchDeclaration + `]}`)
	if got, err := responsesToChatRequest(same, nil); err != nil || len(got.Body["tools"].([]any)) != 1 {
		t.Fatal("duplicate search declaration", err)
	}
	for _, arguments := range []string{`[]`, `null`, `42`, `"text"`} {
		s := newChatResponsesStream("m", nil)
		s.ToolSearch = true
		tool := convertedChatTool{ID: "bad", Type: "function"}
		tool.Function.Name, tool.Function.Arguments = "tool_search", arguments
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []convertedChatTool{tool}}, "finish_reason": "tool_calls"}}})
		_, _ = s.observe(raw, false)
		if _, _, err := s.finish(); err == nil {
			t.Fatal("non-object search arguments accepted", arguments)
		}
	}
}

func testClientToolSearch(t *testing.T, a *App, admin string) {
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
			t.Fatal(path, w.Code, w.Body.String())
		}
		var result struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return result.Data
	}
	id := func(value map[string]any) int64 { return int64(value["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "tool-search@example.test", "password": "tool-search-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "tool-search@example.test", "password": "tool-search-password"})["access_token"].(string)
	var declaration map[string]any
	_ = json.Unmarshal([]byte(clientSearchDeclaration), &declaration)
	var discovered []any
	_ = json.Unmarshal([]byte(discoveredClientTools), &discovered)
	for _, protocol := range []string{"chat_completions", "responses"} {
		var calls, phase atomic.Int32
		var upstreamBody atomic.Value
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			var b map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&b)
			raw, _ := json.Marshal(b)
			upstreamBody.Store(string(raw))
			if r.Header.Get("Authorization") != "Bearer tool-upstream" || r.URL.Path != "/v1/"+strings.ReplaceAll(protocol, "_", "/") {
				t.Error("tool search transport", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			stream := string(b["stream"]) == "true"
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			if protocol == "responses" {
				if phase.Load() == 0 && !strings.Contains(string(b["tools"]), `"execution":"client"`) {
					t.Error("native tool declaration changed")
				}
				output := []any{map[string]any{"type": "tool_search_call", "id": "tsc_native", "call_id": "search_one", "execution": "client", "arguments": map[string]any{"goal": "files"}, "status": "completed"}}
				if phase.Load() == 1 {
					output = []any{map[string]any{"type": "function_call", "id": "fc_loaded", "call_id": "read_one", "namespace": "files", "name": "read", "arguments": "{}", "status": "completed"}}
				}
				var usage any
				_ = json.Unmarshal([]byte(`{`+responseUsage+`}`), &usage)
				response := map[string]any{"object": "response", "id": fmt.Sprintf("resp_search_%d", n), "model": "tool-model", "status": "completed", "output": output, "usage": usage.(map[string]any)["usage"]}
				if stream {
					event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
					fmt.Fprintf(w, "data: %s\n\n", event)
				} else {
					_ = json.NewEncoder(w).Encode(response)
				}
				return
			}
			if string(b["store"]) != "false" || b["input"] != nil {
				t.Error("invalid Chat search request")
			}
			if phase.Load() == 0 && (!strings.Contains(string(b["tools"]), `"name":"tool_search"`) || !strings.Contains(string(b["tools"]), `"goal"`)) {
				t.Error("search schema lost")
			}
			tool := convertedChatTool{ID: "search_one", Type: "function"}
			tool.Function.Name, tool.Function.Arguments = "tool_search", `{"goal":"files"}`
			if phase.Load() == 1 {
				tool.ID, tool.Function.Name, tool.Function.Arguments = "read_one", "files__read", "{}"
			}
			if phase.Load() == 2 {
				tool.Function.Arguments = "[]"
			}
			var usage any
			_ = json.Unmarshal([]byte(`{`+chatBridgeUsage+`}`), &usage)
			message := map[string]any{"role": "assistant", "tool_calls": []convertedChatTool{tool}}
			if !stream {
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "chat_search", "model": "tool-model", "choices": []any{map[string]any{"message": message, "finish_reason": "tool_calls"}}, "usage": usage.(map[string]any)["usage"]})
				return
			}
			event, _ := json.Marshal(map[string]any{"id": "chat_search", "model": "tool-model", "choices": []any{map[string]any{"index": 0, "delta": message}}})
			fmt.Fprintf(w, "data: %s\n\n", event)
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],%s}\n\ndata: [DONE]\n\n", chatBridgeUsage)
		}))
		defer up.Close()
		gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "search " + protocol, "platform": "openai", "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"tool-model"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004}}}))
		must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "search " + protocol, "type": "apikey", "platform": "openai", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "tool-upstream", "api_protocol": protocol, "base_url": up.URL}})
		keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "search " + protocol, "group_id": gid})
		key, kid := keyData["key"].(string), id(keyData)
		check := func(w *httptest.ResponseRecorder) {
			t.Helper()
			if w.Code != 200 {
				t.Fatal(protocol, w.Code, w.Body.String())
			}
			var cost string
			if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost); err != nil || cost != "0.3070000000" {
				t.Fatal("client discovery billing", cost, err)
			}
		}
		body := map[string]any{"model": "tool-model", "input": "find tools", "tools": []any{declaration}, "tool_choice": map[string]string{"type": "tool_search"}}
		first := call("POST", "/responses", key, body, "search-first-"+protocol)
		check(first)
		var response struct {
			ID     string
			Output []map[string]any
		}
		_ = json.Unmarshal(first.Body.Bytes(), &response)
		if len(response.Output) != 1 || response.Output[0]["type"] != "tool_search_call" || response.Output[0]["execution"] != "client" || response.Output[0]["arguments"].(map[string]any)["goal"] != "files" {
			t.Fatal("tool search result", first.Body.String())
		}
		for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
			if w := call("POST", path, key, body, "search-first-"+protocol); w.Body.String() != first.Body.String() || calls.Load() != 1 {
				t.Fatal("search replay", w.Code)
			}
		}
		phase.Store(1)
		follow := map[string]any{"model": "tool-model", "previous_response_id": response.ID, "input": []any{map[string]any{"type": "tool_search_output", "execution": "client", "call_id": "search_one", "status": "completed", "tools": discovered}}}
		second := call("POST", "/responses", key, follow, "")
		check(second)
		if !strings.Contains(second.Body.String(), `"namespace":"files"`) {
			t.Fatal("discovered function call lost", second.Body.String())
		}
		if protocol == "chat_completions" {
			got := upstreamBody.Load().(string)
			if !strings.Contains(got, `"name":"files__read"`) || !strings.Contains(got, `"name":"patch"`) || strings.Contains(got, `"discovered_tools"`) {
				t.Fatal("discovered tools missing on wire", got)
			}
			_ = json.Unmarshal(second.Body.Bytes(), &response)
			third := map[string]any{"model": "tool-model", "previous_response_id": response.ID, "input": []any{map[string]any{"type": "function_call_output", "call_id": "read_one", "output": "file body"}}}
			check(call("POST", "/responses", key, third, ""))
			if got := upstreamBody.Load().(string); !strings.Contains(got, `"name":"files__read"`) || strings.Contains(got, `"tool_search":true`) {
				t.Fatal("discovery lost on later turn", got)
			}
		}
		phase.Store(0)
		body["stream"] = true
		sse := call("POST", "/responses", key, body, "search-stream-"+protocol)
		check(sse)
		if !strings.Contains(sse.Body.String(), "response.completed") || !strings.Contains(sse.Body.String(), `"type":"tool_search_call"`) || strings.Contains(sse.Body.String(), "function_call_arguments") {
			t.Fatal("search SSE", sse.Body.String())
		}
		before := calls.Load()
		if w := call("POST", "/v1/responses", key, body, "search-stream-"+protocol); w.Body.String() != sse.Body.String() || calls.Load() != before {
			t.Fatal("search SSE replay")
		}
		if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_tool_search_receipt CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
			t.Fatal(err)
		}
		failed := call("POST", "/responses", key, body, "")
		if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_tool_search_receipt"); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(failed.Body.String(), "response.completed") || strings.Contains(failed.Body.String(), "output_item.done") || !strings.Contains(failed.Body.String(), "settlement failed") {
			t.Fatal("tool executed before settlement", failed.Body.String())
		}
		before = calls.Load()
		for i := 0; i < 2; i++ {
			if err := a.recoverReceipts(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
			t.Fatal("tool search recovery", count, err)
		}
		for _, invalid := range []any{map[string]any{"type": "tool_search"}, map[string]any{"type": "tool_search", "execution": "server"}} {
			body["tools"] = []any{invalid}
			if w := call("POST", "/responses", key, body, ""); w.Code != 400 || calls.Load() != before {
				t.Fatal("hosted search admitted", w.Code)
			}
		}
		body["tools"] = []any{declaration}
		if protocol == "chat_completions" {
			phase.Store(2)
			w := call("POST", "/responses", key, body, "")
			if strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "event: error") {
				t.Fatal("invalid search arguments succeeded", w.Body.String())
			}
		}
	}
	var balanced bool
	if err := a.DB.QueryRow("SELECT 100-balance=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&balanced); err != nil || !balanced {
		t.Fatal("search cost reconciliation", err)
	}
}
