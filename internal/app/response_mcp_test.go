package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const nativeMCPTools = `[{"type":"mcp","server_label":"team","server_url":"https://example.test/mcp","headers":{"X-Api-Key":"client-mcp-secret"},"allowed_tools":{"read_only":true,"tool_names":["read"]},"require_approval":"always","allowed_callers":["direct"],"defer_loading":true}]`
const nativeMCPCalls = `[{"type":"mcp_list_tools","id":"mcpl_team","server_label":"team","tools":[{"name":"read","input_schema":{"type":"object","maximum":9007199254740993}}]},{"type":"mcp_approval_request","id":"mcpr_team","server_label":"team","name":"read","arguments":"{}"},{"type":"mcp_call","id":"mcpc_team","server_label":"team","name":"read","arguments":"{}","output":"read result","status":"completed"}]`
const nativeMCPHistory = `[{"type":"mcp_approval_response","approval_request_id":"mcpr_team","approve":true,"reason":"Client approved"}]`

func TestResponseMCP(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"use tools"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":` + nativeMCPTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeMCPTools + `}]}`,
		`{"tools":[{"type":"mcp","server_label":"public","server_url":"https://example.test/mcp"}]}`,
		`{"input":` + nativeMCPCalls + `}`,
		`{"input":` + nativeMCPHistory + `}`,
		`{"input":[{"type":"mcp_approval_response","approval_request_id":"mcpr_team","approve":false}]}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.NativeMCP || in.NativeClientTools || in.HostedSearch {
			t.Fatal("MCP rejected or misclassified", raw, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("MCP converted to Chat")
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("MCP converted to Messages")
		}
	}
	in, _, err := parse(`{"input":` + nativeMCPHistory + `}`)
	if err != nil || len(in.ItemReferences) != 1 || in.ItemReferences[0] != "mcpr_team" {
		t.Fatal("MCP approval was not scoped", in.ItemReferences, err)
	}
	for _, patch := range []string{
		`"server_url":"http://example.test/mcp"`, `"server_url":"file:///private"`, `"server_url":"https://secret@example.test/mcp"`,
		`"server_url":"https://example.test/mcp#fragment"`, `"server_label":"../foreign"`,
		`"headers":{"Bad\nName":"value"}`, `"headers":{"Authorization":"Bearer s\r\nInjected: true"}`, `"headers":{"Key":123}`,
		`"allowed_tools":{"read_only":null}`, `"allowed_tools":{"tool_names":[""]}`, `"allowed_tools":[5]`,
		`"require_approval":"sometimes"`, `"require_approval":{"always":null}`, `"require_approval":{"always":["read"]}`,
		`"defer_loading":null`, `"authorization":"oauth"`, `"connector_id":"connector_dropbox"`, `"tunnel_id":"foreign"`,
	} {
		var tool map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"type":"mcp","server_label":"team","server_url":"https://example.test/mcp"}`), &tool)
		var patchObject map[string]json.RawMessage
		_ = json.Unmarshal([]byte("{"+patch+"}"), &patchObject)
		for key, value := range patchObject {
			tool[key] = value
		}
		raw, _ := json.Marshal([]any{tool})
		if _, _, err := parse(`{"tools":` + string(raw) + `}`); err == nil {
			t.Fatal("invalid MCP declaration admitted", patch)
		}
	}
	for _, input := range []string{
		`[{"type":"mcp_approval_response","approval_request_id":"mcpr_team"}]`,
		`[{"type":"mcp_approval_response","approval_request_id":"mcpr_team","approve":null}]`,
		`[{"type":"mcp_approval_response","approval_request_id":"../foreign","approve":true}]`,
		`[{"type":"mcp_call","server_label":"team"}]`,
		`[{"type":"tool_search_output","execution":"client","call_id":"c","tools":` + nativeMCPTools + `}]`,
	} {
		if _, _, err := parse(`{"input":` + input + `}`); err == nil {
			t.Fatal("invalid MCP history admitted", input)
		}
	}
	for _, policy := range []string{`null`, `"always"`, `"never"`, `{"always":{"tool_names":["write"]},"never":{"read_only":true}}`} {
		var tool map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"type":"mcp","server_label":"team","server_url":"https://example.test/mcp","require_approval":`+policy+`}`), &tool)
		if err := validateResponseMCPTool(tool); err != nil {
			t.Fatal("valid approval policy rejected", policy, err)
		}
	}
}

func TestResponseMCPCredentialRedaction(t *testing.T) {
	tool := `{"type":"mcp","server_label":"team","headers":{"X-Key":"private-header"},"authorization":"private-auth"}`
	for _, raw := range []string{
		`{"tools":[` + tool + `],"output":` + nativeMCPCalls + `}`, // JSON / background result
		`{"type":"response.completed","response":{"tools":[` + tool + `]}}`,
		`{"type":"response.output_item.added","item":{"type":"additional_tools","tools":[` + tool + `]}}`,
		`{"data":[{"type":"additional_tools","tools":[` + tool + `]}]}`, // input_items
		`{"tools":[{"type":"mcp","he\u0061ders":{"X-Key":"private-header"}}]}`,
	} {
		clean, err := sanitizeResponseTools([]byte(raw))
		if err != nil || !json.Valid(clean) || strings.Contains(string(clean), "private-") {
			t.Fatal("MCP credentials exposed", string(clean), err)
		}
		if strings.Contains(raw, "9007199254740993") && !strings.Contains(string(clean), "9007199254740993") {
			t.Fatal("MCP schema number rounded")
		}
	}
	for _, raw := range []string{nativeMCPCalls, `{"output":[{"type":"function_call","arguments":"{\"headers\":{\"X-Context\":\"value\"}}"}]}`} {
		clean, err := sanitizeResponseTools([]byte(raw))
		if err != nil || string(clean) != raw {
			t.Fatal("unrelated tool output changed", err)
		}
	}
}

func TestResponseContainerSecretRedaction(t *testing.T) {
	for _, tools := range []string{nativeCodeTools, nativeHostedShellTools} {
		for _, raw := range []string{
			`{"tools":` + tools + `,"output":` + nativeCodeCalls + `}`,
			`{"type":"response.completed","response":{"tools":` + tools + `}}`,
			`{"type":"response.output_item.added","item":{"type":"additional_tools","tools":` + tools + `}}`,
			`{"data":[{"type":"additional_tools","tools":` + tools + `}]}`,
			strings.ReplaceAll(`{"tools":`+tools+`}`, "domain_secrets", `domain_\u0073ecrets`),
		} {
			clean, err := sanitizeResponseTools([]byte(raw))
			if err != nil || !json.Valid(clean) || strings.Contains(string(clean), "client-domain-secret") || strings.Contains(string(clean), "domain_secrets") || !strings.Contains(string(clean), `"allowed_domains":["example.test"]`) {
				t.Fatal("container secret exposed or network policy lost", string(clean), err)
			}
		}
	}
	raw := `{"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{"domain_secrets":{"type":"string"}},"maximum":9007199254740993}}],"output":[{"type":"function_call","arguments":"{\"domain_secrets\":\"user data\"}"}]}`
	if clean, err := sanitizeResponseTools([]byte(raw)); err != nil || string(clean) != raw {
		t.Fatal("unrelated schema or function argument changed", string(clean), err)
	}
}
