package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUpstreamBoundaries(t *testing.T) {
	a := &App{}
	for _, addr := range []string{"127.0.0.1", "::1", "169.254.169.254", "10.0.0.1", "100.100.100.200", "::ffff:127.0.0.1", "0.0.0.0", "224.0.0.1", "2001:db8::1"} {
		if a.allowedAddress(netip.MustParseAddr(addr)) {
			t.Fatalf("unsafe destination accepted: %s", addr)
		}
	}
	for _, raw := range []string{"file:///etc/passwd", "http://user:pass@example.com", "https://example.com?token=secret", "https://example.com/#private", "https://example.com/../secret", "https://example.com:99999"} {
		if _, err := parseUpstreamURL(raw); err == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	for _, tc := range []struct{ base, path, want string }{{"https://example.com", "/v1/chat/completions", "https://example.com/v1/chat/completions"}, {"https://example.com/v1/", "/v1/chat/completions", "https://example.com/v1/chat/completions"}, {"https://example.com/api/paas/v4", "/v1/models", "https://example.com/api/paas/v4/models"}, {"https://example.com/relay", "/v1/responses", "https://example.com/relay/v1/responses"}} {
		got, err := upstreamURL(tc.base, tc.path)
		if err != nil || got != tc.want {
			t.Fatalf("URL join: %s %v", got, err)
		}
	}
	u := &upstreamAccount{Credentials: map[string]json.RawMessage{"model_mapping": json.RawMessage(`{"gpt-*":"broad","gpt-5*":"narrow","gpt-5.6":"exact"}`)}}
	for model, want := range map[string]string{"gpt-5.6": "exact", "gpt-5.7": "narrow", "gpt-4": "broad"} {
		got, err := u.mappedModel(model)
		if err != nil || got != want {
			t.Fatal(model, got, err)
		}
	}
	if _, err := u.mappedModel("unknown"); err == nil {
		t.Fatal("model whitelist bypass")
	}
	from := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	next, err := nextTestRun("*/30 * * * *", from)
	if err != nil || !next.Equal(from.Add(30*time.Minute)) {
		t.Fatal(next, err)
	}
	for _, expr := range []string{"bad", "* * * * * *", "0 0 31 2 *"} {
		if _, err = nextTestRun(expr, from); err == nil {
			t.Fatal("invalid cron accepted", expr)
		}
	}
}

// Uses the same isolated installation as the identity lifecycle test.
func testUpstreamManagement(t *testing.T, a *App, admin, user string, gid int64) {
	t.Helper()
	ctx := context.Background()
	request := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		if body == nil {
			raw = nil
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.RemoteAddr = "192.0.2.30:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	call := func(method, path, token string, body any, want int) json.RawMessage {
		t.Helper()
		w := request(method, path, token, body)
		if w.Code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body)
		}
		raw := json.RawMessage(w.Body.Bytes())
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(raw, &envelope)
		if envelope.Data != nil {
			return envelope.Data
		}
		return raw
	}
	object := func(raw json.RawMessage) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err, string(raw))
		}
		return out
	}
	var calls atomic.Int64
	var httpStatus atomic.Int64
	httpStatus.Store(200)
	secret := "integration-upstream-secret"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret && r.Header.Get("x-api-key") != secret && r.Header.Get("x-goog-api-key") != secret {
			t.Error("incorrect credential forwarding")
			w.WriteHeader(401)
			return
		}
		if httpStatus.Load() != 200 {
			w.WriteHeader(int(httpStatus.Load()))
			fmt.Fprint(w, secret)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			fmt.Fprint(w, `{"data":[{"id":"test-model"}]}`)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid upstream body")
		}
		switch {
		case r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":4,"completion_tokens":1,"total_tokens":5}}`)
		case r.URL.Path == "/v1/messages":
			if r.Header.Get("anthropic-version") != "2023-06-01" {
				t.Error("missing Anthropic version")
			}
			fmt.Fprint(w, `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":4,"output_tokens":1}}`)
		case r.URL.Path == "/v1/responses":
			fmt.Fprint(w, `{"output":[{"content":[{"type":"output_text","text":"OK"}]}]}`)
		case strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`)
		default:
			t.Error("incorrect protocol path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	input := func(platform string) map[string]any {
		return map[string]any{"name": "Verification", "platform": platform, "type": "apikey", "credentials": map[string]any{"api_key": secret, "base_url": upstream.URL}}
	}
	call("POST", "/api/v1/admin/accounts", user, input("openai"), 403)
	badInput := input("openai")
	badInput["type"] = "oauth"
	call("POST", "/api/v1/admin/accounts", admin, badInput, 400)
	badInput = input("openai")
	badInput["credentials"].(map[string]any)["account_mode"] = "coding"
	call("POST", "/api/v1/admin/accounts", admin, badInput, 400)
	badInput = input("openai")
	badInput["credentials"].(map[string]any)["base_url"] = "http://169.254.169.254"
	call("POST", "/api/v1/admin/accounts", admin, badInput, 400)
	in := input("openai")
	in["group_ids"] = []int64{gid}
	in["extra"] = map[string]any{"quota_limit": 10}
	accountRaw := call("POST", "/api/v1/admin/accounts", admin, in, 200)
	if strings.Contains(string(accountRaw), secret) {
		t.Fatal("account credential leaked")
	}
	account := object(accountRaw)
	id := int64(account["id"].(float64))
	path := fmt.Sprintf("/api/v1/admin/accounts/%d", id)
	call("GET", path+"/models", admin, nil, 200)
	if _, err := a.DB.ExecContext(ctx, `UPDATE accounts SET extra=extra || '{"quota_used":3.125,"unknown_state":{"version":1}}'::jsonb WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	changed := object(call("PUT", path, admin, map[string]any{"extra": map[string]any{"quota_limit": 20}, "credentials": map[string]any{"model_mapping": map[string]string{"alias": "test-model"}}}, 200))
	if changed["extra"].(map[string]any)["quota_used"] != 3.125 || changed["extra"].(map[string]any)["unknown_state"] == nil {
		t.Fatal("account update lost counters or unknown state")
	}
	call("PUT", path, admin, map[string]any{"extra": map[string]any{"quota_used": 0}}, 400)
	w := request("POST", path+"/test", admin, map[string]any{"model_id": "alias"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"test_complete"`) || !strings.Contains(w.Body.String(), `"text":"OK"`) {
		t.Fatal("manual test failed", w.Body)
	}
	for _, platform := range []string{"anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		obj := object(call("POST", "/api/v1/admin/accounts", admin, input(platform), 200))
		p := fmt.Sprintf("/api/v1/admin/accounts/%.0f/test", obj["id"])
		w = request("POST", p, admin, map[string]any{"model_id": "test-model"})
		if !strings.Contains(w.Body.String(), `"test_complete"`) {
			t.Fatalf("%s probe failed: %s", platform, w.Body)
		}
	}
	responseInput := input("openai")
	responseInput["credentials"].(map[string]any)["api_protocol"] = "responses"
	obj := object(call("POST", "/api/v1/admin/accounts", admin, responseInput, 200))
	w = request("POST", fmt.Sprintf("/api/v1/admin/accounts/%.0f/test", obj["id"]), admin, map[string]any{"model_id": "test-model"})
	if !strings.Contains(w.Body.String(), `"test_complete"`) {
		t.Fatal("Responses probe failed", w.Body)
	}
	// HTTP proxy receives the API request, with proxy credentials only on that hop.
	var proxyCalls atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if ip, err := netip.ParseAddr(r.URL.Hostname()); err != nil || !ip.IsLoopback() {
			t.Error("proxy target was not pinned", r.RequestURI)
		}
		if r.Header.Get("Proxy-Authorization") == "" {
			t.Error("missing proxy authorization")
		}
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("upstream authorization lost")
		}
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer proxy.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	portNumber, _ := strconv.Atoi(port)
	p := object(call("POST", "/api/v1/admin/proxies", admin, map[string]any{"name": "Proxy", "protocol": "http", "host": host, "port": portNumber, "username": "proxy-user", "password": "proxy-secret"}, 200))
	pid := int64(p["id"].(float64))
	proxyPath := fmt.Sprintf("/api/v1/admin/proxies/%d", pid)
	if p["password"] != nil || p["has_password"] != true {
		t.Fatal("proxy secret exposed")
	}
	call("PUT", path, admin, map[string]any{"proxy_id": pid}, 200)
	call("PUT", path, admin, map[string]any{"credentials": map[string]any{"base_url": strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1)}}, 200)
	call("GET", path+"/models", admin, nil, 200)
	if proxyCalls.Load() != 1 {
		t.Fatal("configured proxy not used")
	}
	call("DELETE", proxyPath, admin, nil, 409)
	call("PUT", proxyPath, admin, map[string]any{"fallback_mode": "proxy", "backup_proxy_id": pid}, 400)
	call("PUT", path, admin, map[string]any{"proxy_id": 0}, 200)
	call("PUT", path, admin, map[string]any{"credentials": map[string]any{"base_url": upstream.URL}}, 200)
	call("DELETE", proxyPath, admin, nil, 200)
	// Redirects never resend credentials to a second destination.
	var redirected atomic.Int64
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer sink.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	loaded, err := a.loadAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Credentials["base_url"], _ = json.Marshal(redirect.URL)
	probe := a.runAccountTest(ctx, loaded, accountTestInput{Model: "alias"})
	if probe.Status != "failed" || redirected.Load() != 0 {
		t.Fatal("redirect followed with credential")
	}
	// Plan CRUD uses the established raw JSON shape.
	plan := object(call("POST", "/api/v1/admin/scheduled-test-plans", admin, map[string]any{"account_id": id, "model_id": "alias", "cron_expression": "*/30 * * * *", "auto_recover": true, "max_results": 2}, 200))
	planID := int64(plan["id"].(float64))
	planPath := fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d", planID)
	call("GET", path+"/scheduled-test-plans", admin, nil, 200)
	call("PUT", planPath, admin, map[string]any{"cron_expression": "bad"}, 400)
	due := func() {
		t.Helper()
		if _, err := a.DB.ExecContext(ctx, "UPDATE scheduled_test_plans SET next_run_at=now()-interval '1 minute' WHERE id=$1", planID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.DB.ExecContext(ctx, "UPDATE accounts SET status='error',error_message='temporary',rate_limit_reset_at=now()+interval '1 hour',updated_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	due()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.runDueTests(ctx); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != before+1 {
		t.Fatal("duplicate plan execution", calls.Load()-before)
	}
	restored := object(call("GET", path, admin, nil, 200))
	if restored["status"] != "active" || restored["rate_limit_reset_at"] != nil || restored["extra"].(map[string]any)["quota_used"] != 3.125 {
		t.Fatal("invalid automatic recovery", restored)
	}
	call("POST", path+"/schedulable", admin, map[string]any{"schedulable": false}, 200)
	if _, err = a.DB.ExecContext(ctx, "UPDATE accounts SET status='error',updated_at=now() WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	due()
	if err = a.runDueTests(ctx); err != nil {
		t.Fatal(err)
	}
	restored = object(call("GET", path, admin, nil, 200))
	if restored["schedulable"] != false || restored["status"] != "error" {
		t.Fatal("test overrode manual unschedulable")
	}
	httpStatus.Store(401)
	due()
	if err = a.runDueTests(ctx); err != nil {
		t.Fatal(err)
	}
	results := call("GET", planPath+"/results", admin, nil, 200)
	var rows []accountTestResult
	if err = json.Unmarshal(results, &rows); err != nil || len(rows) != 2 || rows[0].Status != "failed" || strings.Contains(string(results), secret) {
		t.Fatal("result retention or error redaction failed", string(results), err)
	}
	call("PUT", planPath, admin, map[string]any{"enabled": false}, 200)
	before = calls.Load()
	due()
	if err = a.runDueTests(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before {
		t.Fatal("disabled plan executed")
	}
	call("DELETE", planPath, admin, nil, 200)
	call("GET", planPath+"/results", admin, nil, 404)
	var n int
	if err = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1", planID).Scan(&n); err != nil || n != 0 {
		t.Fatal("plan results not cascaded", n, err)
	}
	// Editing a plan while its request is in flight must win over the old runner.
	httpStatus.Store(200)
	started, release := make(chan struct{}), make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer blocking.Close()
	call("PUT", path, admin, map[string]any{"credentials": map[string]any{"base_url": blocking.URL}}, 200)
	call("POST", path+"/schedulable", admin, map[string]any{"schedulable": true}, 200)
	plan = object(call("POST", "/api/v1/admin/scheduled-test-plans", admin, map[string]any{"account_id": id, "model_id": "alias", "cron_expression": "*/30 * * * *", "auto_recover": true}, 200))
	planID = int64(plan["id"].(float64))
	planPath = fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d", planID)
	due()
	runDone := make(chan error, 1)
	go func() { runDone <- a.runDueTests(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("test did not start")
	}
	edited := object(call("PUT", planPath, admin, map[string]any{"enabled": false, "cron_expression": "0 0 * * *"}, 200))
	close(release)
	if err = <-runDone; err != nil {
		t.Fatal(err)
	}
	var enabled bool
	var next time.Time
	expectedNext, parseErr := time.Parse(time.RFC3339, edited["next_run_at"].(string))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if err = a.DB.QueryRowContext(ctx, "SELECT enabled,next_run_at FROM scheduled_test_plans WHERE id=$1", planID).Scan(&enabled, &next); err != nil || enabled || !next.Equal(expectedNext) {
		t.Fatal("runner overwrote edited schedule", enabled, next, err)
	}
	restored = object(call("GET", path, admin, nil, 200))
	if restored["status"] != "error" {
		t.Fatal("disabled plan still recovered account")
	}
	call("DELETE", planPath, admin, nil, 200)
	// Prove the actual background loop finds an overdue plan without a manual call.
	call("PUT", path, admin, map[string]any{"credentials": map[string]any{"base_url": upstream.URL}}, 200)
	plan = object(call("POST", "/api/v1/admin/scheduled-test-plans", admin, map[string]any{"account_id": id, "model_id": "alias", "cron_expression": "*/30 * * * *"}, 200))
	planID = int64(plan["id"].(float64))
	planPath = fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d", planID)
	due()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err = a.DB.QueryRowContext(ctx, "SELECT count(*) FROM scheduled_test_results WHERE plan_id=$1", planID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background worker did not execute due plan")
		}
		time.Sleep(100 * time.Millisecond)
	}
	call("DELETE", planPath, admin, nil, 200)
	// Context cancellation ends the request and releases the transport.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer slow.Close()
	loaded.Credentials["base_url"], _ = json.Marshal(slow.URL)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	probe = a.runAccountTest(cctx, loaded, accountTestInput{Model: "alias"})
	if probe.Status != "failed" || !strings.Contains(probe.Error, "timed out") {
		t.Fatal("canceled request remained successful", probe)
	}
	call("DELETE", path, admin, nil, 200)
	call("GET", path, admin, nil, 404)
}
