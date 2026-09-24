package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKeyManagement(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token, idem string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.225:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("key management status %d", w.Code)
		}
		return out.Data
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		return data(call(method, path, token, "", body))
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "key-management@example.test", "password": "key-management-password", "balance": 10})
	uid := id(user)
	token := manage("POST", "/api/v1/auth/login", "", map[string]string{"email": "key-management@example.test", "password": "key-management-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Key management", "platform": "openai"}))
	const idem = "key-create-parallel"
	body := map[string]any{"name": "zeta", "group_id": gid, "expires_in_days": 7}
	results := make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = call("POST", "/api/v1/keys", token, idem, body)
		}()
	}
	wg.Wait()
	key := data(results[0])
	kid := id(key)
	keyPath := fmt.Sprintf("/api/v1/keys/%d", kid)
	replays := 0
	for _, w := range results {
		if v := data(w); id(v) != kid || v["key"] != key["key"] || v["expires_at"] != key["expires_at"] {
			t.Fatal("creation replay changed the credential or expiry")
		}
		if w.Header().Get("Idempotency-Replayed") == "true" && w.Header().Get("X-Idempotency-Replayed") == "true" {
			replays++
		}
	}
	if replays != len(results)-1 {
		t.Fatal("concurrent key creation did not replay", replays)
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM api_keys WHERE user_id=$1", uid).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate credential persisted", count, err)
	}
	changed := map[string]any{"name": "different", "group_id": gid, "expires_in_days": 7}
	for _, request := range []struct {
		token string
		body  any
	}{{token, changed}, {admin, body}} {
		w := call("POST", "/api/v1/keys", request.token, idem, request.body)
		if w.Code != 409 || strings.Contains(w.Body.String(), key["key"].(string)) {
			t.Fatal("idempotency actor/payload isolation failed", w.Code)
		}
	}
	for _, invalid := range []string{strings.Repeat("a", 129), "two words"} {
		if w := call("POST", "/api/v1/keys", token, invalid, body); w.Code != 400 {
			t.Fatal("invalid idempotency key accepted", w.Code)
		}
	}
	// A replay-record failure must roll back the credential as well.
	exec(`ALTER TABLE idempotency_records ADD CONSTRAINT test_key_replay_failure CHECK(scope<>'user.api_keys.create') NOT VALID`)
	defer a.DB.Exec("ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS test_key_replay_failure")
	if w := call("POST", "/api/v1/keys", token, "key-create-rollback", map[string]string{"name": "rollback"}); w.Code != 500 {
		t.Fatal("replay persistence failure accepted", w.Code)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM api_keys WHERE user_id=$1", uid).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed transaction left a usable credential", count, err)
	}
	exec("ALTER TABLE idempotency_records DROP CONSTRAINT test_key_replay_failure")
	rollback := data(call("POST", "/api/v1/keys", token, "key-create-rollback", map[string]string{"name": "rollback"}))
	manage("DELETE", fmt.Sprintf("/api/v1/keys/%d", id(rollback)), token, nil)
	// Replay is durable and must not recreate a deleted key.
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock}
	raw, _ := json.Marshal(map[string]string{"name": "rollback"})
	r := httptest.NewRequest("POST", "/api/v1/keys", bytes.NewReader(raw))
	r.Header.Set("Idempotency-Key", "key-create-rollback")
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, &identity{ID: uid}))
	w := httptest.NewRecorder()
	if err := fresh.createKey(w, r); err != nil || id(data(w)) != id(rollback) {
		t.Fatal("durable creation replay failed", err)
	}
	if w := call("GET", fmt.Sprintf("/api/v1/keys/%d", id(rollback)), token, "", nil); w.Code != 404 {
		t.Fatal("replay resurrected a deleted key", w.Code)
	}
	// List filters and sorting apply before pagination and never widen the owner.
	alpha := manage("POST", "/api/v1/keys", token, map[string]any{"name": "alpha", "group_id": gid, "status": "inactive", "custom_key": "list-needle-credential"})
	tied := manage("POST", "/api/v1/keys", token, map[string]any{"name": "alpha", "group_id": gid})
	loose := manage("POST", "/api/v1/keys", token, map[string]any{"name": "loose_%", "custom_key": "loose-list-credential"})
	manage("POST", "/api/v1/keys", admin, map[string]any{"name": "alpha", "group_id": gid})
	checkList := func(path, actor string, total int, want ...int64) {
		t.Helper()
		v := manage("GET", path, actor, nil)
		var got []int64
		for _, item := range v["items"].([]any) {
			row := item.(map[string]any)
			got = append(got, id(row))
			if row["user_id"] != float64(uid) {
				t.Fatal("key list crossed user scope")
			}
		}
		if int(v["total"].(float64)) != total || !slices.Equal(got, want) {
			t.Fatal("key filtering/sorting/pagination", path, got, want, v["total"])
		}
	}
	checkList(fmt.Sprintf("/api/v1/keys?group_id=%d&sort_by=name&sort_order=asc&page_size=1&page=2", gid), token, 3, id(tied))
	checkList("/api/v1/keys?group_id=0", token, 1, id(loose))
	checkList("/api/v1/keys?search=needle&status=inactive", token, 1, id(alpha))
	checkList("/api/v1/keys?search="+url.QueryEscape("_%"), token, 1, id(loose))
	checkList("/api/v1/keys?search=does-not-exist", token, 0)
	checkList(fmt.Sprintf("/api/v1/admin/users/%d/api-keys?sort_by=name&sort_order=asc", uid), admin, 4, id(alpha), id(tied), id(loose), kid)
	checkList("/api/v1/keys?sort_by="+url.QueryEscape("id; DROP TABLE api_keys"), token, 4, id(loose), id(tied), id(alpha), kid)
	for _, query := range []string{"group_id=-1", "group_id=invalid", "search=" + strings.Repeat("a", 101)} {
		if w := call("GET", "/api/v1/keys?"+query, token, "", nil); w.Code != 400 {
			t.Fatal("invalid key list filter accepted", w.Code)
		}
	}
	// Effective windows are a projection; queries and unrelated edits must not
	// reset the stored counters, including a stale counter without a start time.
	exec(`UPDATE api_keys SET usage_5h=1.12345678,window_5h_start=now(),
 usage_1d=2,window_1d_start=now()-interval '25 hours',usage_7d=3,window_7d_start=NULL WHERE id=$1`, kid)
	for _, v := range []map[string]any{
		manage("GET", keyPath, token, nil),
		manage("PUT", keyPath, token, map[string]string{"name": "window-view"}),
		manage("GET", "/api/v1/keys?search=window-view", token, nil)["items"].([]any)[0].(map[string]any),
		manage("GET", fmt.Sprintf("/api/v1/admin/users/%d/api-keys?search=window-view", uid), admin, nil)["items"].([]any)[0].(map[string]any),
	} {
		if v["usage_5h"] != 1.12345678 || v["usage_1d"] != float64(0) || v["usage_7d"] != float64(0) || v["reset_5h_at"] == nil || v["reset_1d_at"] != nil || v["reset_7d_at"] != nil || v["current_concurrency"] != float64(0) || v["last_used_ip"] != nil {
			t.Fatal("invalid effective key window projection")
		}
		start, e1 := time.Parse(time.RFC3339Nano, v["window_5h_start"].(string))
		reset, e2 := time.Parse(time.RFC3339Nano, v["reset_5h_at"].(string))
		if e1 != nil || e2 != nil || reset.Sub(start) != 5*time.Hour {
			t.Fatal("incorrect window reset time", e1, e2)
		}
	}
	var stale bool
	if err := a.DB.QueryRow("SELECT usage_1d=2 AND usage_7d=3 FROM api_keys WHERE id=$1", kid).Scan(&stale); err != nil || !stale {
		t.Fatal("key projection changed stored usage", err)
	}
	// Expiry changes only recover expired keys, preserving manual disables,
	// exhausted quotas, explicit status choices and concurrent consumption fields.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		status string
		body   map[string]any
		want   string
	}{
		{"expired", map[string]any{"expires_at": future}, "active"},
		{"expired", map[string]any{"expires_at": "", "quota": 10}, "active"},
		{"expired", map[string]any{"expires_at": past}, "expired"},
		{"expired", map[string]any{"expires_at": future, "status": "inactive"}, "inactive"},
		{"inactive", map[string]any{"expires_at": "", "quota": 10}, "inactive"},
		{"quota_exhausted", map[string]any{"expires_at": ""}, "quota_exhausted"},
		{"quota_exhausted", map[string]any{"expires_at": "", "quota": 10}, "active"},
	} {
		exec("UPDATE api_keys SET status=$2,expires_at=$3,quota=3,quota_used=3.12345678,usage_5h=1,window_5h_start=now() WHERE id=$1", kid, tc.status, past)
		v := manage("PUT", keyPath, token, tc.body)
		if v["status"] != tc.want || v["quota_used"] != 3.12345678 || v["usage_5h"] != float64(1) {
			t.Fatal("expiry recovery changed status or usage", tc.status, tc.want)
		}
	}
	// Shared balance idempotency remains independent of the creation operation.
	for i := 0; i < 2; i++ {
		v := data(call("POST", fmt.Sprintf("/api/v1/admin/users/%d/balance", uid), admin, idem, map[string]any{"operation": "add", "balance": "0.12345678"}))
		if v["balance"] != 10.12345678 {
			t.Fatal("write scopes collided or balance duplicated")
		}
	}
}
