package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Platform daily/weekly windows retain the established Asia/Shanghai calendar.
// Monthly windows are rolling 30*24 hours, not calendar months.
func quotaStarts(now time.Time) (time.Time, time.Time) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	weekday := (int(local.Weekday()) + 6) % 7
	return day, day.AddDate(0, 0, -weekday)
}
func (a *App) checkPlatformQuota(ctx context.Context, uid int64, platform string) error {
	day, week := quotaStarts(time.Now())
	var blocked bool
	err := a.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM user_platform_quotas WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL AND (
 (daily_limit_usd IS NOT NULL AND CASE WHEN daily_window_start=$3 THEN daily_usage_usd ELSE 0 END>=daily_limit_usd) OR
 (weekly_limit_usd IS NOT NULL AND CASE WHEN weekly_window_start=$4 THEN weekly_usage_usd ELSE 0 END>=weekly_limit_usd) OR
 (monthly_limit_usd IS NOT NULL AND CASE WHEN monthly_window_start+interval '720 hours'>now() THEN monthly_usage_usd ELSE 0 END>=monthly_limit_usd)))`, uid, platform, day, week).Scan(&blocked)
	if err != nil {
		return err
	}
	if blocked {
		return &apiError{429, "platform spending limit exceeded"}
	}
	return nil
}
func incrementPlatformQuota(ctx context.Context, tx *sql.Tx, uid int64, platform, cost string, now time.Time) error {
	day, week := quotaStarts(now)
	_, err := tx.ExecContext(ctx, `UPDATE user_platform_quotas SET daily_usage_usd=CASE WHEN daily_window_start=$4 THEN daily_usage_usd+$3::numeric ELSE $3::numeric END,
 weekly_usage_usd=CASE WHEN weekly_window_start=$5 THEN weekly_usage_usd+$3::numeric ELSE $3::numeric END,
 monthly_usage_usd=CASE WHEN monthly_window_start+interval '720 hours'>$6 THEN monthly_usage_usd+$3::numeric ELSE $3::numeric END,
 daily_window_start=$4,weekly_window_start=$5,monthly_window_start=CASE WHEN monthly_window_start+interval '720 hours'>$6 THEN monthly_window_start ELSE $6 END,updated_at=now() WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL`, uid, platform, cost, day, week, now)
	return err
}
func extraNumber(extra map[string]json.RawMessage, key string) json.Number {
	var n json.Number
	if json.Unmarshal(extra[key], &n) != nil || !validPrice(&n, 12, 12) {
		return "0"
	}
	return n
}
func quotaExpired(extra map[string]json.RawMessage, window string, now time.Time) bool {
	key := "quota_" + window + "_start"
	duration := 24 * time.Hour
	if window == "weekly" {
		duration = 168 * time.Hour
	}
	fixed := credentialString(extra, "quota_"+window+"_reset_mode") == "fixed"
	if fixed {
		key = "quota_" + window + "_reset_at"
		duration = 0
	}
	start, err := time.Parse(time.RFC3339Nano, credentialString(extra, key))
	return err != nil || !now.Before(start.Add(duration))
}
func accountQuotaAvailable(extra map[string]json.RawMessage, now time.Time) bool {
	for _, window := range []string{"", "daily_", "weekly_"} {
		limit := extraNumber(extra, "quota_"+window+"limit")
		if rat(limit).Sign() == 0 {
			continue
		}
		if window != "" && quotaExpired(extra, strings.TrimSuffix(window, "_"), now) {
			continue
		}
		if rat(extraNumber(extra, "quota_"+window+"used")).Cmp(rat(limit)) >= 0 {
			return false
		}
	}
	return true
}
func accountQuotaDelta(extra map[string]json.RawMessage, cost string, now time.Time) (map[string]any, error) {
	delta := map[string]any{}
	add := func(key string, reset bool) {
		n := rat(extraNumber(extra, key))
		if reset {
			n.SetInt64(0)
		}
		n.Add(n, rat(json.Number(cost)))
		delta[key] = json.Number(n.FloatString(8))
	}
	add("quota_used", false)
	for _, window := range []string{"daily", "weekly"} {
		if rat(extraNumber(extra, "quota_"+window+"_limit")).Sign() == 0 {
			continue
		}
		reset := quotaExpired(extra, window, now)
		add("quota_"+window+"_used", reset)
		if reset {
			delta["quota_"+window+"_start"] = now.UTC().Format(time.RFC3339Nano)
			if credentialString(extra, "quota_"+window+"_reset_mode") == "fixed" {
				zone := credentialString(extra, "quota_reset_timezone")
				if zone == "" {
					zone = "UTC"
				}
				loc, err := time.LoadLocation(zone)
				if err != nil {
					return nil, bad("invalid quota reset timezone")
				}
				hour, _ := extraNumber(extra, "quota_"+window+"_reset_hour").Int64()
				day := int64(1)
				if _, ok := extra["quota_weekly_reset_day"]; ok {
					day, _ = extraNumber(extra, "quota_weekly_reset_day").Int64()
				}
				if hour < 0 || hour > 23 || day < 0 || day > 6 {
					return nil, bad("invalid quota reset schedule")
				}
				local := now.In(loc)
				next := time.Date(local.Year(), local.Month(), local.Day(), int(hour), 0, 0, 0, loc)
				step := 1
				if window == "weekly" {
					next = next.AddDate(0, 0, (int(day)-int(local.Weekday())+7)%7)
					step = 7
				}
				if !next.After(now) {
					next = next.AddDate(0, 0, step)
				}
				delta["quota_"+window+"_reset_at"] = next.UTC().Format(time.RFC3339)
			}
		}
	}
	return delta, nil
}
func (a *App) platformQuotas(w http.ResponseWriter, r *http.Request) error {
	uid := current(r).ID
	admin := strings.HasPrefix(r.URL.Path, "/api/v1/admin/")
	if admin {
		var err error
		uid, err = pathID(r)
		if err != nil {
			return err
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(r.Context(), "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL", uid).Scan(&uid); err != nil {
		return err
	}
	now := time.Now()
	day, week := quotaStarts(now)
	// Expose only quota fields. Expiry is a read-only projection; unset starts
	// retain stored usage and have no reset time, while monthly resets stay anchored.
	rows, err := tx.QueryContext(r.Context(), `SELECT jsonb_build_object(
 'platform',platform,'daily_limit_usd',daily_limit_usd,'weekly_limit_usd',weekly_limit_usd,'monthly_limit_usd',monthly_limit_usd,
 'daily_usage_usd',CASE WHEN daily_window_start<$2 THEN 0 ELSE daily_usage_usd END,
 'weekly_usage_usd',CASE WHEN weekly_window_start<$3 THEN 0 ELSE weekly_usage_usd END,
 'monthly_usage_usd',CASE WHEN monthly_window_start+interval '720 hours'<=$4 THEN 0 ELSE monthly_usage_usd END,
 'daily_window_resets_at',CASE WHEN daily_window_start>=$2 THEN $6::timestamptz END,
 'weekly_window_resets_at',CASE WHEN weekly_window_start>=$3 THEN $7::timestamptz END,
 'monthly_window_resets_at',CASE WHEN monthly_window_start+interval '720 hours'>$4 THEN monthly_window_start+interval '720 hours' END)
 || CASE WHEN $5 THEN jsonb_build_object('daily_window_start',daily_window_start,'weekly_window_start',weekly_window_start,'monthly_window_start',monthly_window_start) ELSE '{}'::jsonb END
 FROM user_platform_quotas WHERE user_id=$1 AND deleted_at IS NULL ORDER BY platform`, uid, day, week, now, admin, day.AddDate(0, 0, 1), week.AddDate(0, 0, 7))
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, map[string]any{"platform_quotas": items})
}
func (a *App) setPlatformQuotas(w http.ResponseWriter, r *http.Request) error {
	uid, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Quotas *[]struct {
			Platform string       `json:"platform"`
			Daily    *json.Number `json:"daily_limit_usd"`
			Weekly   *json.Number `json:"weekly_limit_usd"`
			Monthly  *json.Number `json:"monthly_limit_usd"`
		} `json:"quotas"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Quotas == nil || len(*in.Quotas) > 8 {
		return bad("quotas array is required")
	}
	seen := map[string]bool{}
	platforms := []string{}
	for _, q := range *in.Quotas {
		if !supportedPlatform(q.Platform) || seen[q.Platform] || !validPrice(q.Daily, 10, 10) || !validPrice(q.Weekly, 10, 10) || !validPrice(q.Monthly, 10, 10) {
			return bad("invalid or duplicate platform quota")
		}
		seen[q.Platform] = true
		if q.Daily != nil || q.Weekly != nil || q.Monthly != nil {
			platforms = append(platforms, q.Platform)
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(r.Context(), "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", uid).Scan(&uid); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), "UPDATE user_platform_quotas SET deleted_at=now(),updated_at=now() WHERE user_id=$1 AND deleted_at IS NULL AND NOT(platform=ANY($2))", uid, pq.Array(platforms)); err != nil {
		return err
	}
	for _, q := range *in.Quotas {
		if q.Daily == nil && q.Weekly == nil && q.Monthly == nil {
			continue
		}
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,weekly_limit_usd,monthly_limit_usd) VALUES($1,$2,$3,$4,$5) ON CONFLICT(user_id,platform) WHERE deleted_at IS NULL DO UPDATE SET daily_limit_usd=EXCLUDED.daily_limit_usd,weekly_limit_usd=EXCLUDED.weekly_limit_usd,monthly_limit_usd=EXCLUDED.monthly_limit_usd,updated_at=now()`, uid, q.Platform, q.Daily, q.Weekly, q.Monthly); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.platformQuotas(w, r)
}
func (a *App) resetPlatformQuota(w http.ResponseWriter, r *http.Request) error {
	uid, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Platform string `json:"platform"`
		Window   string `json:"window"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if !supportedPlatform(in.Platform) {
		return bad("invalid platform")
	}
	now := time.Now()
	day, week := quotaStarts(now)
	start := now
	switch in.Window {
	case "daily":
		start = day
	case "weekly":
		start = week
	case "monthly":
	default:
		return bad("invalid quota window")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(r.Context(), "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", uid).Scan(&uid); err != nil {
		return err
	}
	result, err := tx.ExecContext(r.Context(), "UPDATE user_platform_quotas SET "+in.Window+"_usage_usd=0,"+in.Window+"_window_start=$3,updated_at=now() WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL", uid, in.Platform, start)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing()
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.platformQuotas(w, r)
}
func (a *App) quotaRoutes() {
	a.route("GET /api/v1/user/platform-quotas", "user", a.platformQuotas)
	a.route("GET /api/v1/admin/users/{id}/platform-quotas", "admin", a.platformQuotas)
	a.route("PUT /api/v1/admin/users/{id}/platform-quotas", "admin", a.setPlatformQuotas)
	a.route("POST /api/v1/admin/users/{id}/platform-quotas/reset", "admin", a.resetPlatformQuota)
}
