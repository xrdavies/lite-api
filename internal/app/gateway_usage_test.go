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

func TestDailyUsageRange(t *testing.T) {
	now := time.Date(2026, 3, 8, 18, 0, 0, 0, time.UTC)
	_, start, end, err := dailyUsageRange(httptest.NewRequest("GET", "/?days=1&timezone=America/New_York", nil), now)
	if err != nil || end.Sub(start) != 23*time.Hour || start.Format("2006-01-02") != "2026-03-08" {
		t.Fatal("calendar day spanning DST", start, end, err)
	}
	for _, query := range []string{"days=0", "days=91", "days=1.5", "timezone=invalid", "timezone=Local"} {
		if _, _, _, err := dailyUsageRange(httptest.NewRequest("GET", "/?"+query, nil), now); err == nil {
			t.Fatal("invalid usage range", query)
		}
	}
}

func testGatewayUsage(t *testing.T, a *App, admin, otherUser string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.210:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
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
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "usage-compat@example.test", "password": "usage-test-password", "balance": 0})
	uid := id(user)
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "usage-compat@example.test", "password": "usage-test-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Usage compatibility", "platform": "openai", "claude_code_only": true}))
	k := manage("POST", "/api/v1/keys", token, map[string]any{"name": "Usage", "group_id": gid})
	key, kid := k["key"].(string), id(k)
	otherKey := manage("POST", "/api/v1/keys", token, map[string]any{"name": "Separate usage", "group_id": gid})
	aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Usage source", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "never-dispatched"}}))
	for _, item := range []struct {
		key   int64
		model string
		at    time.Time
	}{{kid, "visible", time.Now().Add(-time.Second)}, {kid, "older", time.Now().AddDate(0, 0, -2)}, {id(otherKey), "private-sibling", time.Now()}} {
		exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,duration_ms,created_at)
 VALUES($1,$2,$3,$4,10,20,30,40,0.9876543219,0.1234567891,80,$5)`, uid, item.key, aid, item.model, item.at)
	}
	query := func(suffix string) map[string]json.RawMessage {
		t.Helper()
		w := call("GET", "/v1/usage"+suffix, key, nil)
		var out map[string]json.RawMessage
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("usage: %d %s", w.Code, w.Body.String())
		}
		for _, secret := range []string{key, "private-sibling", "account_id", "account_cost", "user_id", "subscription", "credentials"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("usage exposed internal or other key data", secret)
			}
		}
		return out
	}
	assertNumber := func(raw json.RawMessage, expected string) {
		t.Helper()
		var n json.Number
		if json.Unmarshal(raw, &n) != nil || rat(n).Cmp(rat(json.Number(expected))) != 0 {
			t.Fatalf("amount %s, want %s", raw, expected)
		}
	}
	out := query("?days=1&timezone=UTC&user_id=1&api_key_id=" + fmt.Sprint(id(otherKey)))
	if string(out["mode"]) != `"unrestricted"` || string(out["isValid"]) != "true" {
		t.Fatal("wallet mode", out)
	}
	assertNumber(out["balance"], "0")
	var usage struct {
		Total map[string]json.RawMessage
	}
	if json.Unmarshal(out["usage"], &usage) != nil {
		t.Fatal("usage summary")
	}
	assertNumber(usage.Total["requests"], "2")
	assertNumber(usage.Total["total_tokens"], "200")
	assertNumber(usage.Total["actual_cost"], "0.2469135782")
	assertNumber(usage.Total["cost"], "1.9753086438")
	var daily []map[string]json.RawMessage
	if json.Unmarshal(out["daily_usage"], &daily) != nil || len(daily) != 1 {
		t.Fatal("daily range", string(out["daily_usage"]))
	}
	assertNumber(daily[0]["actual_cost"], "0.1234567891")
	var models []map[string]json.RawMessage
	if json.Unmarshal(query("?start_date=2000-01-01&end_date=2000-01-01")["model_stats"], &models) != nil || len(models) != 0 {
		t.Fatal("model range")
	}
	exec(`UPDATE api_keys SET status='quota_exhausted',quota=1,quota_used=1.12345678,
 rate_limit_5h=2,usage_5h=1.12345678,window_5h_start=now(),
 rate_limit_1d=3,usage_1d=2,window_1d_start=now()-interval '25 hours',
 rate_limit_7d=4,usage_7d=3,window_7d_start=NULL,expires_at=now()+interval '49 hours' WHERE id=$1`, kid)
	out = query("")
	if string(out["mode"]) != `"quota_limited"` || string(out["status"]) != `"quota_exhausted"` {
		t.Fatal("exhausted quota lookup", out)
	}
	assertNumber(out["remaining"], "0")
	assertNumber(out["days_until_expiry"], "2")
	var windows []map[string]json.RawMessage
	if json.Unmarshal(out["rate_limits"], &windows) != nil || len(windows) != 3 {
		t.Fatal("windows", out)
	}
	assertNumber(windows[0]["remaining"], "0.87654322")
	for _, w := range windows[1:] {
		assertNumber(w["used"], "0")
		if w["reset_at"] != nil {
			t.Fatal("expired or unset window reset", w)
		}
	}
	var used string
	if err := a.DB.QueryRow("SELECT usage_1d::text FROM api_keys WHERE id=$1", kid).Scan(&used); err != nil || used != "2.00000000" {
		t.Fatal("read changed stored consumption", used, err)
	}
	exec("UPDATE api_keys SET quota=0 WHERE id=$1", kid)
	if out := query(""); string(out["mode"]) != `"quota_limited"` || out["quota"] != nil || out["remaining"] != nil {
		t.Fatal("window-only quota", out)
	}
	for _, query := range []string{"days=91", "timezone=Local", "start_date=bad", "end_date=2026-02-30", "start_date=2026-01-03&end_date=2026-01-01", "api_key=" + key} {
		if w := call("GET", "/v1/usage?"+query, key, nil); w.Code != 400 {
			t.Fatal("invalid usage query", query, w.Code)
		}
	}
	for _, credential := range []string{"", token, admin, otherUser} {
		if w := call("GET", "/v1/usage", credential, nil); w.Code != 401 {
			t.Fatal("usage requires client key", w.Code)
		}
	}
	for _, field := range []string{"status='inactive'", "expires_at=now()-interval '1 second'", "deleted_at=now()", `ip_blacklist='["192.0.2.210"]'::jsonb`} {
		exec("UPDATE api_keys SET status='active',expires_at=NULL,deleted_at=NULL,ip_blacklist=NULL WHERE id=$1", kid)
		exec("UPDATE api_keys SET "+field+" WHERE id=$1", kid)
		if w := call("GET", "/v1/usage", key, nil); w.Code != 401 && w.Code != 403 {
			t.Fatal("usage access boundary", field, w.Code)
		}
	}
	// Version is the existing local build value, with management authentication.
	if got := manage("GET", "/api/v1/admin/system/version", admin, nil); got["version"] != "dev" {
		t.Fatal("admin version", got)
	}
	for credential, want := range map[string]int{"": 401, otherUser: 403, key: 401} {
		if w := call("GET", "/api/v1/admin/system/version", credential, nil); w.Code != want {
			t.Fatal("admin version access", w.Code, want)
		}
	}
}
