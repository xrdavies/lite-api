package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/xrdavies/lite-api/schema"
)

// Isolate manual recovery/failure injection from autonomous worker scans.
// Worker lifecycle tests still start their own runner explicitly.
func pauseTestWorkers(a *App) func() {
	a.workerCancel()
	for _, done := range []chan struct{}{a.workerDone, a.imageWorkerDone, a.batchWorkerDone, a.videoWorkerDone, a.responseWorkerDone, a.balanceWorkerDone, a.billingWorkerDone, a.proxyWorkerDone} {
		<-done
	}
	return a.startWorkers
}

func TestIdentityKeysAndBalance(t *testing.T) {
	databaseURL, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if databaseURL == "" || redisURL == "" {
		t.Skip("run scripts/test-integration.py for isolated PostgreSQL and Redis")
	}
	ctx := context.Background()
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = schema.Initialize(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = schema.Initialize(ctx, db); err == nil {
		t.Fatal("reinitialized a nonempty database")
	}
	if err = schema.Validate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Bootstrap(ctx, db, "admin@example.test", "correct-password"); err != nil {
		t.Fatal(err)
	}
	if err = Bootstrap(ctx, db, "another@example.test", "correct-password"); err == nil {
		t.Fatal("bootstrap replaced an existing installation")
	}
	a, err := New(ctx, Config{DatabaseURL: databaseURL, RedisURL: redisURL, JWTSecret: strings.Repeat("a", 32), UpstreamPrivateCIDRs: "127.0.0.1/32,::1/128"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if second, err := New(ctx, Config{DatabaseURL: databaseURL, RedisURL: redisURL, JWTSecret: strings.Repeat("a", 32)}); err == nil {
		second.Close()
		t.Fatal("two service instances acquired the same database")
	}
	call := func(method, path, token string, body any, headers map[string]string) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		if body == nil {
			raw = nil
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.RemoteAddr = "192.0.2.10:1234"
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		var envelope map[string]any
		json.Unmarshal(w.Body.Bytes(), &envelope)
		return w.Code, envelope
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		code, result := call(method, path, token, body, nil)
		if code != 200 {
			t.Fatalf("%s %s: %d %v", method, path, code, result)
		}
		if data, ok := result["data"].(map[string]any); ok {
			return data
		}
		return result
	}
	expect := func(want int, method, path, token string, body any) {
		t.Helper()
		code, result := call(method, path, token, body, nil)
		if code != want {
			t.Fatalf("%s %s: got %d want %d: %v", method, path, code, want, result)
		}
	}
	login := func(email, password string) map[string]any {
		return must("POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": password})
	}
	expect(404, "POST", "/api/v1/auth/register", "", map[string]string{"email": "not-allowed@example.test", "password": "password"})
	expect(401, "POST", "/api/v1/auth/login", "", map[string]string{"email": "admin@example.test", "password": "wrong"})
	admin := login("admin@example.test", "correct-password")["access_token"].(string)
	user := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "user@example.test", "password": "correct-password", "balance": 100, "username": "Team user"})
	uid := int64(user["id"].(float64))
	upath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	another := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "another@example.test", "password": "correct-password"})
	_ = another
	tokens := login("user@example.test", "correct-password")
	userToken := tokens["access_token"].(string)
	refresh := tokens["refresh_token"].(string)
	otherToken := login("another@example.test", "correct-password")["access_token"].(string)
	testKeyManagement(t, a, admin)
	group := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Private team", "platform": "openai", "is_exclusive": true})
	gid := int64(group["id"].(float64))
	expect(403, "POST", "/api/v1/keys", userToken, map[string]any{"name": "unauthorized group", "group_id": gid})
	must("PUT", upath, admin, map[string]any{"allowed_groups": []int64{gid}, "group_rates": map[string]any{fmt.Sprint(gid): 0.75}})
	privateKey := must("POST", "/api/v1/keys", userToken, map[string]any{"name": "authorized group", "group_id": gid})
	if privateKey["group_id"] != float64(gid) {
		t.Fatal("group binding missing")
	}
	expect(403, "POST", "/api/v1/keys", otherToken, map[string]any{"name": "other user", "group_id": gid})
	expect(400, "POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Subscription", "subscription_type": "subscription"})
	expect(400, "POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Unsupported", "platform": "bedrock"})
	// Updating an allowed field must preserve unrelated JSON configuration.
	if _, err = db.ExecContext(ctx, `UPDATE groups SET model_routing='{"future-key":"preserved"}'::jsonb WHERE id=$1`, gid); err != nil {
		t.Fatal(err)
	}
	changedGroup := must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"description": "Team only"})
	if changedGroup["model_routing"].(map[string]any)["future-key"] != "preserved" {
		t.Fatal("unknown JSON key lost")
	}
	expect(403, "GET", "/api/v1/admin/users", userToken, nil)
	expect(400, "PUT", "/api/v1/user", userToken, map[string]any{"role": "admin"})
	key := must("POST", "/api/v1/keys", userToken, map[string]any{"name": "team", "quota": 10.12345678, "ip_whitelist": []string{"192.0.2.0/24"}})
	kid := int64(key["id"].(float64))
	kpath := fmt.Sprintf("/api/v1/keys/%d", kid)
	expect(401, "GET", "/api/v1/user/profile", key["key"].(string), nil)
	expect(404, "GET", kpath, otherToken, nil)
	expect(404, "PUT", kpath, otherToken, map[string]any{"name": "stolen"})
	expect(404, "DELETE", kpath, otherToken, nil)
	expect(400, "PUT", fmt.Sprintf("/api/v1/admin/api-keys/%d", kid), admin, map[string]any{"name": "forbidden admin edit"})
	expect(404, "POST", "/api/v1/admin/api-keys", admin, map[string]any{"user_id": uid, "name": "not allowed"})
	expect(400, "POST", "/api/v1/keys", userToken, map[string]any{"name": "bad", "ip_blacklist": []string{"not-an-ip"}})
	expect(400, "POST", "/api/v1/keys", userToken, map[string]any{"name": "bad", "quota": -1})
	if _, err = db.ExecContext(ctx, "UPDATE api_keys SET quota_used=3.12345678,usage_5h=1 WHERE id=$1", kid); err != nil {
		t.Fatal(err)
	}
	updated := must("PUT", kpath, userToken, map[string]any{"name": "edited", "quota": 20})
	if updated["quota_used"] != 3.12345678 || updated["usage_5h"] != float64(1) {
		t.Fatal("configuration overwrote consumed amounts", updated)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/api-keys/%d", kid), admin, map[string]any{"reset_rate_limit_usage": true})
	updated = must("GET", kpath, userToken, nil)
	if updated["quota_used"] != 3.12345678 || updated["usage_5h"] != float64(0) {
		t.Fatal("window reset changed total quota", updated)
	}
	expect(400, "PUT", upath, admin, map[string]any{"balance": 999})
	expect(400, "POST", upath+"/balance", admin, map[string]any{"balance": 101, "operation": "subtract"})
	// Parallel retries must produce one balance delta and one ledger row.
	var wg sync.WaitGroup
	errors := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, result := call("POST", upath+"/balance", admin, map[string]any{"balance": 1.00000001, "operation": "add", "notes": "concurrent"}, map[string]string{"Idempotency-Key": "same-adjustment"})
			if code != 200 {
				errors <- fmt.Sprintf("%d %v", code, result)
			}
		}()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		t.Error(e)
	}
	result := must("GET", upath, admin, nil)
	if result["balance"] != 101.00000001 {
		t.Fatal("balance was duplicated", result["balance"])
	}
	code, _ := call("POST", upath+"/balance", admin, map[string]any{"balance": 2, "operation": "add"}, map[string]string{"Idempotency-Key": "same-adjustment"})
	if code != 409 {
		t.Fatal("idempotency conflict not rejected", code)
	}
	var count int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM redeem_codes WHERE used_by=$1 AND notes='concurrent'", uid).Scan(&count); err != nil || count != 1 {
		t.Fatal("ledger mismatch", count, err)
	}
	// Concurrent ordinary deductions and administrator changes must not overwrite each other.
	for i := 0; i < 5; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := db.ExecContext(ctx, "UPDATE users SET balance=balance-1 WHERE id=$1", uid); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			code, result := call("POST", upath+"/balance", admin, map[string]any{"balance": 1, "operation": "add"}, nil)
			if code != 200 {
				t.Error(code, result)
			}
		}()
	}
	wg.Wait()
	result = must("GET", upath, admin, nil)
	if result["balance"] != 101.00000001 {
		t.Fatal("concurrent update lost", result["balance"])
	}
	// A failed ledger write must roll back the balance change.
	if _, err = db.ExecContext(ctx, `ALTER TABLE redeem_codes ADD CONSTRAINT test_reject_note CHECK(notes IS DISTINCT FROM 'reject-ledger')`); err != nil {
		t.Fatal(err)
	}
	expect(500, "POST", upath+"/balance", admin, map[string]any{"balance": 5, "operation": "add", "notes": "reject-ledger"})
	if _, err = db.ExecContext(ctx, "ALTER TABLE redeem_codes DROP CONSTRAINT test_reject_note"); err != nil {
		t.Fatal(err)
	}
	result = must("GET", upath, admin, nil)
	if result["balance"] != 101.00000001 {
		t.Fatal("balance committed without ledger", result["balance"])
	}
	renewed := must("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	expect(401, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	userToken = renewed["access_token"].(string)
	must("POST", "/api/v1/auth/revoke-all-sessions", userToken, map[string]any{})
	expect(401, "GET", "/api/v1/auth/me", userToken, nil)
	expect(401, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": renewed["refresh_token"]})
	tokens = login("user@example.test", "correct-password")
	userToken = tokens["access_token"].(string)
	must("PUT", upath, admin, map[string]any{"status": "disabled"})
	expect(401, "GET", "/api/v1/user/profile", userToken, nil)
	expect(401, "POST", "/api/v1/auth/login", "", map[string]any{"email": "user@example.test", "password": "correct-password"})
	must("PUT", upath, admin, map[string]any{"status": "active"})
	expect(401, "GET", "/api/v1/user/profile", userToken, nil)
	tokens = login("user@example.test", "correct-password")
	userToken = tokens["access_token"].(string)
	must("PUT", "/api/v1/user/password", userToken, map[string]any{"old_password": "correct-password", "new_password": "new-password"})
	expect(401, "GET", "/api/v1/user/profile", userToken, nil)
	expect(401, "POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": tokens["refresh_token"]})
	tokens = login("user@example.test", "new-password")
	userToken = tokens["access_token"].(string)
	must("POST", "/api/v1/auth/logout", userToken, map[string]any{"refresh_token": tokens["refresh_token"]})
	expect(401, "GET", "/api/v1/auth/me", userToken, nil)
	expect(400, "DELETE", "/api/v1/admin/users/1", admin, nil)
	expect(400, "PUT", "/api/v1/admin/users/1", admin, map[string]any{"role": "user"})
	must("DELETE", upath, admin, nil)
	var deleted bool
	if err = db.QueryRowContext(ctx, "SELECT deleted_at IS NOT NULL FROM api_keys WHERE id=$1", kid).Scan(&deleted); err != nil || !deleted {
		t.Fatal("user deletion did not revoke key", err)
	}
	testUpstreamManagement(t, a, admin, otherToken, gid)
	testChannelManagement(t, a, admin, otherToken, gid)
	testGateway(t, a, admin)
	testNativeGateway(t, a, admin)
	testEmbeddings(t, a, admin)
	testImages(t, a, admin)
	testGrokImages(t, a, admin)
	testGrokAudio(t, a, admin)
	testGrokRealtime(t, a, admin)
	testCustomVoices(t, a, admin)
	testImageTasks(t, a, admin)
	testBatchImages(t, a, admin)
	testResponses(t, a, admin)
	testResponseResources(t, a, admin)
	testBackgroundResponses(t, a, admin)
	testModelDiscovery(t, a, admin)
	testModelPlaza(t, a, admin)
	testReferencePricing(t, a, admin)
	testGroupOverrides(t, a, admin)
	testCompositeGateway(t, a, admin)
	testGroupPricing(t, a, admin)
	testModelRouting(t, a, admin)
	testReasoningPolicy(t, a, admin)
	testStickyGateway(t, a, admin)
	testGatewayQueues(t, a, admin)
	testGroupFallback(t, a, admin)
	testResponsesWebSocket(t, a, admin)
	testAlphaSearch(t, a, admin)
	testGrokSearch(t, a, admin)
	testChatResponses(t, a, admin)
	testChatAnthropic(t, a, admin)
	testResponsesAnthropic(t, a, admin)
	testMessagesChat(t, a, admin)
	testMessagesResponses(t, a, admin)
	testChatGemini(t, a, admin)
	testMessagesGemini(t, a, admin)
	testOperational(t, a, admin, otherToken)
	testUsageSummaries(t, a, admin, otherToken)
	testSeedance(t, a, admin)
	testGrokVideo(t, a, admin)
	testResponsesChat(t, a, admin)
	testClientToolSearch(t, a, admin)
	testHostedToolSearch(t, a, admin)
	testLiveGateway(t, a, admin)
	testAccountBalances(t, a, admin, otherToken)
	testGeminiImages(t, a, admin)
	testAccountMediaHealth(t, a, admin, otherToken)
	testGrokProbeHealth(t, a, admin, otherToken)
	testRuntimeSettings(t, a, admin, otherToken)
	testAdminKey(t, a, admin, otherToken)
	testGroupModelCandidates(t, a, admin, otherToken)
	testGatewayUsage(t, a, admin, otherToken)
	testStreamTimeout(t, a, admin, otherToken)
	testAnthropicPolicy(t, a, admin, otherToken)
	testBillingProbes(t, a, admin, otherToken)
	testProxyFallback(t, a, admin, otherToken)
	testProxyQuality(t, a, admin, otherToken)
	testAccountUsage(t, a, admin, otherToken)
	testErrorPassthrough(t, a, admin, otherToken)
	testTLSProfiles(t, a, admin, otherToken)
	testWebSearch(t, a, admin, otherToken)
	testGeminiQuotaPolicy(t, a, admin, otherToken)
	testHostedSearch(t, a, admin, "grok")
	testHostedSearch(t, a, admin, "openai")
	testResponseImages(t, a, admin)
	// Startup detects drift; it never fixes it implicitly.
	if _, err = db.ExecContext(ctx, "ALTER TABLE users ADD COLUMN test_drift boolean"); err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(ctx, db); err == nil {
		t.Fatal("schema drift accepted")
	}
	var lockPID int
	if err = a.instanceLock.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&lockPID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "SELECT pg_terminate_backend($1)", lockPID); err != nil {
		t.Fatal(err)
	}
	expect(503, "GET", "/health", "", nil)
	expect(503, "GET", "/api/v1/admin/accounts", admin, nil)
}

func TestTokenValidation(t *testing.T) {
	a := &App{secret: []byte(strings.Repeat("a", 32))}
	for _, token := range []string{"", "sk-example", strings.Repeat("x", 3000), jwtHeader + ".e30.fake"} {
		if _, err := a.parseToken(token); err == nil {
			t.Fatalf("accepted invalid token %q", token)
		}
	}
	for _, value := range []string{"-1", "NaN", "1e4", "1.000000001", "1000000000000"} {
		if validDecimal(json.Number(value), 12, 8) {
			t.Fatalf("accepted invalid amount %q", value)
		}
	}
}
