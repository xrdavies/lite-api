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

func TestChatAnthropicConversion(t *testing.T) {
	var request map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"model":"m","stream":true,"max_completion_tokens":2048,"reasoning_effort":"xhigh","stop":["END"],"parallel_tool_calls":false,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{}}}},"messages":[{"role":"system","content":"rules"},{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},{"type":"image_url","image_url":{"url":"data:image/png;base64,YQ=="}},{"type":"file","file":{"filename":"test.pdf","file_data":"data:application/pdf;base64,YQ=="}}]},{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get","arguments":"{\"x\":9007199254740993}"}},{"id":"call_b","type":"custom","custom":{"name":"patch","input":"raw patch"}}]},{"role":"tool","tool_call_id":"call_a","content":"yes"},{"role":"tool","tool_call_id":"call_b","content":"done"}],"tools":[{"type":"function","function":{"name":"get","parameters":{"type":"object"},"strict":true}},{"type":"custom","custom":{"name":"patch"}}],"tool_choice":"required"}`), &request)
	raw, custom, effort, err := chatToAnthropic(request)
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if err != nil || !custom["patch"] || effort != "max" || got["max_tokens"] != float64(2048) || got["thinking"].(map[string]any)["budget_tokens"] != float64(2047) || got["output_config"].(map[string]any)["effort"] != "max" || got["stop_sequences"].([]any)[0] != "END" {
		t.Fatal(string(raw), custom, effort, err)
	}
	messages := got["messages"].([]any)
	if len(messages) != 3 || len(messages[0].(map[string]any)["content"].([]any)) != 3 || len(messages[1].(map[string]any)["content"].([]any)) != 2 || len(messages[2].(map[string]any)["content"].([]any)) != 2 || !strings.Contains(string(raw), `"cache_control":{"type":"ephemeral"}`) || !strings.Contains(string(raw), "9007199254740993") || got["tool_choice"].(map[string]any)["disable_parallel_tool_use"] != true {
		t.Fatal("input conversion", string(raw))
	}
	for _, payload := range []string{
		`{"n":2}`, `{"max_completion_tokens":10,"reasoning_effort":"high"}`, `{"stop":[""]}`, `{"service_tier":"priority"}`, `{"verbosity":"high"}`, `{"response_format":{"type":"json_object"}}`,
		`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"other-provider"}}]}]}`,
		`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,invalid"}}]}]}`,
		`{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"get","arguments":"[]"}}]}]}`,
	} {
		patched := map[string]json.RawMessage{}
		for k, v := range request {
			patched[k] = v
		}
		_ = json.Unmarshal([]byte(payload), &patched)
		if _, _, _, err := chatToAnthropic(patched); err == nil {
			t.Fatal("invalid conversion accepted", payload)
		}
	}
	for _, reason := range []string{"end_turn", "max_tokens", "refusal", "tool_use"} {
		s := newAnthropicChatStream("public", true, map[string]bool{"patch": true})
		raw := fmt.Sprintf(`{"type":"message","id":"msg_1","stop_reason":"%s","content":[{"type":"thinking","thinking":"thought","signature":"opaque"},{"type":"text","text":"answer"},{"type":"tool_use","id":"t1","name":"get","input":{"x":9007199254740993}},{"type":"tool_use","id":"t2","name":"patch","input":{"input":"patch body"}}]}`, reason)
		result, err := s.response([]byte(raw), priceUsage{Input: 10, CacheRead: 5, CacheWrite: 6, Output: 8})
		want := map[string]string{"end_turn": "stop", "max_tokens": "length", "refusal": "content_filter", "tool_use": "tool_calls"}[reason]
		if err != nil || !strings.Contains(string(result), `"finish_reason":"`+want+`"`) || !strings.Contains(string(result), `"prompt_tokens":21`) || !strings.Contains(string(result), `"input":"patch body"`) || strings.Contains(string(result), "opaque") {
			t.Fatal(string(result), err)
		}
	}
	events := []string{
		`{"type":"message_start","message":{"id":"m","type":"message","content":[]}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"reason"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"secret"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"one","name":"get","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"two","name":"patch","input":{}}}`,
		`{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"input\":\"custom text\"}"}}`,
		`{"type":"content_block_stop","index":3}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`{"type":"message_stop"}`,
	}
	s := newAnthropicChatStream("public", true, map[string]bool{"patch": true})
	var wire strings.Builder
	for i, event := range events {
		chunk, err := s.event([]byte(event), priceUsage{Input: 10, Output: 8})
		if err != nil {
			t.Fatal(i, err)
		}
		if i < len(events)-1 && strings.Contains(chunk, `"finish_reason":"`) {
			t.Fatal("early terminal")
		}
		wire.WriteString(chunk)
	}
	if strings.Count(wire.String(), `"id":"one"`) != 1 || !strings.Contains(wire.String(), `"input":"custom text"`) || strings.Contains(wire.String(), "secret") || !strings.HasSuffix(wire.String(), "data: [DONE]\n\n") {
		t.Fatal(wire.String())
	}
	for _, tc := range []struct {
		prefix int
		event  string
	}{
		{0, events[2]}, {1, `{"type":"message_stop"}`}, {1, events[0]},
		{2, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`},
		{2, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		{4, events[2]}, {10, events[11]}, {12, events[8]},
	} {
		s := newAnthropicChatStream("p", false, nil)
		for _, event := range events[:tc.prefix] {
			if _, err := s.event([]byte(event), priceUsage{}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.event([]byte(tc.event), priceUsage{}); err == nil {
			t.Fatal("invalid sequence accepted", tc)
		}
	}
}

func testChatAnthropic(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var v struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "chat-anthropic@example.test", "password": "chat-anthropic-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "chat-anthropic@example.test", "password": "chat-anthropic-password"})["access_token"].(string)
	var mode, calls, priceChange atomic.Int32
	var gid int64
	const usage = `"usage":{"input_tokens":10,"output_tokens":8,"cache_read_input_tokens":5,"cache_creation_input_tokens":6,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":4}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"anthropic-up"}]}`)
			return
		}
		calls.Add(1)
		var b map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/v1/messages" || r.Header.Get("X-Api-Key") != "anthropic-secret" || r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || credentialString(b, "model") != "anthropic-up" || b["stream_options"] != nil || b["max_tokens"] == nil {
			t.Error("upstream conversion/isolation", r.URL.Path, b)
		}
		if priceChange.Swap(0) == 1 {
			if _, err := a.DB.Exec("UPDATE groups SET rate_multiplier=3 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		reason := "tool_use"
		if mode.Load() == 1 {
			reason = "max_tokens"
		}
		if mode.Load() == 2 {
			reason = "refusal"
		}
		u := usage
		if mode.Load() == 3 {
			u = `"usage":null`
		}
		content := `[{"type":"thinking","thinking":"thought","signature":"private-signature"},{"type":"text","text":"answer"},{"type":"tool_use","id":"call_a","name":"get","input":{"x":1}}]`
		if string(b["stream"]) != "true" {
			fmt.Fprintf(w, `{"type":"message","id":"msg_test","model":"actual-anthropic","stop_reason":"%s","content":%s,%s}`, reason, content, u)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(s string) { fmt.Fprintf(w, "data: %s\n\n", s); _ = http.NewResponseController(w).Flush() }
		emit(`{"type":"message_start","message":{"id":"msg_test","type":"message","model":"actual-anthropic","content":[],` + u + `}}`)
		emit(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		emit(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"answer"}}`)
		if mode.Load() == 4 {
			return
		}
		emit(`{"type":"content_block_stop","index":0}`)
		emit(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_a","name":"get","input":{}}}`)
		emit(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`)
		emit(`{"type":"content_block_stop","index":1}`)
		if mode.Load() == 5 {
			emit(`{"type":"error","error":{"message":"private-upstream-error"}}`)
			return
		}
		emit(`{"type":"message_delta","delta":{"stop_reason":"` + reason + `"}}`)
		emit(`{"type":"message_stop"}`)
	}))
	defer up.Close()
	price := func(platform string) map[string]any {
		return map[string]any{"platform": platform, "models": []string{"public-anthropic"}, "input_price": 0.01, "output_price": 0.02, "cache_read_price": 0.003, "cache_write_price": 0.004, "cache_write_1h_price": 0.006}
	}
	gid = id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Chat Anthropic", "platform": "anthropic", "rate_multiplier": 2, "model_pricing": []any{price("anthropic")}}))
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Anthropic bridge", "group_id": gid, "quota": 50})["key"].(string)
	account := func(platform string, groups []int64) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Chat " + platform, "platform": platform, "type": "apikey", "rate_multiplier": 3, "group_ids": groups, "credentials": map[string]any{"api_key": "anthropic-secret", "base_url": up.URL, "api_protocol": "anthropic", "model_mapping": map[string]string{"public-anthropic": "anthropic-up"}}}))
	}
	aid := account("anthropic", []int64{gid})
	body := func(stream, include bool) map[string]any {
		return map[string]any{"model": "public-anthropic", "stream": stream, "stream_options": map[string]bool{"include_usage": include}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "get", "parameters": map[string]any{"type": "object"}}}}}
	}
	check := func(w *httptest.ResponseRecorder, cost string, account int64) {
		t.Helper()
		if w.Code != 200 || strings.Contains(w.Body.String(), "gateway_error") {
			t.Fatalf("conversion %d %s", w.Code, w.Body.String())
		}
		var total, actual, upstream, inbound, model, upmodel string
		var logged int64
		err := a.DB.QueryRow(`SELECT total_cost::text,actual_cost::text,upstream_endpoint,inbound_endpoint,requested_model,upstream_model,account_id FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&total, &actual, &upstream, &inbound, &model, &upmodel, &logged)
		if err != nil || total != "0.3070000000" || actual != cost || upstream != "/v1/messages" || !strings.HasSuffix(inbound, "/chat/completions") || model != "public-anthropic" || upmodel != "anthropic-up" || logged != account {
			t.Fatal("billing", total, actual, upstream, inbound, model, upmodel, logged, err)
		}
	}
	first := call("POST", "/v1/chat/completions", key, body(false, false), "anthropic-json")
	check(first, "0.6140000000", aid)
	if !strings.Contains(first.Body.String(), `"model":"public-anthropic"`) || !strings.Contains(first.Body.String(), `"prompt_tokens":21`) || strings.Contains(first.Body.String(), "private-signature") {
		t.Fatal(first.Body.String())
	}
	for _, path := range []string{"/chat/completions", "/backend-api/codex/chat/completions"} {
		w := call("POST", path, key, body(false, false), "anthropic-json")
		if w.Body.String() != first.Body.String() || calls.Load() != 1 || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("alias replay", w.Code, w.Body.String())
		}
	}
	var balance, used string
	if err := a.DB.QueryRow(`SELECT balance::text,(SELECT quota_used::text FROM api_keys WHERE key=$2) FROM users WHERE id=$1`, uid, key).Scan(&balance, &used); err != nil || balance != "99.38600000" || used != "0.61400000" {
		t.Fatal("counters", balance, used, err)
	}
	for _, include := range []bool{false, true} {
		w := call("POST", "/v1/chat/completions", key, body(true, include), "")
		check(w, "0.6140000000", aid)
		if strings.Contains(w.Body.String(), `"usage"`) != include || strings.Contains(w.Body.String(), "message_stop") || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal(w.Body.String())
		}
	}
	priceChange.Store(1)
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.6140000000", aid)
	check(call("POST", "/v1/chat/completions", key, body(false, false), ""), "0.9210000000", aid)
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"rate_multiplier": 2})
	for _, m := range []int32{1, 2, 3, 4, 5} {
		mode.Store(m)
		for _, stream := range []bool{false, true} {
			if m >= 4 && !stream {
				continue
			}
			w := call("POST", "/v1/chat/completions", key, body(stream, true), "")
			if m <= 2 {
				check(w, "0.6140000000", aid)
				want := "length"
				if m == 2 {
					want = "content_filter"
				}
				if !strings.Contains(w.Body.String(), `"finish_reason":"`+want+`"`) {
					t.Fatal(w.Body.String())
				}
			} else {
				if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "gateway_error") || strings.Contains(w.Body.String(), "private-upstream-error") {
					t.Fatal("false success", w.Code, w.Body.String())
				}
				count, want := 0, 1
				if m == 3 {
					want = 0
				}
				if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != want {
					t.Fatal("failure usage", count, want, err)
				}
			}
		}
	}
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_anthropic_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed := call("POST", "/v1/chat/completions", key, body(true, true), "")
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_anthropic_receipt"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(failed.Body.String(), "[DONE]") || strings.Contains(failed.Body.String(), `"finish_reason":"tool_calls"`) || !strings.Contains(failed.Body.String(), "settlement failed") {
		t.Fatal(failed.Body.String())
	}
	before := calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND actual_cost=0.614", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("receipt recovery", count, err)
	}
	invalid := body(false, false)
	invalid["n"] = 2
	if w := call("POST", "/v1/chat/completions", key, invalid, ""); w.Code != 400 || calls.Load() != before {
		t.Fatal("invalid dispatched", w.Code)
	}
	// The same adapter serves configured relay/native Anthropic accounts on supported platforms.
	for _, platform := range []string{"openai", "kimi", "zhipu", "deepseek", "minimax"} {
		group := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Anthropic " + platform, "platform": platform, "rate_multiplier": 2, "model_pricing": []any{price(platform)}}))
		k := must("POST", "/api/v1/keys", user, map[string]any{"name": platform, "group_id": group})["key"].(string)
		upstream := account(platform, []int64{group})
		for _, stream := range []bool{false, true} {
			check(call("POST", "/v1/chat/completions", k, body(stream, true), ""), "0.6140000000", upstream)
		}
	}
	// Native failures can switch to the eligible Anthropic account within the same group.
	var retries atomic.Int32
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { retries.Add(1); w.WriteHeader(503) }))
	defer unavailable.Close()
	failedAccount := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Anthropic failover", "platform": "anthropic", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "retry-secret", "base_url": unavailable.URL}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"public-anthropic": {failedAccount}}})
	before = calls.Load()
	check(call("POST", "/v1/chat/completions", key, body(true, true), ""), "0.6140000000", aid)
	if retries.Load() != 1 || calls.Load() != before+1 {
		t.Fatal("converted account retry", retries.Load(), calls.Load()-before)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_routing_enabled": false})
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", failedAccount), admin, map[string]any{"schedulable": false})
	// Log and charge the actual mapped effort, while retaining what the client asked for.
	effortPrice := price("anthropic")
	effortPrice["reasoning_effort_multipliers"] = map[string]any{"xhigh": 9, "max": 1.5}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_pricing": []any{effortPrice}})
	reasoningBody := body(false, false)
	reasoningBody["reasoning_effort"] = "xhigh"
	w := call("POST", "/v1/chat/completions", key, reasoningBody, "")
	if w.Code != 200 {
		t.Fatal("reasoning", w.Body.String())
	}
	var requested, effective, actual string
	if err := a.DB.QueryRow("SELECT requested_reasoning_effort,reasoning_effort,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&requested, &effective, &actual); err != nil || requested != "xhigh" || effective != "max" || actual != "0.9210000000" {
		t.Fatal("effective effort", requested, effective, actual, err)
	}
	composite := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Anthropic composite", "platform": "composite", "rate_multiplier": 2, "model_pricing": []any{price("anthropic")}}))
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, composite}})
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", composite), admin, map[string]any{"public_model": "public-anthropic", "target_platform": "anthropic", "upstream_model": "public-anthropic", "endpoint": "chat_completions", "match_type": "exact", "enabled": true})
	ck := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite", "group_id": composite})["key"].(string)
	check(call("POST", "/v1/chat/completions", ck, body(true, true), ""), "0.6140000000", aid)
	if w := call("GET", "/v1/models", ck, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "public-anthropic") {
		t.Fatal("composite discovery", w.Code, w.Body.String())
	}
}
