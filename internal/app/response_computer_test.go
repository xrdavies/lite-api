package app

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

const nativeComputerPreview = `[{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"browser"}]`
const nativeComputerCalls = `[{"type":"computer_call","id":"cu_batched","call_id":"call_computer","actions":[{"type":"click","button":"left","x":120,"y":45},{"type":"type","text":"client input"}],"pending_safety_checks":[{"id":"check_1","code":"sensitive_domain","message":"Client must confirm"}],"status":"completed"}]`
const nativeComputerLegacyCall = `[{"type":"computer_call","id":"cu_single","call_id":"call_computer","action":{"type":"screenshot"},"pending_safety_checks":[],"status":"completed"}]`
const nativeComputerHistory = `[{"type":"computer_call_output","call_id":"call_computer","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"original"},"acknowledged_safety_checks":[{"id":"check_1","code":"sensitive_domain","message":"Client must confirm"}]}]`

func TestResponseComputer(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"use computer"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":[{"type":"computer"}],"tool_choice":{"type":"computer"}}`,
		`{"tools":` + nativeComputerPreview + `}`,
		`{"input":[{"type":"additional_tools","tools":[{"type":"computer"}]}]}`,
		`{"input":` + nativeComputerCalls + `}`,
		`{"input":` + nativeComputerLegacyCall + `}`,
		`{"input":` + nativeComputerHistory + `}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"https://example.test/screen.png","file_id":null}}]}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.NativeClientTools || in.ResponseImage != nil || in.HostedSearch {
			t.Fatal("computer request rejected or misclassified", raw, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("computer request converted to Chat", raw)
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("computer request converted to Messages", raw)
		}
	}
	for _, action := range []string{
		`{"type":"double_click","x":1.5,"y":2,"keys":null}`, `{"type":"move","x":-1,"y":2,"keys":["SHIFT"]}`,
		`{"type":"scroll","x":1,"y":2,"scroll_x":0,"scroll_y":-100}`, `{"type":"drag","path":[{"x":1,"y":2},{"x":3,"y":4}]}`,
		`{"type":"keypress","keys":["CTRL","L"]}`, `{"type":"type","text":""}`, `{"type":"wait"}`, `{"type":"screenshot"}`,
	} {
		if _, _, err := parse(`{"input":[{"type":"computer_call","call_id":"c","action":` + action + `}]}`); err != nil {
			t.Fatal("valid computer action rejected", action, err)
		}
	}
	for _, raw := range []string{
		`{"tools":[{"type":"computer","container_id":"foreign"}]}`,
		`{"tools":[{"type":"computer_use_preview"}]}`,
		`{"tools":[{"type":"computer_use_preview","display_width":0,"display_height":768,"environment":"browser"}]}`,
		`{"tools":[{"type":"computer_use_preview","display_width":1024,"display_height":null,"environment":"browser"}]}`,
		`{"tools":[{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"container"}]}`,
		`{"input":[{"type":"tool_search_output","call_id":"c","tools":[{"type":"computer"}]}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","actions":[]}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","actions":[{"type":"wait"}],"action":{"type":"wait"}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"exec"}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"move","x":"1","y":2}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"drag","path":[{"x":1}]}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"keypress","keys":null}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"type","text":null}}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","file_id":"../foreign"}}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"file:///client.png"}}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,%%%"}}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"https://secret@example.test/x"}}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","output":null}]}`,
		`{"input":[{"type":"computer_call_output","call_id":"c","container_id":"foreign","output":{"type":"computer_screenshot","image_url":"https://example.test/x"}}]}`,
		`{"input":[{"type":"computer_call","call_id":"c","action":{"type":"wait"},"pending_safety_checks":[{"code":"sensitive_domain"}]}]}`,
	} {
		if _, _, err := parse(raw); err == nil {
			t.Fatal("invalid computer request admitted", raw)
		}
	}
}
