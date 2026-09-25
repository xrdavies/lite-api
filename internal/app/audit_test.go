package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuditContracts(t *testing.T) {
	for _, tc := range []struct {
		pattern, action string
		authenticated   bool
		status          int
	}{
		{"POST /api/v1/auth/login", "auth.login", false, 401},
		{"POST /api/v1/auth/refresh", "", true, 200},
		{"POST /api/v1/auth/refresh", "auth.token.refresh", false, 429},
		{"POST /api/v1/auth/logout", "auth.logout.create", false, 200},
		{"GET /api/v1/admin/users/{id}/api-keys", "admin.users.api_keys.read", true, 200},
		{"GET /api/v1/admin/groups/{id}/api-keys", "", false, 401},
		{"POST /api/v1/admin/settings/admin-api-key/regenerate", "admin.admin_api_key.regenerate", true, 200},
		{"DELETE /api/v1/admin/settings/admin-api-key", "admin.admin_api_key.delete", true, 200},
		{"PUT /api/v1/admin/accounts/{id}", "admin.accounts.update", true, 400},
		{"GET /api/v1/admin/audit-logs", "", true, 200},
		{"PUT /api/v1/admin/users/{id}", "", false, 401},
	} {
		if action := auditAction(tc.pattern, tc.authenticated, tc.status); action != tc.action {
			t.Fatalf("%s: %q, want %q", tc.pattern, action, tc.action)
		}
	}
	for _, status := range []int{200, 201, 304} {
		recorder := httptest.NewRecorder()
		w := &auditResponseWriter{ResponseWriter: recorder}
		w.WriteHeader(status)
		if _, err := w.Write([]byte("result")); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(500) // An error after headers cannot change the wire status.
		if w.status != status || recorder.Code != status {
			t.Fatal("audit status differs from response", w.status, recorder.Code)
		}
	}
	recorder := httptest.NewRecorder()
	w := &auditResponseWriter{ResponseWriter: recorder}
	if err := http.NewResponseController(w).Flush(); err != nil || !recorder.Flushed || w.status != 200 {
		t.Fatal("audit wrapper blocked SSE flushing", err, w.status)
	}
}

func testAuditQueries(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const marker = "audit-query-contract"
	const path = "/api/v1/admin/audit-logs"
	call := func(suffix, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path+suffix, nil)
		r.RemoteAddr = "192.0.2.207:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	type record struct {
		ID   int64
		Body string `json:"request_body"`
	}
	list := func(query string) ([]record, int) {
		t.Helper()
		w := call("?client_ip="+marker+"&"+query, admin)
		var result struct {
			Data struct {
				Items []record
				Total int
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("audit list: %d %s", w.Code, w.Body.String())
		}
		for _, item := range result.Data.Items {
			if item.Body != "" {
				t.Fatal("audit list exposed stored request body")
			}
		}
		return result.Data.Items, result.Data.Total
	}
	var actor int64
	if err := a.DB.QueryRow("SELECT id FROM users WHERE email='admin@example.test'").Scan(&actor); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ids := []int64{}
	for i, offset := range []time.Duration{2 * time.Hour, 0, 2 * time.Hour, time.Hour} {
		var id int64
		if err := a.DB.QueryRow(`INSERT INTO audit_logs(created_at,actor_user_id,actor_email,auth_method,action,method,path,client_ip,status_code,request_body)
VALUES($1,$2,$3,'jwt',$4,'POST',$5,$6,$7,'previously redacted body') RETURNING id`, created.Add(offset), actor, fmt.Sprintf("Audit%%_%d@example.test", i), fmt.Sprintf("audit.action_%d", i), fmt.Sprintf("/audit/%%_/%d", i), marker, 200+i*100).Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	defer a.DB.Exec("DELETE FROM audit_logs WHERE client_ip=$1", marker)
	assertList := func(query string, wantTotal int, want ...int64) {
		t.Helper()
		items, total := list(query)
		if total != wantTotal || len(items) != len(want) {
			t.Fatalf("%s: total=%d items=%v want=%v", query, total, items, want)
		}
		for i, item := range items {
			if item.ID != want[i] {
				t.Fatalf("%s: IDs=%v want=%v", query, items, want)
			}
		}
	}
	assertList("page_size=2", 4, ids[2], ids[0])
	assertList("page_size=2&page=2", 4, ids[3], ids[1])
	assertList("page=9", 4)
	assertList("actor_email="+url.QueryEscape(" AUDIT%_ "), 4, ids[2], ids[0], ids[3], ids[1])
	assertList("q="+url.QueryEscape("%_"), 4, ids[2], ids[0], ids[3], ids[1])
	assertList("q="+url.QueryEscape("' OR true --"), 0)
	assertList("action=ACTION_2", 1, ids[2])
	assertList("success="+url.QueryEscape(" true "), 2, ids[0], ids[1])
	assertList("success=false", 2, ids[2], ids[3])
	assertList("actor_user_id="+url.QueryEscape(fmt.Sprintf(" %d ", actor))+"&auth_method=jwt&method=post", 4, ids[2], ids[0], ids[3], ids[1])
	assertList("start_time="+url.QueryEscape(" "+created.Add(2*time.Hour).Format(time.RFC3339)+" ")+"&end_time="+url.QueryEscape(created.Add(2*time.Hour).In(time.FixedZone("local", 9*3600)).Format(time.RFC3339)), 2, ids[2], ids[0])
	for key, value := range map[string]string{
		"actor_user_id": "0", "start_time": "yesterday", "end_time": "tomorrow", "success": "1",
		"actor_email": strings.Repeat("界", 256), "action": strings.Repeat("x", 129), "auth_method": strings.Repeat("x", 33),
		"method": strings.Repeat("x", 17), "client_ip": strings.Repeat("x", 65), "q": strings.Repeat("x", 513),
	} {
		if w := call("?"+key+"="+url.QueryEscape(value), admin); w.Code != 400 {
			t.Fatalf("invalid %s: %d %s", key, w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"q=%00", "q=%FF", "start_time=2026-01-02T00:00:00Z&end_time=2026-01-01T00:00:00Z"} {
		if w := call("?"+query, admin); w.Code != 400 {
			t.Fatalf("invalid query %s: %d", query, w.Code)
		}
	}
	detail := fmt.Sprintf("/%d", ids[0])
	var result struct{ Data record }
	w := call(detail, admin)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.ID != ids[0] || result.Data.Body != "previously redacted body" {
		t.Fatal("audit detail lost stored data", w.Code, w.Body.String())
	}
	for _, suffix := range []string{"", detail} {
		for _, auth := range []struct {
			token string
			code  int
		}{{ordinary, 403}, {"", 401}} {
			if w := call(suffix, auth.token); w.Code != auth.code {
				t.Fatal("audit authorization", suffix, w.Code)
			}
		}
	}
	if w := call("/9223372036854775807", admin); w.Code != 404 {
		t.Fatal("missing audit detail", w.Code)
	}
	// Count and rows must agree while another connection changes the match set.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			var id int64
			if err := a.DB.QueryRow("INSERT INTO audit_logs(client_ip) VALUES($1) RETURNING id", marker).Scan(&id); err != nil {
				done <- err
				return
			}
			if _, err := a.DB.Exec("DELETE FROM audit_logs WHERE id=$1", id); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 24 {
		items, total := list("page_size=200")
		if total != len(items) || total < 4 || total > 5 {
			t.Fatal("audit count and page used different snapshots", total, len(items))
		}
	}
}

func testAuthenticationAudit(t *testing.T, a *App, admin string) {
	t.Helper()
	const ip = "192.0.2.204"
	const email = "audit-auth@example.test"
	const password = "audit-password-canary"
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = ip + ":1234"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", strings.Repeat("界", 520)+"\x00\xff")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
		}
		return out.Data
	}
	assertAudit := func(w *httptest.ResponseRecorder, action string, uid int64, method string) {
		t.Helper()
		var raw []byte
		if err := a.DB.QueryRow("SELECT to_jsonb(l) FROM audit_logs l WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&raw); err != nil {
			t.Fatal("audit record missing", err, action, w.Code)
		}
		var record struct {
			ActorID    *int64 `json:"actor_user_id"`
			Email      string `json:"actor_email"`
			Role       string `json:"actor_role"`
			AuthMethod string `json:"auth_method"`
			Action     string
			IP         string `json:"client_ip"`
			Agent      string `json:"user_agent"`
			Status     int    `json:"status_code"`
			Body       string `json:"request_body"`
			Credential string `json:"credential_masked"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.Action != action || record.Status != w.Code || record.AuthMethod != method || record.IP != ip || record.Agent != strings.Repeat("界", 512) {
			t.Fatalf("incorrect audit metadata: %+v", record)
		}
		if uid == 0 && (record.ActorID != nil || record.Email != "" || record.Role != "") || uid > 0 && (record.ActorID == nil || *record.ActorID != uid) {
			t.Fatal("unverified actor or missing identity", string(raw))
		}
		if record.Body != "" || record.Credential != "" {
			t.Fatal("audit captured credentials or body", string(raw))
		}
		var count int
		if err := a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 {
			t.Fatal("duplicate audit record", count, err)
		}
	}
	assertNoAudit := func(w *httptest.ResponseRecorder) {
		t.Helper()
		var count int
		if err := a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 0 {
			t.Fatal("unexpected audit", count, err)
		}
	}
	u := data(call("POST", "/api/v1/admin/users", admin, map[string]any{"email": email, "password": password}))
	uid := int64(u["id"].(float64))
	login := func(p string) *httptest.ResponseRecorder {
		return call("POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": p})
	}
	w := login("wrong-password")
	if w.Code != 401 {
		t.Fatal("wrong password accepted", w.Code)
	}
	assertAudit(w, "auth.login", 0, "")
	w = login(password)
	tokens := data(w)
	access, refresh := tokens["access_token"].(string), tokens["refresh_token"].(string)
	assertAudit(w, "auth.login", uid, "jwt")
	w = call("POST", "/api/v1/auth/login", "", map[string]any{"email": email, "password": password, "role": "admin"})
	if w.Code != 400 {
		t.Fatal("invalid login accepted", w.Code)
	}
	assertAudit(w, "auth.login", 0, "")
	w = call("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	next := data(w)
	assertNoAudit(w)
	w = call("POST", "/api/v1/auth/refresh", "", map[string]any{"refresh_token": refresh})
	if w.Code != 401 {
		t.Fatal("refresh replay accepted", w.Code)
	}
	assertAudit(w, "auth.token.refresh", 0, "")
	refresh = next["refresh_token"].(string)
	sid, _, _ := splitRefresh(refresh)
	w = call("POST", "/api/v1/auth/logout", "", map[string]any{"refresh_token": sid + "." + strings.Repeat("x", 43)})
	data(w)
	assertAudit(w, "auth.logout.create", 0, "")
	data(call("GET", "/api/v1/auth/me", access, nil)) // Wrong secret did not revoke.
	w = call("PUT", "/api/v1/user?credential=query-canary", access, map[string]any{"username": "body-canary"})
	data(w)
	assertAudit(w, "user.update", uid, "jwt")
	group := data(call("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Audit composite", "platform": "composite"}))
	gid := int64(group["id"].(float64))
	key := data(call("POST", "/api/v1/keys", access, map[string]any{"name": "audit", "group_id": gid}))["key"].(string)
	adminClaims, err := a.parseToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	for path, action := range map[string]string{
		"/api/v1/admin/settings/admin-api-key":               "admin.admin_api_key.read",
		fmt.Sprintf("/api/v1/admin/users/%d/api-keys", uid):  "admin.users.api_keys.read",
		fmt.Sprintf("/api/v1/admin/groups/%d/api-keys", gid): "admin.groups.api_keys.read",
	} {
		w = call("GET", path, admin, nil)
		data(w)
		assertAudit(w, action, adminClaims.UserID, "jwt")
	}
	w = call("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", gid), admin, map[string]any{"public_model": "audit-model", "match_type": "exact", "target_platform": "openai", "endpoint": "any", "enabled": true})
	if w.Code != 201 {
		t.Fatal("expected real 201 status", w.Code, w.Body.String())
	}
	assertAudit(w, "admin.groups.composite_routes.create", adminClaims.UserID, "jwt")
	w = call("GET", "/api/v1/admin/settings/admin-api-key", access, nil)
	if w.Code != 403 {
		t.Fatal("sensitive read authorized nonadmin", w.Code)
	}
	assertAudit(w, "admin.admin_api_key.read", uid, "jwt")
	assertNoAudit(call("GET", "/api/v1/admin/users", admin, nil))
	w = call("POST", "/api/v1/auth/logout", "", map[string]any{"refresh_token": refresh})
	data(w)
	assertAudit(w, "auth.logout.create", uid, "jwt")
	if w = call("GET", "/api/v1/auth/me", access, nil); w.Code != 401 {
		t.Fatal("logout did not revoke session", w.Code)
	}
	w = call("POST", "/api/v1/auth/logout", "", map[string]any{"refresh_token": refresh})
	assertAudit(w, "auth.logout.create", 0, "")
	w = login(password)
	access = data(w)["access_token"].(string)
	// An audit write failure must not turn a committed update into a retryable
	// business error; the next request can record normally after recovery.
	if _, err := a.DB.Exec("ALTER TABLE audit_logs ADD CONSTRAINT test_auth_audit_failure CHECK(action<>'user.update') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS test_auth_audit_failure")
	w = call("PUT", "/api/v1/user", access, map[string]any{"username": "audit-write-recovery"})
	if profile := data(w); profile["username"] != "audit-write-recovery" {
		t.Fatal("audit failure changed committed response", profile)
	}
	assertNoAudit(w)
	if _, err := a.DB.Exec("ALTER TABLE audit_logs DROP CONSTRAINT test_auth_audit_failure"); err != nil {
		t.Fatal(err)
	}
	w = call("PUT", "/api/v1/user", access, map[string]any{"username": "audit-recovered"})
	data(w)
	assertAudit(w, "user.update", uid, "jwt")
	w = call("POST", "/api/v1/auth/logout", access, nil)
	data(w)
	assertAudit(w, "auth.logout.create", uid, "jwt")
	if err := a.Redis.Set(context.Background(), "lite-api:login:ip:"+ip, "10", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	w = login(password)
	if w.Code != 429 {
		t.Fatal("login limit bypass", w.Code)
	}
	assertAudit(w, "auth.login", 0, "")
	for _, secret := range []string{password, refresh, access, key, "query-canary", "body-canary"} {
		var found bool
		if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM audit_logs WHERE strpos(to_jsonb(audit_logs)::text,$1)>0)", secret).Scan(&found); err != nil || found {
			t.Fatal("audit contains secret or request content", err)
		}
	}
	// The public failure is discoverable through the real administrator query.
	list := data(call("GET", "/api/v1/admin/audit-logs?action=auth.login&success=false&client_ip="+ip, admin, nil))
	if list["total"].(float64) != 3 {
		t.Fatal("failed authentication query", list)
	}
	// Client disconnects must not cancel recording of an observed result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/api/v1/auth/login", nil).WithContext(ctx)
	requestID := randomToken(18)
	a.recordAudit(r, "POST /api/v1/auth/login", &identity{}, 401, requestID, time.Now())
	var status int
	if err := a.DB.QueryRow("SELECT status_code FROM audit_logs WHERE request_id=$1", requestID).Scan(&status); err != nil || status != 401 {
		t.Fatal("client cancellation dropped audit", status, err)
	}
}
