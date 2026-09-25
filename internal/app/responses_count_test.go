package app

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponsesLocalCount(t *testing.T) {
	in := textRequest{Protocol: "responses", CountOnly: true, Action: "/input_tokens"}
	account := &upstreamAccount{Platform: "openai", Type: "apikey", Credentials: map[string]json.RawMessage{}}
	for _, tc := range []struct {
		base  string
		local bool
	}{
		{"", false}, {"https://api.openai.com/v1", false}, {"https://API.OPENAI.COM:443", false},
		{"https://api.openai.com.relay.test", true}, {"https://relay.test/v1", true},
	} {
		account.Credentials["base_url"], _ = json.Marshal(tc.base)
		for _, status := range []int{0, 404, 401, 403, 429, 500} {
			resp, err := localResponsesCount(in, account, []byte(`{"model":"gpt-5","input":"hello world"}`), status)
			want := status == 404 || status == 0 && tc.local
			if err != nil || (resp != nil) != want {
				t.Fatal(tc, status, resp, err)
			}
			if resp != nil {
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(raw) != `{"input_tokens":2,"object":"response.input_tokens"}` {
					t.Fatal(string(raw))
				}
			}
		}
	}
	for _, raw := range []string{
		`{"input":[{"type":"item_reference","id":"msg_one"}]}`,
		`{"input":[{"id":"msg_one"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"prompt":{"id":"pmpt_team"},"input":"hello"}`,
	} {
		for _, status := range []int{0, 404} {
			if resp, err := localResponsesCount(in, account, []byte(raw), status); resp != nil || err != nil {
				t.Fatal("invented remote context count", raw, status, err)
			}
		}
	}
	continued := in
	continued.Previous = "resp_team"
	if resp, err := localResponsesCount(continued, account, []byte(`{"input":"hello"}`), 404); resp != nil || err != nil {
		t.Fatal("lost previous response", err)
	}
	for _, protocol := range []string{"anthropic", "gemini"} {
		other := in
		other.Protocol = protocol
		if resp, err := localResponsesCount(other, account, []byte(`{"input":"hello"}`), 404); resp != nil || err != nil {
			t.Fatal("changed another count endpoint", protocol, err)
		}
	}
	generation := in
	generation.CountOnly = false
	if resp, err := localResponsesCount(generation, account, []byte(`{"input":"hello"}`), 404); resp != nil || err != nil {
		t.Fatal("generation turned into a token estimate", err)
	}
	for _, raw := range []string{`{"instructions":5}`, `{"input":3}`, `{"input":[null]}`} {
		if _, err := localResponsesCount(in, account, []byte(raw), 404); err == nil {
			t.Fatal("invalid estimate accepted", raw)
		}
	}
	// Count hosted declarations and complete tool histories without running tools.
	for _, raw := range []string{
		`{"model":"gpt-5","instructions":"hello world"}`,
		`{"model":"gpt-5","input":[],"tools":` + nativeCodeTools + `}`,
		`{"model":"gpt-5","input":` + nativeCodeCalls + `}`,
		`{"model":"gpt-5","input":` + nativeHostedShellCalls + `}`,
		`{"model":"gpt-5","input":[],"tools":[{"type":"web_search"}]}`,
		`{"model":"gpt-5","input":[],"tools":[{"type":"file_search","vector_store_ids":["vs_team"]}]}`,
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		req := httptest.NewRequest("POST", "/responses/input_tokens", nil)
		req.SetPathValue("action", "input_tokens")
		parsed, err := parseTextRequest(req, "responses", body)
		if err != nil {
			t.Fatal("count parser rejected tools", raw, err)
		}
		resp, err := localResponsesCount(parsed, account, []byte(raw), 0)
		if err != nil || resp == nil {
			t.Fatal("no local count", raw, err)
		}
		resp.Body.Close()
		req.SetPathValue("action", "compact")
		if body["tools"] != nil {
			if _, err := parseTextRequest(req, "responses", body); err == nil {
				t.Fatal("compaction admitted hosted tools")
			}
		}
	}
	raw := []byte(`{"model":"gpt-5","input":[],"tools":` + nativeCodeTools + `}`)
	first, _ := localResponsesCount(in, account, raw, 0)
	second, _ := localResponsesCount(in, account, []byte(strings.ReplaceAll(string(raw), "client-domain-secret", strings.Repeat("private-secret", 100))), 0)
	if first == nil || second == nil {
		t.Fatal("no secret count")
	}
	defer first.Body.Close()
	defer second.Body.Close()
	one, _ := io.ReadAll(first.Body)
	two, _ := io.ReadAll(second.Body)
	if string(one) != string(two) {
		t.Fatal("transport secrets affect count", string(one), string(two))
	}
	for _, field := range []string{"input", "code", "result", "summary", "action", "tools", "outputs"} {
		before := `{"model":"gpt-5","input":[{"type":"custom_tool_call","` + field + `":""}]}`
		after := strings.Replace(before, `"`+field+`":""`, `"`+field+`":"`+strings.Repeat("hello world ", 20)+`"`, 1)
		small, e1 := estimateInputTokens([]byte(before))
		large, e2 := estimateInputTokens([]byte(after))
		if e1 != nil || e2 != nil || large <= small {
			t.Fatal("history payload not counted", field, small, large, e1, e2)
		}
	}
	for _, kind := range []string{"input_image", "computer_screenshot"} {
		counts := []int{}
		for _, payload := range []string{"AAAA", strings.Repeat("AAAA", 10000)} {
			raw := mustJSON(map[string]any{"input": []any{map[string]any{"type": "computer_call_output", "call_id": "call_one", "output": map[string]any{"type": kind, "image_url": "data:image/png;base64," + payload}}}})
			n, err := estimateInputTokens(raw)
			if err != nil {
				t.Fatal(err)
			}
			counts = append(counts, n)
		}
		if counts[0] != counts[1] {
			t.Fatal("screenshot bytes counted as text", kind, counts)
		}
	}
	for _, platform := range []string{"grok", "kimi", "zhipu", "deepseek", "minimax"} {
		account.Platform = platform
		account.Credentials = nil
		if resp, err := localResponsesCount(in, account, []byte(`{"input":"hello"}`), 0); resp == nil || err != nil {
			t.Fatal(platform, err)
		} else {
			resp.Body.Close()
		}
	}
}
