package app

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const nativeFileTools = `[{"type":"file_search","vector_store_ids":["vs_team"],"max_num_results":5,"filters":{"type":"and","filters":[{"type":"eq","key":"category","value":"team"},{"type":"in","key":"revision","value":[9007199254740993,2]}]},"ranking_options":{"ranker":"auto","score_threshold":0.2,"hybrid_search":{"embedding_weight":0.7,"text_weight":0.3}}}]`
const nativeFileCalls = `[{"type":"file_search_call","id":"fs_team","status":"completed","queries":["team docs"],"results":[{"file_id":"file_team","filename":"team.txt","score":0.9,"text":"A team result","attributes":{"revision":9007199254740993}}]}]`

func TestResponseFileSearch(t *testing.T) {
	parse := func(raw string) (textRequest, map[string]json.RawMessage, error) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		body["model"] = json.RawMessage(`"model"`)
		if body["input"] == nil {
			body["input"] = json.RawMessage(`"find team docs"`)
		}
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		return in, body, err
	}
	for _, raw := range []string{
		`{"tools":` + nativeFileTools + `}`,
		`{"input":[{"type":"additional_tools","tools":` + nativeFileTools + `}]}`,
		`{"input":` + nativeFileCalls + `}`,
		`{"tools":[{"type":"file_search","vector_store_ids":["vs_team"],"filters":null}]}`,
	} {
		in, body, err := parse(raw)
		if err != nil || !in.NativeFileSearch || in.HostedSearch || in.NativeCode {
			t.Fatal("file search admission", raw, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("file search converted to Chat")
		}
		if _, _, err := responsesToAnthropicRequest(body, nil); err == nil {
			t.Fatal("file search converted to Messages")
		}
	}
	for _, patch := range []string{
		`"vector_store_ids":null`, `"vector_store_ids":[]`, `"vector_store_ids":["../other"]`,
		`"max_num_results":0`, `"max_num_results":51`, `"max_num_results":2.5`,
		`"filters":{}`, `"filters":{"type":"or","filters":[]}`, `"filters":{"type":"eq","key":"k","value":null}`,
		`"filters":{"type":"in","key":"k","value":[true]}`, `"filters":{"type":"in","key":"k","value":"x"}`,
		`"filters":{"type":"eq","key":"k","value":"x","vector_store_ids":["foreign"]}`,
		`"ranking_options":{"ranker":"unknown"}`, `"ranking_options":{"score_threshold":1.1}`, `"ranking_options":{"score_threshold":null}`,
		`"ranking_options":{"hybrid_search":{"embedding_weight":0,"text_weight":0}}`, `"ranking_options":{"hybrid_search":{"embedding_weight":-1,"text_weight":2}}`,
		`"allowed_callers":["programmatic"]`, `"file_ids":["foreign"]`,
	} {
		var tool, patchObject map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"type":"file_search","vector_store_ids":["vs_team"]}`), &tool)
		_ = json.Unmarshal([]byte("{"+patch+"}"), &patchObject)
		for key, value := range patchObject {
			tool[key] = value
		}
		raw, _ := json.Marshal([]any{tool})
		if _, _, err := parse(`{"tools":` + string(raw) + `}`); err == nil {
			t.Fatal("invalid file search tool admitted", patch)
		}
	}
	for _, input := range []string{
		`[{"type":"tool_search_output","call_id":"c","tools":` + nativeFileTools + `}]`,
		`[{"type":"file_search_call","status":"completed","queries":[]}]`,
		`[{"type":"file_search_call","id":"fs_team","status":"unknown","queries":[]}]`,
		`[{"type":"file_search_call","id":"fs_team","status":"completed","queries":null}]`,
		`[{"type":"file_search_call","id":"fs_team","status":"completed","queries":[],"vector_store_ids":["foreign"]}]`,
	} {
		if _, _, err := parse(`{"input":` + input + `}`); err == nil {
			t.Fatal("invalid file search history admitted", input)
		}
	}
	in, _, err := parse(`{"input":` + nativeFileCalls + `}`)
	if err != nil || !slices.Equal(in.ItemReferences, []string{"fs_team"}) {
		t.Fatal("unscoped file search history", in, err)
	}
	for _, input := range []string{
		`[{"role":"user","content":[{"type":"input_file","file_id":"file_foreign"}]}]`,
		`[{"type":"function_call_output","call_id":"c","output":[{"type":"input_file","file_id":"file_foreign"}]}]`,
	} {
		if _, _, err := parse(`{"tools":` + nativeFileTools + `,"input":` + input + `}`); err == nil {
			t.Fatal("file search grant authorized a shared file ID", input)
		}
	}
	filter := `{"type":"eq","key":"k","value":"v"}`
	filter = strings.Repeat(`{"type":"and","filters":[`, 10) + filter + strings.Repeat(`]}`, 10)
	nodes := 0
	if responseFileFilter(json.RawMessage(filter), 0, &nodes) == nil {
		t.Fatal("unbounded filter depth")
	}
	for _, raw := range []string{`null`, `[]`, `{"0":["vs_team"]}`, `{"01":["vs_team"]}`, `{"1":["vs_team","vs_team"]}`, `{"1":["../other"]}`} {
		if _, err := parseResponseStoreGrants(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid store grant", raw)
		}
	}
	u := &upstreamAccount{Platform: "openai", Credentials: map[string]json.RawMessage{"api_key": json.RawMessage(`"source"`), "api_protocol": json.RawMessage(`"responses"`)}}
	input := accountInput{Extra: map[string]json.RawMessage{responseStoresKey: json.RawMessage(`{"1":["vs_team"]}`)}}
	if err := input.bindResponseStoreGrants(u); err != nil {
		t.Fatal(err)
	}
	u.Extra = input.Extra
	if !u.allowsResponseStores(1, []string{"vs_team"}) || u.allowsResponseStores(2, []string{"vs_team"}) || u.allowsResponseStores(1, []string{"vs_foreign"}) {
		t.Fatal("store group authorization")
	}
	u.Credentials["api_key"] = json.RawMessage(`"rotated"`)
	if u.allowsResponseStores(1, []string{"vs_team"}) {
		t.Fatal("store grant followed credential rotation")
	}
}
