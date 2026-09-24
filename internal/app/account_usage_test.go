package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGrokQuotaHeaders(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	for raw, seconds := range map[string]int64{
		"60": now.Unix() + 60, "0": now.Unix(), "1m": now.Unix() + 60,
		"100ms": now.Unix() + 1, "1790241660": 1790241660,
		"1790241660000": 1790241660, "2026-09-24T12:01:00Z": now.Unix() + 60,
	} {
		if got := quotaReset(raw, now); got == nil || *got != seconds {
			t.Fatal("reset", raw, got)
		}
	}
	for _, raw := range []string{"", "-1", "-1m", "1e20", "9223372036854775807", "not-a-date"} {
		if quotaReset(raw, now) != nil {
			t.Fatal("invalid reset", raw)
		}
	}
	h := http.Header{}
	h.Set("X-Rate-Limit-Limit-Requests", "9007199254740993")
	h.Set("X-Rate-Limit-Remaining-Requests", "0")
	h.Set("X-Ratelimit-Reset-Requests", "60")
	h.Set("X-Ratelimit-Limit-Tokens", "-1")
	h.Set("X-Ratelimit-Remaining-Tokens", "secret-value")
	h.Set("Retry-After", now.Add(time.Minute).Format(http.TimeFormat))
	h.Set("X-Subscription-Tier", "secret-value")
	h.Set("Authorization", "secret-value")
	s := observeGrokQuota(h, 429, now)
	if !s.Observed || *s.Requests.Limit != 9007199254740993 || *s.Requests.Remaining != 0 || *s.Requests.ResetUnix != now.Unix()+60 || s.Tokens != nil || *s.RetryAfter != 60 {
		t.Fatal("parsed quota", s)
	}
	raw, _ := json.Marshal(s)
	if bytes.Contains(raw, []byte("secret-value")) {
		t.Fatal("untrusted header retained")
	}
	for _, status := range []int{200, 500} {
		if observeGrokQuota(nil, status, now) != nil {
			t.Fatal("no observation fabricated", status)
		}
	}
	for _, status := range []int{401, 403, 429} {
		if got := observeGrokQuota(nil, status, now); got == nil || got.Observed || got.Status != status {
			t.Fatal("rejection lost", status)
		}
	}
	h = http.Header{}
	h.Set("Retry-After", "0")
	if got := observeGrokQuota(h, 429, now); got.RetryAfter == nil || *got.RetryAfter != 0 {
		t.Fatal("explicit zero retry")
	}
	h.Set("Retry-After", "-1")
	h.Set("X-Ratelimit-Limit-Requests", strings.Repeat("1", 129))
	if observeGrokQuota(h, 200, now) != nil {
		t.Fatal("invalid headers observed")
	}
}

func TestGeminiUsageDay(t *testing.T) {
	for _, test := range []struct {
		At    string
		Hours time.Duration
	}{{"2026-03-08T12:00:00Z", 23}, {"2026-11-01T12:00:00Z", 25}} {
		now, _ := time.Parse(time.RFC3339, test.At)
		start, end := geminiUsageDay(now)
		if start.Hour() != 0 || end.Hour() != 0 || end.Sub(start) != test.Hours*time.Hour || now.Before(start) || !now.Before(end) {
			t.Fatal("calendar day", start, end)
		}
	}
}

func testAccountUsage(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(method, path string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer usage-secret" {
			t.Error("incorrect upstream credential")
		}
		w.Header().Set("X-Ratelimit-Limit-Requests", "100")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "99")
		w.Header().Set("X-Ratelimit-Reset-Requests", "60")
		w.Header().Set("Set-Cookie", "private-secret")
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			_, _, _ = conn.Read(r.Context())
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer provider.Close()
	aid := string(data("POST", "/api/v1/admin/accounts", map[string]any{"name": "quota-observations", "platform": "grok", "type": "apikey", "credentials": map[string]string{"api_key": "usage-secret", "base_url": provider.URL}})["id"])
	path := "/api/v1/admin/accounts/" + aid
	var id int64
	if json.Unmarshal([]byte(aid), &id) != nil {
		t.Fatal("account ID")
	}
	load := func() *upstreamAccount {
		t.Helper()
		u, err := a.loadAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	got := data("GET", path+"/usage", nil)
	if string(got["error_code"]) != `"quota_unknown"` || string(got["five_hour"]) != "null" {
		t.Fatal("unknown quota reported as zero", got)
	}
	u := load()
	response, err := a.upstreamRequest(ctx, u, "POST", "/v1/chat/completions", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	var writers sync.WaitGroup
	for range 8 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			a.recordGrokQuota(ctx, u, response)
		}()
	}
	writers.Wait()
	if w := call("PUT", path, admin, map[string]any{"credentials": map[string]string{"tier_id": "aistudio_paid"}}); w.Code != 400 {
		t.Fatal("quota tier crossed provider boundary", w.Code)
	}
	if !load().UpdatedAt.Equal(u.UpdatedAt) {
		t.Fatal("observation changed scheduling revision")
	}
	got = data("GET", path+"/usage?source=active&force=true", nil)
	var quota grokQuotaWindow
	if json.Unmarshal(got["grok_request_quota"], &quota) != nil || quota.Remaining == nil || *quota.Remaining != 99 || string(got["source"]) != `"passive"` || got["error_code"] != nil || requests.Load() != 1 {
		t.Fatal("usage snapshot or unintended probe", got, requests.Load())
	}
	for _, suffix := range []string{"?source=invalid", "?force=1"} {
		if w := call("GET", path+"/usage"+suffix, admin, nil); w.Code != 400 {
			t.Fatal("invalid query", w.Code)
		}
	}
	for _, token := range []string{"", ordinary, "client-api-key"} {
		for _, request := range []struct{ Method, Path string }{{"GET", path + "/usage"}, {"POST", "/api/v1/admin/accounts/check-mixed-channel"}} {
			if w := call(request.Method, request.Path, token, map[string]any{"platform": "openai"}); w.Code != 401 && w.Code != 403 {
				t.Fatal("admin permission", w.Code)
			}
		}
	}
	uid := string(data("POST", "/api/v1/admin/users", map[string]any{"email": "quota-observer@example.test", "password": "quota-observer-password"})["id"])
	var kid int64
	if err := a.DB.QueryRow("INSERT INTO api_keys(user_id,key,name) VALUES($1,'quota-observer-key','quota') RETURNING id", uid).Scan(&kid); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, at := range []time.Time{now, now.Add(-25 * time.Hour)} {
		exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,total_cost,actual_cost,account_stats_cost,account_rate_multiplier,created_at)
 VALUES($1,$2,$3,$4,'quota-test',2000000000,2000000000,0.1000000001,0.2000000002,0.1234567890,2,$5)`, uid, kid, aid, fmt.Sprintf("quota-observation-%d", i), at)
	}
	got = data("GET", path+"/usage?source=passive", nil)
	for _, name := range []string{"grok_local_usage", "grok_local_usage_24h"} {
		var stats map[string]json.RawMessage
		if json.Unmarshal(got[name], &stats) != nil || string(stats["tokens"]) != "4000000000" || rat(json.Number(stats["cost"])).Cmp(rat("0.2469135780")) != 0 || rat(json.Number(stats["user_cost"])).Cmp(rat("0.2000000002")) != 0 {
			t.Fatal("precise local usage", name, stats)
		}
	}
	// Admin state is never repaired by reading usage; observations merge extra.
	exec(`UPDATE accounts SET status='inactive',schedulable=false,extra=extra || '{"quota_used":7,"future_field":"kept"}'::jsonb WHERE id=$1`, id)
	before := load()
	data("GET", path+"/usage?force=true", nil)
	if after := load(); after.Status != "inactive" || after.Schedulable || string(after.Extra["quota_used"]) != "7" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("read changed account")
	}
	// A stale response cannot put an old credential's quota back after rotation.
	data("PUT", path, map[string]any{"credentials": map[string]string{"api_key": "rotated-secret"}})
	a.recordGrokQuota(ctx, before, response)
	if load().Extra["grok_usage_snapshot"] != nil {
		t.Fatal("stale observation survived rotation")
	}
	data("PUT", path, map[string]any{"credentials": map[string]string{"api_key": "usage-secret"}})
	u = load()
	conn, _, err := a.dialUpstreamSocket(ctx, u, "/v1/realtime", http.Header{"Authorization": []string{"Bearer usage-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
	if load().Extra["grok_usage_snapshot"] == nil {
		t.Fatal("WebSocket quota not observed")
	}
	// Proxy edits share snapshot invalidation with other upstream diagnostics.
	proxyID := string(data("POST", "/api/v1/admin/proxies", map[string]any{"name": "quota-proxy", "protocol": "http", "host": "127.0.0.1", "port": 12345})["id"])
	data("PUT", path, map[string]any{"proxy_id": json.Number(proxyID)})
	u = load()
	a.recordGrokQuota(ctx, u, response)
	data("PUT", "/api/v1/admin/proxies/"+proxyID, map[string]any{"port": 12346})
	a.recordGrokQuota(ctx, u, response)
	if load().Extra["grok_usage_snapshot"] != nil {
		t.Fatal("proxy change retained or restored snapshot")
	}
	for _, status := range []int{401, 403, 429} {
		a.recordGrokQuota(ctx, load(), &http.Response{StatusCode: status})
		got = data("GET", path+"/usage", nil)
		if string(got["grok_last_status_code"]) != fmt.Sprint(status) || got["error_code"] == nil {
			t.Fatal("failed response diagnostics", status, got)
		}
	}
	if requests.Load() != 2 {
		t.Fatal("usage query unexpectedly contacted provider", requests.Load())
	}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		got := data("POST", "/api/v1/admin/accounts/check-mixed-channel", map[string]any{"platform": platform, "group_ids": []int64{1}, "account_id": id})
		if string(got["has_risk"]) != "false" {
			t.Fatal("inapplicable mixed risk", platform)
		}
		if platform != "grok" && platform != "gemini" {
			exec("UPDATE accounts SET platform=$2 WHERE id=$1", id, platform)
			if w := call("GET", path+"/usage", admin, nil); w.Code != 400 {
				t.Fatal("fabricated upstream quota", platform, w.Code)
			}
		}
	}
	exec("UPDATE accounts SET platform='gemini' WHERE id=$1", id)
	for _, tier := range []any{nil, false, 1, "google_one", "invalid"} {
		if w := call("PUT", path, admin, map[string]any{"credentials": map[string]any{"tier_id": tier}}); w.Code != 400 {
			t.Fatal("invalid Gemini quota tier", tier, w.Code)
		}
	}
	got = data("GET", path+"/usage", nil)
	var progress struct {
		Used  int64                      `json:"used_requests"`
		Limit int64                      `json:"limit_requests"`
		Stats map[string]json.RawMessage `json:"window_stats"`
	}
	if json.Unmarshal(got["gemini_pro_daily"], &progress) != nil || progress.Used != 1 || progress.Limit != 50 || rat(json.Number(progress.Stats["cost"])).Cmp(rat("0.2000000002")) != 0 || string(got["source"]) != `"local"` {
		t.Fatal("Gemini local usage", got)
	}
	if json.Unmarshal(got["gemini_flash_daily"], &progress) != nil || progress.Used != 0 || progress.Limit != 1500 {
		t.Fatal("Gemini model classification", got)
	}
	data("PUT", path, map[string]any{"credentials": map[string]string{"tier_id": "aistudio_paid"}})
	got = data("GET", path+"/usage", nil)
	if got["gemini_pro_daily"] != nil || got["gemini_flash_daily"] != nil || json.Unmarshal(got["gemini_pro_minute"], &progress) != nil || progress.Limit != 1000 {
		t.Fatal("Gemini paid snapshot", got)
	}
	for _, in := range []any{nil, map[string]any{"platform": "antigravity"}, map[string]any{"platform": "openai", "account_id": -1}, map[string]any{"platform": "openai", "group_ids": []int64{0}}, map[string]any{"platform": "openai", "unsupported": true}} {
		if w := call("POST", "/api/v1/admin/accounts/check-mixed-channel", admin, in); w.Code != 400 {
			t.Fatal("invalid mixed channel input", in, w.Code)
		}
	}
	exec("UPDATE accounts SET deleted_at=now() WHERE id=$1", id)
	if w := call("GET", path+"/usage", admin, nil); w.Code != 404 {
		t.Fatal("deleted account usage", w.Code)
	}
	if w := call("GET", "/api/v1/admin/accounts/invalid/usage", admin, nil); w.Code != 400 {
		t.Fatal("invalid account ID", w.Code)
	}
}
