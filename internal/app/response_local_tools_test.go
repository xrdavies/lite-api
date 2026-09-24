package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

const nativeLocalTools = `[{"type":"apply_patch","allowed_callers":["direct"]},{"type":"local_shell"},{"type":"shell","environment":{"type":"local","skills":[{"name":"client-skill","path":"/client/skills","description":"Use the client workspace"}]}}]`
const nativeLocalCalls = `[{"type":"apply_patch_call","id":"apc_local","call_id":"call_patch","status":"completed","operation":{"type":"update_file","path":"client.txt","diff":"@@\n-before\n+after"}},{"type":"local_shell_call","id":"lsc_local","call_id":"call_legacy","status":"completed","action":{"type":"exec","command":["printf","client-only"],"env":{"LANG":"C"},"working_directory":"/client"}},{"type":"shell_call","id":"sc_local","call_id":"call_shell","action":{"commands":["printf client-only"],"timeout_ms":1000,"max_output_length":2000},"environment":{"type":"local"},"status":"completed"}]`
const nativeLocalHistory = `[{"type":"apply_patch_call_output","call_id":"call_patch","status":"completed","output":"patched"},{"type":"local_shell_call_output","id":"call_legacy","output":"client-only","status":"completed"},{"type":"shell_call_output","call_id":"call_shell","output":[{"stdout":"client-only","stderr":"","outcome":{"type":"exit","exit_code":0}},{"stdout":"partial","stderr":"timeout","outcome":{"type":"timeout"}}]}]`

func TestResponseLocalTools(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"edit"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":` + nativeLocalTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeLocalTools + `}]}`,
		`{"input":` + nativeLocalCalls + `}`,
		`{"input":` + nativeLocalHistory + `}`,
		`{"input":[{"type":"apply_patch_call","call_id":"c","operation":{"type":"delete_file","path":"client.txt"}}]}`,
		`{"input":[{"type":"apply_patch_call_output","call_id":"c","status":"failed"}]}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.NativeClientTools || in.HostedSearch || in.HostedToolSearch {
			t.Fatal("client tool rejected or misclassified", raw, in, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("native client tool converted to Chat", raw)
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("native client tool converted to Messages", raw)
		}
	}
	for _, tool := range []string{
		`{"type":"shell"}`, `{"type":"shell","environment":null}`,
		`{"type":"shell","environment":{"type":"container_auto"}}`,
		`{"type":"shell","environment":{"type":"container_reference","container_id":"foreign"}}`,
		`{"type":"shell","environment":{"type":"local","file_ids":["foreign"]}}`,
		`{"type":"shell","environment":{"type":"local","skills":null}}`,
		`{"type":"shell","environment":{"type":"local","skills":[{"name":"n","path":"p"}]}}`,
		`{"type":"apply_patch","allowed_callers":["programmatic"]}`,
		`{"type":"local_shell","environment":{"type":"local"}}`,
		`{"type":"local_shell","allowed_callers":["direct"]}`,
		`{"type":"apply_patch","container_id":"foreign"}`,
	} {
		if _, _, err := parse(`{"tools":[` + tool + `]}`); err == nil {
			t.Fatal("unsupported tool admitted", tool)
		}
	}
	for _, item := range []string{
		`{"type":"shell_call","call_id":"c","environment":{"type":"container_reference","container_id":"foreign"},"action":{"commands":["ls"]}}`,
		`{"type":"shell_call","call_id":"c","caller":{"type":"program","caller_id":"p"},"action":{"commands":["ls"]}}`,
		`{"type":"shell_call","call_id":"c","action":{"commands":null}}`,
		`{"type":"shell_call","call_id":"c","action":{"commands":"ls"}}`,
		`{"type":"shell_call_output","call_id":"c","container_id":"foreign","output":[]}`,
		`{"type":"shell_call_output","call_id":"c","output":null}`,
		`{"type":"shell_call_output","call_id":"c","output":[{"stdout":"x","stderr":"","outcome":{"type":"exit"}}]}`,
		`{"type":"shell_call_output","call_id":"c","output":[{"stdout":null,"stderr":"","outcome":{"type":"timeout"}}]}`,
		`{"type":"local_shell_call","call_id":"c","action":{"type":"bad","command":["ls"]}}`,
		`{"type":"local_shell_call_output","call_id":"c","output":"ok"}`,
		`{"type":"local_shell_call_output","id":"../foreign","output":"ok"}`,
		`{"type":"local_shell_call_output","id":"c","output":null}`,
		`{"type":"apply_patch_call","call_id":"c","operation":{"type":"update_file","path":"f","diff":null}}`,
		`{"type":"apply_patch_call","call_id":"c","operation":{"type":"bad","path":"f"}}`,
		`{"type":"apply_patch_call_output","call_id":"c","status":"incomplete"}`,
		`{"type":"apply_patch_call_output","call_id":"c","status":"completed","output":{}}`,
		`{"type":"tool_search_output","call_id":"c","tools":[{"type":"shell","environment":{"type":"container_auto"}}]}`,
	} {
		if _, _, err := parse(`{"input":[` + item + `]}`); err == nil {
			t.Fatal("invalid client tool history admitted", item)
		}
	}
}
