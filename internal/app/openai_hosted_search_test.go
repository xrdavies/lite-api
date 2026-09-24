package app

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIHostedSearch(t *testing.T) {
	decode := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		return object
	}
	for _, kind := range []string{"web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11"} {
		for _, inputTools := range []bool{false, true} {
			body := decode(`{"model":"team-search","input":"search"}`)
			tool := `{"type":"` + kind + `","search_context_size":"low","user_location":{"type":"approximate","country":"GB","city":"London","region":null,"timezone":"Europe/London"}}`
			body["tools"] = json.RawMessage("[" + tool + "]")
			if inputTools {
				delete(body, "tools")
				body["input"] = json.RawMessage(`[{"type":"additional_tools","tools":[` + tool + `]},{"role":"user","content":"search"}]`)
			}
			in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
			if err != nil || !in.HostedSearch || validateHostedSearchTools(body, "openai") != nil {
				t.Fatal("valid hosted search", kind, inputTools, err)
			}
			if validateHostedSearchTools(body, "grok") == nil || validateHostedSearchTools(body, "anthropic") == nil {
				t.Fatal("OpenAI options accepted on another platform")
			}
			if _, err := responsesToChatRequest(body, nil); err == nil {
				t.Fatal("hosted tool silently converted to Chat")
			}
		}
	}
	for _, raw := range []string{
		`{"type":"web_search","filters":{"allowed_domains":["example.test"],"blocked_domains":["sub.example.test"]},"external_web_access":false,"return_token_budget":"unlimited"}`,
		`{"type":"web_search","filters":null,"user_location":null}`,
		`{"type":"web_search","filters":{"allowed_domains":null},"user_location":{}}`,
	} {
		if err := validateOpenAIHostedSearch(decode(raw)); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{
		`{"type":"x_search"}`, `{"type":"web_search","allowed_domains":["a"]}`,
		`{"type":"web_search","filters":[]}`, `{"type":"web_search","filters":{"unknown":[]}}`,
		`{"type":"web_search","filters":{"allowed_domains":["https://example.test"]}}`,
		`{"type":"web_search","filters":{"allowed_domains":["a..test"]}}`,
		`{"type":"web_search","filters":{"allowed_domains":["a/b"]}}`,
		`{"type":"web_search","filters":{"allowed_domains":["-a.test"]}}`,
		`{"type":"web_search","search_context_size":null}`, `{"type":"web_search","search_context_size":"huge"}`,
		`{"type":"web_search","external_web_access":"false"}`, `{"type":"web_search","return_token_budget":null}`,
		`{"type":"web_search","return_token_budget":100}`, `{"type":"web_search","return_token_budget":"other"}`,
		`{"type":"web_search_preview","filters":{}}`, `{"type":"web_search_preview","return_token_budget":"unlimited"}`,
		`{"type":"web_search","user_location":{"type":"precise"}}`,
		`{"type":"web_search","user_location":{"country":"GBR"}}`,
		`{"type":"web_search","user_location":{"city":2}}`,
		`{"type":"web_search","user_location":{"timezone":"not/a/timezone"}}`,
		`{"type":"web_search","user_location":{"coordinates":[]}}`, `{"type":"web_search","api_key":"secret"}`,
	} {
		if validateOpenAIHostedSearch(decode(raw)) == nil {
			t.Fatal("invalid hosted search", raw)
		}
	}
	domains := strings.Repeat(`"example.test",`, 100) + `"example.test"`
	if validateOpenAIHostedSearch(decode(`{"type":"web_search","filters":{"allowed_domains":[`+domains+`]}}`)) == nil {
		t.Fatal("unbounded domain filter")
	}
	meter := hostedSearchMeter{OpenAI: true}
	for _, raw := range []string{
		`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"ws_one","status":"completed"}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"web_search_call","id":"ws_one","status":"completed"},{"type":"web_search_call","id":"ws_two","status":"completed"}],"usage":{"server_side_tool_usage_details":{"web_search_calls":9000}}}}`,
	} {
		if err := meter.observe([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if meter.count() != 2 {
		t.Fatal("duplicated calls or Grok usage applied to OpenAI", meter.count())
	}
	if meter.observe([]byte(`{"status":"completed","output":[{"type":"x_search_call"}]}`)) == nil {
		t.Fatal("OpenAI accepted unexpected X usage")
	}
}
