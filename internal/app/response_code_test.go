package app

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
)

const nativeCodeTools = `[{"type":"code_interpreter","container":{"type":"auto","memory_limit":"4g","network_policy":{"type":"disabled"}}}]`
const nativeCodeCalls = `[{"type":"code_interpreter_call","id":"ci_team","container_id":"cntr_team","status":"completed","code":"print(2+2)","outputs":[{"type":"logs","logs":"4"}]}]`

func TestResponseCode(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"calculate"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":` + nativeCodeTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeCodeTools + `}]}`,
		`{"input":` + nativeCodeCalls + `}`,
		`{"tools":[{"type":"code_interpreter","container":"cntr_team"}]}`,
		`{"tools":[{"type":"code_interpreter","container":{"type":"auto","file_ids":[],"memory_limit":null}}]}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.NativeCode || in.NativeClientTools || in.HostedSearch {
			t.Fatal("code interpreter rejected or misclassified", raw, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("code interpreter converted to Chat")
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("code interpreter converted to Messages")
		}
	}
	for _, tool := range []string{
		`{"type":"code_interpreter"}`, `{"type":"code_interpreter","container":null}`,
		`{"type":"code_interpreter","container":"../foreign"}`,
		`{"type":"code_interpreter","container":{"type":"auto","file_ids":["../foreign"]}}`,
		`{"type":"code_interpreter","container":{"type":"auto","memory_limit":"2g"}}`,
		`{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["example.test"]}}}`,
		`{"type":"code_interpreter","container":{"type":"auto"},"allowed_callers":["programmatic"]}`,
	} {
		if _, _, err := parse(`{"tools":[` + tool + `]}`); err == nil {
			t.Fatal("invalid code interpreter declaration accepted", tool)
		}
	}
	for _, raw := range []string{
		`{"input":[{"type":"tool_search_output","call_id":"c","tools":` + nativeCodeTools + `}]}`,
		`{"input":[{"type":"code_interpreter_call","id":"c","status":"completed"}]}`,
		`{"input":[{"type":"code_interpreter_call","id":"c","container_id":"cntr","status":"invalid"}]}`,
		`{"input":[{"type":"code_interpreter_call","id":"c","container_id":"cntr","status":"completed","outputs":[{"type":"logs","logs":null}]}]}`,
		`{"input":[{"type":"code_interpreter_call","id":"c","container_id":"cntr","status":"completed","file_ids":["foreign"]}]}`,
		`{"tools":` + nativeCodeTools + `,"input":[{"role":"user","content":[{"type":"input_file","file_id":"../foreign"}]}]}`,
		`{"tools":` + nativeCodeTools + `,"input":[{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"../foreign"}]}]}`,
	} {
		if _, _, err := parse(raw); err == nil {
			t.Fatal("invalid code interpreter history accepted", raw)
		}
	}
	in, _, err := parse(`{"input":` + nativeCodeCalls + `}`)
	if err != nil || !slices.Equal(in.ItemReferences, []string{"ci_team"}) || !slices.Equal(in.ContainerReferences, []string{"cntr_team"}) {
		t.Fatal("code history lost ownership references", in, err)
	}
	if validateResponseContainers(in, nil) == nil || validateResponseContainers(in, &responseBinding{Containers: []string{"foreign"}}) == nil || validateResponseContainers(in, &responseBinding{Containers: []string{"cntr_team"}}) != nil {
		t.Fatal("code container ownership gate")
	}
	for _, action := range []string{"/compact", "/input_tokens"} {
		if validateResponseContainers(textRequest{NativeCode: true, Action: action}, nil) == nil {
			t.Fatal("implicit code history bypassed action restrictions")
		}
	}
	ids, err := mergeResponseContainers([]string{"cntr_b", "cntr_a"}, []string{"cntr_b", "cntr_c"})
	if err != nil || !slices.Equal(ids, []string{"cntr_a", "cntr_b", "cntr_c"}) {
		t.Fatal("unstable container context", ids, err)
	}
	observation := textObservation{Protocol: "responses"}
	if err := observation.observe([]byte(`{"id":"resp_code","object":"response","status":"completed","output":` + nativeCodeCalls + `,` + responseUsage + `}`)); err != nil || !slices.Equal(observation.ResponseContainers, []string{"cntr_team"}) {
		t.Fatal("output container observation", observation, err)
	}
}
