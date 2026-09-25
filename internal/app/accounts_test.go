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
	"time"
)

func testAccountQueries(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const root = "/api/v1/admin/accounts"
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.227:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("account query %s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	group := int64(must("POST", "/api/v1/admin/groups", map[string]any{"name": "Account directory", "platform": "openai"})["id"].(float64))
	defer must("DELETE", fmt.Sprintf("/api/v1/admin/groups/%d", group), nil)
	ids := make([]int64, 6)
	for i, name := range []string{"zulu", "alpha", "alpha", "literal_%", "temporary", "error"} {
		v := must("POST", root, map[string]any{
			"name": "account-directory-" + name, "platform": "openai", "type": "apikey", "priority": i,
			"credentials": map[string]any{"api_key": "account-directory-upstream-secret"},
		})
		ids[i] = int64(v["id"].(float64))
		defer must("DELETE", fmt.Sprintf("%s/%d", root, ids[i]), nil)
	}
	must("PUT", fmt.Sprintf("%s/%d", root, ids[0]), map[string]any{"group_ids": []int64{group}})
	check := func(path, query string, total int, want ...int64) {
		t.Helper()
		v := must("GET", path+"?search=account-directory-&"+query, nil)
		got := []int64{}
		for _, entry := range v["items"].([]any) {
			row := entry.(map[string]any)
			field := "id"
			if path != root {
				field = "account_id"
			} else if row["has_api_key"] != true || row["credentials"].(map[string]any)["api_key"] != nil || row["deleted_at"] != nil || row["current_concurrency"] != float64(0) {
				t.Fatal("invalid account projection", row["id"])
			}
			got = append(got, int64(row[field].(float64)))
		}
		if v["total"] != float64(total) || !slices.Equal(got, want) {
			t.Fatal("account filter/order/page", path, query, got, want, v["total"])
		}
	}
	check(root, "", 6, ids[1], ids[2], ids[5], ids[3], ids[4], ids[0])
	check(root, "page_size=1&page=2", 6, ids[2])
	check(root, "sort_by=name&sort_order=desc&page_size=1&page=5", 6, ids[2])
	check(root, "sort_by=priority&sort_order=desc&page_size=1", 6, ids[5])
	check(root, "sort_by="+url.QueryEscape("id; DROP TABLE accounts"), 6, ids[1], ids[2], ids[5], ids[3], ids[4], ids[0])
	exec("UPDATE accounts SET schedulable=false WHERE id=$1", ids[1])
	exec("UPDATE accounts SET rate_limit_reset_at=now()+interval '1 hour' WHERE id IN ($1,$2)", ids[2], ids[4])
	exec("UPDATE accounts SET temp_unschedulable_until=now()+interval '1 hour' WHERE id=$1", ids[4])
	exec("UPDATE accounts SET status='error' WHERE id=$1", ids[5])
	// Both consumers must bind optional status/group arguments before pagination.
	// Temporary suspension takes precedence over the simultaneous rate limit.
	for _, path := range []string{root, root + "/upstream-billing-rates"} {
		check(path, "status=active&sort_by=id", 2, ids[0], ids[3])
		check(path, "status=unschedulable&sort_by=id", 1, ids[1])
		check(path, "status=rate_limited&sort_by=id", 1, ids[2])
		check(path, "status=temp_unschedulable&sort_by=id", 1, ids[4])
		check(path, "status=error&sort_by=id", 1, ids[5])
		check(path, fmt.Sprintf("status=active&group=%d&sort_by=id", group), 1, ids[0])
		check(path, fmt.Sprintf("status=error&group=%d", group), 0)
		check(path, "status=active&group=ungrouped&sort_by=id", 1, ids[3])
		check(path, "group=0&type=apikey&platform=openai&sort_by=id&page_size=1&page=6", 6, ids[5])
		check(path, "platform=grok", 0)
		check(path, "type=oauth", 0)
		literal := must("GET", path+"?search="+url.QueryEscape("literal_%"), nil)
		if literal["total"] != float64(1) {
			t.Fatal("search treated literal wildcard characters as a pattern")
		}
		for _, query := range []string{"group=-1", "group=invalid", "group=9223372036854775808", "search=" + strings.Repeat("x", 101)} {
			if w := call("GET", path+"?"+query, admin, nil); w.Code != 400 {
				t.Fatal("invalid account filter accepted", path, query, w.Code)
			}
		}
		if w := call("GET", path, ordinary, nil); w.Code != 403 {
			t.Fatal("ordinary user read administrator accounts", w.Code)
		}
	}
	// Reads must not clear persisted cooldowns; expired windows only affect filtering.
	exec("UPDATE accounts SET rate_limit_reset_at=now()-interval '1 hour',temp_unschedulable_until=now()-interval '1 hour' WHERE id IN ($1,$2)", ids[2], ids[4])
	check(root, "status=active&sort_by=id", 4, ids[0], ids[2], ids[3], ids[4])
	var persisted bool
	if err := a.DB.QueryRow("SELECT rate_limit_reset_at IS NOT NULL AND temp_unschedulable_until IS NOT NULL FROM accounts WHERE id=$1", ids[4]).Scan(&persisted); err != nil || !persisted {
		t.Fatal("account listing rewrote cooldown state", err)
	}
	// An unchanged schema may hold out-of-scope rows, but they must stay excluded.
	exec("UPDATE accounts SET type='oauth' WHERE id=$1", ids[1])
	exec("UPDATE accounts SET platform='antigravity' WHERE id=$1", ids[2])
	exec(`UPDATE accounts SET credentials=credentials||'{"account_mode":"plan"}'::jsonb WHERE id=$1`, ids[3])
	must("DELETE", fmt.Sprintf("%s/%d", root, ids[4]), nil)
	// Restore the deleted test row only for the deferred cleanup above.
	defer exec("UPDATE accounts SET deleted_at=NULL WHERE id=$1", ids[4])
	for _, path := range []string{root, root + "/upstream-billing-rates"} {
		check(path, "sort_by=id", 2, ids[0], ids[5])
	}
}

func testAccountState(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.RemoteAddr = "192.0.2.229:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("account state %s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	id := int64(must("POST", "/api/v1/admin/accounts", map[string]any{"name": "State controls", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "state-test-secret"}})["id"].(float64))
	path := fmt.Sprintf("/api/v1/admin/accounts/%d", id)
	defer a.DB.Exec("UPDATE accounts SET deleted_at=now(),status='inactive',schedulable=false WHERE id=$1", id)
	if v := must("GET", path+"/temp-unschedulable", nil); v["active"] != false || v["state"] != nil {
		t.Fatal("empty temporary status", v)
	}
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, reason := range []string{"plain state-test-secret", `{"error_message":"state-test-secret","matched_keyword":"state-test-secret","status_code":429,"rule_index":2,"trigger_count":3,"trigger_threshold":3,"trigger_window_minutes":5,"unknown":"hidden"}`, `{"until_unix":1,"status_code":"invalid"}`} {
		exec("UPDATE accounts SET temp_unschedulable_until=$2,temp_unschedulable_reason=$3 WHERE id=$1", id, until, reason)
		v := must("GET", path+"/temp-unschedulable", nil)
		state, ok := v["state"].(map[string]any)
		raw, _ := json.Marshal(v)
		if v["active"] != true || !ok || state["until_unix"] != float64(until.Unix()) || strings.Contains(string(raw), "state-test-secret") || state["unknown"] != nil {
			t.Fatal("temporary status projection", string(raw))
		}
		if strings.Contains(reason, "trigger_count") && (state["trigger_count"] != float64(3) || state["status_code"] != float64(429) || state["matched_keyword"] != "[REDACTED]") {
			t.Fatal("structured temporary state lost fields", state)
		}
	}
	exec("UPDATE accounts SET temp_unschedulable_until=now()-interval '1 minute' WHERE id=$1", id)
	if v := must("GET", path+"/temp-unschedulable", nil); v["active"] != false {
		t.Fatal("expired temporary block reported active", v)
	}
	var retained bool
	if err := a.DB.QueryRow("SELECT temp_unschedulable_until IS NOT NULL FROM accounts WHERE id=$1", id).Scan(&retained); err != nil || !retained {
		t.Fatal("status read rewrote persisted state", err)
	}
	seed := func() {
		exec(`UPDATE accounts SET status='error',schedulable=false,error_message='keep',rate_limited_at=now(),rate_limit_reset_at=now()+interval '1 hour',overload_until=now()+interval '1 hour',temp_unschedulable_until=now()+interval '1 hour',temp_unschedulable_reason='keep',extra=extra||'{"quota_limit":10,"quota_used":9,"quota_daily_used":8,"quota_weekly_used":7,"quota_daily_start":"2026-01-01","quota_weekly_start":"2026-01-01","quota_daily_reset_at":"2026-01-02","quota_weekly_reset_at":"2026-01-08","model_rate_limits":{"m":1},"unknown":{"keep":true}}'::jsonb WHERE id=$1`, id)
	}
	seed()
	for _, endpoint := range []struct{ method, path string }{{"POST", path + "/reset-quota"}, {"GET", path + "/temp-unschedulable"}, {"DELETE", path + "/temp-unschedulable"}} {
		if w := call(endpoint.method, endpoint.path, ordinary, nil); w.Code != 403 {
			t.Fatal("ordinary user can manage account state", w.Code)
		}
	}
	// One failing UPDATE cannot partially reset counters or scheduler state.
	exec(fmt.Sprintf(`CREATE FUNCTION reject_state_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=%d THEN RAISE EXCEPTION 'injected state reset failure'; END IF; RETURN NEW; END $$`, id))
	exec("CREATE TRIGGER reject_state_reset BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION reject_state_reset()")
	defer a.DB.Exec("DROP TRIGGER IF EXISTS reject_state_reset ON accounts; DROP FUNCTION IF EXISTS reject_state_reset()")
	if w := call("POST", path+"/reset-quota", admin, nil); w.Code != 500 {
		t.Fatal("failed quota reset returned success", w.Code)
	}
	if err := a.DB.QueryRow("SELECT extra->>'quota_used'='9' AND rate_limit_reset_at IS NOT NULL FROM accounts WHERE id=$1", id).Scan(&retained); err != nil || !retained {
		t.Fatal("quota reset partially committed", err)
	}
	exec("DROP TRIGGER reject_state_reset ON accounts; DROP FUNCTION reject_state_reset()")
	v := must("POST", path+"/reset-quota", nil)
	extra := v["extra"].(map[string]any)
	if v["rate_limited_at"] != nil || v["rate_limit_reset_at"] != nil || v["overload_until"] == nil || v["temp_unschedulable_until"] == nil || v["status"] != "error" || v["schedulable"] != false || v["error_message"] != "keep" || extra["quota_limit"] != float64(10) || extra["unknown"] == nil || extra["model_rate_limits"] == nil {
		t.Fatal("quota reset changed unrelated account state")
	}
	for _, name := range []string{"quota_used", "quota_daily_used", "quota_weekly_used"} {
		if extra[name] != float64(0) {
			t.Fatal("quota counter not reset", name)
		}
	}
	for _, name := range []string{"quota_daily_start", "quota_weekly_start", "quota_daily_reset_at", "quota_weekly_reset_at"} {
		if extra[name] != nil {
			t.Fatal("quota window not cleared", name)
		}
	}
	seed()
	v = must("DELETE", path+"/temp-unschedulable", nil)
	if v["temp_unschedulable_until"] != nil || v["temp_unschedulable_reason"] != nil || v["rate_limit_reset_at"] == nil || v["overload_until"] == nil || v["status"] != "error" || v["schedulable"] != false || v["extra"].(map[string]any)["quota_used"] != float64(9) || v["extra"].(map[string]any)["model_rate_limits"] != nil {
		t.Fatal("clearing temporary state changed unrelated blocks/counters")
	}
	must("DELETE", path, nil)
	for _, endpoint := range []struct{ method, path string }{{"POST", path + "/reset-quota"}, {"GET", path + "/temp-unschedulable"}, {"DELETE", path + "/temp-unschedulable"}} {
		if w := call(endpoint.method, endpoint.path, admin, nil); w.Code != 404 {
			t.Fatal("deleted account state accessible", w.Code)
		}
	}
}
