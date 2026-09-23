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

func TestReasoningPolicy(t *testing.T) {
	p := reasoningPolicy{MaxEffort: "high", OverLimit: "downgrade", Mappings: []effortMapping{
		{From: "none", To: "max"}, {From: "xhigh", To: "low"},
		{From: "xhigh", To: "medium", Match: "prefix", Model: "family"},
		{From: "xhigh", To: "high", Match: "suffix", Model: "long-model"},
		{From: "xhigh", To: "max", Match: "exact", Model: "family-long-model"},
	}}
	for _, tc := range []struct{ model, input, want string }{
		{"other", "xhigh", "low"}, {"family-other", "xhigh", "medium"},
		{"other-long-model", "xhigh", "high"}, {"FAMILY-LONG-MODEL", "Extra_High", "high"},
		{"other", "none", "high"}, {"other", "max", "high"}, {"other", "low", "low"},
		{"other", "future-value", "future-value"}, {"other", "deny", "deny"},
	} {
		for _, field := range []string{"reasoning", "reasoning_effort", "output_config"} {
			var body map[string]json.RawMessage
			raw, _ := json.Marshal(map[string]any{field: tc.input})
			if field != "reasoning_effort" {
				raw, _ = json.Marshal(map[string]any{field: map[string]any{"effort": tc.input, "keep": 123}})
			}
			_ = json.Unmarshal(raw, &body)
			if err := p.apply(body, tc.model, "openai"); err != nil {
				t.Fatal(err)
			}
			var value string
			if field == "reasoning_effort" {
				_ = json.Unmarshal(body[field], &value)
			} else {
				var nested map[string]json.RawMessage
				_ = json.Unmarshal(body[field], &nested)
				_ = json.Unmarshal(nested["effort"], &value)
				if string(nested["keep"]) != "123" {
					t.Fatal("unknown nested field removed")
				}
			}
			if value != tc.want {
				t.Fatal("effort policy", tc, field, value)
			}
		}
	}
	for _, raw := range []string{`{}`, `{"reasoning":null}`, `{"reasoning":{"summary":"auto"}}`, `{"reasoning_effort":""}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		before, _ := json.Marshal(body)
		if err := p.apply(body, "m", "openai"); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(body)
		if !bytes.Equal(before, after) {
			t.Fatal("policy synthesized effort", raw, string(after))
		}
	}
	p.OverLimit = "deny"
	body := map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"max"`)}
	if err := p.apply(body, "m", "openai"); err == nil {
		t.Fatal("ceiling deny ignored")
	}
	p.Mappings = []effortMapping{{From: "max", To: "low"}, {From: "low", To: "deny"}}
	if err := p.apply(body, "m", "openai"); err != nil || string(body["reasoning_effort"]) != `"low"` {
		t.Fatal("mapping chained or ceiling ran first", err)
	}
	if err := p.apply(body, "m", "openai"); err == nil {
		t.Fatal("mapping deny ignored")
	}
	p = reasoningPolicy{MaxEffort: "minimal"}
	body["reasoning_effort"] = json.RawMessage(`"max"`)
	if err := p.apply(body, "m", "anthropic"); err != nil || string(body["reasoning_effort"]) != `"low"` {
		t.Fatal("composite minimal must adapt to Anthropic", err)
	}
	body["reasoning_effort"] = json.RawMessage(`"max"`)
	if err := p.apply(body, "m", "deepseek"); err != nil || string(body["reasoning_effort"]) != `"max"` {
		t.Fatal("policy applied outside supported platforms", err)
	}
	for _, tc := range []struct{ platform, raw string }{
		{"anthropic", `{"max_reasoning_effort":"minimal"}`}, {"openai", `{"max_reasoning_effort":"none"}`},
		{"gemini", `{"max_reasoning_effort":"high"}`}, {"deepseek", `{"max_reasoning_effort_over_limit":"deny"}`},
		{"openai", `{"max_reasoning_effort_over_limit":"ignore"}`}, {"openai", `{"reasoning_effort_mappings":[{"from":"high","to":"none"}]}`},
		{"openai", `{"reasoning_effort_mappings":[{"from":"high","to":"low","model":"m","match_type":"regex"}]}`},
		{"openai", `{"reasoning_effort_mappings":[{"from":"high","to":"low","model":"M"},{"from":"HIGH","to":"max","model":"m","match_type":"exact"}]}`},
	} {
		var in groupInput
		if err := json.Unmarshal([]byte(tc.raw), &in); err != nil {
			t.Fatal(err)
		}
		if in.validateReasoning(tc.platform) == nil {
			t.Fatal("invalid reasoning configuration accepted", tc)
		}
	}
	for _, tc := range []struct{ body, model, want string }{
		{`{"reasoning":{"effort":" MAX "}}`, "m", "max"}, {`{}`, "provider/model-high", "high"},
		{`{"output_config":{"effort":"extrahigh"}}`, "m", "xhigh"}, {`{"reasoning_effort":"none"}`, "m-high", ""},
		{`{"reasoning_effort":"unknown"}`, "m-high", ""},
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tc.body), &body)
		got := requestedEffort(body, tc.model)
		if got == nil && tc.want != "" || got != nil && *got != tc.want {
			t.Fatal("requested effort", tc, got)
		}
	}
	for _, raw := range []string{`{"reasoning_effort":"HIGH"}`, `{"reasoning":{"summary":"auto"},"reasoning_effort":"high"}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if value, err := requestEffort(body, "responses"); err != nil || value != "high" {
			t.Fatal("Responses top-level effort compatibility", value, err)
		}
	}
}

func testReasoningPolicy(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.96:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
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
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	idOf := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "reasoning@example.test", "password": "reasoning-password", "balance": "10"}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "reasoning@example.test", "password": "reasoning-password"})["access_token"].(string)
	var calls atomic.Int32
	var want atomic.Value
	want.Store("high")
	var changeGroup atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		protocol := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer effort-")
		if r.Header.Get("X-Api-Key") != "" {
			protocol = "anthropic"
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		got, err := requestEffort(body, protocol)
		if err != nil || got != want.Load().(string) || string(body["model"]) != `"up-effort"` {
			t.Error("forwarded effort or model", got, err, string(body["model"]))
		}
		if gid := changeGroup.Swap(0); gid != 0 {
			if _, err := a.DB.Exec("UPDATE groups SET max_reasoning_effort='low' WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		var response, terminal string
		switch protocol {
		case "responses":
			response = fmt.Sprintf(`{"id":"resp_effort_%d","object":"response","status":"completed","model":"up-effort","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n)
			terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
		case "anthropic":
			response = `{"id":"msg_effort","type":"message","role":"assistant","model":"up-effort","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
			terminal = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":" + response + "}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		default:
			response = `{"id":"effort","model":"up-effort","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
			terminal = "data: " + response + "\n\ndata: [DONE]\n\n"
		}
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, terminal)
		} else {
			fmt.Fprint(w, response)
		}
	}))
	defer up.Close()
	for _, test := range []struct{ protocol, group string }{{"chat_completions", "openai"}, {"responses", "openai"}, {"anthropic", "anthropic"}, {"responses", "composite"}, {"anthropic", "composite"}} {
		protocol := test.protocol
		platform := "openai"
		if protocol == "anthropic" {
			platform = "anthropic"
		}
		gid := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Effort " + protocol + test.group, "platform": test.group, "max_reasoning_effort": "HIGH", "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"public-effort"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0", "reasoning_effort_multipliers": map[string]any{"high": 2, "medium": 3, "low": 4}}}}))
		gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		if test.group == "composite" {
			must("POST", gp+"/composite-routes", admin, map[string]any{"public_model": "public-effort", "target_platform": platform})
		}
		key := must("POST", "/api/v1/keys", user, map[string]any{"name": "effort", "group_id": gid})["key"].(string)
		must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Effort " + protocol, "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"base_url": up.URL, "api_key": "effort-" + protocol, "api_protocol": protocol, "model_mapping": map[string]string{"public-effort": "up-effort"}}})
		path := "/v1/chat/completions"
		body := map[string]any{"model": "public-effort", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
		setEffort := func(value string) {
			switch protocol {
			case "responses":
				body["reasoning"] = map[string]any{"effort": value, "summary": "auto"}
			case "anthropic":
				body["output_config"] = map[string]any{"effort": value}
			default:
				body["reasoning_effort"] = value
			}
		}
		if protocol == "responses" {
			path = "/v1/responses"
			delete(body, "messages")
			body["input"] = "ok"
		}
		if protocol == "anthropic" {
			path = "/v1/messages"
			body["max_tokens"] = 16
		}
		setEffort("max")
		want.Store("high")
		check := func(w *httptest.ResponseRecorder, original, effective, cost string) {
			t.Helper()
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var requested *string
			var effort, actual string
			if err := a.DB.QueryRow(`SELECT requested_reasoning_effort,reasoning_effort,actual_cost::text FROM usage_logs WHERE request_id=$1 AND user_id=$2`, w.Header().Get("X-Request-ID"), uid).Scan(&requested, &effort, &actual); err != nil {
				t.Fatal(err)
			}
			if effort != effective || actual != cost || original == "" && requested != nil || original != "" && (requested == nil || *requested != original) {
				t.Fatal("requested/effective effort or cost", requested, effort, actual, original, effective, cost)
			}
		}
		w := call("POST", path, key, body, "policy-replay")
		check(w, "max", "high", "0.0400000000")
		must("PUT", gp, admin, map[string]any{"max_reasoning_effort_over_limit": "deny"})
		before := calls.Load()
		if replay := call("POST", path, key, body, "policy-replay"); replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
			t.Fatal("policy edit broke completed replay", replay.Code)
		}
		if denied := call("POST", path, key, body, ""); denied.Code != 403 || calls.Load() != before {
			t.Fatal("deny called upstream", denied.Code)
		}
		must("PUT", gp, admin, map[string]any{"reasoning_effort_mappings": []effortMapping{{From: "max", To: "medium", Model: "public-effort", Match: "exact"}, {From: "medium", To: "low"}}})
		want.Store("medium")
		check(call("POST", path, key, body, ""), "max", "medium", "0.0600000000")
		body["stream"] = true
		check(call("POST", path, key, body, ""), "max", "medium", "0.0600000000")
		delete(body, "stream")
		must("PUT", gp, admin, map[string]any{"reasoning_effort_mappings": []effortMapping{{From: "max", To: "deny"}}})
		before = calls.Load()
		if denied := call("POST", path, key, body, ""); denied.Code != 403 || calls.Load() != before {
			t.Fatal("mapping deny called upstream", denied.Code)
		}
		must("PUT", gp, admin, map[string]any{"reasoning_effort_mappings": []any{}, "max_reasoning_effort_over_limit": "downgrade"})
		want.Store("high")
		// Change the group policy after sending; settlement must keep the forwarded value.
		changeGroup.Store(gid)
		check(call("POST", path, key, body, ""), "max", "high", "0.0400000000")
		want.Store("low")
		check(call("POST", path, key, body, ""), "max", "low", "0.0800000000")
		if protocol == "chat_completions" {
			if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_effort_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
				t.Fatal(err)
			}
			failed := call("POST", path, key, body, "")
			if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_effort_receipt"); err != nil {
				t.Fatal(err)
			}
			if failed.Code != 503 {
				t.Fatal("settlement failure was not reported", failed.Code)
			}
			if err := a.recoverReceipts(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := a.recoverReceipts(t.Context()); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND requested_reasoning_effort='max' AND reasoning_effort='low' AND actual_cost=0.08", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 {
				t.Fatal("receipt lost effort or duplicated settlement", count, err)
			}
		}
		for _, field := range []string{"reasoning", "reasoning_effort", "output_config"} {
			delete(body, field)
		}
		want.Store("")
		check(call("POST", path, key, body, ""), "", "", "0.0200000000")
		// The response schema includes the original/effective effort in ordinary user views.
		usage := call("GET", "/api/v1/usage?model=public-effort", user, nil, "")
		if usage.Code != 200 || !strings.Contains(usage.Body.String(), `"requested_reasoning_effort":"max"`) {
			t.Fatal("user usage omitted requested effort", usage.Code, usage.Body.String())
		}
		if denied := call("PUT", gp, user, map[string]any{"max_reasoning_effort": "max"}, ""); denied.Code != 403 {
			t.Fatal("user edited policy", denied.Code)
		}
		if rejected := call("PUT", gp, admin, map[string]any{"reasoning_effort_mappings": []any{map[string]any{"from": "high", "to": "low", "unknown": true}}}, ""); rejected.Code != 400 {
			t.Fatal("unknown policy field accepted", rejected.Code)
		}
	}
}
