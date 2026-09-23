package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGateway(t *testing.T, a *App, admin string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		// These independent admission scenarios exercise ordinary account ordering.
		req.Header.Set("Session-Id", randomToken(12))
		req.RemoteAddr = "192.0.2.10:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var e struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	var mode atomic.Int32
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-test-secret" || r.Header.Get("X-Api-Key") != "" {
			t.Error("upstream credentials not isolated")
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Error("invalid upstream path", r.URL.Path)
		}
		var body struct {
			Model   string `json:"model"`
			Stream  bool   `json:"stream"`
			Options struct {
				Usage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "upstream-model" {
			t.Error("mapping not applied", body.Model)
		}
		switch mode.Load() {
		case 1:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"key upstream-test-secret quota"}`))
			return
		case 2:
			w.WriteHeader(401)
			return
		case 3:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"no usage"}}]}`))
			return
		case 4:
			started <- struct{}{}
			select {
			case <-unblock:
			case <-r.Context().Done():
				return
			}
		}
		if body.Stream {
			if !body.Options.Usage {
				t.Error("stream usage was not requested")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"model\":\"response-model\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
			w.(http.Flusher).Flush()
			if mode.Load() == 5 {
				return
			}
			_, _ = fmt.Fprint(w, "data: {\"model\":\"response-model\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n")
			if mode.Load() != 6 {
				_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"up-1","model":"response-model","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`)
		}
	}))
	defer upstream.Close()
	user := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "gateway@example.test", "password": "correct-password", "balance": 1, "concurrency": 1})
	uid := int64(user["id"].(float64))
	upath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "gateway@example.test", "password": "correct-password"})["access_token"].(string)
	gid := int64(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Gateway group", "platform": "openai", "rate_multiplier": 1.25})["id"].(float64))
	groupPath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	// The input price makes the discounted total land exactly on an eight-decimal half.
	price := map[string]any{"platform": "openai", "models": []string{"mapped-model", "response-model"}, "input_price": json.Number("0.00000125"), "output_price": json.Number("0.000010"), "cache_read_price": json.Number("0.00000125")}
	channel := must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Gateway channel", "group_ids": []int64{gid}, "model_mapping": map[string]any{"openai": map[string]string{"client-model": "mapped-model"}}, "model_pricing": []any{price}})
	channelPath := fmt.Sprintf("/api/v1/admin/channels/%d", int64(channel["id"].(float64)))
	createAccount := func(name string, priority int) int64 {
		return int64(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "priority": priority, "rate_multiplier": 0.5, "credentials": map[string]any{"api_key": "upstream-test-secret", "base_url": upstream.URL + "/v1", "model_mapping": map[string]string{"mapped-model": "upstream-model"}}, "extra": map[string]any{"quota_limit": 10, "quota_daily_limit": 1, "quota_weekly_limit": 2}})["id"].(float64))
	}
	aid := createAccount("Gateway upstream", 1)
	accountPath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	keyData := must("POST", "/api/v1/keys", token, map[string]any{"name": "Gateway key", "group_id": gid, "quota": 1, "rate_limit_5h": 1, "rate_limit_1d": 1, "rate_limit_7d": 1, "ip_whitelist": []string{"192.0.2.0/24"}})
	key := keyData["key"].(string)
	kid := int64(keyData["id"].(float64))
	keyPath := fmt.Sprintf("/api/v1/keys/%d", kid)
	must("PUT", upath+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": 1, "weekly_limit_usd": 2, "monthly_limit_usd": 3}}})
	request := func(stream bool) map[string]any {
		return map[string]any{"model": "client-model", "stream": stream, "messages": []any{map[string]string{"role": "user", "content": "Reply OK"}}}
	}
	expect := func(code int, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		w := call("POST", "/v1/chat/completions", token, body)
		if w.Code != code {
			t.Fatalf("gateway got %d want %d: %s", w.Code, code, w.Body.String())
		}
		return w
	}
	expect(401, token, request(false))
	expect(401, "not-a-key", request(false))
	w := expect(200, key, request(false))
	if !strings.Contains(w.Body.String(), "OK") || !strings.Contains(w.Body.String(), "response-model") {
		t.Fatal(w.Body.String())
	}
	var balance, used, window, account, platform, total, actual string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text,k.usage_5h::text,a.extra->>'quota_used',pq.daily_usage_usd::text,l.total_cost::text,l.actual_cost::text FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts a ON a.id=$3 JOIN user_platform_quotas pq ON pq.user_id=u.id AND pq.platform='openai' JOIN usage_logs l ON l.request_id=$4 WHERE u.id=$1 AND k.id=$2`, uid, kid, aid, w.Header().Get("X-Request-ID")).Scan(&balance, &used, &window, &account, &platform, &total, &actual); err != nil {
		t.Fatal(err)
	}
	if balance != "0.99992187" || used != "0.00007813" || window != used || account != "0.00003125" || platform != "0.0000781250" || total != "0.0000625000" || actual != "0.0000781250" {
		t.Fatal("money mismatch", balance, used, window, account, platform, total, actual)
	}
	w = expect(200, key, request(true))
	if !strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), "error") {
		t.Fatal("stream failed", w.Body.String())
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	// Missing usage is never recorded as a zero-cost success.
	mode.Store(3)
	expect(502, key, request(false))
	mode.Store(5)
	w = expect(200, key, request(true))
	if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "error") {
		t.Fatal("truncated stream reported success")
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != 2 {
		t.Fatal("missing usage logged as free", count, err)
	}
	// A broken stream still bills usage that the upstream has already reported.
	mode.Store(6)
	w = expect(200, key, request(true))
	if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "error") {
		t.Fatal("incomplete metered stream reported success")
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 {
		t.Fatal("reported consumption lost on interruption", count, err)
	}
	mode.Store(0)
	must("PUT", keyPath, token, map[string]any{"ip_blacklist": []string{"192.0.2.10"}})
	expect(403, key, request(false))
	must("PUT", keyPath, token, map[string]any{"ip_blacklist": []string{}})
	must("PUT", keyPath, token, map[string]any{"expires_at": "2020-01-01T00:00:00Z"})
	expect(401, key, request(false))
	must("PUT", keyPath, token, map[string]any{"expires_at": ""})
	must("PUT", upath, admin, map[string]any{"status": "disabled"})
	expect(401, key, request(false))
	must("PUT", upath, admin, map[string]any{"status": "active"})
	// Platform zero means disabled, while null means unlimited.
	must("PUT", upath+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": 0}}})
	expect(429, key, request(false))
	must("PUT", upath+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "weekly_limit_usd": 2}}})
	expect(200, key, request(false))
	must("POST", upath+"/platform-quotas/reset", admin, map[string]any{"platform": "openai", "window": "weekly"})
	// A price change during a request cannot change the captured tariff.
	mode.Store(4)
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() { completed <- call("POST", "/v1/chat/completions", key, request(false)) }()
	<-started
	before := calls.Load()
	expect(429, key, request(false))
	if calls.Load() != before {
		t.Fatal("concurrency overflow reached upstream")
	}
	must("PUT", channelPath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"mapped-model", "response-model"}, "input_price": 1, "output_price": 1, "cache_read_price": 1}}})
	close(unblock)
	w = <-completed
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&actual); err != nil || actual != "0.0000781250" {
		t.Fatal("request pricing snapshot changed", actual, err)
	}
	mode.Store(0)
	must("PUT", channelPath, admin, map[string]any{"model_pricing": []any{price}, "billing_model_source": "response_model"})
	expect(200, key, request(false))
	// RPM is counted before dispatch, and disabled groups revoke the key's route immediately.
	must("PUT", groupPath, admin, map[string]any{"rpm_limit": 1})
	expect(429, key, request(false))
	must("PUT", groupPath, admin, map[string]any{"rpm_limit": 0, "status": "inactive"})
	expect(403, key, request(false))
	must("PUT", groupPath, admin, map[string]any{"status": "active"})
	// Exhaustion changes only key status; explicit reset can reactivate it.
	token = must("POST", "/api/v1/auth/login", "", map[string]any{"email": "gateway@example.test", "password": "correct-password"})["access_token"].(string)
	must("PUT", keyPath, token, map[string]any{"quota": 0.00001, "reset_quota": true})
	expect(200, key, request(false))
	expect(429, key, request(false))
	must("PUT", keyPath, token, map[string]any{"quota": 1, "reset_quota": true})
	expect(200, key, request(false))
	// Database failures roll back the entire settlement and leave a replayable receipt.
	if _, err := a.DB.Exec(`ALTER TABLE usage_logs ADD CONSTRAINT test_gateway_billing_failure CHECK(model<>'response-model') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE id=$1", uid).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	w = expect(503, key, request(false))
	failedID := w.Header().Get("X-Request-ID")
	var after string
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE id=$1", uid).Scan(&after); err != nil || after != balance {
		t.Fatal("settlement partially committed", balance, after, err)
	}
	pending, err := a.Redis.HGet(ctx, "gateway:pending-billing", failedID).Result()
	if err != nil || strings.Contains(pending, "upstream-test-secret") || strings.Contains(pending, "Reply OK") {
		t.Fatal("pending receipt missing or leaking", err)
	}
	if _, err = a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_gateway_billing_failure"); err != nil {
		t.Fatal(err)
	}
	if err = a.recoverReceipts(ctx); err != nil {
		t.Fatal(err)
	}
	var receipt usageReceipt
	if err = json.Unmarshal([]byte(pending), &receipt); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.applyReceipt(ctx, &receipt); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if err = a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", failedID).Scan(&count); err != nil || count != 1 {
		t.Fatal("dedup failed", count, err)
	}
	receipt.Usage.Output++
	receipt.Fingerprint = receipt.fingerprint()
	if err = a.applyReceipt(ctx, &receipt); err == nil {
		t.Fatal("fingerprint conflict accepted")
	}
	receipt.Usage.Output--
	receipt.Fingerprint = receipt.fingerprint()
	if _, err = a.DB.Exec("INSERT INTO usage_billing_dedup_archive(request_id,api_key_id,request_fingerprint,created_at) SELECT request_id,api_key_id,request_fingerprint,created_at FROM usage_billing_dedup WHERE request_id=$1;", failedID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.Exec("DELETE FROM usage_billing_dedup WHERE request_id=$1", failedID); err != nil {
		t.Fatal(err)
	}
	if err = a.applyReceipt(ctx, &receipt); err != nil {
		t.Fatal("archived dedup", err)
	}
	// Successful work can overdraw a positive starting balance; debt must not be lost.
	must("POST", upath+"/balance", admin, map[string]any{"operation": "set", "balance": json.Number("0.00000001")})
	expect(200, key, request(false))
	expect(402, key, request(false))
	must("POST", upath+"/balance", admin, map[string]any{"operation": "set", "balance": 1})
	// Status and cooldown updates never copy upstream error bodies or credentials.
	mode.Store(1)
	expect(503, key, request(false))
	var limited bool
	if err = a.DB.QueryRow("SELECT rate_limit_reset_at>now() FROM accounts WHERE id=$1", aid).Scan(&limited); err != nil || !limited {
		t.Fatal("cooldown missing", err)
	}
	must("POST", accountPath+"/clear-rate-limit", admin, map[string]any{})
	mode.Store(2)
	expect(502, key, request(false))
	var status string
	if err = a.DB.QueryRow("SELECT status FROM accounts WHERE id=$1", aid).Scan(&status); err != nil || status != "error" {
		t.Fatal(status, err)
	}
	must("POST", accountPath+"/clear-error", admin, map[string]any{})
	mode.Store(0)
	if err = a.DB.QueryRow("SELECT count(*) FROM ops_error_logs WHERE user_id=$1 AND (error_message LIKE '%secret%' OR error_body IS NOT NULL)", uid).Scan(&count); err != nil || count != 0 {
		t.Fatal("error log secrets", err)
	}
	// Allowlist checks use the client's model name before channel/account rewriting.
	must("PUT", groupPath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"mapped-model"}}})
	expect(403, key, request(false))
	must("PUT", groupPath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"client-*"}}})
	expect(200, key, request(false))
	must("PUT", groupPath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	// Request replay returns the original response without repeating upstream work or billing.
	idempotent := func(body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		return w
	}
	for _, stream := range []bool{false, true} {
		before := calls.Load()
		first := idempotent(request(stream), fmt.Sprintf("replay-%v", stream))
		second := idempotent(request(stream), fmt.Sprintf("replay-%v", stream))
		if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before+1 {
			t.Fatal("request replay failed", first.Code, second.Code, first.Body.String(), second.Body.String())
		}
	}
	changed := request(false)
	changed["messages"] = []any{map[string]string{"role": "user", "content": "different"}}
	if w := idempotent(changed, "replay-false"); w.Code != 409 {
		t.Fatal("request fingerprint conflict", w.Code, w.Body.String())
	}
	// An expired claim left by a crashed process is uncertain, never safe to resend.
	if _, err := a.DB.Exec("UPDATE idempotency_records SET status='processing',expires_at=now()-interval '1 day' WHERE scope=$1 AND idempotency_key_hash=$2", fmt.Sprintf("gateway.chat.%d", kid), digest("replay-false")); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if w := idempotent(request(false), "replay-false"); w.Code != 409 || calls.Load() != before {
		t.Fatal("uncertain request was sent again", w.Code)
	}
	// User usage queries always scope by authenticated owner, even with query overrides.
	usageList := must("GET", "/api/v1/usage", token, nil)
	if usageList["total"].(float64) < 1 {
		t.Fatal("usage query empty")
	}
	firstUsage := usageList["items"].([]any)[0].(map[string]any)
	if _, ok := firstUsage["account_id"]; ok {
		t.Fatal("upstream account leaked in user usage")
	}
	detail := fmt.Sprintf("/api/v1/usage/%d", int64(firstUsage["id"].(float64)))
	if w := call("GET", detail, admin, nil); w.Code != 404 {
		t.Fatal("cross-user detail exposed", w.Code)
	}
	if w := call("GET", "/api/v1/usage?user_id="+fmt.Sprint(uid), admin, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal("user filter escaped owner", w.Body.String())
	}
	if w := call("GET", "/api/v1/admin/usage", token, nil); w.Code != 403 {
		t.Fatal("nonadmin usage access", w.Code)
	}
	if w := call("GET", "/v1/billing", key, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "quota_used") {
		t.Fatal("billing introspection", w.Code, w.Body.String())
	}
	// 5h expiration resets only its window on the next charge; total quota remains cumulative.
	if _, err := a.DB.Exec("UPDATE api_keys SET rate_limit_5h=0.01,usage_5h=0.01,window_5h_start=now() WHERE id=$1", kid); err != nil {
		t.Fatal(err)
	}
	expect(429, key, request(false))
	if _, err := a.DB.Exec("UPDATE api_keys SET window_5h_start=now()-interval '6 hours' WHERE id=$1", kid); err != nil {
		t.Fatal(err)
	}
	expect(200, key, request(false))
	if err := a.DB.QueryRow("SELECT usage_5h::text FROM api_keys WHERE id=$1", kid).Scan(&window); err != nil || window != "0.00007813" {
		t.Fatal("expired key window", window, err)
	}
	// A rate-limited first account may fail over only before output is sent.
	retryCalls := atomic.Int32{}
	retryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		retryCalls.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	}))
	defer retryServer.Close()
	retryAccount := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Retry upstream", "platform": "openai", "type": "apikey", "priority": 0, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "retry-secret", "base_url": retryServer.URL, "model_mapping": map[string]string{"mapped-model": "upstream-model"}}})
	before = calls.Load()
	expect(200, key, request(true))
	if retryCalls.Load() != 1 || calls.Load() != before+1 {
		t.Fatal("bounded failover did not select backup", retryCalls.Load(), calls.Load()-before)
	}
	must("DELETE", fmt.Sprintf("/api/v1/admin/accounts/%d", int64(retryAccount["id"].(float64))), admin, nil)
	// With upstream-based restriction, skip an unpriced candidate and use a priced account.
	upstreamPrice := map[string]any{"platform": "openai", "models": []string{"upstream-model"}, "input_price": json.Number("0.00000125"), "output_price": json.Number("0.000010"), "cache_read_price": json.Number("0.00000125")}
	must("PUT", channelPath, admin, map[string]any{"billing_model_source": "upstream", "restrict_models": true, "model_pricing": []any{upstreamPrice}})
	unpriced := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Unpriced upstream", "platform": "openai", "type": "apikey", "priority": 0, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "retry-secret", "base_url": retryServer.URL, "model_mapping": map[string]string{"mapped-model": "unpriced-model"}}})
	previousRetries := retryCalls.Load()
	expect(200, key, request(false))
	if retryCalls.Load() != previousRetries {
		t.Fatal("restricted upstream model was dispatched")
	}
	must("DELETE", fmt.Sprintf("/api/v1/admin/accounts/%d", int64(unpriced["id"].(float64))), admin, nil)
	must("PUT", channelPath, admin, map[string]any{"billing_model_source": "response_model", "restrict_models": false, "model_pricing": []any{price}})

	// Cancellation reaches the upstream and releases both concurrency slots.
	cancelStarted := make(chan struct{})
	cancelObserved := make(chan struct{})
	cancelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(cancelStarted)
		select {
		case <-r.Context().Done():
			close(cancelObserved)
		case <-time.After(5 * time.Second):
		}
	}))
	defer cancelServer.Close()
	must("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"base_url": cancelServer.URL}})
	cancelCtx, cancelRequest := context.WithCancel(context.Background())
	rawRequest, _ := json.Marshal(request(false))
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(rawRequest)).WithContext(cancelCtx)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("Authorization", "Bearer "+key)
	canceledDone := make(chan struct{})
	go func() { defer close(canceledDone); a.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-cancelStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation request did not reach upstream")
	}
	cancelRequest()
	select {
	case <-cancelObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	<-canceledDone
	a.gatewayMu.Lock()
	active := len(a.gatewayActive)
	a.gatewayMu.Unlock()
	if active != 0 {
		t.Fatal("concurrency slot leaked", active)
	}
	must("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"base_url": upstream.URL}})
	// Account quota configuration changes preserve consumption and enforce expiration.
	if _, err := a.DB.Exec(`UPDATE accounts SET extra=extra||'{"quota_daily_used":1,"quota_daily_start":"2099-01-01T00:00:00Z"}' WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	expect(503, key, request(false))
	if _, err := a.DB.Exec(`UPDATE accounts SET extra=extra||'{"quota_daily_start":"2020-01-01T00:00:00Z"}' WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	expect(200, key, request(false))
	if err := a.DB.QueryRow("SELECT extra->>'quota_daily_used' FROM accounts WHERE id=$1", aid).Scan(&account); err != nil || account != "0.00003125" {
		t.Fatal("account day reset", account, err)
	}

	// No key limit configured means the legacy counters remain inactive.
	noLimit := must("POST", "/api/v1/keys", token, map[string]any{"name": "unlimited counters", "group_id": gid})
	expect(200, noLimit["key"].(string), request(false))
	if err = a.DB.QueryRow("SELECT quota_used::text FROM api_keys WHERE id=$1", int64(noLimit["id"].(float64))).Scan(&used); err != nil || used != "0.00000000" {
		t.Fatal("unconfigured quota counter changed", used, err)
	}
	// Queries use the authenticated owner; requested IDs never broaden that scope.
	foreign := must("POST", "/api/v1/keys", admin, map[string]any{"name": "Query ownership", "group_id": gid})
	foreignID := int64(foreign["id"].(float64))
	costs := must("POST", "/api/v1/usage/dashboard/api-keys-usage", token, map[string]any{"api_key_ids": []int64{kid, kid, foreignID}})["stats"].(map[string]any)
	if len(costs) != 1 || costs[fmt.Sprint(kid)].(map[string]any)["total_actual_cost"].(float64) <= 0 {
		t.Fatal("key costs escaped owner or lost consumption", costs)
	}
	emptyCosts := must("POST", "/api/v1/usage/dashboard/api-keys-usage", token, map[string]any{"api_key_ids": []int64{}})["stats"].(map[string]any)
	if len(emptyCosts) != 0 {
		t.Fatal("empty key selection was broadened")
	}
	if w := call("POST", "/api/v1/usage/dashboard/api-keys-usage", token, map[string]any{"api_key_ids": make([]int64, 101)}); w.Code != 400 {
		t.Fatal("key cost query bound", w.Code)
	}
	dailyPath := fmt.Sprintf("/api/v1/user/api-keys/%d/usage/daily", kid)
	daily := must("GET", dailyPath+"?days=1&timezone=UTC", token, nil)
	points := daily["items"].([]any)
	if len(points) != 1 || points[0].(map[string]any)["actual_cost"].(float64) <= 0 {
		t.Fatal("daily usage missing", daily)
	}
	for _, path := range []string{dailyPath + "?days=91", dailyPath + "?timezone=Invalid/Zone"} {
		if w := call("GET", path, token, nil); w.Code != 400 {
			t.Fatal("daily query validation", path, w.Code)
		}
	}
	if w := call("GET", dailyPath, admin, nil); w.Code != 403 {
		t.Fatal("daily usage ownership bypass", w.Code)
	}
	// Old consumption is excluded from the 30-day total while raw logs remain intact.
	if _, err = a.DB.Exec("UPDATE usage_logs SET created_at=now()-interval '31 days' WHERE api_key_id=$1", kid); err != nil {
		t.Fatal(err)
	}
	costs = must("POST", "/api/v1/usage/dashboard/api-keys-usage", token, map[string]any{"api_key_ids": []int64{kid}})["stats"].(map[string]any)
	if costs[fmt.Sprint(kid)].(map[string]any)["total_actual_cost"].(float64) != 0 {
		t.Fatal("30-day total reinterpreted as lifetime total")
	}
	audit := must("GET", "/api/v1/admin/audit-logs?q=scheduled-test-plans&method=post&success=true&page_size=500", admin, nil)
	if audit["total"].(float64) < 1 || audit["page_size"].(float64) != 200 {
		t.Fatal("audit filters or page size", audit)
	}
	entry := audit["items"].([]any)[0].(map[string]any)
	if entry["method"] != "POST" || entry["status_code"].(float64) >= 400 || !strings.Contains(entry["path"].(string), "scheduled-test-plans") {
		t.Fatal("audit filter mismatch", entry)
	}
	auditPath := fmt.Sprintf("/api/v1/admin/audit-logs/%d", int64(entry["id"].(float64)))
	must("GET", auditPath, admin, nil)
	if w := call("GET", auditPath, token, nil); w.Code != 403 {
		t.Fatal("nonadmin audit access", w.Code)
	}
	if w := call("GET", "/api/v1/admin/audit-logs?actor_user_id=invalid", admin, nil); w.Code != 400 {
		t.Fatal("audit ID validation", w.Code)
	}
	for _, path := range []string{"/api/v1/admin/usage/search-users?q=gateway", "/api/v1/admin/usage/search-api-keys?user_id=" + fmt.Sprint(uid)} {
		w := call("GET", path, admin, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id"`) || strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), "password") {
			t.Fatal("usage search failed or exposed credentials", w.Code)
		}
		if w := call("GET", path, token, nil); w.Code != 403 {
			t.Fatal("nonadmin usage search", w.Code)
		}
	}
}
