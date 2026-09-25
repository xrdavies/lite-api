package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testAdminCreationIdempotency(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token, idem string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
		r.RemoteAddr = "192.0.2.206:1234"
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
			t.Fatalf("admin creation failed: %d %s", w.Code, w.Body.String())
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
	clone := func(body map[string]any) map[string]any {
		raw, _ := json.Marshal(body)
		var result map[string]any
		_ = json.Unmarshal(raw, &result)
		return result
	}
	other := id(data(call("POST", "/api/v1/admin/users", admin, "", map[string]any{"email": "creation-admin@example.test", "password": "creation-admin-password", "role": "admin"})))
	defer a.DB.Exec("UPDATE users SET deleted_at=now(),status='inactive' WHERE id=$1", other)
	otherAdmin := data(call("POST", "/api/v1/auth/login", "", "", map[string]string{"email": "creation-admin@example.test", "password": "creation-admin-password"}))["access_token"].(string)
	backup := id(data(call("POST", "/api/v1/admin/proxies", admin, "", map[string]any{"name": "creation-backup", "protocol": "http", "host": "127.0.0.1", "port": 29200})))
	gid := id(data(call("POST", "/api/v1/admin/groups", admin, "", map[string]any{"name": "Creation idempotency", "platform": "openai"})))
	defer a.DB.Exec("UPDATE proxies SET deleted_at=now(),status='inactive' WHERE id=$1", backup)
	defer a.DB.Exec("UPDATE groups SET deleted_at=now(),status='inactive' WHERE id=$1", gid)
	claims, err := a.parseToken(admin)
	if err != nil {
		t.Fatal(err)
	}
	// More simultaneous requests than pool connections expose nested DB checkout
	// deadlocks while the first writer holds the idempotency transaction lock.
	a.DB.SetMaxOpenConns(6)
	defer a.DB.SetMaxOpenConns(20)
	for _, resource := range []struct {
		table  string
		body   map[string]any
		secret string
	}{
		{"accounts", map[string]any{"name": "creation-account", "platform": "openai", "type": "apikey", "proxy_id": backup, "group_ids": []int64{gid},
			"upstream_billing_probe_enabled": false, "credentials": map[string]any{"api_key": "creation-upstream-canary", "base_url": "http://127.0.0.1:29201", "api_protocol": "responses"},
			"extra": map[string]any{responseFilesKey: map[string]any{fmt.Sprint(gid): []string{"file_creation"}}}}, "creation-upstream-canary"},
		{"proxies", map[string]any{"name": "creation-proxy", "protocol": "http", "host": "127.0.0.1", "port": 29202, "password": "creation-proxy-canary", "fallback_mode": "proxy", "backup_proxy_id": backup}, "creation-proxy-canary"},
	} {
		t.Run(resource.table, func(t *testing.T) {
			path, scope := "/api/v1/admin/"+resource.table, "admin."+resource.table+".create"
			const idem = "admin-create-shared-key" // Separate operation scopes.
			body := resource.body
			defer a.DB.Exec("UPDATE " + resource.table + " SET deleted_at=now(),status='inactive' WHERE name LIKE 'creation-%'")
			var wg sync.WaitGroup
			results := make([]*httptest.ResponseRecorder, 10)
			for i := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = call("POST", path, admin, idem, body)
				}()
			}
			wg.Wait()
			created := data(results[0])
			createdID := id(created)
			replays := 0
			for _, w := range results {
				if id(data(w)) != createdID || !bytes.Equal(w.Body.Bytes(), results[0].Body.Bytes()) || bytes.Contains(w.Body.Bytes(), []byte(resource.secret)) {
					t.Fatal("replay changed result or leaked credential")
				}
				if w.Header().Get("Idempotency-Replayed") == "true" && w.Header().Get("X-Idempotency-Replayed") == "true" {
					replays++
				}
			}
			if replays != len(results)-1 {
				t.Fatal("concurrent creation not replayed", replays)
			}
			countName := func(name any) int {
				t.Helper()
				var n int
				if err := a.DB.QueryRow("SELECT count(*) FROM "+resource.table+" WHERE name=$1", name).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if countName(body["name"]) != 1 {
				t.Fatal("duplicate resource")
			}
			if resource.table == "accounts" {
				var groups int
				if err := a.DB.QueryRow("SELECT count(*) FROM account_groups WHERE account_id=$1 AND group_id=$2", createdID, gid).Scan(&groups); err != nil || groups != 1 {
					t.Fatal("account group transaction", groups, err)
				}
			} else if created["backup_proxy_id"] != float64(backup) {
				t.Fatal("fallback lost", created)
			}
			changed := clone(body)
			changed["name"] = "changed"
			changedSecret := clone(body)
			if resource.table == "accounts" {
				changedSecret["credentials"].(map[string]any)["api_key"] = "different-upstream-key"
			} else {
				changedSecret["password"] = "different-proxy-password"
			}
			for _, request := range []struct {
				token string
				body  any
			}{{admin, changed}, {admin, changedSecret}, {otherAdmin, body}} {
				if w := call("POST", path, request.token, idem, request.body); w.Code != 409 || bytes.Contains(w.Body.Bytes(), []byte(resource.secret)) {
					t.Fatal("request or actor conflict bypass", w.Code)
				}
			}
			for _, invalid := range []string{"two words", strings.Repeat("x", 129)} {
				if w := call("POST", path, admin, invalid, body); w.Code != 400 {
					t.Fatal("invalid idempotency header accepted", w.Code)
				}
			}
			duplicateRaw, _ := json.Marshal(body)
			duplicate := httptest.NewRequest("POST", path, bytes.NewReader(duplicateRaw))
			duplicate.Header.Set("Authorization", "Bearer "+admin)
			duplicate.Header.Add("Idempotency-Key", idem)
			duplicate.Header.Add("Idempotency-Key", "another")
			duplicateResult := httptest.NewRecorder()
			a.Handler().ServeHTTP(duplicateResult, duplicate)
			if duplicateResult.Code != 400 {
				t.Fatal("ambiguous idempotency headers accepted", duplicateResult.Code)
			}
			for _, request := range []struct {
				token string
				code  int
			}{{ordinary, 403}, {"", 401}} {
				if w := call("POST", path, request.token, idem, body); w.Code != request.code {
					t.Fatal("replay bypassed admin authorization", w.Code)
				}
			}
			// Roll back the resource and relationship when persisting its response
			// fails; a retry after SQL recovery may perform the creation once.
			exec("ALTER TABLE idempotency_records ADD CONSTRAINT test_admin_creation_failure CHECK(scope<>'" + scope + "') NOT VALID")
			defer a.DB.Exec("ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS test_admin_creation_failure")
			failed := clone(body)
			failed["name"] = "creation-rollback-" + resource.table
			if w := call("POST", path, admin, "rollback", failed); w.Code != 500 || countName(failed["name"]) != 0 {
				t.Fatal("replay record failure partially committed", w.Code)
			}
			var records int
			if err := a.DB.QueryRow("SELECT count(*) FROM idempotency_records WHERE scope=$1 AND idempotency_key_hash=$2", scope, digest("rollback")).Scan(&records); err != nil || records != 0 {
				t.Fatal("failed write persisted replay", records, err)
			}
			exec("ALTER TABLE idempotency_records DROP CONSTRAINT test_admin_creation_failure")
			recovered := id(data(call("POST", path, admin, "rollback", failed)))
			if id(data(call("POST", path, admin, "rollback", failed))) != recovered || countName(failed["name"]) != 1 {
				t.Fatal("recovered creation duplicated")
			}
			invalidRelation := clone(body)
			invalidRelation["name"] = "creation-invalid-relation-" + resource.table
			if resource.table == "accounts" {
				invalidRelation["group_ids"] = []int64{gid, 9223372036854775807}
			} else {
				invalidRelation["backup_proxy_id"] = int64(9223372036854775807)
			}
			if w := call("POST", path, admin, "invalid-relation", invalidRelation); w.Code != 400 || countName(invalidRelation["name"]) != 0 {
				t.Fatal("failed relation left a resource", w.Code)
			}
			// A rolled-back validation failure does not reserve the key or its
			// fingerprint, so a corrected request can safely succeed.
			data(call("POST", path, admin, "invalid-relation", body))
			// Replaying an old response never reapplies configuration or recreates
			// a deleted resource, even with a new App and changed address policy.
			resourcePath := fmt.Sprintf("%s/%d", path, createdID)
			data(call("PUT", resourcePath, admin, "", map[string]any{"name": "creation-renamed-" + resource.table, "status": "inactive"}))
			if id(data(call("POST", path, admin, idem, body))) != createdID {
				t.Fatal("updated resource replay failed")
			}
			if v := data(call("GET", resourcePath, admin, "", nil)); v["status"] != "inactive" || v["name"] != "creation-renamed-"+resource.table {
				t.Fatal("replay overwrote current configuration")
			}
			data(call("DELETE", resourcePath, admin, "", nil))
			exec("UPDATE proxies SET status='inactive' WHERE id=$1", backup)
			fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret}
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
			r.Header.Set("Idempotency-Key", idem)
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, &identity{ID: claims.UserID}))
			w := httptest.NewRecorder()
			var err error
			if resource.table == "accounts" {
				err = fresh.createAccount(w, r)
			} else {
				err = fresh.saveProxy(w, r)
			}
			if err != nil || id(data(w)) != createdID || w.Header().Get("Idempotency-Replayed") != "true" {
				t.Fatal("durable replay depends on current resource/transport", err)
			}
			if w := call("GET", resourcePath, admin, "", nil); w.Code != 404 {
				t.Fatal("deleted resource resurrected", w.Code)
			}
			if resource.table == "accounts" {
				if w := call("POST", path, admin, "inactive-proxy", body); w.Code != 400 {
					t.Fatal("new creation bypassed inactive proxy", w.Code)
				}
			}
			exec("UPDATE proxies SET status='active' WHERE id=$1", backup)
			exec("UPDATE idempotency_records SET expires_at=now()-interval '1 second' WHERE scope=$1 AND idempotency_key_hash=$2", scope, digest(idem))
			renewed := call("POST", path, admin, idem, body)
			if id(data(renewed)) == createdID || renewed.Header().Get("Idempotency-Replayed") != "" {
				t.Fatal("expired replay did not allow a new creation")
			}
			exec("UPDATE idempotency_records SET status='processing',expires_at=now()-interval '1 second' WHERE scope=$1 AND idempotency_key_hash=$2", scope, digest(idem))
			if w := call("POST", path, admin, idem, body); w.Code != 409 {
				t.Fatal("unresolved creation retried after expiry", w.Code)
			}
			noKey := clone(body)
			noKey["name"] = "creation-no-key-" + resource.table
			first := id(data(call("POST", path, admin, "", noKey)))
			if id(data(call("POST", path, admin, "", noKey))) == first || countName(noKey["name"]) != 2 {
				t.Fatal("missing key acquired unintended deduplication")
			}
			var leaked bool
			if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM idempotency_records WHERE scope=$1 AND strpos(to_jsonb(idempotency_records)::text,$2)>0)", scope, resource.secret).Scan(&leaked); err != nil || leaked {
				t.Fatal("creation replay stored secret", err)
			}
		})
	}
}
