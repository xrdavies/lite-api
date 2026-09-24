package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRetryAfterSeconds(t *testing.T) {
	now := time.Date(2026, 9, 24, 0, 0, 0, 100, time.UTC)
	for _, tc := range []struct {
		raw  string
		want int64
	}{
		{"", 0}, {"invalid", 0}, {"-1", 0}, {"0", 0}, {" 17 ", 17}, {"99999", 7200},
		{now.Add(18 * time.Second).Format(http.TimeFormat), 18}, {now.Add(-time.Second).Format(http.TimeFormat), 0},
		{time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat), 7200},
	} {
		if got := retryAfterSeconds(tc.raw, now, 7200); got != tc.want {
			t.Fatal(tc.raw, got, tc.want)
		}
	}
}

func TestPanelLimiterUnavailable(t *testing.T) {
	// A closed Redis client fails deterministically without a network dependency.
	cache := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = cache.Close()
	s := defaultPanelRate()
	a := &App{Redis: cache, panelCache: &s, panelExpires: time.Now().Add(time.Minute)}
	for _, user := range []*identity{nil, {ID: 7, Role: "user"}} {
		r := httptest.NewRequest("GET", "/api/v1/settings/public", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		if err := a.panelRateLimit(w, r, user); err != nil || w.Header().Get("Retry-After") != "" {
			t.Fatal("panel protection blocked request during Redis failure", err)
		}
	}
}

func testRuntimeSettings(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token, ip string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = ip
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Session-Id", randomToken(12))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	expect := func(code int, method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		w := call(method, path, token, "192.0.2.206:1234", body)
		if w.Code != code {
			t.Fatalf("%s %s: %d want %d: %s", method, path, w.Code, code, w.Body.String())
		}
		return w
	}
	data := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := expect(200, method, path, token, body)
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const root = "/api/v1/admin/settings/"
	defer func() {
		a.panelMu.Lock()
		defer a.panelMu.Unlock()
		_, _ = a.DB.Exec("DELETE FROM settings WHERE key IN ($1,$2,$3)", overloadSetting, rate429Setting, panelSetting)
		a.panelCache = nil
	}()
	for _, name := range []string{"overload-cooldown", "rate-limit-429-cooldown", "panel-rate-limit"} {
		expect(401, "GET", root+name, "", nil)
		expect(403, "GET", root+name, ordinary, nil)
		expect(403, "PUT", root+name, ordinary, map[string]any{})
		expect(400, "PUT", root+name, admin, nil)
		expect(400, "PUT", root+name, admin, map[string]any{"unknown": true})
	}
	if v := data("GET", root+"overload-cooldown", admin, nil); v["enabled"] != true || v["cooldown_minutes"] != float64(10) {
		t.Fatal("overload defaults", v)
	}
	if v := data("GET", root+"rate-limit-429-cooldown", admin, nil); v["enabled"] != true || v["cooldown_seconds"] != float64(5) {
		t.Fatal("429 defaults", v)
	}
	for _, raw := range []string{"", "null", "bad json", `{"enabled":true,"cooldown_minutes":"bad"}`} {
		exec("INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", overloadSetting, raw)
		if v := data("GET", root+"overload-cooldown", admin, nil); v["cooldown_minutes"] != float64(10) {
			t.Fatal("corrupt setting did not default", v)
		}
	}
	for _, tc := range []struct {
		name, field string
		max         int
	}{{"overload-cooldown", "cooldown_minutes", 120}, {"rate-limit-429-cooldown", "cooldown_seconds", 7200}} {
		for _, n := range []int{-1, 0, tc.max + 1} {
			expect(400, "PUT", root+tc.name, admin, map[string]any{"enabled": true, tc.field: n})
		}
		v := data("PUT", root+tc.name, admin, map[string]any{"enabled": false, tc.field: -1})
		want := float64(5)
		if tc.name == "overload-cooldown" {
			want = 10
		}
		if v[tc.field] != want || v["enabled"] != false {
			t.Fatal("disabled normalization", v)
		}
	}
	data("PUT", root+"overload-cooldown", admin, overloadSettings{true, 2})
	data("PUT", root+"rate-limit-429-cooldown", admin, rate429Settings{true, 17})
	// Settings control the shared failure writer, preserving unrelated state and
	// rejecting stale in-flight results after an administrator edits the account.
	aid := id(data("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "cooldown", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "cooldown-secret", "base_url": "http://127.0.0.1:1"}, "extra": map[string]any{"quota_limit": 100}}))
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	exec(`UPDATE accounts SET extra=extra || '{"quota_used":3,"unknown_state":"keep"}'::jsonb WHERE id=$1`, aid)
	mark := func(status int, retry string) {
		t.Helper()
		u, err := a.loadAccount(ctx, aid)
		if err != nil {
			t.Fatal(err)
		}
		a.markGatewayFailure(ctx, &gatewaySelection{Account: u}, status, retry, nil)
	}
	remaining := func(column string, want float64) {
		t.Helper()
		var seconds sql.NullFloat64
		if err := a.DB.QueryRow("SELECT EXTRACT(EPOCH FROM ("+column+"-now())) FROM accounts WHERE id=$1", aid).Scan(&seconds); err != nil {
			t.Fatal(err)
		}
		if want == 0 && seconds.Valid || want > 0 && (!seconds.Valid || seconds.Float64 < want-3 || seconds.Float64 > want+1) {
			t.Fatal(column, seconds, want)
		}
	}
	mark(429, "")
	remaining("rate_limit_reset_at", 17)
	mark(429, "1")
	remaining("rate_limit_reset_at", 17)
	data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
	data("PUT", root+"rate-limit-429-cooldown", admin, rate429Settings{false, 17})
	mark(429, "invalid")
	remaining("rate_limit_reset_at", 0)
	mark(429, "41")
	remaining("rate_limit_reset_at", 41) // Explicit upstream reset still applies.
	mark(529, "1")
	remaining("overload_until", 120)
	var quota, unknown string
	if err := a.DB.QueryRow("SELECT extra->>'quota_used',extra->>'unknown_state' FROM accounts WHERE id=$1", aid).Scan(&quota, &unknown); err != nil || quota != "3" || unknown != "keep" {
		t.Fatal(quota, unknown, err)
	}
	data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
	data("PUT", root+"overload-cooldown", admin, overloadSettings{false, 2})
	mark(529, "100")
	remaining("overload_until", 0)
	mark(503, "")
	remaining("overload_until", 30) // 529 toggle does not disable transport cooling.
	data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
	stale, err := a.loadAccount(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	data("PUT", apath, admin, map[string]any{"notes": "changed during request"})
	a.markGatewayFailure(ctx, &gatewaySelection{Account: stale}, 401, "", nil)
	var status string
	if err := a.DB.QueryRow("SELECT status FROM accounts WHERE id=$1", aid).Scan(&status); err != nil || status != "active" {
		t.Fatal("stale failure overwrote edit", status, err)
	}
	data("PUT", apath, admin, map[string]any{"status": "inactive"})
	mark(429, "50")
	remaining("rate_limit_reset_at", 0)
	data("PUT", root+"overload-cooldown", admin, overloadSettings{true, 2})
	data("PUT", root+"rate-limit-429-cooldown", admin, rate429Settings{true, 17})
	// Exercise actual dispatch and billing: 529 switches once to a healthy account.
	var rejected, completed atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fail-secret" {
			rejected.Add(1)
			w.WriteHeader(529)
			return
		}
		completed.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"model":"runtime-model","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	uid := id(data("POST", "/api/v1/admin/users", admin, map[string]any{"email": "runtime@example.test", "password": "runtime-password", "balance": 1}))
	user := data("POST", "/api/v1/auth/login", "", map[string]any{"email": "runtime@example.test", "password": "runtime-password"})["access_token"].(string)
	gid := id(data("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "runtime", "platform": "openai"}))
	data("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "runtime", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"runtime-model"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0.001"}}})
	var failID int64
	for i, secret := range []string{"fail-secret", "healthy-secret"} {
		v := data("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "runtime-" + secret, "platform": "openai", "type": "apikey", "priority": i, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": secret, "base_url": provider.URL}})
		if i == 0 {
			failID = id(v)
		}
	}
	key := data("POST", "/api/v1/keys", user, map[string]any{"name": "runtime", "group_id": gid})["key"].(string)
	request := map[string]any{"model": "runtime-model", "messages": []any{map[string]string{"role": "user", "content": "OK"}}}
	expect(200, "POST", "/v1/chat/completions", key, request)
	if rejected.Load() != 1 || completed.Load() != 1 {
		t.Fatal("529 failover", rejected.Load(), completed.Load())
	}
	var cooled bool
	var amount string
	if err := a.DB.QueryRow("SELECT overload_until>now() FROM accounts WHERE id=$1", failID).Scan(&cooled); err != nil || !cooled {
		t.Fatal("529 runtime cooldown", err)
	}
	if err := a.DB.QueryRow("SELECT sum(actual_cost)::text FROM usage_logs WHERE user_id=$1", uid).Scan(&amount); err != nil || amount != "0.0040000000" {
		t.Fatal("failover billing", amount, err)
	}
	// Panel policy is independent of gateway billing and authentication quotas.
	const panel = root + "panel-rate-limit"
	for _, field := range []string{"user_rpm", "heavy_rpm", "public_ip_rpm"} {
		for _, n := range []int{-1, 100001} {
			expect(400, "PUT", panel, admin, map[string]any{field: n})
		}
	}
	policy := defaultPanelRate()
	policy.UserRPM = 1
	data("PUT", panel, admin, policy)
	globalKey := "lite-api:panel:global:user:" + strconv.FormatInt(uid, 10)
	heavyKey := "lite-api:panel:heavy:user:" + strconv.FormatInt(uid, 10)
	clear := func(keys ...string) {
		t.Helper()
		if err := a.Redis.Del(ctx, keys...).Err(); err != nil {
			t.Fatal(err)
		}
	}
	clear(globalKey, heavyKey)
	expect(200, "GET", "/api/v1/user/profile", user, nil)
	w := call("GET", "/api/v1/keys", user, "198.51.100.1:12", nil)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("user limit depended on IP", w.Code)
	}
	expect(401, "GET", "/api/v1/user/profile", "bad-token", nil)
	expect(200, "POST", "/v1/chat/completions", key, request)
	policy.UserRPM = 0
	policy.HeavyRPM = 1
	data("PUT", panel, admin, policy)
	clear(heavyKey)
	expect(200, "GET", "/api/v1/usage", user, nil)
	expect(429, "GET", "/api/v1/usage/stats", user, nil)
	expect(200, "GET", "/api/v1/user/profile", user, nil)
	// Concurrent admission is atomic and expired counters do not extend windows.
	policy.UserRPM = 3
	policy.HeavyRPM = 0
	data("PUT", panel, admin, policy)
	clear(globalKey)
	var wg sync.WaitGroup
	var allowed, limited atomic.Int32
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call("GET", "/api/v1/user/profile", user, "192.0.2.211:1", nil)
			if w.Code == 200 {
				allowed.Add(1)
			} else if w.Code == 429 {
				limited.Add(1)
			} else {
				t.Errorf("parallel panel status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 3 || limited.Load() != 7 {
		t.Fatal("non-atomic panel limit", allowed.Load(), limited.Load())
	}
	if err := a.Redis.Set(ctx, globalKey, 3, 0).Err(); err != nil {
		t.Fatal(err)
	}
	expect(429, "GET", "/api/v1/user/profile", user, nil)
	if ttl, err := a.Redis.PTTL(ctx, globalKey).Result(); err != nil || ttl <= 0 || ttl > time.Minute {
		t.Fatal("missing expiry not repaired", ttl, err)
	}
	clear(globalKey)
	expect(200, "GET", "/api/v1/user/profile", user, nil)
	// Shared public buckets cover settings and plaza; spoofed forwarded IPs are ignored.
	policy.PublicIPRPM = 1
	data("PUT", panel, admin, policy)
	clear("lite-api:panel:public:ip:192.0.2.206")
	expect(200, "GET", "/api/v1/settings/public", "", nil)
	expect(429, "GET", "/api/v1/model-plaza", "", nil)
	for _, ip := range []string{"127.0.0.1:12", "10.1.2.3:12", "[::ffff:127.0.0.1]:12"} {
		for i := 0; i < 2; i++ {
			if w := call("GET", "/api/v1/settings/public", "", ip, nil); w.Code != 200 {
				t.Fatal("private proxy throttled", ip, w.Code)
			}
		}
	}
	policy.Enabled = false
	data("PUT", panel, admin, policy)
	expect(200, "GET", "/api/v1/settings/public", "", nil)
	// The persisted policy is used by a new instance, not only by the writer's cache.
	fresh := &App{DB: a.DB, Redis: a.Redis}
	if got := fresh.cachedPanelSettings(ctx); got != policy {
		t.Fatal("restart policy", got, policy)
	}
	policy.Enabled = true
	policy.UserRPM = 100000
	policy.ExemptAdmin = false
	data("PUT", panel, admin, policy)
	claims, err := a.parseToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	adminKey := "lite-api:panel:global:user:" + strconv.FormatInt(claims.UserID, 10)
	if err := a.Redis.Set(ctx, adminKey, 100000, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	expect(429, "GET", "/api/v1/admin/settings", admin, nil)
	clear(adminKey)
	data("PUT", panel, admin, defaultPanelRate())
	// Sensitive config is not included in public settings; successful writes are audited.
	if w := call("GET", "/api/v1/settings/public", "", "198.51.100.222:1", nil); w.Code != 200 || strings.Contains(w.Body.String(), "cooldown") || strings.Contains(w.Body.String(), "user_rpm") {
		t.Fatal("runtime policy exposed", w.Code, w.Body.String())
	}
	var audits int
	if err := a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE path=$1 AND method='PUT' AND status_code=200", panel).Scan(&audits); err != nil || audits == 0 {
		t.Fatal("runtime audit", audits, err)
	}
}
