package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

func TestAnthropicPolicy(t *testing.T) {
	s := defaultBetaSettings()
	for _, tc := range []struct{ model, want string }{
		{"claude-sonnet-5", "context-1m-2025-08-07,other"}, {"claude-sonnet-5-20260924", "context-1m-2025-08-07,other"},
		{"claude-sonnet-50", "other"}, {"claude-opus-5", "other"}, {"CLAUDE-SONNET-5", "other"},
	} {
		got, err := s.apply("fast-mode-2026-02-01, context-1m-2025-08-07, other,other", tc.model)
		if err != nil || got != tc.want {
			t.Fatal(tc.model, got, err)
		}
	}
	s.Rules = []betaRule{{Token: "x", Action: "filter", Scope: "apikey"}, {Token: "x", Action: "block", Scope: "all", Message: "disabled"}}
	if _, err := s.apply("x", "m"); err == nil || err.Error() != "disabled" {
		t.Fatal("block must inspect original tokens despite earlier filter", err)
	}
	s.Rules = []betaRule{{Token: "x", Action: "pass", Scope: "all", Models: []string{"yes*"}, Fallback: "block", FallbackMessage: "model denied"}}
	if got, err := s.apply("x-extra", "no"); err != nil || got != "x-extra" {
		t.Fatal("token prefix matched", got, err)
	}
	if _, err := s.apply("x", "no"); err == nil || err.Error() != "model denied" {
		t.Fatal("model fallback", err)
	}
	s.Rules[0].Fallback = ""
	if got, err := s.apply("x", "no"); err != nil || got != "x" {
		t.Fatal("default fallback", got, err)
	}
	for _, raw := range []string{`{"rules":[{}]}`, `{"rules":[{"beta_token":"x,y","action":"filter","scope":"all"}]}`, `{"rules":[{"beta_token":"x","action":"pass","scope":"oauth"}]}`, `{"rules":[{"beta_token":"x","action":"pass","scope":"apikey","model_whitelist":["x*y"]}]}`} {
		var s betaSettings
		if json.Unmarshal([]byte(raw), &s) != nil || s.validate() == nil {
			t.Fatal("accepted invalid policy", raw)
		}
	}
	r := defaultRectifierSettings()
	r.Patterns = []string{" ", " Custom Error "}
	if r.validate() != nil || len(r.Patterns) != 1 || r.Patterns[0] != "Custom Error" || r.APIKey {
		t.Fatal("rectifier defaults/normalization", r)
	}
	const original = `{"model":"claude-test","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"kept","signature":"bad"},{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"tool","name":"f","input":{"n":9007199254740993}}]}],"context_management":{"edits":[{"type":"clear_thinking_20251015"},{"type":"other"}]}}`
	if _, changed := r.rectify([]byte(original), "claude-test", "invalid signature"); changed {
		t.Fatal("API key signature correction enabled by default")
	}
	r.APIKey = true
	for _, model := range []string{"deepseek-v4", "kimi-test", "glm-test", "MiniMax-M3", "unknown"} {
		if got, changed := r.rectify([]byte(original), model, "invalid signature"); changed || string(got) != original {
			t.Fatal("modified third-party thinking", model)
		}
	}
	for _, msg := range []string{"invalid signature", "CUSTOM ERROR", "expected thinking", "thinking block must contain thinking", "text content blocks must be non-empty"} {
		got, changed := r.rectify([]byte(original), "claude-test", msg)
		if !changed || !bytes.Contains(got, []byte(`"text":"kept"`)) || !bytes.Contains(got, []byte("9007199254740993")) || bytes.Contains(got, []byte("opaque")) || bytes.Contains(got, []byte("clear_thinking")) || bytes.Contains(got, []byte(`"thinking"`)) || !bytes.Contains(got, []byte(`"type":"tool_use"`)) {
			t.Fatal("signature correction", msg, string(got))
		}
		if _, changed = r.rectify(got, "claude-test", msg); changed {
			t.Fatal("unchanged correction retried")
		}
	}
	for _, msg := range []string{"thinking.budget_tokens must be >= 1024", "thinking budget tokens input should be at least 1024", "must be greater than 1024 to reserve tokens for a final answer: baseten reasoning is enabled"} {
		got, changed := r.rectify([]byte(original), "claude-test", msg)
		if !changed || !bytes.Contains(got, []byte(`"budget_tokens":32000`)) || !bytes.Contains(got, []byte(`"max_tokens":64000`)) || !bytes.Contains(got, []byte(`"signature":"bad"`)) {
			t.Fatal("budget correction", string(got))
		}
		if _, changed = r.rectify(got, "claude-test", msg); changed {
			t.Fatal("budget retried without changes")
		}
	}
	for _, msg := range []string{"context length exceeded", "thinking budget_tokens exceeded", "signature"} {
		if _, changed := r.rectify([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), "claude-test", msg); changed {
			t.Fatal("unrelated error retried", msg)
		}
	}
	if _, changed := r.rectify([]byte(`{"thinking":{"type":"adaptive"}}`), "claude-test", "thinking budget_tokens >= 1024"); changed {
		t.Fatal("adaptive mode modified")
	}
}

func TestAnthropicRetryBoundary(t *testing.T) {
	var calls atomic.Int32
	var mode atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch mode.Load() {
		case 1:
			w.Header().Set("Location", "/must-not-follow")
			w.WriteHeader(307)
		case 2:
			c, _, _ := w.(http.Hijacker).Hijack()
			c.Close()
			return
		case 3:
			w.WriteHeader(200)
		default:
			w.WriteHeader(400)
		}
		fmt.Fprint(w, `{"error":{"message":"thinking budget_tokens >= 1024"}}`)
	}))
	defer up.Close()
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	base, _ := json.Marshal(up.URL)
	u := &upstreamAccount{Platform: "anthropic", Credentials: map[string]json.RawMessage{"base_url": base, "api_key": json.RawMessage(`"test-key"`)}}
	body := []byte(`{"model":"claude-test","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1}}`)
	for _, tc := range []struct{ mode, status, count int32 }{{0, 400, 2}, {1, 307, 1}, {2, 0, 1}, {3, 200, 1}} {
		mode.Store(tc.mode)
		before := calls.Load()
		resp, err := a.upstreamRequest(context.Background(), u, "POST", "/v1/messages", body)
		if resp != nil {
			resp.Body.Close()
		}
		if calls.Load()-before != tc.count || (err != nil) != (tc.status == 0) || resp != nil && resp.StatusCode != int(tc.status) {
			t.Fatal("unconfirmed rejection retried", tc, resp, err, calls.Load()-before)
		}
	}
	mode.Store(0)
	before := calls.Load()
	resp, err := a.upstreamRequest(context.Background(), u, "POST", "/v1/messages/count_tokens", body)
	if err != nil || calls.Load()-before != 1 {
		t.Fatal("counting retried", err)
	}
	resp.Body.Close()
	// A closed database proves read failures cannot authorize request mutation.
	db, err := sql.Open("postgres", "")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	a.DB = db
	before = calls.Load()
	resp, err = a.upstreamRequest(context.Background(), u, "POST", "/v1/messages", body)
	if err != nil || calls.Load()-before != 1 {
		t.Fatal("settings failure retried", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(data, []byte("budget_tokens")) {
		t.Fatal("error body lost")
	}
	before = calls.Load()
	_, err = a.upstreamRequestHeaders(context.Background(), u, "POST", "/v1/messages", body, http.Header{"Anthropic-Beta": []string{"x"}})
	if err == nil || calls.Load() != before {
		t.Fatal("unavailable beta policy allowed dispatch", err)
	}
}

func testAnthropicPolicy(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	defer a.DB.Exec("DELETE FROM settings WHERE key IN ($1,$2)", betaSetting, rectifierSetting)
	call := func(method, path, token string, body any, beta []string, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		for _, value := range beta {
			r.Header.Add("Anthropic-Beta", value)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	expect := func(code int, method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, nil, "")
		if w.Code != code {
			t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, code, w.Body.String())
		}
		var result struct{ Data map[string]any }
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		return result.Data
	}
	const root = "/api/v1/admin/settings/"
	for _, name := range []string{"beta-policy", "rectifier"} {
		expect(401, "GET", root+name, "", nil)
		expect(403, "GET", root+name, ordinary, nil)
		expect(403, "PUT", root+name, ordinary, map[string]any{})
		expect(400, "PUT", root+name, admin, nil)
		expect(400, "PUT", root+name, admin, map[string]any{"unknown": true})
	}
	if s := expect(200, "GET", root+"rectifier", admin, nil); s["enabled"] != true || s["apikey_signature_enabled"] != false || len(s["apikey_signature_patterns"].([]any)) != 0 {
		t.Fatal("rectifier default", s)
	}
	if s := expect(200, "GET", root+"beta-policy", admin, nil); len(s["rules"].([]any)) != 2 {
		t.Fatal("beta defaults", s)
	}
	for _, key := range []string{betaSetting, rectifierSetting} {
		if _, err := a.DB.Exec("INSERT INTO settings(key,value) VALUES($1,'bad json') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", key); err != nil {
			t.Fatal(err)
		}
	}
	expect(200, "GET", root+"beta-policy", admin, nil)
	expect(200, "GET", root+"rectifier", admin, nil)
	rules := betaSettings{Rules: []betaRule{{Token: "drop", Action: "filter", Scope: "all"}, {Token: "deny", Action: "block", Scope: "apikey", Models: []string{"claude-*"}, Message: "disabled beta"}}}
	expect(200, "PUT", root+"beta-policy", admin, rules)
	rectifier := defaultRectifierSettings()
	expect(200, "PUT", root+"rectifier", admin, rectifier)
	fresh := &App{DB: a.DB}
	if s, err := fresh.loadBetaSettings(ctx); err != nil || len(s.Rules) != 2 || s.Rules[0].Token != "drop" {
		t.Fatal("persisted policy", s, err)
	}
	var calls, mode atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("X-Api-Key") != "policy-secret" || r.Header.Get("Authorization") != "" || strings.Contains(r.Header.Get("Anthropic-Beta"), "drop") || strings.Contains(r.Header.Get("Anthropic-Beta"), "deny") {
			t.Error("policy or credential isolation failed")
		}
		m := mode.Load()
		if m == 1 || m == 3 || m == 4 || m == 5 || m == 7 || m == 8 {
			if m != 1 || body["thinking"] != nil {
				status := 400
				if m == 5 {
					status = 403
				}
				w.WriteHeader(status)
				if m == 7 {
					fmt.Fprint(w, strings.Repeat("x", (64<<10)+1))
				} else if m == 8 {
					fmt.Fprint(w, `{"error":{"message":"unrelated error policy-secret"}}`)
				} else {
					fmt.Fprint(w, `{"error":{"message":"invalid signature policy-secret"}}`)
				}
				return
			}
		}
		if m == 2 || m == 6 {
			var thinking map[string]json.RawMessage
			_ = json.Unmarshal(body["thinking"], &thinking)
			if string(thinking["budget_tokens"]) != "32000" || m == 6 {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":{"message":"thinking.budget_tokens must be >= 1024"}}`)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			fmt.Fprint(w, `{"input_tokens":3}`)
			return
		}
		const message = `{"type":"message","id":"msg_policy","model":"claude-policy","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":"+message+"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			fmt.Fprint(w, message)
		}
	}))
	defer up.Close()
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	manage := func(path string, body any) map[string]any { return expect(200, "POST", path, admin, body) }
	gid := id(manage("/api/v1/admin/groups", map[string]any{"name": "policy", "platform": "anthropic"}))
	manage("/api/v1/admin/channels", map[string]any{"name": "policy", "group_ids": []int64{gid}, "model_pricing": []map[string]any{{"platform": "anthropic", "models": []string{"*"}, "billing_mode": "token", "input_price": "0.000001", "output_price": "0.000002", "cache_read_price": "0.000001", "cache_write_price": "0.000001"}}})
	aid := id(manage("/api/v1/admin/accounts", map[string]any{"name": "policy", "platform": "anthropic", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "policy-secret", "base_url": up.URL, "model_mapping": map[string]string{"public": "claude-policy", "third": "deepseek-v4"}}}))
	uid := id(manage("/api/v1/admin/users", map[string]any{"email": "policy@example.test", "password": "policy-password", "balance": 100}))
	user := expect(200, "POST", "/api/v1/auth/login", "", map[string]any{"email": "policy@example.test", "password": "policy-password"})["access_token"].(string)
	keyInfo := expect(200, "POST", "/api/v1/keys", user, map[string]any{"name": "policy", "group_id": gid, "quota": 100})
	key, kid := keyInfo["key"].(string), id(keyInfo)
	request := map[string]any{"model": "public", "max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1}, "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	for _, path := range []string{"/v1/messages", "/messages/count_tokens"} {
		before := calls.Load()
		w := call("POST", path, key, request, []string{"drop", "deny"}, "")
		if w.Code != 400 || calls.Load() != before || !strings.Contains(w.Body.String(), "disabled beta") {
			t.Fatal("block across multiple header lines/aliases", path, w.Code, w.Body.String())
		}
	}
	success := 0
	for _, tc := range []struct {
		mode       int32
		on, stream bool
		model      string
		code       int
		calls      int32
	}{
		{0, false, false, "public", 200, 1}, {1, false, false, "public", 502, 1},
		{1, true, false, "public", 200, 2}, {1, true, true, "public", 200, 2},
		{3, true, false, "public", 502, 2}, {4, true, false, "third", 502, 1},
		{5, true, false, "public", 502, 1}, {2, false, false, "public", 200, 2},
		{6, false, false, "public", 502, 2}, {7, true, false, "public", 502, 1}, {8, true, false, "public", 502, 1},
	} {
		mode.Store(tc.mode)
		rectifier.APIKey = tc.on
		expect(200, "PUT", root+"rectifier", admin, rectifier)
		request["model"], request["stream"] = tc.model, tc.stream
		before := calls.Load()
		idem := randomToken(12)
		w := call("POST", "/v1/messages", key, request, []string{"drop", "keep"}, idem)
		if w.Code != tc.code || calls.Load()-before != tc.calls || strings.Contains(w.Body.String(), "policy-secret") {
			t.Fatal("rectifier request", tc, w.Code, calls.Load()-before, w.Body.String())
		}
		if tc.code == 200 {
			success++
		}
		if !tc.stream {
			before = calls.Load()
			replay := call("POST", "/v1/messages", key, request, []string{"drop", "keep"}, idem)
			if replay.Code != w.Code || calls.Load() != before || replay.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatal("corrected response replay", tc, replay.Code, replay.Body.String())
			}
		}
		if tc.mode == 5 {
			expect(200, "PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"status": "active"})
		}
	}
	mode.Store(0)
	expect(200, "POST", "/messages/count_tokens", key, request)
	mode.Store(2)
	before := calls.Load()
	chat := map[string]any{"model": "public", "max_completion_tokens": 2048, "reasoning_effort": "low", "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	expect(200, "POST", "/v1/chat/completions", key, chat)
	if calls.Load()-before != 2 {
		t.Fatal("converted request bypassed rectifier")
	}
	success++
	// A health request reaches the same transport boundary but never user billing.
	u, err := a.loadAccount(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if result := a.runAccountTest(ctx, u, accountTestInput{Model: "public"}); result.Status != "success" || calls.Load()-before != 2 {
		t.Fatal("health rectification", result, calls.Load()-before)
	}
	var count int
	var balance, used string
	if err = a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != success {
		t.Fatal("retry produced extra receipts", count, success, err)
	}
	if err = a.DB.QueryRow("SELECT balance::text,(SELECT quota_used::text FROM api_keys WHERE id=$2) FROM users WHERE id=$1", uid, kid).Scan(&balance, &used); err != nil || rat(json.Number(balance)).RatString() != rat(json.Number(fmt.Sprintf("%.8f", 100-float64(success)*0.000014))).RatString() || rat(json.Number(used)).RatString() != rat(json.Number(fmt.Sprintf("%.8f", float64(success)*0.000014))).RatString() {
		t.Fatal("corrective retry accounting", balance, used, err)
	}
	var audited int
	if err = a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE path=$1 AND status_code=200", root+"rectifier").Scan(&audited); err != nil || audited < 1 {
		t.Fatal("settings audit", audited, err)
	}
}
