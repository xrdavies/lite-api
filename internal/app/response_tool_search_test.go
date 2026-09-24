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
	"time"

	"github.com/coder/websocket"
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
		`{"tools":[{"type":"tool_search","execution":"invalid"}]}`,
		`{"tools":[{"type":"tool_search","execution":null}]}`,
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
		for _, invalid := range []any{map[string]any{"type": "tool_search", "execution": "invalid"}, map[string]any{"type": "tool_search", "execution": "server", "parameters": map[string]any{}}} {
			body["tools"] = []any{invalid}
			if w := call("POST", "/responses", key, body, ""); w.Code != 400 || calls.Load() != before {
				t.Fatal("invalid search admitted", w.Code)
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

const serverSearchHistory = `[{"type":"tool_search_call","execution":"server","call_id":null,"status":"completed","arguments":{"paths":["files"]}},{"type":"tool_search_output","execution":"server","call_id":null,"status":"completed","tools":` + discoveredClientTools + `}]`

func TestHostedToolSearch(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"m"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"hello"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":[{"type":"tool_search"}]}`,
		`{"tools":[{"type":"tool_search","execution":"server","parameters":null,"description":null}]}`,
		`{"input":[{"type":"additional_tools","tools":[{"type":"tool_search"}]}]}`,
		`{"input":` + serverSearchHistory + `}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.HostedToolSearch || in.HostedSearch {
			t.Fatal("server search admission", raw, in, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("server search converted to client execution", raw)
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("server search converted to Messages", raw)
		}
		meter := hostedSearchMeter{OpenAI: true}
		if err := meter.observe([]byte(`{"status":"completed","output":` + serverSearchHistory + `}`)); err != nil || meter.count() != 0 {
			t.Fatal("tool discovery billed as web search", err)
		}
	}
	for _, raw := range []string{
		`{"tools":[{"type":"tool_search","parameters":{}}]}`,
		`{"tools":[{"type":"tool_search","description":"client configuration"}]}`,
		`{"tools":[{"type":"tool_search","endpoint":"https://example.test"}]}`,
		`{"tools":[{"type":"tool_search"},{"type":"mcp","server_url":"https://example.test"}]}`,
		`{"input":[{"type":"tool_search_output","execution":"server","tools":[{"type":"image_generation"}]}]}`,
		`{"input":[{"type":"tool_search_output","execution":"server","status":"incomplete","tools":[{"type":"web_search"}]}]}`,
		`{"input":[{"type":"tool_search_output","execution":"server","tools":[{"type":"namespace","name":"x","tools":[{"type":"mcp"}]}]}]}`,
		`{"input":[{"type":"tool_search_output","execution":"server","tools":null}]}`,
		`{"input":[{"type":"tool_search_output","execution":"server","status":"bad","tools":[]}]}`,
		`{"input":[{"type":"tool_search_call","execution":"server","id":"../other","arguments":{}}]}`,
		`{"input":[{"type":"tool_search_call","execution":"server","call_id":"client","arguments":{}}]}`,
		`{"input":[{"type":"tool_search_call","execution":"server","arguments":[]}]}`,
	} {
		if _, _, err := parse(raw); err == nil {
			t.Fatal("invalid server search admitted", raw)
		}
	}
}

func testHostedToolSearch(t *testing.T, a *App, admin string) {
	for _, kind := range []string{"search", "local", "computer", "computer_use_preview", "mcp", "code", "shell", "files", "uploads", "program"} {
		t.Run("native-tools-"+kind, func(t *testing.T) { testNativeResponseTools(t, a, admin, kind) })
	}
}

// Shared native transports, identity, and recovery assertions apply to both
// server tool discovery and client-owned execution tools.
func testNativeResponseTools(t *testing.T, a *App, admin, kind string) {
	t.Helper()
	containerTool := kind == "code" || kind == "shell"
	programTool := kind == "program"
	reference := func(id string) json.RawMessage {
		if kind == "shell" {
			return json.RawMessage(fmt.Sprintf(`[{"type":"shell","environment":{"type":"container_reference","container_id":%q}}]`, id))
		}
		return json.RawMessage(fmt.Sprintf(`[{"type":"code_interpreter","container":%q}]`, id))
	}
	defer pauseTestWorkers(a)()
	ctx := context.Background()
	email := "native-tools-" + kind + "@example.test"
	name := "Native tools " + kind
	ip := "192.0.2.181:1234"
	declaration := `[{"type":"tool_search"},{"type":"namespace","name":"files","tools":[{"type":"function","name":"read","parameters":{"type":"object"},"defer_loading":true}]}]`
	output, history, marker, toolMarker := serverSearchHistory, serverSearchHistory, `"execution":"server"`, `"defer_loading":true`
	if kind == "local" {
		declaration, output, history, marker, toolMarker = nativeLocalTools, nativeLocalCalls, nativeLocalHistory, `"type":"shell_call"`, `"type":"local"`
		ip = "192.0.2.182:1234"
	}
	if kind == "computer" || kind == "computer_use_preview" {
		declaration, output, history, marker, toolMarker = `[{"type":"computer"}]`, nativeComputerCalls, nativeComputerHistory, `"pending_safety_checks"`, `"type":"computer"`
		ip = "192.0.2.183:1234"
		if kind == "computer_use_preview" {
			declaration, output, toolMarker = nativeComputerPreview, nativeComputerLegacyCall, `"environment":"browser"`
			ip = "192.0.2.184:1234"
		}
	}
	if kind == "mcp" {
		declaration, output, history, marker, toolMarker = nativeMCPTools, nativeMCPCalls, nativeMCPHistory, `"type":"mcp_approval_request"`, `"require_approval":"always"`
		ip = "192.0.2.185:1234"
	}
	if containerTool {
		declaration, output, history, marker, toolMarker = nativeCodeTools, nativeCodeCalls, nativeCodeCalls, `"type":"code_interpreter_call"`, `"memory_limit":"4g"`
		ip = "192.0.2.186:1234"
	}
	if kind == "shell" {
		declaration, output, history, marker, toolMarker = nativeHostedShellTools, nativeHostedShellCalls, nativeHostedShellCalls, `"type":"shell_call"`, `"type":"container_auto"`
		declaration = strings.Replace(declaration, `"memory_limit":"4g"`, `"skills":`+nativeHostedSkills+`,"memory_limit":"4g"`, 1)
		ip = "192.0.2.188:1234"
	}

	if kind == "files" || kind == "uploads" {
		declaration, output, history, marker, toolMarker = nativeFileTools, nativeFileCalls, nativeFileCalls, `"type":"file_search_call"`, `"vector_store_ids":["vs_team"]`
		ip = "192.0.2.187:1234"
	}
	if programTool {
		declaration, output, history, marker, toolMarker = nativeProgramTools, nativeProgramCalls, nativeProgramCalls[:len(nativeProgramCalls)-1]+","+nativeProgramHistory[1:], `"type":"program"`, `"programmatic"`
		ip = "192.0.2.189:1234"
	}
	grantKey, resourceID := responseStoresKey, "vs_team"
	if kind == "uploads" {
		declaration = `[]`
		output = `[{"type":"message","id":"msg_uploaded","role":"assistant","content":[{"type":"output_text","text":"Read the file.","annotations":[]}]}]`
		history = `[{"type":"item_reference","id":"msg_uploaded"}]`
		marker, toolMarker = `"text":"Read the file."`, `[]`
		grantKey, resourceID = responseFilesKey, "file_team"
		ip = "192.0.2.188:1234"
	}
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = ip
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-cookie")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": email, "password": "search-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": "search-password"})["access_token"].(string)
	prices := []any{map[string]any{"platform": "openai", "models": []string{"tool-model"}, "input_price": "0.01", "output_price": "0.02", "cache_read_price": "0.003", "cache_write_price": "0.004"}}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": name, "platform": "openai", "search_price_per_1k": 1000, "model_pricing": prices}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "search", "group_id": gid, "quota": 100})
	key, kid := k["key"].(string), id(k)
	var calls atomic.Int64
	var reject atomic.Bool
	var rejectResources atomic.Bool
	var resourceRejections atomic.Int64
	var textOnly atomic.Bool
	var invalidToolEnvelope atomic.Bool
	var pending, received atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hosted-search-provider" || r.Header.Get("Cookie") != "" {
			t.Error("search credential isolation")
		}
		if strings.HasPrefix(r.URL.Path, "/v1/responses/") && rejectResources.Load() {
			resourceRejections.Add(1)
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"unavailable client-mcp-secret client-domain-secret"}}`)
			return
		}
		if r.Method == "GET" && r.Header.Get("Upgrade") == "" {
			fmt.Fprint(w, pending.Load().(string))
			return
		}
		var conn *websocket.Conn
		var body map[string]json.RawMessage
		if r.Header.Get("Upgrade") == "websocket" {
			var err error
			conn, err = websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			_, raw, err := conn.Read(r.Context())
			if err != nil || json.Unmarshal(raw, &body) != nil {
				t.Error("search WS request", err)
				return
			}
		} else if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("search JSON request")
			return
		}
		if kind == "uploads" && r.URL.Path == "/v1/chat/completions" {
			calls.Add(1)
			received.Store(body)
			fmt.Fprint(w, `{"id":"chat_file","object":"chat.completion","model":"native-tool","choices":[{"index":0,"message":{"role":"assistant","content":"Read the file."},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":4}}}`)
			return
		}
		if r.URL.Path != "/v1/responses" || credentialString(body, "model") != "native-tool" {
			t.Error("search native routing")
		}
		received.Store(body)
		n := calls.Add(1)
		if reject.Load() {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"unavailable client-mcp-secret client-domain-secret"}}`)
			return
		}
		response := fmt.Sprintf(`{"id":"resp_hosted_tool_%d","object":"response","model":"native-tool","status":"completed","output":%s,%s}`, n, output, responseUsage)
		if kind == "uploads" && n > 1 {
			response = strings.ReplaceAll(response, `"msg_uploaded"`, fmt.Sprintf(`"msg_uploaded_%d"`, n))
		}
		if containerTool && conn != nil {
			response = strings.ReplaceAll(strings.ReplaceAll(response, "ci_team", "ci_socket"), "cntr_team", "cntr_socket")
		}
		if programTool && conn != nil {
			response = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(response, "pc_team", "pc_socket"), "prog_team", "prog_socket"), "po_team", "po_socket")
		}
		if textOnly.Load() {
			response = fmt.Sprintf(`{"id":"resp_hosted_tool_%d","object":"response","model":"native-tool","status":"completed","output":[{"id":"msg_code_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":"4","annotations":[]}]}],%s}`, n, n, responseUsage)
		}
		if kind == "mcp" || containerTool {
			response = strings.TrimSuffix(response, "}") + `,"tools":` + declaration + `}`
			if tools := body["tools"]; kind == "mcp" && tools != nil && !strings.Contains(string(tools), "client-mcp-secret") {
				t.Error("MCP authentication headers not forwarded in body")
			}
			if r.Header.Get("X-Api-Key") != "" {
				t.Error("MCP headers leaked into provider HTTP headers")
			}
		}
		if string(body["background"]) == "true" {
			pending.Store(response)
			if invalidToolEnvelope.Load() {
				// A valid accepted ID must survive a later credential-cleaning failure.
				queued := fmt.Sprintf(`{"id":"resp_hosted_tool_%d","object":"response","status":"queued","tools":%s%s%s}`, n, strings.Repeat("[", 34), declaration, strings.Repeat("]", 34))
				if string(body["stream"]) == "true" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"type\":\"response.created\",\"response\":%s}\n\n", queued)
				} else {
					fmt.Fprint(w, queued)
				}
				return
			}
			fmt.Fprintf(w, `{"id":"resp_hosted_tool_%d","object":"response","status":"queued"}`, n)
			return
		}
		if string(body["stream"]) != "true" && conn == nil {
			fmt.Fprint(w, response)
			return
		}
		event := `{"type":"response.completed","response":` + response + `}`
		if conn != nil {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(event)); err != nil {
				t.Error(err)
			}
			if containerTool || kind == "files" || kind == "uploads" || programTool {
				_, next, err := conn.Read(r.Context())
				if err != nil || containerTool && !bytes.Contains(next, []byte("cntr_socket")) || !bytes.Contains(next, []byte(`"store":false`)) {
					t.Error("socket container continuation", err, string(next))
					return
				}
				n := calls.Add(1)
				plain := fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_hosted_tool_%d","object":"response","model":"native-tool","status":"completed","output":[],%s}}`, n, responseUsage)
				if err := conn.Write(r.Context(), websocket.MessageText, []byte(plain)); err != nil {
					t.Error(err)
				}
			}
			_, _, _ = conn.Read(r.Context())
		} else {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Hosted search provider", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}, "credentials": map[string]any{"api_key": "hosted-search-provider", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"tool-model": "native-tool"}}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "tool-model", "input": "find tools", "tools": json.RawMessage(declaration)}
	if kind == "shell" {
		if got := call("POST", "/responses", key, body, ""); got.Code != 503 || calls.Load() != 0 {
			t.Fatal("ungranted skill dispatched", got.Code)
		}
		grants := map[string]any{responseSkillsKey: map[string][]string{fmt.Sprint(gid): {"skill_team"}}}
		if got := call("PUT", ap, user, map[string]any{"extra": grants}, ""); got.Code != 403 {
			t.Fatal("user changed skill grants", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"extra": grants})
		unknown := map[string]any{"model": "tool-model", "input": "calculate", "tools": json.RawMessage(strings.ReplaceAll(declaration, "skill_team", "skill_foreign"))}
		if got := call("POST", "/responses", key, unknown, ""); got.Code != 503 || calls.Load() != 0 {
			t.Fatal("unknown skill dispatched", got.Code)
		}
	}
	if kind == "files" || kind == "uploads" {
		body["include"] = []string{"file_search_call.results"}
		if kind == "uploads" {
			delete(body, "include")
			body["input"] = json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file_team"}]}]`)
		}
		before := calls.Load()
		if got := call("POST", "/responses", key, body, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("ungranted store dispatched", got.Code)
		}
		grants := map[string]any{grantKey: map[string][]string{fmt.Sprint(gid): {resourceID}}}
		if got := call("PUT", ap, user, map[string]any{"extra": grants}, ""); got.Code != 403 {
			t.Fatal("user changed store grants", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"extra": grants})
		unknown := map[string]any{"model": "tool-model", "input": "read", "tools": json.RawMessage(`[{"type":"file_search","vector_store_ids":["vs_foreign"]}]`)}
		if kind == "uploads" {
			unknown = map[string]any{"model": "tool-model", "input": json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file_foreign"}]}]`)}
		}
		if got := call("POST", "/responses", key, unknown, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("foreign store dispatched", got.Code)
		}
	}
	check := func(logs int) {
		t.Helper()
		var count int
		var correct bool
		if err := a.DB.QueryRow(`SELECT count(*),bool_and(actual_cost=0.307 AND model='tool-model') FROM usage_logs WHERE api_key_id=$1`, kid).Scan(&count, &correct); err != nil || count != logs || !correct {
			t.Fatal("search token billing", count, logs, correct, err)
		}
	}
	w := call("POST", "/responses", key, body, "hosted-search-json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), marker) || !strings.Contains(string(received.Load().(map[string]json.RawMessage)["tools"]), toolMarker) {
		t.Fatal("native hosted search", w.Code, w.Body.String())
	}
	if (kind == "mcp" || containerTool) && (bytes.Contains(w.Body.Bytes(), []byte("client-mcp-secret")) || bytes.Contains(w.Body.Bytes(), []byte("client-domain-secret"))) {
		t.Fatal("tool credentials exposed in JSON response")
	}
	if containerTool && !bytes.Contains(received.Load().(map[string]json.RawMessage)["tools"], []byte("client-domain-secret")) {
		t.Fatal("container domain secret not forwarded to provider")
	}
	if kind == "shell" {
		wire := string(received.Load().(map[string]json.RawMessage)["tools"])
		if !strings.Contains(wire, inlineSkillBundle) || !strings.Contains(wire, `"version":"2"`) {
			t.Fatal("inline bundle or skill version changed")
		}
	}
	var initial struct{ Output json.RawMessage }
	if json.Unmarshal(w.Body.Bytes(), &initial) != nil || string(initial.Output) != output {
		t.Fatal("native tool output changed", string(initial.Output))
	}
	check(1)
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
		if replay := call("POST", path, key, body, "hosted-search-json"); replay.Body.String() != w.Body.String() || calls.Load() != 1 {
			t.Fatal("hosted search replay", replay.Code)
		}
	}
	var first struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "other-search", "group_id": gid})["key"].(string)
	body["previous_response_id"] = first.ID
	if w = call("POST", "/responses", other, body, ""); w.Code != 404 || calls.Load() != 1 {
		t.Fatal("foreign search continuation", w.Code)
	}
	if containerTool {
		for _, request := range []map[string]any{
			{"model": "tool-model", "input": "calculate", "tools": reference("cntr_team")},
			{"model": "tool-model", "input": "calculate", "previous_response_id": first.ID, "tools": reference("cntr_foreign")},
		} {
			if got := call("POST", "/responses", key, request, ""); got.Code != 404 || calls.Load() != 1 {
				t.Fatal("unowned container dispatched", got.Code, got.Body.String())
			}
		}
		implicit := map[string]any{"model": "tool-model", "previous_response_id": first.ID, "input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_file", "file_id": "foreign"}}}}}
		if got := call("POST", "/responses", key, implicit, ""); got.Code != 503 || calls.Load() != 1 {
			t.Fatal("implicit code file access bypass", got.Code, got.Body.String())
		}
		implicit["input"] = "continue"
		if got := call("POST", "/responses/compact", key, implicit, ""); got.Code != 400 || calls.Load() != 1 {
			t.Fatal("implicit code compaction bypass", got.Code)
		}
		foreign := map[string]any{"model": "tool-model", "input": json.RawMessage(history)}
		if got := call("POST", "/responses", other, foreign, ""); got.Code != 404 || calls.Load() != 1 {
			t.Fatal("foreign full code history dispatched", got.Code)
		}
		delete(body, "previous_response_id") // Owned item IDs alone must select the original source.
	}
	if programTool {
		foreign := map[string]any{"model": "tool-model", "input": json.RawMessage(history)}
		for _, raw := range []string{history, nativeProgramHistory} {
			foreign["input"] = json.RawMessage(raw)
			if got := call("POST", "/responses", other, foreign, ""); got.Code != 404 || calls.Load() != 1 {
				t.Fatal("foreign program history dispatched", got.Code)
			}
		}
		for _, raw := range []string{
			strings.ReplaceAll(history, "opaque-program-replay", "altered-fingerprint"),
			strings.ReplaceAll(history, "return await", "throw await"),
			strings.ReplaceAll(nativeProgramCalls, `"result":"9007199254740993"`, `"result":"altered"`),
			strings.ReplaceAll(nativeProgramHistory, "pc_team", "pc_unknown"),
		} {
			foreign["input"] = json.RawMessage(raw)
			if got := call("POST", "/responses", key, foreign, ""); got.Code != 404 || calls.Load() != 1 {
				t.Fatal("unknown or altered program history dispatched", got.Code, got.Body.String())
			}
		}
		for _, path := range []string{"/responses/compact", "/responses/input_tokens"} {
			request := map[string]any{"model": "tool-model", "input": "continue", "previous_response_id": first.ID}
			if got := call("POST", path, key, request, ""); got.Code != 400 || calls.Load() != 1 {
				t.Fatal("implicit program admitted on auxiliary endpoint", got.Code)
			}
		}
		delete(body, "previous_response_id")
	}
	if kind == "files" || kind == "uploads" {
		foreign := map[string]any{"model": "tool-model", "input": json.RawMessage(history)}
		if got := call("POST", "/responses", other, foreign, ""); got.Code != 404 || calls.Load() != 1 {
			t.Fatal("foreign file search history dispatched", got.Code)
		}
		implicit := map[string]any{"model": "tool-model", "input": "continue", "previous_response_id": first.ID}
		if kind == "files" {
			if got := call("POST", "/responses/compact", key, implicit, ""); got.Code != 400 || calls.Load() != 1 {
				t.Fatal("implicit file search compaction dispatched", got.Code)
			}
		}
		implicit["input"] = json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file_foreign"}]}]`)
		if got := call("POST", "/responses", key, implicit, ""); got.Code != 503 || calls.Load() != 1 {
			t.Fatal("implicit file search file access bypass", got.Code)
		}
		implicit["input"] = "continue"
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{grantKey: map[string][]string{}}})
		for _, request := range []map[string]any{implicit, foreign} {
			if got := call("POST", "/responses", key, request, ""); got.Code != 503 || calls.Load() != 1 {
				t.Fatal("revoked store history dispatched", got.Code, got.Body.String())
			}
		}
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{grantKey: map[string][]string{fmt.Sprint(gid): {resourceID}}}})
		delete(body, "previous_response_id")
	}
	body["input"], body["stream"] = json.RawMessage(history), true
	if kind == "mcp" {
		delete(body, "previous_response_id")
		if got := call("POST", "/responses", other, body, ""); got.Code != 404 || calls.Load() != 1 {
			t.Fatal("foreign MCP approval without previous response dispatched", got.Code)
		}
		body["input"] = json.RawMessage(`[{"type":"mcp_approval_response","approval_request_id":"mcpr_unknown","approve":true}]`)
		if got := call("POST", "/responses", key, body, ""); got.Code != 404 || calls.Load() != 1 {
			t.Fatal("unknown MCP approval dispatched", got.Code)
		}
		body["input"] = json.RawMessage(history)
	}
	if kind != "search" {
		delete(body, "tools") // Full native history alone must preserve platform admission.
	}
	w = call("POST", "/responses", key, body, "")
	if !strings.Contains(w.Body.String(), "response.completed") || responseToolDefinition(map[string]json.RawMessage{"input": received.Load().(map[string]json.RawMessage)["input"]}) != responseToolDefinition(map[string]json.RawMessage{"input": json.RawMessage(history)}) {
		t.Fatal("hosted history or SSE lost", w.Code, w.Body.String())
	}
	if (kind == "mcp" || containerTool) && (bytes.Contains(w.Body.Bytes(), []byte("client-mcp-secret")) || bytes.Contains(w.Body.Bytes(), []byte("client-domain-secret"))) {
		t.Fatal("tool credentials exposed in SSE response")
	}
	check(2)
	delete(body, "previous_response_id")
	delete(body, "stream")
	body["input"] = "continue"
	body["tools"] = json.RawMessage(declaration)
	if kind == "uploads" {
		body["input"] = json.RawMessage(`[{"role":"user","content":[{"type":"input_image","file_id":"file_team"}]}]`)
	}
	// Both WebSocket and background execution retain native semantics and pricing.
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	wsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	body["type"] = "response.create"
	if containerTool || kind == "files" || kind == "uploads" || programTool {
		body["store"] = false
	}
	raw, _ := json.Marshal(body)
	if err = conn.Write(wsCtx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = conn.Read(wsCtx)
	if err != nil || !bytes.Contains(raw, []byte(`"response.completed"`)) || !bytes.Contains(raw, []byte(marker)) {
		conn.CloseNow()
		t.Fatal("hosted search WS", string(raw), err)
	}
	if (kind == "mcp" || containerTool) && (bytes.Contains(raw, []byte("client-mcp-secret")) || bytes.Contains(raw, []byte("client-domain-secret"))) {
		conn.CloseNow()
		t.Fatal("tool credentials exposed over WS")
	}
	contextExtra := 0
	if containerTool || kind == "files" || kind == "uploads" || programTool {
		var event struct{ Response struct{ ID string } }
		_ = json.Unmarshal(raw, &event)
		request := map[string]any{"type": "response.create", "model": "tool-model", "input": "continue", "store": false, "previous_response_id": event.Response.ID, "tools": reference("cntr_socket")}
		if kind == "files" || kind == "uploads" || programTool {
			delete(request, "tools")
		}
		if programTool {
			delete(request, "previous_response_id")
			request["input"] = json.RawMessage(strings.ReplaceAll(nativeProgramHistory, "pc_team", "pc_socket"))
		}
		next, _ := json.Marshal(request)
		if err := conn.Write(wsCtx, websocket.MessageText, next); err != nil {
			conn.CloseNow()
			t.Fatal(err)
		}
		_, next, err = conn.Read(wsCtx)
		if err != nil || !bytes.Contains(next, []byte(`"response.completed"`)) {
			conn.CloseNow()
			t.Fatal("socket scoped container reuse", string(next), err)
		}
		delete(request, "type")
		if got := call("POST", "/responses", key, request, ""); got.Code != 404 {
			conn.CloseNow()
			t.Fatal("store=false container escaped socket context", got.Code)
		}
		contextExtra = 1
	}
	conn.CloseNow()
	delete(body, "type")
	check(3 + contextExtra)
	body["background"], body["store"] = true, true
	if containerTool || kind == "files" || kind == "uploads" || programTool {
		body["previous_response_id"] = first.ID
		delete(body, "tools")
		textOnly.Store(!programTool) // Programs exercise terminal native output on recovery; others inherit grants.
	}
	if kind == "search" {
		body["tools"] = json.RawMessage(`[{"type":"tool_search","execution":"server"}]`)
	}
	w = call("POST", "/responses", key, body, "hosted-search-background")
	var accepted struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" {
		t.Fatal("hosted search background", w.Code, w.Body.String())
	}
	identity := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	taskID, err := a.Redis.Get(ctx, backgroundIndex(identity, accepted.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if kind == "uploads" {
		// Removing future access must not lose consumption already accepted upstream.
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseFilesKey: map[string][]string{}}})
	}
	if kind == "shell" {
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseSkillsKey: map[string][]string{}}})
	}
	must("PUT", gp, admin, map[string]any{"rate_multiplier": 9})
	if _, err = a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_hosted_tool_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_hosted_tool_failure")
	task, err := a.loadBackgroundResponse(ctx, taskID)
	if err != nil || a.refreshBackgroundResponse(ctx, task) == nil {
		t.Fatal("unsettled search succeeded", err)
	}
	if _, err = a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_hosted_tool_failure"); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, privateUpstreams: a.privateUpstreams}
	for range 2 {
		task, err = fresh.loadBackgroundResponse(ctx, taskID)
		if err != nil || fresh.refreshBackgroundResponse(ctx, task) != nil {
			t.Fatal("search recovery", err)
		}
	}
	check(4 + contextExtra)
	if programTool {
		binding, err := fresh.previousResponse(ctx, identity, accepted.ID)
		if err != nil || binding == nil || !binding.ProgrammaticTool || !task.ProgrammaticTool {
			t.Fatal("program scope lost across background recovery", binding, err)
		}
		if source, err := fresh.responseItemSource(ctx, identity, []string{programCallReference("pc_team")}, nil); err != nil || !source.ProgrammaticTool {
			t.Fatal("program caller not bound after recovery", err)
		}
	}
	if containerTool {
		binding, err := fresh.previousResponse(ctx, identity, accepted.ID)
		if err != nil || !binding.CodeTool || len(binding.Containers) != 1 || binding.Containers[0] != "cntr_team" {
			t.Fatal("code ownership lost across background recovery", binding, err)
		}
		if kind == "shell" {
			if len(task.SkillIDs) != 1 || len(binding.SkillIDs) != 1 || binding.SkillIDs[0] != "skill_team" {
				t.Fatal("skill authorization lost across background recovery", binding)
			}
			for _, previous := range []string{first.ID, accepted.ID} {
				got := call("POST", "/responses", key, map[string]any{"model": "tool-model", "input": "continue", "previous_response_id": previous}, "")
				if got.Code != 503 || calls.Load() != before {
					t.Fatal("revoked skill admitted through implicit continuation", got.Code)
				}
			}
			must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseSkillsKey: map[string][]string{fmt.Sprint(gid): {"skill_team"}}}})
		}
	}
	if kind == "files" || kind == "uploads" {
		binding, err := fresh.previousResponse(ctx, identity, accepted.ID)
		var resources []string
		if binding != nil {
			resources = binding.VectorStores
			if kind == "uploads" {
				resources = binding.FileIDs
			}
		}
		if err != nil || len(resources) != 1 || resources[0] != resourceID {
			t.Fatal("store scope lost across background recovery", binding, err)
		}
	}
	if kind == "mcp" || containerTool {
		if bytes.Contains(task.Result, []byte("client-mcp-secret")) || bytes.Contains(task.Result, []byte("client-domain-secret")) {
			t.Fatal("tool credentials persisted in recovered background result")
		}
		got := call("GET", "/responses/"+accepted.ID+"?include=reasoning.encrypted_content", key, nil, "")
		if got.Code != 200 || strings.Contains(got.Body.String(), "client-mcp-secret") || strings.Contains(got.Body.String(), "client-domain-secret") {
			t.Fatal("tool resource read redaction", got.Code, got.Body.String())
		}
	}
	if calls.Load() != before {
		t.Fatal("search recovery dispatched again")
	}
	var balanced bool
	if err = a.DB.QueryRow(`SELECT u.balance=100-s.cost AND k.quota_used=s.cost FROM users u JOIN api_keys k ON k.user_id=u.id CROSS JOIN (SELECT sum(round(actual_cost,8)) cost FROM usage_logs WHERE api_key_id=$1)s WHERE k.id=$1`, kid).Scan(&balanced); err != nil || !balanced {
		t.Fatal("search wallet/key drift", err)
	}
	delete(body, "background")
	if containerTool || kind == "files" || kind == "uploads" || programTool {
		delete(body, "previous_response_id")
		body["tools"] = json.RawMessage(declaration)
		textOnly.Store(false)
		if kind == "uploads" {
			body["input"] = json.RawMessage(`[{"role":"user","content":[{"type":"input_file","file_id":"file_team"}]}]`)
		}
	}
	// Composite admission uses the resolved provider, not the public model name.
	cgid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite " + name, "platform": "composite", "model_pricing": prices}))
	cp := fmt.Sprintf("/api/v1/admin/groups/%d", cgid)
	route := must("POST", cp+"/composite-routes", admin, map[string]any{"public_model": "tool-model", "match_type": "exact", "target_platform": "openai", "upstream_model": "tool-model", "endpoint": "responses", "enabled": true})
	must("PUT", ap, admin, map[string]any{"group_ids": []int64{gid, cgid}})
	ckey := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite-search", "group_id": cgid})["key"].(string)
	if kind == "shell" {
		before := calls.Load()
		if got := call("POST", "/responses", ckey, body, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("skill escaped its group", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseSkillsKey: map[string][]string{fmt.Sprint(gid): {"skill_team"}, fmt.Sprint(cgid): {"skill_team"}}}})
	}
	if kind == "files" || kind == "uploads" {
		before := calls.Load()
		if got := call("POST", "/responses", ckey, body, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("cross-group store access", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{grantKey: map[string][]string{fmt.Sprint(gid): {resourceID}, fmt.Sprint(cgid): {resourceID}}}})
	}
	if w = call("POST", "/responses", ckey, body, ""); w.Code != 200 {
		t.Fatal("composite hosted search", w.Code, w.Body.String())
	}
	if containerTool {
		request := map[string]any{"model": "tool-model", "previous_response_id": accepted.ID, "input": "reuse", "tools": reference("cntr_team")}
		textOnly.Store(true)
		got := call("POST", "/responses", key, request, "")
		textOnly.Store(false)
		if got.Code != 200 {
			t.Fatal("owned container reuse after plain background reply", got.Code, got.Body.String())
		}
		var continued struct{ ID string }
		_ = json.Unmarshal(got.Body.Bytes(), &continued)
		binding, err := fresh.previousResponse(ctx, identity, continued.ID)
		if err != nil || !binding.CodeTool || len(binding.Containers) != 1 || binding.Containers[0] != "cntr_team" {
			t.Fatal("code context lost across plain HTTP reply", binding, err)
		}
		if kind == "shell" && (len(binding.SkillIDs) != 1 || binding.SkillIDs[0] != "skill_team") {
			t.Fatal("skill grant lost across plain reply")
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
		before := calls.Load()
		if got := call("POST", "/responses", key, request, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("code context switched credentials", got.Code, got.Body.String())
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "hosted-search-provider"}})
		second := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Second code provider", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "hosted-search-provider", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"tool-model": "native-tool"}}})
		if kind == "shell" {
			must("PUT", "/api/v1/admin/accounts/"+fmt.Sprint(id(second)), admin, map[string]any{"extra": map[string]any{responseSkillsKey: map[string][]string{fmt.Sprint(gid): {"skill_team"}}}})
		}
		reject.Store(true)
		got = call("POST", "/responses", key, body, "code-ambiguous")
		if got.Code != 502 || calls.Load() != before+1 {
			t.Fatal("code execution retried after ambiguous rejection", got.Code, calls.Load()-before)
		}
		_ = call("POST", "/responses", key, body, "code-ambiguous")
		if calls.Load() != before+1 {
			t.Fatal("code idempotency repeated rejected execution")
		}
		reject.Store(false)
		must("PUT", "/api/v1/admin/accounts/"+fmt.Sprint(id(second)), admin, map[string]any{"status": "inactive"})
		must("POST", ap+"/recover-state", admin, map[string]any{})
		rule := must("POST", "/api/v1/admin/error-passthrough-rules", admin, map[string]any{"name": "Container upstream failure", "enabled": true, "error_codes": []int{503}, "keywords": []string{"client-domain-secret"}, "match_mode": "all", "passthrough_code": true, "passthrough_body": true})
		reject.Store(true)
		before = calls.Load()
		for _, request := range []map[string]any{body, {"model": "tool-model", "input": "continue", "previous_response_id": accepted.ID}} {
			got := call("POST", "/responses", key, request, "")
			before++
			if got.Code != 503 || calls.Load() != before || strings.Contains(got.Body.String(), "client-domain-secret") || !strings.Contains(got.Body.String(), "upstream tool request rejected") {
				t.Fatal("container secret exposed by error rule or implicit continuation", got.Code, got.Body.String())
			}
			must("POST", ap+"/recover-state", admin, map[string]any{})
		}
		reject.Store(false)
		rejectResources.Store(true)
		for _, operation := range []struct{ method, path string }{
			{"GET", "/responses/" + first.ID},
			{"GET", "/responses/" + first.ID + "/input_items"},
			{"GET", "/responses/" + accepted.ID + "?include=reasoning.encrypted_content"},
			{"DELETE", "/responses/" + continued.ID},
		} {
			before := resourceRejections.Load()
			if got := call(operation.method, operation.path, other, nil, ""); got.Code != 404 || resourceRejections.Load() != before {
				t.Fatal("foreign container resource request reached provider", got.Code)
			}
			got := call(operation.method, operation.path, key, nil, "")
			if got.Code != 503 || resourceRejections.Load() != before+1 || strings.Contains(got.Body.String(), "client-domain-secret") || !strings.Contains(got.Body.String(), "upstream tool request rejected") {
				t.Fatal("container resource error exposed credentials", operation, got.Code, got.Body.String())
			}
		}
		// A pending worker query has the same secret protection after a restart.
		pendingTask := *task
		pendingTask.Stage = "pending"
		if err := fresh.refreshBackgroundResponse(ctx, &pendingTask); err == nil || strings.Contains(err.Error(), "client-domain-secret") || !strings.Contains(err.Error(), "upstream tool request rejected") {
			t.Fatal("background reconciliation error exposed credentials", err)
		}
		rejectResources.Store(false)
		var leaked bool
		if err := a.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM idempotency_records WHERE response_body LIKE '%client-domain-secret%')`).Scan(&leaked); err != nil || leaked {
			t.Fatal("container credentials persisted in idempotency results", err)
		}
		must("DELETE", "/api/v1/admin/error-passthrough-rules/"+fmt.Sprint(id(rule)), admin, nil)
	}
	if kind == "files" || kind == "uploads" {
		request := map[string]any{"model": "tool-model", "previous_response_id": accepted.ID, "input": "continue"}
		textOnly.Store(true)
		got := call("POST", "/responses", key, request, "")
		textOnly.Store(false)
		if got.Code != 200 {
			t.Fatal("file search plain continuation", got.Code, got.Body.String())
		}
		var continued struct{ ID string }
		_ = json.Unmarshal(got.Body.Bytes(), &continued)
		binding, err := fresh.previousResponse(ctx, identity, continued.ID)
		var resources []string
		if binding != nil {
			resources = binding.VectorStores
			if kind == "uploads" {
				resources = binding.FileIDs
			}
		}
		if err != nil || len(resources) != 1 || resources[0] != resourceID {
			t.Fatal("store scope lost across plain HTTP reply", binding, err)
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
		before := calls.Load()
		for _, input := range []map[string]any{body, request} {
			if got := call("POST", "/responses", key, input, ""); got.Code != 503 || calls.Load() != before {
				t.Fatal("store grant followed credential rotation", got.Code)
			}
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "hosted-search-provider"}})
		second := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Second file search provider", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "hosted-search-provider", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"tool-model": "native-tool"}}, "extra": map[string]any{grantKey: map[string][]string{fmt.Sprint(gid): {resourceID}}}})
		reject.Store(true)
		got = call("POST", "/responses", key, body, "files-ambiguous")
		if got.Code != 502 || calls.Load() != before+1 {
			t.Fatal("file search retried after ambiguous rejection", got.Code, calls.Load()-before)
		}
		_ = call("POST", "/responses", key, body, "files-ambiguous")
		if calls.Load() != before+1 {
			t.Fatal("file search idempotency repeated execution")
		}
		reject.Store(false)
		must("PUT", "/api/v1/admin/accounts/"+fmt.Sprint(id(second)), admin, map[string]any{"status": "inactive"})
		must("POST", ap+"/recover-state", admin, map[string]any{})
	}
	if kind == "mcp" || containerTool {
		for _, stream := range []bool{false, true} {
			request := map[string]any{"model": "tool-model", "input": "recover accepted tool", "tools": json.RawMessage(declaration), "background": true, "store": true, "stream": stream}
			idem := fmt.Sprintf("tool-sanitization-%t", stream)
			invalidToolEnvelope.Store(true)
			before := calls.Load()
			got := call("POST", "/responses", key, request, idem)
			invalidToolEnvelope.Store(false)
			if calls.Load() != before+1 || strings.Contains(got.Body.String(), "client-mcp-secret") || strings.Contains(got.Body.String(), "client-domain-secret") || !stream && got.Code != 502 || stream && !strings.Contains(got.Body.String(), `"type":"error"`) {
				t.Fatal("invalid tool response published", got.Code, got.Body.String())
			}
			nativeID := fmt.Sprintf("resp_hosted_tool_%d", before+1)
			id, err := a.Redis.Get(ctx, backgroundIndex(identity, nativeID)).Result()
			if err != nil {
				t.Fatal("accepted tool identity lost before sanitization", err)
			}
			task, err := fresh.loadBackgroundResponse(ctx, id)
			if err != nil || task.Stage != "pending" || task.UpstreamID != nativeID || kind == "mcp" && !task.MCPTool || containerTool && !task.CodeTool || bytes.Contains(task.Result, []byte("client-mcp-secret")) || bytes.Contains(task.Result, []byte("client-domain-secret")) {
				t.Fatal("unsafe or unrecoverable tool checkpoint", err)
			}
			for range 2 {
				if err := fresh.refreshBackgroundResponse(ctx, task); err != nil {
					t.Fatal("tool sanitization recovery", err)
				}
			}
			var count int
			var cost string
			if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE request_id=$1", id).Scan(&count, &cost); err != nil || count != 1 || cost != "2.7630000000" || task.Stage != "terminal" {
				t.Fatal("recovered tool settlement", count, cost, err)
			}
			_ = call("POST", "/responses", key, request, idem)
			if calls.Load() != before+1 {
				t.Fatal("tool recovery repeated generation")
			}
		}
	}
	if programTool {
		request := map[string]any{"model": "tool-model", "input": "continue", "previous_response_id": first.ID}
		textOnly.Store(true)
		got := call("POST", "/responses", key, request, "")
		textOnly.Store(false)
		if got.Code != 200 {
			t.Fatal("plain program continuation", got.Code, got.Body.String())
		}
		var continued struct{ ID string }
		_ = json.Unmarshal(got.Body.Bytes(), &continued)
		binding, err := fresh.previousResponse(ctx, identity, continued.ID)
		if err != nil || binding == nil || !binding.ProgrammaticTool {
			t.Fatal("program safeguards lost on plain response", err)
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
		before := calls.Load()
		if got := call("POST", "/responses", key, request, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("program context followed changed credentials", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "hosted-search-provider"}})
	}
	if kind == "mcp" || programTool {
		rule := must("POST", "/api/v1/admin/error-passthrough-rules", admin, map[string]any{"name": "MCP upstream failure", "enabled": true, "error_codes": []int{503}, "keywords": []string{"client-mcp-secret"}, "match_mode": "all", "passthrough_code": true, "passthrough_body": true})
		defer must("DELETE", "/api/v1/admin/error-passthrough-rules/"+fmt.Sprint(id(rule)), admin, nil)
		must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Second MCP provider", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "hosted-search-provider", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"tool-model": "native-tool"}}})
		reject.Store(true)
		before := calls.Load()
		got := call("POST", "/responses", key, body, "mcp-ambiguous")
		if got.Code != 503 || calls.Load() != before+1 || strings.Contains(got.Body.String(), "client-mcp-secret") || !strings.Contains(got.Body.String(), "upstream tool request rejected") {
			t.Fatal("MCP retry or secret exposure", got.Code, calls.Load(), got.Body.String())
		}
		if replay := call("POST", "/responses", key, body, "mcp-ambiguous"); calls.Load() != before+1 || strings.Contains(replay.Body.String(), "client-mcp-secret") {
			t.Fatal("MCP failure replayed upstream or leaked header")
		}
		must("POST", ap+"/recover-state", admin, map[string]any{})
		continuation := map[string]any{"model": "tool-model", "input": "continue", "previous_response_id": first.ID}
		got = call("POST", "/responses", key, continuation, "mcp-implicit")
		if got.Code != 503 || calls.Load() != before+2 || strings.Contains(got.Body.String(), "client-mcp-secret") || !strings.Contains(got.Body.String(), "upstream tool request rejected") {
			t.Fatal("implicit MCP continuation lost safeguards", got.Code, got.Body.String())
		}
		if programTool {
			rejectResources.Store(true)
			for _, operation := range []struct{ method, path string }{
				{"GET", "/responses/" + first.ID},
				{"GET", "/responses/" + first.ID + "/input_items"},
				{"GET", "/responses/" + accepted.ID + "?include=reasoning.encrypted_content"},
				{"DELETE", "/responses/" + accepted.ID},
			} {
				prior := resourceRejections.Load()
				if got := call(operation.method, operation.path, other, nil, ""); got.Code != 404 || resourceRejections.Load() != prior {
					t.Fatal("foreign program resource dispatched", got.Code)
				}
				got := call(operation.method, operation.path, key, nil, "")
				if got.Code != 503 || resourceRejections.Load() != prior+1 || strings.Contains(got.Body.String(), "client-domain-secret") {
					t.Fatal("program resource error exposed", got.Code, got.Body.String())
				}
			}
			pendingTask := *task
			pendingTask.Stage = "pending"
			if err := fresh.refreshBackgroundResponse(ctx, &pendingTask); err == nil || strings.Contains(err.Error(), "client-domain-secret") {
				t.Fatal("program worker error exposed", err)
			}
			rejectResources.Store(false)
		}
		var leaked bool
		if err := a.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM idempotency_records WHERE response_body LIKE '%client-mcp-secret%')`).Scan(&leaked); err != nil || leaked {
			t.Fatal("MCP header persisted in idempotency results", err)
		}
		reject.Store(false)
		// Exclude the extra account from the conversion-rejection checks below.
		if _, err := a.DB.Exec("UPDATE accounts SET status='disabled' WHERE name='Second MCP provider'"); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "uploads" {
		requests := []map[string]any{
			{"model": "tool-model", "input": "calculate", "tools": json.RawMessage(`[{"type":"code_interpreter","container":{"type":"auto","file_ids":["file_team"]}}]`)},
			{"model": "tool-model", "input": "calculate", "tools": json.RawMessage(`[{"type":"shell","environment":{"type":"container_auto","file_ids":["file_team"]}}]`)},
			{"model": "tool-model", "input": json.RawMessage(`[{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_file","file_id":"file_team"}]}]`)},
			{"model": "tool-model", "input": json.RawMessage(`[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","file_id":"file_team"}}]`)},
			{"model": "tool-model", "prompt": json.RawMessage(`{"id":"pmpt_team","variables":{"doc":{"type":"input_file","file_id":"file_team"}}}`)},
		}
		for _, request := range requests {
			raw, _ := json.Marshal(request)
			var foreign map[string]any
			_ = json.Unmarshal(bytes.ReplaceAll(raw, []byte("file_team"), []byte("file_foreign")), &foreign)
			before := calls.Load()
			if got := call("POST", "/responses", key, foreign, ""); got.Code != 503 || calls.Load() != before {
				t.Fatal("ungranted tool or prompt file dispatched", got.Code)
			}
			got := call("POST", "/responses", key, request, "")
			wire, _ := json.Marshal(received.Load())
			if got.Code != 200 || calls.Load() != before+1 || !bytes.Contains(wire, []byte("file_team")) {
				t.Fatal("authorized file tool input", got.Code, got.Body.String())
			}
			var response struct{ ID string }
			_ = json.Unmarshal(got.Body.Bytes(), &response)
			binding, err := a.previousResponse(ctx, identity, response.ID)
			if err != nil || binding == nil || len(binding.FileIDs) != 1 || binding.FileIDs[0] != "file_team" {
				t.Fatal("tool or prompt file scope not retained", binding, err)
			}
		}
		chat := map[string]any{"model": "tool-model", "messages": json.RawMessage(`[{"role":"user","content":[{"type":"file","file":{"file_id":"file_team"}}]}]`)}
		if got := call("POST", "/v1/chat/completions", key, chat, ""); got.Code != 200 || !bytes.Contains(received.Load().(map[string]json.RawMessage)["input"], []byte("file_team")) {
			t.Fatal("Chat file converted to Responses", got.Code, got.Body.String())
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
		before := calls.Load()
		if got := call("POST", "/v1/chat/completions", key, chat, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("file grant reused after protocol change", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseFilesKey: map[string][]string{fmt.Sprint(gid): {"file_team"}}}})
		if got := call("POST", "/v1/chat/completions", key, chat, ""); got.Code != 200 || !bytes.Contains(received.Load().(map[string]json.RawMessage)["messages"], []byte("file_team")) {
			t.Fatal("native Chat file authorization", got.Code, got.Body.String())
		}
		before = calls.Load()
		foreignChat := map[string]any{"model": "tool-model", "messages": json.RawMessage(`[{"role":"user","content":[{"type":"file","file":{"file_id":"file_foreign"}}]}]`)}
		if got := call("POST", "/v1/chat/completions", key, foreignChat, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("native Chat bypassed file authorization", got.Code)
		}
		// Even a valid grant cannot make Responses resource IDs convertible to Chat.
		if got := call("POST", "/responses", key, body, ""); got.Code != 503 || calls.Load() != before {
			t.Fatal("authorized Responses file converted to Chat", got.Code)
		}
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
		must("PUT", ap, admin, map[string]any{"extra": map[string]any{responseFilesKey: map[string][]string{fmt.Sprint(gid): {"file_team"}, fmt.Sprint(cgid): {"file_team"}}}})
	}
	before = calls.Load()
	must("PUT", cp+"/composite-routes/"+fmt.Sprint(id(route)), admin, map[string]any{"public_model": "tool-model", "match_type": "exact", "target_platform": "grok", "upstream_model": "tool-model", "endpoint": "responses", "enabled": true})
	if w = call("POST", "/responses", ckey, body, ""); w.Code != 400 && !(kind == "uploads" && w.Code == 503) {
		t.Fatal("hosted discovery routed to Grok", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
	if w = call("POST", "/responses", key, body, ""); w.Code != 503 {
		t.Fatal("hosted search converted", w.Code)
	}
	if w = call("POST", "/responses", admin, body, ""); w.Code != 401 {
		t.Fatal("JWT admitted", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"status": "disabled"})
	if w = call("POST", "/responses", key, body, ""); w.Code != 401 {
		t.Fatal("disabled user admitted", w.Code)
	}
	if calls.Load() != before {
		t.Fatal("rejected search dispatched")
	}
}
