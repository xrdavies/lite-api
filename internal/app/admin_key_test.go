package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testAdminKey(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const path = "/api/v1/admin/settings/admin-api-key"
	const regenerate = path + "/regenerate"
	call := func(method, url, jwt, key string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, url, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.224:1234"
		if jwt != "" {
			r.Header.Set("Authorization", "Bearer "+jwt)
		}
		if key != "" {
			r.Header.Set("X-Api-Key", key)
		}
		r.Header.Set("Idempotency-Key", "admin-key-adjustment")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	expect := func(code int, method, url, jwt, key string, body any) *httptest.ResponseRecorder {
		t.Helper()
		w := call(method, url, jwt, key, body)
		if w.Code != code {
			t.Fatalf("%s %s: status %d want %d", method, url, w.Code, code)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("management response protection missing")
		}
		return w
	}
	data := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", adminKeySetting)
	if got := data(expect(200, "GET", path, admin, "", nil)); got["exists"] != false || got["masked_key"] != "" {
		t.Fatal("unexpected initial machine credential")
	}
	for _, operation := range []struct{ method, url string }{{"GET", path}, {"POST", regenerate}, {"DELETE", path}} {
		expect(401, operation.method, operation.url, "", "", nil)
		expect(403, operation.method, operation.url, ordinary, "", nil)
		expect(401, operation.method, operation.url, "", "invalid-key", nil)
	}
	key := data(expect(200, "POST", regenerate, admin, "", nil))["key"].(string)
	if !strings.HasPrefix(key, "admin-") || len(key) != 70 {
		t.Fatal("invalid machine credential format")
	}
	if b, err := hex.DecodeString(key[6:]); err != nil || len(b) != 32 {
		t.Fatal("invalid machine credential entropy")
	}
	var stored string
	if err := a.DB.QueryRow("SELECT value FROM settings WHERE key=$1", adminKeySetting).Scan(&stored); err != nil || stored != key {
		t.Fatal("credential setting meaning changed", err)
	}
	masked := data(expect(200, "GET", path, "", key, nil))
	if masked["exists"] != true || masked["masked_key"] != key[:10]+"..."+key[len(key)-4:] || masked["key"] != nil {
		t.Fatal("status did not mask credential")
	}
	expect(401, "GET", path, admin, "wrong-key", nil) // Header wins; no JWT fallback.
	expect(401, "GET", path, admin, key+" ", nil)
	expect(401, "GET", path, "", strings.Repeat("x", 129), nil)
	expect(401, "GET", path, key, "", nil) // Machine keys are not bearer sessions.
	expect(401, "GET", path+"?api_key="+key, "", "", nil)
	expect(200, "GET", path, "invalid-jwt", key, nil)
	for _, values := range [][]string{{""}, {key, key}, {"wrong", key}} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header["X-Api-Key"] = values
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatal("ambiguous machine credentials allowed", w.Code)
		}
	}
	for _, endpoint := range []string{"/api/v1/keys", "/api/v1/auth/me", "/api/v1/user/profile"} {
		expect(401, "GET", endpoint, "", key, nil)
	}
	expect(401, "POST", "/api/v1/keys", "", key, map[string]string{"name": "must-not-create"})
	expect(401, "POST", "/api/v1/auth/revoke-all-sessions", "", key, nil)
	if w := call("GET", "/v1/billing", "", key, nil); w.Code != 401 {
		t.Fatal("machine key accepted as gateway key", w.Code)
	}
	// Machine auth resolves a real administrator; existing self/last-admin guards apply.
	claims, err := a.parseToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	expect(400, "DELETE", fmt.Sprintf("/api/v1/admin/users/%d", claims.UserID), "", key, nil)
	expect(400, "PUT", fmt.Sprintf("/api/v1/admin/users/%d", claims.UserID), "", key, map[string]string{"role": "user"})
	created := data(expect(200, "POST", "/api/v1/admin/users", "", key, map[string]any{"email": "machine-managed@example.test", "password": "machine-password"}))
	uid := int64(created["id"].(float64))
	upath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	user := data(expect(200, "POST", "/api/v1/auth/login", "", "", map[string]string{"email": "machine-managed@example.test", "password": "machine-password"}))["access_token"].(string)
	clientKey := data(expect(200, "POST", "/api/v1/keys", user, "", map[string]string{"name": "self-managed"}))["key"].(string)
	expect(401, "GET", path, "", clientKey, nil)
	expect(401, "GET", path, admin, clientKey, nil)
	// Balance changes retain transactional idempotency and original internal ledger semantics.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := call("POST", upath+"/balance", "", key, map[string]any{"operation": "add", "balance": json.Number("0.12345678"), "notes": "machine-balance"})
			if w.Code != 200 {
				t.Errorf("machine balance status %d", w.Code)
			}
		}()
	}
	wg.Wait()
	var balance string
	var records int
	if err := a.DB.QueryRow("SELECT balance::text,(SELECT count(*) FROM redeem_codes WHERE used_by=$1 AND notes='machine-balance') FROM users WHERE id=$1", uid).Scan(&balance, &records); err != nil || balance != "0.12345678" || records != 1 {
		t.Fatal("machine balance duplication", balance, records, err)
	}
	var actor int64
	var method string
	if err := a.DB.QueryRow("SELECT actor_user_id,auth_method FROM audit_logs WHERE path=$1 AND status_code=200 ORDER BY id DESC LIMIT 1", upath+"/balance").Scan(&actor, &method); err != nil || actor != claims.UserID || method != "admin_api_key" {
		t.Fatal("machine audit identity", actor, method, err)
	}
	for _, endpoint := range []string{path, "/api/v1/admin/settings", "/api/v1/settings/public", "/api/v1/admin/audit-logs"} {
		w := expect(200, "GET", endpoint, admin, "", nil)
		if strings.Contains(w.Body.String(), key) {
			t.Fatal("credential exposed outside regeneration", endpoint)
		}
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE to_jsonb(audit_logs)::text LIKE '%' || $1 || '%'", key).Scan(&records); err != nil || records != 0 {
		t.Fatal("credential persisted in audit", records, err)
	}
	// A failed rotation must leave the old credential usable and return no secret.
	exec("ALTER TABLE settings ADD CONSTRAINT test_admin_rotation_failure CHECK(key<>'admin_api_key') NOT VALID")
	defer a.DB.Exec("ALTER TABLE settings DROP CONSTRAINT IF EXISTS test_admin_rotation_failure")
	w := expect(500, "POST", regenerate, "", key, nil)
	if strings.Contains(w.Body.String(), "admin-") || strings.Contains(w.Body.String(), `"key"`) {
		t.Fatal("failed rotation exposed credential")
	}
	exec("ALTER TABLE settings DROP CONSTRAINT test_admin_rotation_failure")
	expect(200, "GET", path, "", key, nil)
	// Limits use the resolved administrator's existing policy, with no parallel quota.
	policy := defaultPanelRate()
	policy.ExemptAdmin = false
	policy.UserRPM = 1
	expect(200, "PUT", "/api/v1/admin/settings/panel-rate-limit", admin, "", policy)
	defer func() {
		a.panelMu.Lock()
		defer a.panelMu.Unlock()
		a.DB.Exec("DELETE FROM settings WHERE key=$1", panelSetting)
		a.panelCache = nil
	}()
	bucket := "lite-api:panel:global:user:" + strconv.FormatInt(claims.UserID, 10)
	if err := a.Redis.Set(context.Background(), bucket, 1, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	w = expect(429, "GET", path, "", key, nil)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("machine limit missing retry time")
	}
	if err := a.Redis.Del(context.Background(), bucket).Err(); err != nil {
		t.Fatal(err)
	}
	expect(200, "PUT", "/api/v1/admin/settings/panel-rate-limit", admin, "", defaultPanelRate())
	// Credential rotation/removal have no auth cache; the next request observes DB state.
	next := data(expect(200, "POST", regenerate, "", key, nil))["key"].(string)
	if next == key {
		t.Fatal("rotation reused key")
	}
	expect(401, "GET", path, "", key, nil)
	expect(200, "GET", path, "", next, nil)
	fresh := &App{DB: a.DB}
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("X-Api-Key", next)
	if u, err := fresh.authenticateAdmin(r); err != nil || u.ID != claims.UserID || u.AuthMethod != "admin_api_key" {
		t.Fatal("machine auth depended on process state", err)
	}
	expect(200, "DELETE", path, "", next, nil)
	expect(401, "GET", path, "", next, nil)
	expect(200, "DELETE", path, admin, "", nil)
	if got := data(expect(200, "GET", path, admin, "", nil)); got["exists"] != false {
		t.Fatal("key deletion not persistent")
	}
	// Database corruption never publishes a short credential in the status API.
	exec("INSERT INTO settings(key,value) VALUES($1,'short-secret')", adminKeySetting)
	if got := data(expect(200, "GET", path, admin, "", nil)); got["masked_key"] != "****" {
		t.Fatal("short secret exposed")
	}
	next = data(expect(200, "POST", regenerate, admin, "", nil))["key"].(string)
	// Resolve another active administrator when the former actor is unavailable.
	second := data(expect(200, "POST", "/api/v1/admin/users", admin, "", map[string]any{"email": "machine-second-admin@example.test", "password": "second-password", "role": "admin"}))
	secondID := int64(second["id"].(float64))
	r.Header.Set("X-Api-Key", next)
	for _, edit := range []string{"status='disabled'", "role='user'", "deleted_at=now()", "totp_enabled=true"} {
		exec("UPDATE users SET "+edit+" WHERE id=$1", claims.UserID)
		u, authErr := a.authenticateAdmin(r)
		exec("UPDATE users SET status='active',role='admin',deleted_at=NULL,totp_enabled=false WHERE id=$1", claims.UserID)
		if authErr != nil || u.ID != secondID {
			t.Fatal("unavailable administrator used for machine identity", edit, authErr)
		}
	}
	exec("UPDATE users SET status='disabled' WHERE id IN ($1,$2)", claims.UserID, secondID)
	w = call("GET", path, "", next, nil)
	exec("UPDATE users SET status='active' WHERE id IN ($1,$2)", claims.UserID, secondID)
	if w.Code != 401 {
		t.Fatal("machine authentication allowed without active administrator", w.Code)
	}
	expect(200, "DELETE", path, admin, "", nil)
	if err := a.DB.QueryRow("SELECT auth_method FROM audit_logs WHERE path=$1 AND method='DELETE' AND status_code=200 ORDER BY id DESC LIMIT 1", path).Scan(&method); err != nil || method != "jwt" {
		t.Fatal("JWT audit identity changed", method, err)
	}
}
