package app

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const nativeProgramTools = `[{"type":"programmatic_tool_calling"},{"type":"function","name":"lookup","parameters":{"type":"object"},"allowed_callers":["direct","programmatic"]}]`
const nativeProgramCalls = `[{"type":"program","id":"prog_team","call_id":"pc_team","code":"return await tools.lookup({n:9007199254740993n});","fingerprint":"opaque-program-replay"},{"type":"function_call","id":"fc_program","call_id":"call_lookup","name":"lookup","arguments":"{\"n\":9007199254740993}","caller":{"type":"program","caller_id":"pc_team"}},{"type":"program_output","id":"po_team","call_id":"pc_team","result":"9007199254740993","status":"completed"}]`
const nativeProgramHistory = `[{"type":"function_call_output","call_id":"call_lookup","output":"9007199254740993","caller":{"type":"program","caller_id":"pc_team"}}]`

func TestResponseProgrammaticTool(t *testing.T) {
	parse := func(raw string) (textRequest, error) {
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return textRequest{}, err
		}
		body["model"] = json.RawMessage(`"gpt-test"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"run"`)
		}
		return parseTextRequest(httptest.NewRequest("POST", "/v1/responses", nil), "responses", body)
	}
	for _, raw := range []string{
		`{"tools":` + nativeProgramTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeProgramTools + `}]}`,
		`{"input":` + nativeProgramCalls + `}`,
		`{"input":` + nativeProgramHistory + `}`,
		`{"tools":[{"type":"function","name":"lookup","allowed_callers":["programmatic"]}]}`,
		`{"tools":[{"type":"namespace","name":"team","tools":[{"type":"function","name":"lookup","allowed_callers":["programmatic"]}]}]}`,
		`{"input":[{"type":"shell_call","call_id":"c","caller":{"type":"program","caller_id":"p"},"action":{"commands":["ls"]}}]}`,
	} {
		in, err := parse(raw)
		if err != nil || !in.NativeProgrammatic {
			t.Fatalf("valid programmatic request rejected: %v, %+v", err, in)
		}
		if strings.Contains(raw, `"caller_id":"pc_team"`) && !slices.Contains(in.ItemReferences, programCallReference("pc_team")) {
			t.Fatal("caller lacks scoped ownership check", in)
		}
		if strings.Contains(raw, `"type":"program"`) && strings.Contains(raw, `"fingerprint"`) && !slices.Contains(in.ItemReferences, "prog_team") {
			t.Fatal("program history lacks scoped ownership check", in)
		}
	}
	for _, raw := range []string{
		`{"tools":[{"type":"programmatic_tool_calling","extra":true}]}`,
		`{"input":[{"type":"program_output","id":"out_1","call_id":"call_1","result":1,"status":"completed"}]}`,
		`{"input":[{"type":"program","id":"prog_1","call_id":"call_1","code":null,"fingerprint":"fp"}]}`,
		`{"input":[{"type":"program","id":"prog_1","call_id":"call_1","code":"run"}]}`,
		`{"input":[{"type":"program_output","id":"out_1","call_id":"call_1","result":"1","status":"running"}]}`,
		`{"input":[{"type":"function_call","call_id":"c","caller":{"type":"program","caller_id":"../foreign"}}]}`,
		`{"tools":[{"type":"function","name":"lookup","allowed_callers":["program"]}]}`,
		`{"tools":[{"type":"function","name":"lookup","allowed_callers":["direct","direct"]}]}`,
		`{"tools":[{"type":"namespace","name":"team","tools":[{"type":"function","name":"lookup","allowed_callers":["invalid"]}]}]}`,
		`{"tools":[{"type":"local_shell","allowed_callers":["programmatic"]}]}`,
		`{"tools":[{"type":"computer","allowed_callers":["programmatic"]}]}`,
	} {
		if _, err := parse(raw); err == nil {
			t.Fatalf("invalid programmatic request admitted: %s", raw)
		}
	}
	for _, tool := range []string{
		`{"type":"custom","name":"lookup","allowed_callers":["programmatic"]}`,
		`{"type":"apply_patch","allowed_callers":["programmatic"]}`,
		`{"type":"shell","environment":{"type":"container_auto"},"allowed_callers":["programmatic"]}`,
		`{"type":"code_interpreter","container":{"type":"auto"},"allowed_callers":["programmatic"]}`,
		`{"type":"mcp","server_label":"team","server_url":"https://example.test/mcp","allowed_callers":["programmatic"]}`,
	} {
		in, err := parse(`{"tools":[` + tool + `]}`)
		if err != nil || !in.NativeProgrammatic {
			t.Fatal("programmatic tool declaration lost", tool, in, err)
		}
	}
	for _, raw := range []string{nativeProgramCalls, nativeProgramHistory} {
		out := []byte(`{"id":"resp_1","object":"response","status":"completed","output":` + raw + `,"usage":{"input_tokens":1,"output_tokens":1}}`)
		o := &textObservation{Protocol: "responses", Programmatic: true}
		if err := o.observe(out); err != nil {
			t.Fatalf("valid programmatic output rejected: %v", err)
		}
		if raw == nativeProgramCalls && !slices.Contains(o.ResponseItems, programCallReference("pc_team")) {
			t.Fatal("program caller ownership not recorded")
		}
		if err := (&textObservation{Protocol: "responses"}).observe(out); err == nil {
			t.Fatal("unsolicited program execution accepted")
		}
	}
}
