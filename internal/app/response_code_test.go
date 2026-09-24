package app

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const nativeCodeTools = `[{"type":"code_interpreter","container":{"type":"auto","memory_limit":"4g","network_policy":{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"API_KEY","value":"client-domain-secret"}]}}}]`
const nativeCodeCalls = `[{"type":"code_interpreter_call","id":"ci_team","container_id":"cntr_team","status":"completed","code":"print(2+2)","outputs":[{"type":"logs","logs":"4"}]}]`
const nativeHostedShellTools = `[{"type":"shell","environment":{"type":"container_auto","memory_limit":"4g","network_policy":{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"API_KEY","value":"client-domain-secret"}]}}}]`
const nativeHostedShellCalls = `[{"type":"shell_call","id":"sc_team","call_id":"call_shell","environment":{"type":"container_reference","container_id":"cntr_team"},"action":{"commands":["printf 4"],"timeout_ms":1000,"max_output_length":2000},"status":"completed"},{"type":"shell_call_output","id":"sco_team","call_id":"call_shell","output":[{"stdout":"4","stderr":"","outcome":{"type":"exit","exit_code":0}}],"status":"completed"}]`
const inlineSkillBundle = "UEsDBBQAAAAAAAAAIVy2UAotTQAAAE0AAAAIAAAAU0tJTEwubWQtLS0KbmFtZTogdGVhbQpkZXNjcmlwdGlvbjogVGVhbSBjYWxjdWxhdGlvbi4KLS0tCkNvbXB1dGUgdGhlIHJlcXVlc3RlZCBzdW0uClBLAQIUAxQAAAAAAAAAIVy2UAotTQAAAE0AAAAIAAAAAAAAAAAAAACAAQAAAABTS0lMTC5tZFBLBQYAAAAAAQABADYAAABzAAAAAAA="
const nativeHostedSkills = `[{"type":"skill_reference","skill_id":"skill_team","version":"2"},{"type":"inline","name":"team","description":"Team calculation.","source":{"type":"base64","media_type":"application/zip","data":"` + inlineSkillBundle + `"}}]`

func TestResponseHostedSkills(t *testing.T) {
	for _, raw := range []string{nativeHostedSkills, `[]`, `[{"type":"skill_reference","skill_id":"openai-spreadsheets","version":"latest"}]`, `[{"type":"skill_reference","skill_id":"skill_team"}]`} {
		if _, err := responseSkills(json.RawMessage(raw)); err != nil {
			t.Fatal("valid hosted skill rejected", err)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `[null]`,
		`[{"type":"skill_reference","skill_id":"../foreign"}]`,
		`[{"type":"skill_reference","skill_id":"skill_team","version":0}]`,
		`[{"type":"skill_reference","skill_id":"skill_team","version":"0"}]`,
		`[{"type":"skill_reference","skill_id":"skill_team","version":"01"}]`,
		`[{"type":"skill_reference","skill_id":"skill_team","version":null}]`,
		`[{"type":"skill_reference","skill_id":"skill_team","path":"/tmp"}]`,
		strings.Replace(nativeHostedSkills, `"description":"Team calculation."`, `"description":null`, 1),
		strings.Replace(nativeHostedSkills, "application/zip", "text/plain", 1),
		strings.Replace(nativeHostedSkills, inlineSkillBundle, "%%%", 1),
		strings.Replace(nativeHostedSkills, inlineSkillBundle, "", 1),
		strings.Replace(nativeHostedSkills, `"source":`, `"unknown":true,"source":`, 1),
	} {
		if _, err := responseSkills(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid hosted skill admitted", raw)
		}
	}
	declaration := `[{"type":"shell","environment":{"type":"container_auto","skills":` + nativeHostedSkills + `}}]`
	for _, raw := range []string{`{"tools":` + declaration + `}`, `{"input":[{"type":"additional_tools","tools":` + declaration + `}]}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"calculate"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		if err != nil || !in.NativeCode || !slices.Equal(in.SkillIDs, []string{"skill_team"}) {
			t.Fatal("hosted skill admission or scope", in, err)
		}
	}
	if err := validateResponseAutoContainer(map[string]json.RawMessage{"type": json.RawMessage(`"auto"`), "skills": json.RawMessage(nativeHostedSkills)}); err == nil {
		t.Fatal("Code Interpreter accepted Shell-only skills")
	}
	u := &upstreamAccount{Platform: "openai", Credentials: map[string]json.RawMessage{"api_key": json.RawMessage(`"source"`), "api_protocol": json.RawMessage(`"responses"`)}}
	in := accountInput{Extra: map[string]json.RawMessage{responseSkillsKey: json.RawMessage(`{"1":["skill_team"]}`)}}
	if err := in.bindResponseResourceGrants(u); err != nil {
		t.Fatal(err)
	}
	u.Extra = in.Extra
	if !u.allowsResponseResources(responseSkillsKey, 1, []string{"skill_team"}) || u.allowsResponseResources(responseSkillsKey, 2, []string{"skill_team"}) || u.allowsResponseResources(responseFilesKey, 1, []string{"skill_team"}) {
		t.Fatal("skill authorization escaped its group or kind")
	}
	u.Credentials["api_key"] = json.RawMessage(`"rotated"`)
	if u.allowsResponseResources(responseSkillsKey, 1, []string{"skill_team"}) {
		t.Fatal("skill grant followed credential rotation")
	}
}

func TestContainerNetworkPolicy(t *testing.T) {
	for _, raw := range []string{
		`{"type":"disabled"}`,
		`{"type":"allowlist","allowed_domains":[]}`,
		`{"type":"allowlist","allowed_domains":["example.test","cdn.example.test"],"domain_secrets":[{"domain":"EXAMPLE.TEST","name":"API_KEY","value":"private-key"}]}`,
	} {
		if err := validateContainerNetwork(json.RawMessage(raw)); err != nil {
			t.Fatal("valid network policy rejected", err)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"type":"disabled","allowed_domains":[]}`,
		`{"type":"allowlist","allowed_domains":null}`, `{"type":"allowlist","allowed_domains":["example.test","EXAMPLE.TEST"]}`,
		`{"type":"allowlist","allowed_domains":["https://example.test"]}`, `{"type":"allowlist","allowed_domains":["*.example.test"]}`,
		`{"type":"allowlist","allowed_domains":["-bad.test"]}`, `{"type":"allowlist","allowed_domains":["a..test"]}`,
		`{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":null}`,
		`{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"other.test","name":"API_KEY","value":"secret"}]}`,
		`{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"API_KEY","value":""}]}`,
		`{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"API_KEY","value":"s\r\nHeader: injected"}]}`,
		`{"type":"allowlist","allowed_domains":["example.test"],"domain_secrets":[{"domain":"example.test","name":"API_KEY","value":"secret"},{"domain":"example.test","name":"API_KEY","value":"other"}]}`,
	} {
		if err := validateContainerNetwork(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid network policy admitted", raw)
		}
	}
}

func TestResponseHostedShell(t *testing.T) {
	for _, raw := range []string{
		`{"tools":` + nativeHostedShellTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeHostedShellTools + `}]}`,
		`{"tools":[{"type":"shell","environment":{"type":"container_reference","container_id":"cntr_team"}}]}`,
		`{"input":` + nativeHostedShellCalls + `}`,
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"calculate"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		if err != nil || !in.NativeCode || in.HostedSearch {
			t.Fatal("hosted shell admission", in, err)
		}
		if len(in.ContainerReferences) > 0 && (validateResponseContainers(in, nil) == nil || validateResponseContainers(in, &responseBinding{Containers: []string{"cntr_team"}}) != nil) {
			t.Fatal("shell container ownership bypass")
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("hosted shell converted to Chat")
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("hosted shell converted to Messages")
		}
	}
	observation := textObservation{Protocol: "responses"}
	if err := observation.observe([]byte(`{"id":"resp_shell","object":"response","status":"completed","output":` + nativeHostedShellCalls + `,` + responseUsage + `}`)); err != nil || !slices.Equal(observation.ResponseContainers, []string{"cntr_team"}) {
		t.Fatal("hosted shell container lost", observation, err)
	}
	for _, tool := range []string{
		`{"type":"shell","environment":{"type":"container_auto"},"allowed_callers":["programmatic"]}`,
		`{"type":"shell","environment":{"type":"container_auto","file_ids":[42]}}`,
		`{"type":"shell","environment":{"type":"container_auto","network_policy":{"type":"allowlist","allowed_domains":["https://example.test"]}}}`,
		`{"type":"shell","environment":{"type":"container_reference","container_id":"cntr_team","file_ids":["foreign"]}}`,
	} {
		var value map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tool), &value)
		if _, _, err := responseShellTool(value); err == nil {
			t.Fatal("invalid hosted shell option admitted", tool)
		}
	}
	for _, id := range []string{"", "../foreign"} {
		item := map[string]json.RawMessage{
			"type": json.RawMessage(`"shell_call"`), "environment": json.RawMessage(`{"type":"container_reference","container_id":"cntr_team"}`),
			"call_id": json.RawMessage(`"call_shell"`), "action": json.RawMessage(`{"commands":["ls"]}`),
		}
		item["id"], _ = json.Marshal(id)
		if _, _, err := responseShellItem(item); err == nil {
			t.Fatal("hosted shell history without owned item reference admitted")
		}
		if _, err := responseContainerIDs([]map[string]json.RawMessage{item}); err == nil {
			t.Fatal("invalid hosted shell output authorized a container")
		}
	}
}

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
		`{"type":"code_interpreter","container":{"type":"auto","network_policy":{"type":"allowlist","allowed_domains":["https://example.test"]}}}`,
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
