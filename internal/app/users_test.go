package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func testUserQueries(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.226:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("user query %s %s: %d", method, path, w.Code)
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
	uid := make([]int64, 3)
	for i, name := range []string{"charlie", "alpha", "alpha"} {
		uid[i] = id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": fmt.Sprintf("directory-%d@example.test", i), "password": "directory-password", "username": name, "balance": i + 1}))
	}
	userPath := func(id int64) string { return fmt.Sprintf("/api/v1/admin/users/%d", id) }
	login := must("POST", "/api/v1/auth/login", "", map[string]string{"email": "directory-0@example.test", "password": "directory-password"})
	user := login["access_token"].(string)
	group := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Directory_% team", "platform": "openai"}))
	grantOnly := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Directory grants", "platform": "openai", "is_exclusive": true}))
	must("PUT", userPath(uid[0]), admin, map[string]any{"notes": "literal_% marker", "allowed_groups": []int64{grantOnly}})
	must("PUT", userPath(uid[1]), admin, map[string]any{"allowed_groups": []int64{group}, "status": "disabled"})
	must("PUT", userPath(uid[2]), admin, map[string]any{"allowed_groups": []int64{group}})
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "directory", "group_id": group, "custom_key": "directory-search-credential"})
	kid := id(key)
	must("POST", "/api/v1/keys", user, map[string]any{"name": "duplicate membership", "group_id": group})
	check := func(query string, total int, want ...int64) {
		t.Helper()
		v := must("GET", "/api/v1/admin/users?"+query, admin, nil)
		var got []int64
		for _, item := range v["items"].([]any) {
			u := item.(map[string]any)
			got = append(got, id(u))
			if u["password_hash"] != nil || u["deleted_at"] != nil || u["current_concurrency"] != float64(0) {
				t.Fatal("invalid listed user fields")
			}
		}
		if v["total"] != float64(total) || !slices.Equal(got, want) {
			t.Fatal("user query filtering/sorting/pagination", query, got, want, v["total"])
		}
	}
	check("search=directory-&sort_by=username&sort_order=asc", 3, uid[1], uid[2], uid[0])
	check("search=directory-&sort_by=username&sort_order=asc&page_size=1&page=2", 3, uid[2])
	check("search=directory-&sort_by=balance&sort_order=desc&page_size=1", 3, uid[2])
	check("search=directory-&status=disabled", 1, uid[1])
	check("search=directory-&role=admin", 0)
	check("search="+url.QueryEscape("literal_%"), 1, uid[0])
	check("search=directory-search-credential", 1, uid[0])
	check("group_name="+url.QueryEscape("_%")+"&sort_by=id&sort_order=asc", 2, uid[1], uid[2])
	check(fmt.Sprintf("api_key_group_id=%d", group), 1, uid[0])
	check(fmt.Sprintf("api_key_group_id=%d", grantOnly), 0)
	check(fmt.Sprintf("api_key_group_id=%d&group_name=Directory", group), 1, uid[0])
	check("search=directory-&api_key_group_id=0", 3, uid[2], uid[1], uid[0])
	check("search=directory-&sort_by="+url.QueryEscape("id; DROP TABLE users"), 3, uid[2], uid[1], uid[0])
	for _, query := range []string{"api_key_group_id=-1", "api_key_group_id=invalid", "search=" + strings.Repeat("x", 101), "group_name=" + strings.Repeat("x", 101)} {
		if w := call("GET", "/api/v1/admin/users?"+query, admin, nil); w.Code != 400 {
			t.Fatal("invalid user query accepted", query, w.Code)
		}
	}
	// Explicit timestamps exercise stable ordering and null placement, independent
	// of wall-clock resolution and the sequence in which the users were inserted.
	exec("UPDATE users SET created_at='2026-01-01',last_active_at=NULL WHERE id IN ($1,$2,$3)", uid[0], uid[1], uid[2])
	exec("UPDATE users SET created_at='2025-01-01',last_active_at='2026-01-01' WHERE id=$1", uid[2])
	exec("UPDATE users SET last_active_at='2026-02-01' WHERE id=$1", uid[0])
	check("search=directory-", 3, uid[1], uid[0], uid[2])
	check("search=directory-&sort_by=last_active_at&sort_order=asc", 3, uid[2], uid[0], uid[1])
	check("search=directory-&sort_by=last_active_at", 3, uid[0], uid[2], uid[1])
	// Reuse a settled request from the preceding gateway check to preserve every
	// required usage column; only its owner, Key, request identity and date change.
	exec(`INSERT INTO usage_logs SELECT (jsonb_populate_record(NULL::usage_logs,
 to_jsonb(l)||jsonb_build_object('id',nextval('usage_logs_id_seq'),'user_id',$1::bigint,'api_key_id',$2::bigint,
 'request_id','directory-usage-first','created_at','2026-03-01T00:00:00Z'))).* FROM usage_logs l
 WHERE model='relations-model' LIMIT 1`, uid[0], kid)
	check("search=directory-&sort_by=last_used_at", 3, uid[0], uid[2], uid[1])
	check("search=directory-&sort_by=last_used_at&sort_order=asc&page_size=1&page=3", 3, uid[0])
	// Deleted keys stop matching both credential and actual group membership.
	must("DELETE", fmt.Sprintf("/api/v1/keys/%d", kid), user, nil)
	check("search=directory-search-credential", 0)
	check(fmt.Sprintf("api_key_group_id=%d", group), 1, uid[0])
	exec("UPDATE api_keys SET deleted_at=now(),status='inactive' WHERE user_id=$1", uid[0])
	check(fmt.Sprintf("api_key_group_id=%d", group), 0)
	must("DELETE", fmt.Sprintf("/api/v1/admin/groups/%d", group), admin, nil)
	check("group_name="+url.QueryEscape("_%"), 0)
	// A deleted user's history is explicitly readable by administrators, while
	// sessions, keys, edits and balance changes continue to reject that identity.
	for _, credential := range []string{"", user, key["key"].(string)} {
		want := 401
		if credential == user {
			want = 403
		}
		for _, path := range []string{"/api/v1/admin/users", userPath(uid[1]) + "?include_deleted=true"} {
			if w := call("GET", path, credential, nil); w.Code != want {
				t.Fatal("user directory permission boundary", w.Code, want)
			}
		}
	}
	must("DELETE", userPath(uid[0]), admin, nil)
	check("search=directory-&include_deleted=true&sort_by=id&sort_order=asc", 2, uid[1], uid[2])
	if w := call("GET", userPath(uid[0]), admin, nil); w.Code != 404 {
		t.Fatal("deleted user returned without opt-in", w.Code)
	}
	history := must("GET", userPath(uid[0])+"?include_deleted=true", admin, nil)
	if history["deleted_at"] == nil || history["status"] != "disabled" || history["last_used_at"] == nil || history["password_hash"] != nil || len(history["allowed_groups"].([]any)) != 0 {
		t.Fatal("invalid historical user projection")
	}
	for _, path := range []string{"/api/v1/user/profile", "/api/v1/keys", userPath(uid[0]) + "?include_deleted=true"} {
		if w := call("GET", path, user, nil); w.Code != 401 {
			t.Fatal("deleted user's session remained active", path, w.Code)
		}
	}
	for _, request := range []struct {
		method, path string
		body         any
	}{
		{"PUT", userPath(uid[0]) + "?include_deleted=true", map[string]any{"status": "active"}},
		{"POST", userPath(uid[0]) + "/balance?include_deleted=true", map[string]any{"operation": "add", "balance": 1}},
	} {
		if w := call(request.method, request.path, admin, request.body); w.Code != 404 {
			t.Fatal("history option allowed deleted user mutation", w.Code)
		}
	}
	if w := call("GET", "/v1/models", key["key"].(string), nil); w.Code != 401 {
		t.Fatal("deleted user's key remained usable", w.Code)
	}
	if w := call("POST", "/api/v1/auth/login", "", map[string]string{"email": "directory-0@example.test", "password": "directory-password"}); w.Code != 401 {
		t.Fatal("deleted user could log in", w.Code)
	}
}
