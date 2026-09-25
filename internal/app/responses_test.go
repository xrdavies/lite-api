package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const responseUsage = `"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":3},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":6},"total_tokens":28}`

func TestResponsesCompactionSubpaths(t *testing.T) {
	parse := func(action string, stream bool) (textRequest, error) {
		r := httptest.NewRequest("POST", "/responses/"+action, nil)
		r.SetPathValue("action", action)
		return parseResponsesRequest(r, textRequest{Protocol: "responses", Stream: stream}, map[string]json.RawMessage{"input": json.RawMessage(`"compact me"`)})
	}
	maxSegment := 128 - len("/backend-api/codex/responses/compact/")
	for _, action := range []string{"compact", "compact/", "compact/detail", "compact/a-b_C.1/detail/", "compact/" + strings.Repeat("a", maxSegment), "compact/a/b/c/d/e/f/g", "input_tokens/", "resp_owned/compact", "resp_owned/compact/detail/"} {
		in, err := parse(action, false)
		want := "/" + strings.TrimRight(action, "/")
		path, pathErr := in.upstreamPath("model")
		if err != nil || in.Action != want || pathErr != nil || path != "/v1/responses"+want || len("gateway."+in.Scope+".9223372036854775807") > 128 {
			t.Fatal("compaction path or idempotency scope", action, in, path, err, pathErr)
		}
		if _, err = parse(action, true); err == nil {
			t.Fatal("streaming compaction/count accepted", action)
		}
		if in.CountOnly {
			continue
		}
		o := textObservation{Protocol: "responses", Action: in.Action}
		if err := o.observe([]byte(`{"object":"response.compaction","id":"cmp_test","output":[],` + responseUsage + `}`)); err != nil || !o.complete() || !o.HasUsage {
			t.Fatal("compaction extension result", action, o, err)
		}
	}
	for _, action := range []string{"compact//detail", "compact/.", "compact/..", "compact/...", "compact/%2e%2e", "compact/x?y", "compact/x#y", `compact/x\y`, "compact/中文", "compact/" + strings.Repeat("a", maxSegment+1), "compact/a/b/c/d/e/f/g/h", "compact-other", "resp_foreign/unknown", "input_tokens/detail", "input_tokens/compact", "resp_1/compact/..", "resp_1//compact", "resp.1/compact"} {
		if _, err := parse(action, false); err == nil {
			t.Fatal("unsafe or unknown operation accepted", action)
		}
	}
	first, _ := parse("compact/detail", false)
	alias, _ := parse("compact/detail/", false)
	other, _ := parse("compact/detail.v2", false)
	if first.Scope != alias.Scope || first.Scope == other.Scope {
		t.Fatal("compaction extension replay scope collision")
	}
	r := httptest.NewRequest("POST", "/responses/resp_owned/compact", nil)
	r.SetPathValue("action", "resp_owned/compact")
	body := map[string]json.RawMessage{"previous_response_id": json.RawMessage(`"resp_previous"`)}
	in, err := parseResponsesRequest(r, textRequest{Protocol: "responses"}, body)
	if err != nil || in.ResponseResource != "resp_owned" || in.Previous != "resp_previous" || len(body) != 1 {
		t.Fatal("resource path replaced or injected body history", in, body, err)
	}
	delete(body, "previous_response_id")
	if _, err = parseResponsesRequest(r, textRequest{Protocol: "responses"}, body); err != nil {
		t.Fatal("owned path should provide resource context", err)
	}
}

func TestMergeResponseSources(t *testing.T) {
	previous := &responseBinding{AccountID: 1, Target: "source", MCPTool: true, FileIDs: []string{"file_previous"}, SkillIDs: []string{"skill_previous"}}
	resource := &responseBinding{AccountID: 1, Target: "source", CodeTool: true, Containers: []string{"cntr_resource"}, VectorStores: []string{"vs_resource"}, Items: []string{"msg_resource"}}
	merged, err := mergeResponseSources(previous, resource)
	if err != nil || !merged.MCPTool || !merged.CodeTool || len(merged.FileIDs) != 1 || len(merged.SkillIDs) != 1 || len(merged.VectorStores) != 1 || len(merged.Containers) != 1 || len(merged.Items) != 0 || len(resource.FileIDs) != 0 || previous.CodeTool {
		t.Fatal("response resource permissions lost or inputs mutated", merged, err)
	}
	for _, source := range []responseBinding{{AccountID: 2, Target: "source"}, {AccountID: 1, Target: "rotated"}, {AccountID: 1, Target: "source", History: "converted"}} {
		if _, err := mergeResponseSources(previous, &source); err == nil {
			t.Fatal("incompatible source accepted", source)
		}
	}
	previous.History = "converted"
	if _, err := mergeResponseSources(previous, resource); err == nil {
		t.Fatal("converted previous response accepted")
	}
}

func TestResponseItemReferences(t *testing.T) {
	for _, item := range []string{`{"id":"msg_one"}`, `{"id":"msg_one","type":null}`, `{"id":"msg_one","type":"item_reference"}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"model":"model","input":[`+item+`]}`), &body)
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		if err != nil || len(in.ItemReferences) != 1 || in.ItemReferences[0] != "msg_one" || !strings.Contains(string(body["input"]), `"type":"item_reference"`) {
			t.Fatal("valid reference rejected or not normalized", item, in, err)
		}
		if _, err := responsesToChatRequest(body, nil); err == nil {
			t.Fatal("reference was sent through a conversion")
		}
	}
	for _, item := range []string{`{"type":"item_reference"}`, `{"id":"../escape"}`, `{"id":"msg_one","type":false}`, `{"id":"msg_one","type":""}`, `{"id":"msg_one","role":"user","content":"foreign"}`, `{"id":"msg_one","type":"item_reference","content":"foreign"}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"model":"model","input":[`+item+`]}`), &body)
		if _, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body); err == nil {
			t.Fatal("invalid reference accepted", item)
		}
	}
	for _, stream := range []bool{false, true} {
		raw := `{"id":"resp_one","object":"response","status":"completed","output":[{"id":"msg_one","type":"message"},{"id":"rs_one","type":"reasoning"}],` + responseUsage + `}`
		if stream {
			raw = `{"type":"response.completed","response":` + raw + `}`
		}
		o := textObservation{Protocol: "responses"}
		if err := o.observe([]byte(raw)); err != nil || len(o.ResponseItems) != 2 || o.ResponseItems[1] != "rs_one" || !o.HasUsage {
			t.Fatal("output item observation", stream, o, err)
		}
	}
	var limitBody map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"model","input":[`+strings.Repeat(`{"id":"msg_one"},`, 1024)+`{"id":"msg_one"}]}`), &limitBody)
	if _, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", limitBody); err == nil {
		t.Fatal("unbounded references accepted")
	}
	o := textObservation{Protocol: "responses"}
	if err := o.observe([]byte(`{"id":"resp_one","object":"response","status":"completed","output":[{"id":"../escape"}],` + responseUsage + `}`)); err == nil || !o.HasUsage {
		t.Fatal("invalid output ID accepted or consumption lost", err)
	}
}

func TestResponsesProtocol(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed"} {
		o := textObservation{Protocol: "responses"}
		raw := `{"type":"response.` + status + `","response":{"object":"response","id":"resp_test","model":"test","status":"` + status + `",` + responseUsage + `}}`
		err := o.observe([]byte(raw))
		if (err != nil) != (status == "failed") || !o.HasUsage || o.Usage.Input != 12 || o.Usage.CacheRead != 5 || o.Usage.CacheWrite != 3 || o.Usage.Output != 8 {
			t.Fatal("Responses terminal usage", status, o, err)
		}
		if status != "failed" && (!o.complete() || o.ResponseID != "resp_test") {
			t.Fatal("missing Responses terminal", o)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"type":"error","message":"secret"}`, `{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"type":"response.completed","response":{}}}`,
		`{"type":"response.completed","response":{"object":"response","id":"resp_test","status":"incomplete"}}`,
		`{"object":"response","id":"bad/id","status":"completed"}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":8,"input_tokens_details":{"cached_tokens":19,"cache_write_tokens":3}}}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":8,"output_tokens_details":{"reasoning_tokens":9}}}`,
		`{"object":"response","id":"resp_test","status":"completed","usage":{"input_tokens":20,"output_tokens":null}}`,
	} {
		o := textObservation{Protocol: "responses"}
		if err := o.observe([]byte(raw)); err == nil {
			t.Fatal("invalid response accepted", raw)
		}
	}
	u, err := parseChatUsage([]byte(`{"prompt_tokens":20,"completion_tokens":8,"cache_creation_input_tokens":15,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":5}}`))
	if err != nil || u.Input != 15 || u.CacheWrite != 0 {
		t.Fatal("explicit zero cache-write precedence", u, err)
	}
}

func testResponses(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-cookie")
		if !strings.HasPrefix(path, "/api/v1/") {
			r.Header.Set("X-Api-Key", "client-header-key")
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		t.Helper()
		w := call(path, token, body, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	var calls, mode atomic.Int32
	var otherCalls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer responses-upstream" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.URL.RawQuery != "" {
			t.Error("Responses credentials leaked or incorrect upstream credentials")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || string(body["model"]) != `"upstream-responses"` {
			t.Error("Responses model mapping")
		}
		if body["stream_options"] != nil {
			t.Error("Chat options injected into Responses")
		}
		if mode.Load() == 6 {
			if !strings.Contains(string(body["tools"]), `"type":"namespace"`) || !strings.Contains(string(body["tool_choice"]), `"namespace":"files"`) {
				t.Error("native namespace declaration was rewritten")
			}
		}
		if r.Header.Get("X-Codex-Beta-Features") != "" {
			var items []map[string]json.RawMessage
			if r.Header.Get("X-Codex-Beta-Features") != "remote_compaction_v2" || json.Unmarshal(body["input"], &items) != nil || len(items) != 2 || credentialString(items[0], "role") != "user" || credentialString(items[1], "type") != "compaction_trigger" {
				t.Error("native compaction input or negotiation")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "responses-request")
		if r.URL.Path == "/v1/responses/input_tokens" {
			_, _ = fmt.Fprint(w, `{"object":"response.input_tokens","input_tokens":20}`)
			return
		}
		resourceCompact := r.URL.Path == "/v1/responses/resp_1/compact" || r.URL.Path == "/v1/responses/resp_1/compact/detail.v2" || r.URL.Path == "/v1/responses/resp_owned_alias/compact"
		if resourceCompact {
			want := ""
			if credentialString(body, "instructions") == "explicit previous" {
				want = "resp_1"
			}
			if credentialString(body, "previous_response_id") != want {
				t.Error("resource path changed previous_response_id")
			}
		}
		if resourceCompact || r.URL.Path == "/v1/responses/compact" || r.URL.Path == "/v1/responses/compact/detail.v2" {
			_, _ = fmt.Fprint(w, `{"object":"response.compaction","id":"cmp_test","output":[{"type":"compaction","encrypted_content":"opaque-content"}],`+responseUsage+`}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Error("unapproved Responses upstream path", r.URL.Path)
		}
		if mode.Load() == 4 {
			w.WriteHeader(503)
			return
		}
		status := "completed"
		if mode.Load() == 1 {
			status = "incomplete"
		} else if mode.Load() == 2 {
			status = "failed"
		}
		usage := responseUsage
		if mode.Load() == 3 {
			usage = `"usage":null`
		}
		response := fmt.Sprintf(`{"object":"response","id":"resp_%d","model":"response-model","status":%q,"service_tier":"default","output":[{"type":"function_call","call_id":"call_one","name":"weather","arguments":"{\"city\":\"Tokyo\"}"},{"type":"reasoning","encrypted_content":"encrypted-reasoning"}],%s}`, n, status, usage)
		response = strings.Replace(response, `"type":"function_call"`, fmt.Sprintf(`"type":"function_call","id":"fc_%d"`, n), 1)
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(body["input"], &items)
		for _, item := range items {
			if item["id"] != nil && credentialString(item, "type") != "item_reference" {
				t.Error("item reference type was not normalized")
			}
		}
		if mode.Load() == 6 {
			response = strings.Replace(response, `"name":"weather"`, `"name":"weather","namespace":"files"`, 1)
		}
		if string(body["stream"]) != "true" {
			_, _ = fmt.Fprint(w, response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"object\":\"response\",\"id\":\"resp_%d\",\"status\":\"in_progress\"}}\n\n", n)
		_, _ = fmt.Fprint(w, "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"Tokyo\",\"sequence_number\":1}\n\n")
		if mode.Load() == 5 {
			return
		}
		_, _ = fmt.Fprintf(w, "event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":%s}\n\n", status, status, response)
	}))
	defer up.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer other.Close()
	uid := int64(manage("/api/v1/admin/users", admin, map[string]any{"email": "responses@example.test", "password": "responses-password", "balance": 10})["id"].(float64))
	user := manage("/api/v1/auth/login", "", map[string]any{"email": "responses@example.test", "password": "responses-password"})["access_token"].(string)
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Responses", "platform": "openai"})["id"].(float64))
	manage("/api/v1/admin/channels", admin, map[string]any{"name": "Responses", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"client-response"}, "input_price": json.Number("0.000001"), "output_price": json.Number("0.000002"), "cache_read_price": json.Number("0.0000001"), "cache_write_price": json.Number("0.000003"), "reasoning_effort_multipliers": map[string]any{"high": 2}}}})
	account := func(name, base string, priority int) int64 {
		return int64(manage("/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "openai", "type": "apikey", "priority": priority, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "responses-upstream", "base_url": base, "api_protocol": "responses", "model_mapping": map[string]string{"client-response": "upstream-responses"}}})["id"].(float64))
	}
	aid := account("Responses", up.URL+"/v1", 1)
	k := manage("/api/v1/keys", user, map[string]any{"name": "Responses", "group_id": gid, "quota": 10})
	key, kid := k["key"].(string), int64(k["id"].(float64))
	body := map[string]any{"model": "client-response", "input": []any{map[string]any{"role": "user", "content": "weather"}}, "reasoning": map[string]any{"effort": "high"}, "tools": []any{map[string]any{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}}}}
	first := call("/v1/responses", key, body, "responses-once")
	for _, alias := range []string{"/responses", "/backend-api/codex/responses"} {
		replay := call(alias, key, body, "responses-once")
		if first.Code != 200 || replay.Body.String() != first.Body.String() || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("Responses JSON/alias replay", first.Code, first.Body.String(), replay.Code, replay.Body.String())
		}
	}
	if !strings.Contains(first.Body.String(), "encrypted-reasoning") || !strings.Contains(first.Body.String(), "function_call") {
		t.Fatal("Responses reasoning or tool output lost")
	}
	var input, output, read, write int
	var cost, balance, consumed, effort string
	if err := a.DB.QueryRow(`SELECT l.input_tokens,l.output_tokens,l.cache_read_tokens,l.cache_creation_tokens,l.actual_cost::text,u.balance::text,k.quota_used::text,l.reasoning_effort FROM usage_logs l JOIN users u ON u.id=l.user_id JOIN api_keys k ON k.id=l.api_key_id WHERE k.id=$1`, kid).Scan(&input, &output, &read, &write, &cost, &balance, &consumed, &effort); err != nil || input != 12 || output != 8 || read != 5 || write != 3 || cost != "0.0000750000" || balance != "9.99992500" || consumed != "0.00007500" || effort != "high" {
		t.Fatal("Responses accounting", input, output, read, write, cost, balance, consumed, effort, err)
	}
	countInput := mustJSON(map[string]any{"model": "upstream-responses", "input": body["input"], "tools": body["tools"]})
	estimated, err := estimateInputTokens(countInput)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/responses/input_tokens", "/backend-api/codex/responses/input_tokens", "/v1/responses/input_tokens/"} {
		if w := call(path, key, body, "response-count"); w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), fmt.Sprintf(`"input_tokens":%d`, estimated)) {
			t.Fatal("Responses token count", w.Code, w.Body.String())
		}
	}
	var logs int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&logs); err != nil || logs != 1 {
		t.Fatal("Responses count charged consumption", logs, err)
	}
	if w := call("/v1/responses/compact", key, body, "response-compact"); w.Code != 200 || !strings.Contains(w.Body.String(), "opaque-content") {
		t.Fatal("Responses compaction", w.Code, w.Body.String())
	}
	beforeCompact := calls.Load()
	if w := call("/backend-api/codex/responses/compact/", key, body, "response-compact"); w.Code != 200 || calls.Load() != beforeCompact || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("compaction trailing slash changed replay scope", w.Code, w.Body.String())
	}
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, suffix := range []string{"/compact/detail.v2", "/compact/detail.v2/"} {
			w := call(prefix+suffix, key, body, "response-compact")
			if w.Code != 200 || !strings.Contains(w.Body.String(), "opaque-content") || calls.Load() != beforeCompact+1 {
				t.Fatal("compaction extension path/alias/replay", prefix+suffix, w.Code, w.Body.String(), calls.Load())
			}
		}
	}
	var compactCost, upstreamEndpoint string
	if err := a.DB.QueryRow(`SELECT count(*),min(actual_cost)::text,min(upstream_endpoint) FROM usage_logs WHERE api_key_id=$1 AND inbound_endpoint='/v1/responses/compact/detail.v2'`, kid).Scan(&logs, &compactCost, &upstreamEndpoint); err != nil || logs != 1 || compactCost != "0.0000750000" || upstreamEndpoint != "/v1/responses/compact/detail.v2" {
		t.Fatal("compaction extension accounting", logs, compactCost, upstreamEndpoint, err)
	}
	for _, path := range []string{"/responses/compact/%2e%2e", "/responses/compact/detail%3Fescape", "/responses/compact/detail%23escape", "/responses/compact/%252e%252e", "/responses/compact/" + strings.Repeat("x", 129), "/responses/resp_unowned/compact", "/responses/unknown"} {
		w := call(path, key, body, "")
		if w.Code != 404 || calls.Load() != beforeCompact+1 {
			t.Fatal("invalid compaction path reached upstream", path, w.Code)
		}
	}
	for _, credential := range []string{"", user} {
		if w := call("/responses/compact/detail.v2", credential, body, ""); w.Code != 401 || calls.Load() != beforeCompact+1 {
			t.Fatal("compaction extension Key authentication", w.Code)
		}
	}
	body["stream"] = true
	if w := call("/responses/compact/detail.v2", key, body, ""); w.Code != 400 || calls.Load() != beforeCompact+1 {
		t.Fatal("streaming extension dispatched", w.Code)
	}
	stream := call("/responses", key, body, "response-stream")
	if stream.Code != 200 || !strings.Contains(stream.Body.String(), "event: response.completed") || strings.Contains(stream.Body.String(), "[DONE]") {
		t.Fatal("Responses stream", stream.Code, stream.Body.String())
	}
	if w := call("/backend-api/codex/responses", key, body, "response-stream"); w.Body.String() != stream.Body.String() || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("Responses stream replay")
	}
	for _, state := range []int32{1, 2, 3, 5} {
		mode.Store(state)
		w := call("/responses", key, body, fmt.Sprint("response-state-", state))
		if state == 1 {
			if !strings.Contains(w.Body.String(), "event: response.incomplete") || strings.Contains(w.Body.String(), "event: error") {
				t.Fatal("Responses output limit must remain native incomplete", w.Body.String())
			}
		} else if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
			t.Fatal("Responses failure falsely completed", state, w.Body.String())
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_responses_settlement CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	compactRecovery := map[string]any{"model": "client-response", "input": "compact while SQL fails"}
	if w := call("/responses/compact/detail.v2", key, compactRecovery, "compact-recovery"); w.Code != 503 || strings.Contains(w.Body.String(), "opaque-content") {
		t.Fatal("compaction extension completed before settlement", w.Code, w.Body.String())
	}
	w := call("/responses", key, body, "response-settlement")
	if !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("Responses completed before billing", w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_responses_settlement"); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverReceipts(t.Context()); err != nil {
		t.Fatal(err)
	}
	beforeRecoveryReplay := calls.Load()
	if w := call("/responses/compact/detail.v2", key, compactRecovery, "compact-recovery"); w.Code != 503 || calls.Load() != beforeRecoveryReplay || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("failed compaction extension was resubmitted", w.Code, w.Body.String())
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&logs); err != nil || logs != 8 {
		t.Fatal("Responses failed/partial billing or settlement recovery", logs, err)
	}
	delete(body, "stream")
	before := calls.Load()
	for _, tools := range []any{[]any{map[string]any{"type": "file_search"}}, []any{nil}, "invalid"} {
		hidden := map[string]any{"model": "client-response", "input": []any{map[string]any{"type": "additional_tools", "tools": tools}, map[string]any{"role": "user", "content": "hello"}}}
		if w := call("/responses", key, hidden, ""); w.Code != 400 || calls.Load() != before {
			t.Fatal("additional tools bypassed admission", w.Code, calls.Load())
		}
	}
	for _, field := range []string{"conversation", "tools", "previous_response_id"} {
		saved, exists := body[field]
		body[field] = map[string]any{"background": true, "conversation": "conv_foreign", "input": []any{map[string]any{"type": "item_reference", "id": "msg_foreign"}}, "tools": []any{map[string]any{"type": "file_search"}}, "previous_response_id": "../escape"}[field]
		if w := call("/responses", key, body, ""); w.Code != 400 {
			t.Fatal("unsupported/unsafe Responses request accepted", field, w.Code)
		}
		if exists {
			body[field] = saved
		} else {
			delete(body, field)
		}
	}
	for _, path := range []string{"/responses/other", "/responses/resp_foreign/cancel", "/responses/compact-other/extra", "/responses/%2e%2e/other"} {
		if w := call(path, key, body, ""); w.Code != 404 {
			t.Fatal("unapproved Responses subpath", path, w.Code)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid Responses request reached upstream")
	}
	inputBefore := body["input"]
	body["input"] = []any{map[string]any{"type": "compaction_trigger"}, map[string]any{"role": "user", "content": "compact"}, map[string]any{"type": "compaction_trigger"}}
	if w := call("/responses", key, body, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("non-streaming native compaction accepted", w.Code)
	}
	body["stream"], body["store"] = true, false
	if w := call("/responses", key, body, "response-native-compact"); !strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("native compaction forwarding", w.Code, w.Body.String())
	}
	var native bool
	if err := a.DB.QueryRow("SELECT native_compaction_v2 FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&native); err != nil || !native {
		t.Fatal("native compaction marker", native, err)
	}
	delete(body, "stream")
	delete(body, "store")
	body["input"] = inputBefore
	body["previous_response_id"] = fmt.Sprintf("resp_%d", calls.Load())
	if w := call("/responses", key, body, ""); w.Code != 404 {
		t.Fatal("store=false response was bound", w.Code)
	}
	delete(body, "previous_response_id")
	mode.Store(6)
	for _, stream := range []bool{false, true} {
		request := map[string]any{"model": "client-response", "input": "hello", "stream": stream, "tools": []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{map[string]any{"type": "function", "name": "weather", "parameters": map[string]any{"type": "object"}}}}}, "tool_choice": map[string]string{"type": "function", "namespace": "files", "name": "weather"}}
		if w := call("/responses", key, request, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"namespace":"files"`) {
			t.Fatal("native namespace forwarding", stream, w.Code, w.Body.String())
		}
	}
	mode.Store(0)
	// Item ownership is scoped to the Key, independently of a previous response.
	referenced := map[string]any{"model": "client-response", "input": []any{map[string]any{"id": "fc_1"}}}
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		if w := call(path, key, referenced, "item-once"); w.Code != 200 {
			t.Fatal("owned item reference", path, w.Code, w.Body.String())
		}
	}
	referenced["previous_response_id"] = "resp_1"
	referenced["stream"] = true
	if w := call("/responses", key, referenced, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("streamed item reference", w.Code, w.Body.String())
	}
	delete(referenced, "stream")
	delete(referenced, "previous_response_id")
	streamItem := fmt.Sprintf("fc_%d", calls.Load())
	referenced["input"] = []any{map[string]any{"id": streamItem}}
	if w := call("/responses", key, referenced, ""); w.Code != 200 {
		t.Fatal("SSE output item not bound", w.Code)
	}
	var countBefore int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&countBefore); err != nil {
		t.Fatal(err)
	}
	if w := call("/responses/input_tokens", key, referenced, ""); w.Code != 200 {
		t.Fatal("item token count", w.Code, w.Body.String())
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&logs); err != nil || logs != countBefore {
		t.Fatal("item token count billed", logs, countBefore, err)
	}
	referenced["input"] = []any{map[string]any{"id": "fc_1"}}
	foreign := manage("/api/v1/keys", user, map[string]any{"name": "Foreign item key", "group_id": gid})["key"].(string)
	if w := call("/responses", foreign, body, ""); w.Code != 200 {
		t.Fatal("foreign response setup", w.Code)
	}
	foreignResponse := fmt.Sprintf("resp_%d", calls.Load())
	foreignItem := fmt.Sprintf("fc_%d", calls.Load())
	before = calls.Load()
	for _, previous := range []string{"", foreignResponse} {
		if previous != "" {
			referenced["previous_response_id"] = previous
		}
		if w := call("/responses", foreign, referenced, ""); w.Code != 404 || calls.Load() != before {
			t.Fatal("foreign item accepted with owned previous response", w.Code)
		}
	}
	referenced["previous_response_id"] = "resp_1"
	referenced["input"] = []any{map[string]any{"type": "item_reference", "id": foreignItem}}
	if w := call("/responses", key, referenced, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("foreign item accepted on shared upstream", w.Code)
	}
	referenced["input"] = []any{map[string]any{"type": "item_reference", "id": "fc_unknown"}}
	if w := call("/responses", key, referenced, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("unobserved item accepted", w.Code)
	}
	nonstored := map[string]any{"model": "client-response", "input": "private", "store": false}
	if w := call("/responses", key, nonstored, ""); w.Code != 200 {
		t.Fatal("nonstored response", w.Code)
	}
	referenced["input"] = []any{map[string]any{"id": fmt.Sprintf("fc_%d", calls.Load())}}
	before = calls.Load()
	if w := call("/responses", key, referenced, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("nonstored item survived HTTP request", w.Code)
	}
	// A more preferred account must never receive the first account's response ID.
	account("Other Responses", other.URL, 0)
	body["previous_response_id"] = "resp_1"
	if w := call("/responses", key, body, "response-followup"); w.Code != 200 || otherCalls.Load() != 0 {
		t.Fatal("Responses continuation affinity", w.Code, w.Body.String())
	}
	otherKey := manage("/api/v1/keys", user, map[string]any{"name": "Other Responses", "group_id": gid})["key"].(string)
	if w := call("/responses", otherKey, body, ""); w.Code != 404 || otherCalls.Load() != 0 {
		t.Fatal("cross-key response reference allowed", w.Code)
	}
	compactBody := map[string]any{"model": "client-response", "input": "compact history", "previous_response_id": "resp_1"}
	if w := call("/responses/compact/detail.v2", key, compactBody, ""); w.Code != 200 || otherCalls.Load() != 0 {
		t.Fatal("compaction extension source affinity", w.Code, w.Body.String())
	}
	before = calls.Load()
	if w := call("/responses/compact/detail.v2", otherKey, compactBody, ""); w.Code != 404 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("compaction extension cross-key affinity", w.Code)
	}
	var keyInfo gatewayKey
	keyInfo.ID, keyInfo.GroupID = kid, gid
	identity := &gatewayIdentity{Key: keyInfo}
	original, err := a.loadAccount(t.Context(), aid)
	if err != nil {
		t.Fatal(err)
	}
	resourceBody := map[string]any{"model": "client-response"}
	beforeResource := calls.Load()
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, suffix := range []string{"/resp_1/compact/detail.v2", "/resp_1/compact/detail.v2/"} {
			if w := call(prefix+suffix, key, resourceBody, "resource-compaction"); w.Code != 200 || !strings.Contains(w.Body.String(), "opaque-content") || calls.Load() != beforeResource+1 || otherCalls.Load() != 0 {
				t.Fatal("resource compaction source/path/alias/replay", w.Code, w.Body.String())
			}
		}
	}
	if err := a.DB.QueryRow(`SELECT count(*),min(actual_cost)::text,min(upstream_endpoint) FROM usage_logs WHERE api_key_id=$1 AND inbound_endpoint='/v1/responses/resp_1/compact/detail.v2'`, kid).Scan(&logs, &compactCost, &upstreamEndpoint); err != nil || logs != 1 || compactCost != "0.0000375000" || upstreamEndpoint != "/v1/responses/resp_1/compact/detail.v2" {
		t.Fatal("resource compaction accounting", logs, compactCost, upstreamEndpoint, err)
	}
	for _, credential := range []string{otherKey, foreign} {
		if w := call("/responses/resp_1/compact", credential, resourceBody, ""); w.Code != 404 || calls.Load() != beforeResource+1 || otherCalls.Load() != 0 {
			t.Fatal("cross-Key resource compact", w.Code, w.Body.String())
		}
	}
	for _, field := range []string{"previous_response_id", "input", "stream", "background"} {
		resourceBody[field] = map[string]any{"previous_response_id": foreignResponse, "input": []any{map[string]any{"type": "item_reference", "id": foreignItem}}, "stream": true, "background": true}[field]
		w := call("/responses/resp_1/compact", key, resourceBody, "")
		delete(resourceBody, field)
		if (w.Code != 400 && w.Code != 404) || calls.Load() != beforeResource+1 || otherCalls.Load() != 0 {
			t.Fatal("resource compaction bypassed body authorization", field, w.Code)
		}
	}
	if err := a.bindResponse(t.Context(), identity, original, "resp_owned_alias"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/resp_owned_alias/compact", "/resp_1/compact"} {
		if w := call("/responses"+suffix, key, resourceBody, "resource-compaction"); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "" || otherCalls.Load() != 0 {
			t.Fatal("different resource or action shared replay", suffix, w.Code, w.Body.String())
		}
	}
	resourceBody["previous_response_id"], resourceBody["instructions"] = "resp_1", "explicit previous"
	if w := call("/responses/resp_owned_alias/compact", key, resourceBody, ""); w.Code != 200 || otherCalls.Load() != 0 {
		t.Fatal("owned resource with same-source previous response", w.Code, w.Body.String())
	}
	delete(resourceBody, "previous_response_id")
	delete(resourceBody, "instructions")
	beforeResource = calls.Load()
	for name, source := range map[string]responseBinding{
		"resp_converted_resource": {AccountID: aid, Target: responseTarget(original), History: "converted"},
		"resp_revoked_file":       {AccountID: aid, Target: responseTarget(original), FileIDs: []string{"file_revoked"}},
		"resp_other_account":      {AccountID: aid + 999, Target: "other-source"},
	} {
		if err := a.storeResponseBinding(t.Context(), identity, name, source); err != nil {
			t.Fatal(err)
		}
		resourceBody["previous_response_id"] = name
		w := call("/responses/resp_1/compact", key, resourceBody, "")
		delete(resourceBody, "previous_response_id")
		if w.Code < 400 || calls.Load() != beforeResource || otherCalls.Load() != 0 {
			t.Fatal("resource compaction lost source restrictions", name, w.Code)
		}
		if w := call("/responses/"+name+"/compact", key, resourceBody, ""); w.Code < 400 || calls.Load() != beforeResource || otherCalls.Load() != 0 {
			t.Fatal("resource path bypassed source restrictions", name, w.Code)
		}
	}
	if err := a.Redis.Set(t.Context(), responseDeletionKey(identity, "resp_owned_alias"), "deleted", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if w := call("/responses/resp_owned_alias/compact", key, resourceBody, ""); w.Code != 404 || calls.Load() != beforeResource {
		t.Fatal("deleted resource compact dispatched", w.Code)
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_resource_compact CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_resource_compact")
	failedResource := call("/responses/resp_1/compact", key, resourceBody, "resource-recovery")
	if failedResource.Code != 503 || strings.Contains(failedResource.Body.String(), "opaque-content") {
		t.Fatal("resource compact escaped failed settlement", failedResource.Code)
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_resource_compact"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE request_id=$1", failedResource.Header().Get("X-Request-ID")).Scan(&logs, &compactCost); err != nil || logs != 1 || compactCost != "0.0000375000" {
		t.Fatal("resource compaction recovery billed incorrectly", logs, compactCost, err)
	}
	if w := call("/backend-api/codex/responses/resp_1/compact", key, resourceBody, "resource-recovery"); w.Code != 503 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != beforeResource+1 {
		t.Fatal("failed resource compact was regenerated", w.Code)
	}
	referenced["input"] = []any{map[string]any{"id": "fc_1", "type": nil}}
	delete(referenced, "previous_response_id")
	if w := call("/responses", key, referenced, ""); w.Code != 200 || otherCalls.Load() != 0 {
		t.Fatal("item reference fell back to preferred account", w.Code)
	}
	if err := a.Redis.Del(t.Context(), responseItemKey(identity, streamItem)).Err(); err != nil {
		t.Fatal(err)
	}
	referenced["input"] = []any{map[string]any{"id": streamItem}}
	before = calls.Load()
	if w := call("/responses", key, referenced, ""); w.Code != 404 || calls.Load() != before {
		t.Fatal("expired item was sent upstream", w.Code)
	}
	referenced["input"] = []any{map[string]any{"id": "fc_1"}}
	// All stored IDs are checked before any part of a binding is published.
	otherSource := responseBinding{AccountID: aid + 999, Target: "other-source"}
	if err := a.storeResponseBinding(t.Context(), identity, "resp_other_source", otherSource); err != nil {
		t.Fatal(err)
	}
	referenced["previous_response_id"] = "resp_other_source"
	before = calls.Load()
	if w := call("/responses", key, referenced, ""); w.Code != 400 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("mixed response/item sources accepted", w.Code)
	}
	otherSource.Items = []string{"fc_1"}
	if err := a.storeResponseBinding(t.Context(), identity, "resp_collision", otherSource); err == nil {
		t.Fatal("item source collision accepted")
	}
	if n, err := a.Redis.Exists(t.Context(), responseBindingKey(identity, "resp_collision")).Result(); err != nil || n != 0 {
		t.Fatal("partial binding after collision", n, err)
	}
	bound, err := a.previousResponse(t.Context(), identity, "resp_1")
	if err != nil || bound.AccountID != aid {
		t.Fatal("persisted response binding", bound, err)
	}
	if err := a.Redis.Del(t.Context(), responseBindingKey(identity, "resp_1")).Err(); err != nil {
		t.Fatal(err)
	}
	if w := call("/responses", key, body, "response-followup"); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("completed continuation could not replay after affinity expiry", w.Code)
	}
	if w := call("/responses", key, body, ""); w.Code != 404 || otherCalls.Load() != 0 {
		t.Fatal("missing response binding fell back to another account", w.Code)
	}
	if err := a.bindResponse(t.Context(), identity, original, "resp_1"); err != nil {
		t.Fatal(err)
	}
	mode.Store(4)
	if w := call("/responses", key, body, "response-unavailable"); w.Code != 503 || otherCalls.Load() != 0 {
		t.Fatal("affinity fell back to another upstream", w.Code)
	}
	if _, err := a.DB.Exec(`UPDATE accounts SET overload_until=NULL,credentials=jsonb_set(credentials,'{api_key}','"rotated"') WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if w := call("/responses/resp_1/compact", key, resourceBody, ""); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("resource compaction survived key rotation", w.Code)
	}
	if w := call("/responses/compact/detail.v2", key, compactBody, ""); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("compaction extension survived upstream key rotation", w.Code)
	}
	if w := call("/responses", key, body, "response-rotated"); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("continuation survived upstream key rotation", w.Code)
	}
	delete(referenced, "previous_response_id")
	if w := call("/responses", key, referenced, ""); w.Code != 503 || calls.Load() != before || otherCalls.Load() != 0 {
		t.Fatal("item survived upstream key rotation", w.Code)
	}
	var reconciled bool
	if err := a.DB.QueryRow("SELECT (10-balance)=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&reconciled); err != nil || !reconciled {
		t.Fatal("Responses balance reconciliation", err)
	}
}
