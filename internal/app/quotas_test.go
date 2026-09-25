package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testPlatformQuotaViews(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.248:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("quota %s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	u := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "quota-view@example.test", "password": "quota-view-password"})
	uid := string(u["id"])
	root := "/api/v1/admin/users/" + uid
	user := credentialString(manage("POST", "/api/v1/auth/login", "", map[string]string{"email": "quota-view@example.test", "password": "quota-view-password"}), "access_token")
	key := credentialString(manage("POST", "/api/v1/keys", user, map[string]string{"name": "quota-view"}), "key")
	view := func(path, token string) []map[string]json.RawMessage {
		t.Helper()
		data := manage("GET", path, token, nil)
		var rows []map[string]json.RawMessage
		if json.Unmarshal(data["platform_quotas"], &rows) != nil || rows == nil {
			t.Fatal("quota list must be an array", data)
		}
		return rows
	}
	const personal = "/api/v1/user/platform-quotas"
	if len(view(personal, user)) != 0 || len(view(root+"/platform-quotas", admin)) != 0 {
		t.Fatal("new user has quotas")
	}
	manage("PUT", root+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": json.Number("1234567890.1234567890"), "weekly_limit_usd": nil, "monthly_limit_usd": 0}}})
	now := time.Now().UTC().Truncate(time.Second)
	day, week := quotaStarts(now)
	monthly := now.Add(-15 * 24 * time.Hour)
	exec(`UPDATE user_platform_quotas SET daily_usage_usd=1.1234567890,weekly_usage_usd=2.1234567890,monthly_usage_usd=3.1234567890,
 daily_window_start=$2,weekly_window_start=$3,monthly_window_start=$4 WHERE user_id=$1`, uid, day.Add(time.Second), week.Add(time.Second), monthly)
	exec(`INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,deleted_at) VALUES($1,'gemini',1,now())`, uid)
	snapshot := func() string {
		t.Helper()
		var raw string
		if err := a.DB.QueryRow("SELECT jsonb_agg(to_jsonb(q) ORDER BY id)::text FROM user_platform_quotas q WHERE user_id=$1", uid).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for _, adminView := range []bool{false, true} {
		path, token, fields := personal+"?user_id=1", user, 10
		if adminView {
			path, token, fields = root+"/platform-quotas", admin, 13
		}
		rows := view(path, token)
		if len(rows) != 1 || len(rows[0]) != fields || credentialString(rows[0], "platform") != "openai" {
			t.Fatal("quota ownership or projection", rows)
		}
		row := rows[0]
		if string(row["daily_limit_usd"]) != "1234567890.1234567890" || string(row["weekly_limit_usd"]) != "null" || rat(json.Number(row["monthly_limit_usd"])).Sign() != 0 {
			t.Fatal("quota precision or null/zero meanings", row)
		}
		for i, window := range []string{"daily", "weekly", "monthly"} {
			if string(row[window+"_usage_usd"]) != fmt.Sprintf("%d.1234567890", i+1) {
				t.Fatal("active quota usage", row)
			}
			reset, err := time.Parse(time.RFC3339Nano, credentialString(row, window+"_window_resets_at"))
			want := []time.Time{day.AddDate(0, 0, 1), week.AddDate(0, 0, 7), monthly.Add(720 * time.Hour)}[i]
			if err != nil || !reset.Equal(want) || adminView && row[window+"_window_start"] == nil {
				t.Fatal("quota window reset/start", window, reset, want, err)
			}
		}
	}
	if snapshot() != before || len(view(personal+"?user_id="+uid, ordinary)) != 0 {
		t.Fatal("quota read mutated storage or escaped owner")
	}
	for _, state := range []string{"'2000-01-01'", "NULL"} {
		exec("UPDATE user_platform_quotas SET daily_window_start="+state+",weekly_window_start="+state+",monthly_window_start="+state+" WHERE user_id=$1", uid)
		before = snapshot()
		row := view(personal, user)[0]
		for i, window := range []string{"daily", "weekly", "monthly"} {
			usage := json.Number(row[window+"_usage_usd"])
			if state == "NULL" && usage.String() != fmt.Sprintf("%d.1234567890", i+1) || state != "NULL" && rat(usage).Sign() != 0 || string(row[window+"_window_resets_at"]) != "null" {
				t.Fatal("expired/uninitialized quota projection", row)
			}
		}
		if snapshot() != before {
			t.Fatal("quota read rewrote expired windows")
		}
	}
	for _, window := range []string{"daily", "weekly", "monthly"} {
		data := manage("POST", root+"/platform-quotas/reset", admin, map[string]string{"platform": "openai", "window": window})
		var rows []map[string]json.RawMessage
		if json.Unmarshal(data["platform_quotas"], &rows) != nil || len(rows) != 1 || rat(json.Number(rows[0][window+"_usage_usd"])).Sign() != 0 || string(rows[0][window+"_window_resets_at"]) == "null" {
			t.Fatal("quota reset response", data)
		}
	}
	for _, path := range []string{personal, root + "/platform-quotas"} {
		for _, token := range []string{"", key} {
			if w := call("GET", path, token, nil); w.Code != 401 {
				t.Fatal("quota authentication", path, w.Code)
			}
		}
	}
	if w := call("GET", root+"/platform-quotas", user, nil); w.Code != 403 {
		t.Fatal("quota administrator boundary", w.Code)
	}
	if w := call("GET", "/api/v1/admin/users/9223372036854775807/platform-quotas", admin, nil); w.Code != 404 {
		t.Fatal("nonexistent quota user", w.Code)
	}
	manage("DELETE", root, admin, nil)
	before = snapshot()
	for _, method := range []string{"GET", "POST"} {
		path := root + "/platform-quotas"
		if method == "POST" {
			path += "/reset"
		}
		if w := call(method, path, admin, map[string]string{"platform": "openai", "window": "daily"}); w.Code != 404 {
			t.Fatal("deleted quota user accessible", method, w.Code)
		}
	}
	if snapshot() != before {
		t.Fatal("deleted user's quota reset")
	}
	if w := call("GET", personal, user, nil); w.Code != 401 || strings.Contains(w.Body.String(), "1234567890") {
		t.Fatal("deleted user quota session", w.Code)
	}
}

func TestQuotaWindows(t *testing.T) {
	now := time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC) // Monday midnight in Asia/Shanghai.
	day, week := quotaStarts(now)
	if !day.Equal(now) || !week.Equal(now) {
		t.Fatal(day, week)
	}
	beforeDay, beforeWeek := quotaStarts(now.Add(-time.Second))
	if !beforeDay.Equal(now.Add(-24*time.Hour)) || !beforeWeek.Equal(now.Add(-7*24*time.Hour)) {
		t.Fatal(beforeDay, beforeWeek)
	}
	extra := map[string]json.RawMessage{}
	_ = json.Unmarshal([]byte(`{"quota_limit":10,"quota_used":1,"quota_daily_limit":2,"quota_daily_used":2,"quota_daily_start":"2026-09-26T16:00:00Z","quota_weekly_limit":3,"quota_weekly_used":3,"quota_weekly_reset_mode":"fixed","quota_weekly_reset_at":"2026-09-27T16:00:00Z","quota_weekly_reset_day":1,"quota_weekly_reset_hour":0,"quota_reset_timezone":"Asia/Shanghai"}`), &extra)
	if accountQuotaAvailable(extra, now.Add(-time.Nanosecond)) {
		t.Fatal("exhausted current window available")
	}
	if !accountQuotaAvailable(extra, now) {
		t.Fatal("expired window not available")
	}
	delta, err := accountQuotaDelta(extra, "0.12345678", now)
	if err != nil {
		t.Fatal(err)
	}
	if delta["quota_used"] != json.Number("1.12345678") || delta["quota_daily_used"] != json.Number("0.12345678") || delta["quota_weekly_reset_at"] != "2026-10-04T16:00:00Z" {
		t.Fatal(delta)
	}
	// Daily fixed reset is a local calendar transition, including a DST shift.
	_ = json.Unmarshal([]byte(`{"quota_daily_reset_mode":"fixed","quota_daily_reset_hour":3,"quota_reset_timezone":"America/New_York"}`), &extra)
	dst := time.Date(2026, 3, 7, 9, 0, 0, 0, time.UTC)
	delta, err = accountQuotaDelta(extra, "0.1", dst)
	if err != nil {
		t.Fatal(err)
	}
	// Force the window expired without changing the other accounting fields.
	extra["quota_daily_reset_at"] = json.RawMessage(`"2020-01-01T00:00:00Z"`)
	delta, err = accountQuotaDelta(extra, "0.1", dst)
	if err != nil || delta["quota_daily_reset_at"] != "2026-03-08T07:00:00Z" {
		t.Fatal(delta, err)
	}
}
func TestChatUsageValidation(t *testing.T) {
	for _, raw := range []string{`{}`, `{"prompt_tokens":-1,"completion_tokens":2}`, `{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}`, `{"prompt_tokens":3,"completion_tokens":2147483648}`} {
		if _, err := parseChatUsage([]byte(raw)); err == nil {
			t.Fatal("invalid usage accepted", raw)
		}
	}
	u, err := parseChatUsage([]byte(`{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3,"image_tokens":2}}`))
	if err != nil || u.Input != 7 || u.CacheRead != 3 || u.ImageInput != 2 {
		t.Fatal(u, err)
	}
}
