package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultAccountTestModels(t *testing.T) {
	for _, tc := range []struct{ platform, protocol, model string }{
		{"openai", "chat_completions", "gpt-5.4"}, {"openai", "responses", "gpt-5.4"},
		{"openai", "anthropic", "gpt-5.4"}, {"anthropic", "", "claude-sonnet-4-5-20250929"},
		{"gemini", "", "gemini-2.0-flash"}, {"grok", "", "grok-4.5"},
		{"kimi", "responses", "gpt-5.4"}, {"zhipu", "anthropic", "claude-sonnet-4-5-20250929"},
		{"deepseek", "chat_completions", "gpt-5.4"}, {"minimax", "anthropic", "claude-sonnet-4-5-20250929"},
	} {
		protocol, _ := json.Marshal(tc.protocol)
		u := &upstreamAccount{Platform: tc.platform, Credentials: map[string]json.RawMessage{"api_protocol": protocol}}
		model, mode, err := prepareAccountTest(u, accountTestInput{})
		if err != nil || model != tc.model || mode != "text" {
			t.Fatal("default model", tc, model, mode, err)
		}
		u.Credentials["model_mapping"], _ = json.Marshal(map[string]string{tc.model: "mapped-model"})
		if model, _, err = prepareAccountTest(u, accountTestInput{Mode: "text"}); err != nil || model != "mapped-model" {
			t.Fatal("default did not pass through mapping", tc, model, err)
		}
		u.Credentials["model_mapping"] = json.RawMessage(`{"another-model":"mapped-model"}`)
		if _, _, err = prepareAccountTest(u, accountTestInput{}); err == nil {
			t.Fatal("default bypassed allowlist", tc)
		}
	}
	from := time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC)
	next, err := nextTestRun("CRON_TZ=Asia/Shanghai 0 9 * * *", from)
	if err != nil || !next.Equal(time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC)) {
		t.Fatal("explicit cron timezone", next, err)
	}
}

func testPlanLifecycle(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	const root = "/api/v1/admin/scheduled-test-plans"
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.228:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	planReply := func(w *httptest.ResponseRecorder) testPlan {
		t.Helper()
		var p testPlan
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.ID == 0 {
			t.Fatalf("plan response: %d %s", w.Code, w.Body.String())
		}
		return p
	}
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "team-default" || r.URL.Path != "/v1/chat/completions" {
			t.Error("default model dispatch")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer up.Close()
	w := call("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Plan lifecycle", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "plan-test-key", "base_url": up.URL, "model_mapping": map[string]string{"gpt-5.4": "team-default"}}})
	var account struct{ Data struct{ ID int64 } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &account) != nil {
		t.Fatal("plan account", w.Code)
	}
	aid := account.Data.ID
	defer a.DB.Exec("DELETE FROM scheduled_test_plans WHERE account_id=$1", aid)
	defer a.DB.Exec("UPDATE accounts SET deleted_at=now(),status='inactive',schedulable=false WHERE id=$1", aid)
	input := map[string]any{"account_id": aid, "cron_expression": "0 * * * *", "max_results": 0, "auto_recover": true}
	plan := planReply(call("POST", root, admin, input))
	path := fmt.Sprintf("%s/%d", root, plan.ID)
	if plan.Model != "" || plan.MaxResults != 50 || !plan.Enabled || !plan.AutoRecover || plan.NextRun == nil {
		t.Fatal("plan defaults changed", plan)
	}
	if w = call("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/test", aid), admin, map[string]any{}); w.Code != 200 || !strings.Contains(w.Body.String(), `"test_complete"`) {
		t.Fatal("manual default model test failed", w.Code, w.Body.String())
	}
	if err := a.runTestPlan(context.Background(), plan); err != nil || calls.Load() != 2 {
		t.Fatal("scheduled default model dispatch", calls.Load(), err)
	}
	for _, raw := range []string{"", "null", "{", "{} {}", `{"unknown":true}`} {
		r := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/test", aid), strings.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+admin)
		r.RemoteAddr = "192.0.2.228:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if raw == "" {
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"test_complete"`) {
				t.Fatal("empty-body health test failed", w.Code)
			}
		} else if w.Code != 400 {
			t.Fatal("malformed health request invoked default model", raw, w.Code)
		}
	}
	if calls.Load() != 3 {
		t.Fatal("invalid body dispatched test", calls.Load())
	}
	// Omitted/null fields retain their values; an empty model restores defaults.
	planReply(call("PUT", path, admin, map[string]any{"model_id": "explicit-model", "max_results": 2}))
	plan = planReply(call("PUT", path, admin, map[string]any{"model_id": "", "auto_recover": nil}))
	if plan.Model != "" || plan.MaxResults != 2 || !plan.AutoRecover || plan.LastRun == nil {
		t.Fatal("partial plan update", plan)
	}
	for _, body := range []any{map[string]any{"account_id": aid + 1}, map[string]any{"model_id": "../bad"}, map[string]any{"model_id": strings.Repeat("x", 101)}, map[string]any{"max_results": -1}, map[string]any{"cron_expression": "0 0 31 2 *"}} {
		if w := call("PUT", path, admin, body); w.Code != 400 {
			t.Fatal("invalid plan edit accepted", body, w.Code)
		}
	}
	for _, endpoint := range []struct{ method, path string }{{"POST", root}, {"PUT", path}, {"DELETE", path}, {"GET", path + "/results"}, {"GET", fmt.Sprintf("/api/v1/admin/accounts/%d/scheduled-test-plans", aid)}} {
		if w := call(endpoint.method, endpoint.path, ordinary, input); w.Code != 403 {
			t.Fatal("plan permission boundary", endpoint, w.Code)
		}
	}
	// Hold the account row as a deleting transaction does. Both create and edit
	// must wait here, then observe deletion; neither may leave an enabled plan.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE accounts SET deleted_at=now(),status='inactive',schedulable=false WHERE id=$1", aid); err != nil {
		t.Fatal(err)
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- call("POST", root, admin, input) }()
	go func() { results <- call("PUT", path, admin, map[string]any{"enabled": true}) }()
	for {
		select {
		case w := <-results:
			t.Fatal("plan mutation bypassed account lock", w.Code)
		default:
		}
		var waiting int
		if err = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query=$1", "SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE").Scan(&waiting); err != nil {
			t.Fatal("waiting plan mutations", err)
		}
		if waiting == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE scheduled_test_plans SET enabled=false,updated_at=now() WHERE account_id=$1", aid); err != nil {
		t.Fatal("account/plan lock order", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case w := <-results:
			if w.Code != 404 {
				t.Fatal("plan accepted deleted account", w.Code)
			}
		case <-ctx.Done():
			t.Fatal("plan mutation remained blocked")
		}
	}
	var total, enabled int
	if err = a.DB.QueryRow("SELECT count(*),count(*) FILTER(WHERE enabled) FROM scheduled_test_plans WHERE account_id=$1", aid).Scan(&total, &enabled); err != nil || total != 1 || enabled != 0 {
		t.Fatal("deleted account retained enabled/new plan", total, enabled, err)
	}
}
