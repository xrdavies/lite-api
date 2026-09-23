package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testGroupOverrides(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.92:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var envelope struct{ Data json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	expect := func(status int, method, path, token string, body any) {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != status {
			t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, status, w.Body.String())
		}
	}
	idOf := func(raw json.RawMessage) int64 {
		var obj struct{ ID int64 }
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		return obj.ID
	}
	textField := func(raw json.RawMessage, name string) string {
		var obj map[string]json.RawMessage
		var value string
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(obj[name], &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "overrides@example.test", "password": "override-password", "balance": "10", "rpm_limit": 1}))
	other := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "overrides-other@example.test", "password": "override-password"}))
	user := textField(must("POST", "/api/v1/auth/login", "", map[string]any{"email": "overrides@example.test", "password": "override-password"}), "access_token")
	gid := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Overrides", "platform": "openai", "rpm_limit": 1}))
	group := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	userPath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	key := textField(must("POST", "/api/v1/keys", user, map[string]any{"name": "Overrides", "group_id": gid}), "key")
	key2 := textField(must("POST", "/api/v1/keys", user, map[string]any{"name": "Same user and group", "group_id": gid}), "key")
	body := func(field string, entries map[int64]any) map[string]any {
		items := []any{}
		for id, value := range entries {
			items = append(items, map[string]any{"user_id": id, field: value})
		}
		return map[string]any{"entries": items}
	}
	setRate := func(entries map[int64]any) {
		t.Helper()
		must("PUT", group+"/rate-multipliers", admin, body("rate_multiplier", entries))
	}
	setRPM := func(entries map[int64]any) {
		t.Helper()
		must("PUT", group+"/rpm-overrides", admin, body("rpm_override", entries))
	}
	check := func(id int64, wantRate, wantRPM any) {
		t.Helper()
		var rate sql.NullString
		var rpm sql.NullInt64
		err := a.DB.QueryRow("SELECT rate_multiplier::text,rpm_override FROM user_group_rate_multipliers WHERE user_id=$1 AND group_id=$2", id, gid).Scan(&rate, &rpm)
		if wantRate == nil && wantRPM == nil {
			if err != sql.ErrNoRows {
				t.Fatal("empty override row retained", rate, rpm, err)
			}
			return
		}
		if err != nil || rate.Valid != (wantRate != nil) || rpm.Valid != (wantRPM != nil) || wantRate != nil && rate.String != wantRate || wantRPM != nil && rpm.Int64 != int64(wantRPM.(int)) {
			t.Fatal("override mismatch", rate, rpm, wantRate, wantRPM, err)
		}
	}
	for _, path := range []string{group + "/rate-multipliers", userPath + "/rpm-status"} {
		expect(401, "GET", path, "", nil)
		expect(403, "GET", path, user, nil)
	}
	for _, suffix := range []string{"/rate-multipliers", "/rpm-overrides"} {
		for _, method := range []string{"PUT", "DELETE"} {
			expect(403, method, group+suffix, user, map[string]any{"entries": []any{}})
		}
		expect(400, "PUT", group+suffix, admin, map[string]any{})
	}
	setRate(map[int64]any{uid: "0.1234", other: "2"})
	setRPM(map[int64]any{uid: 0, other: 4})
	check(uid, "0.1234", 0)
	check(other, "2.0000", 4)
	setRate(map[int64]any{uid: "0.1234"})
	check(other, nil, 4)
	setRPM(map[int64]any{uid: 0, other: nil})
	check(other, nil, nil)
	list := must("GET", group+"/rate-multipliers", admin, nil)
	if !bytes.Contains(list, []byte(`"rate_multiplier":0.1234`)) || !bytes.Contains(list, []byte(`"rpm_override":0`)) || bytes.Contains(list, []byte("password")) {
		t.Fatal("override list", string(list))
	}
	for _, entry := range []any{map[string]any{"user_id": uid, "rate_multiplier": 0}, map[string]any{"user_id": uid, "rate_multiplier": "0.00001"}, map[string]any{"user_id": uid, "rate_multiplier": nil}} {
		expect(400, "PUT", group+"/rate-multipliers", admin, map[string]any{"entries": []any{entry}})
	}
	expect(400, "PUT", group+"/rpm-overrides", admin, body("rpm_override", map[int64]any{uid: -1}))
	expect(400, "PUT", group+"/rpm-overrides", admin, map[string]any{"entries": []any{map[string]any{"user_id": uid, "rpm_override": 1}, map[string]any{"user_id": uid, "rpm_override": 2}}})
	expect(400, "PUT", group+"/rate-multipliers", admin, body("rate_multiplier", map[int64]any{uid: "4", 999999: "5"}))
	check(uid, "0.1234", 0)
	// A database failure after clearing the old values rolls back the whole replacement.
	if _, err := a.DB.Exec(`CREATE FUNCTION test_override_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'override test failure'; END $$;
 CREATE TRIGGER test_override_failure BEFORE INSERT ON user_group_rate_multipliers FOR EACH ROW EXECUTE FUNCTION test_override_failure()`); err != nil {
		t.Fatal(err)
	}
	expect(500, "PUT", group+"/rate-multipliers", admin, body("rate_multiplier", map[int64]any{other: "5"}))
	if _, err := a.DB.Exec("DROP TRIGGER test_override_failure ON user_group_rate_multipliers; DROP FUNCTION test_override_failure()"); err != nil {
		t.Fatal(err)
	}
	check(uid, "0.1234", 0)
	// Both replacement dimensions survive concurrent writes and user-level clears.
	var wg sync.WaitGroup
	for _, field := range []string{"rate_multiplier", "rpm_override"} {
		wg.Add(1)
		go func(field string) {
			defer wg.Done()
			path, value := group+"/rate-multipliers", any("0.1234")
			if field == "rpm_override" {
				path, value = group+"/rpm-overrides", 0
			}
			w := call("PUT", path, admin, body(field, map[int64]any{uid: value}))
			if w.Code != 200 {
				t.Error("concurrent override write", w.Code, w.Body.String())
			}
		}(field)
	}
	wg.Wait()
	check(uid, "0.1234", 0)
	must("PUT", userPath, admin, map[string]any{"group_rates": map[string]any{}})
	check(uid, nil, 0)
	setRate(map[int64]any{uid: "0.1234"})
	var calls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"id":"rpm","model":"rpm-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer up.Close()
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "RPM upstream", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "rpm-upstream", "base_url": up.URL}})
	must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "RPM prices", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"rpm-model"}, "billing_mode": "per_request", "per_request_price": "0.1"}}})
	request := map[string]any{"model": "rpm-model", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
	// Seed adjacent minute buckets too, so checks are stable over a clock boundary.
	seed := func(userCount, groupCount int) {
		t.Helper()
		minute := time.Now().Unix() / 60
		for _, m := range []int64{minute, minute + 1} {
			for key, value := range map[string]int{fmt.Sprintf("gateway:rpm:u:%d:%d", uid, m): userCount, fmt.Sprintf("gateway:rpm:g:%d:%d:%d", uid, gid, m): groupCount} {
				if err := a.Redis.Set(context.Background(), key, value, 2*time.Minute).Err(); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	seed(1, 20)
	expect(429, "POST", "/v1/chat/completions", key, request) // Zero group override cannot bypass the user cap.
	if calls.Load() != 0 {
		t.Fatal("RPM rejection reached upstream")
	}
	must("PUT", userPath, admin, map[string]any{"rpm_limit": 0})
	w := call("POST", "/v1/chat/completions", key, request) // Zero override does bypass the group cap.
	if w.Code != 200 {
		t.Fatal("zero override", w.Code, w.Body.String())
	}
	var actual string
	if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1 AND user_id=$2", w.Header().Get("X-Request-ID"), uid).Scan(&actual); err != nil || actual != "0.0123400000" {
		t.Fatal("dedicated multiplier not billed exactly", actual, err)
	}
	setRPM(map[int64]any{uid: nil})
	seed(2, 1)
	expect(429, "POST", "/v1/chat/completions", key, request)
	setRPM(map[int64]any{uid: 2})
	seed(2, 2)
	expect(429, "POST", "/v1/chat/completions", key2, request) // Keys share the user/group bucket.
	setRPM(map[int64]any{uid: 3})
	expect(200, "POST", "/v1/chat/completions", key, request) // Configuration applies without clearing counters.
	seed(3, 3)
	expect(429, "POST", "/v1/chat/completions", key2, request)
	status := must("GET", userPath+"/rpm-status", admin, nil)
	if !bytes.Contains(status, []byte(`"user_rpm_used":3`)) || !bytes.Contains(status, []byte(`"used":3`)) || !bytes.Contains(status, []byte(`"source":"override"`)) || !bytes.Contains(status, []byte(`"limit":3`)) {
		t.Fatal("runtime RPM status", string(status))
	}
	must("DELETE", group+"/rpm-overrides", admin, nil)
	check(uid, "0.1234", nil)
	setRPM(map[int64]any{uid: 7})
	must("DELETE", group+"/rate-multipliers", admin, nil)
	check(uid, nil, nil) // This endpoint preserves the established whole-entry clear.
	setRate(map[int64]any{uid: "1"})
	must("PUT", userPath, admin, map[string]any{"group_rates": map[string]any{fmt.Sprint(gid): nil}})
	check(uid, nil, nil)
	if raw := must("GET", group+"/rate-multipliers", admin, nil); string(raw) != "[]" {
		t.Fatal("empty override list", string(raw))
	}
	must("DELETE", group, admin, nil)
	expect(404, "PUT", group+"/rpm-overrides", admin, body("rpm_override", map[int64]any{uid: 1}))
	expect(404, "GET", group+"/rate-multipliers", admin, nil)
	expect(404, "GET", "/api/v1/admin/users/999999/rpm-status", admin, nil)
}
