package app

import (
	"encoding/json"
	"testing"
	"time"
)

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
