package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type grokQuotaWindow struct {
	Limit     *int64 `json:"limit,omitempty"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetUnix *int64 `json:"reset_unix,omitempty"`
	ResetAt   string `json:"reset_at,omitempty"`
}

// Only parsed quota values are persisted; arbitrary response headers and plan
// claims are neither credentials nor an authoritative API-key spending limit.
type grokQuotaSnapshot struct {
	Requests    *grokQuotaWindow `json:"requests,omitempty"`
	Tokens      *grokQuotaWindow `json:"tokens,omitempty"`
	RetryAfter  *int64           `json:"retry_after_seconds,omitempty"`
	Status      int              `json:"status_code"`
	Observed    bool             `json:"headers_observed"`
	Source      string           `json:"observation_source"`
	HeadersSeen string           `json:"last_headers_seen_at,omitempty"`
	UpdatedAt   string           `json:"updated_at"`
}

func quotaHeader(h http.Header, name string) string {
	for _, prefix := range []string{"X-Ratelimit-", "X-Rate-Limit-"} {
		if s := strings.TrimSpace(h.Get(prefix + name)); s != "" {
			if len(s) <= 128 {
				return s
			}
			return ""
		}
	}
	return ""
}

func quotaHeaderNumber(raw string) *int64 {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

func quotaReset(raw string, now time.Time) *int64 {
	var seconds int64
	if n := quotaHeaderNumber(raw); n != nil {
		switch {
		case *n >= 1_000_000_000_000:
			seconds = *n / 1000
		case *n >= 1_000_000_000:
			seconds = *n
		default:
			seconds = now.Unix() + *n
		}
	} else if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		seconds = now.Add(max(d, time.Second)).Unix()
	} else if at, err := time.Parse(time.RFC3339, raw); err == nil {
		seconds = at.Unix()
	} else {
		return nil
	}
	if seconds < 0 || seconds > 253402300799 {
		return nil
	}
	return &seconds
}

func observeGrokQuota(h http.Header, status int, now time.Time) *grokQuotaSnapshot {
	// Fixed-width UTC timestamps also provide ordering for concurrent SQL writes.
	s := &grokQuotaSnapshot{Status: status, Source: "passive", UpdatedAt: now.UTC().Format("2006-01-02T15:04:05.000000000Z")}
	for name, target := range map[string]**grokQuotaWindow{"requests": &s.Requests, "tokens": &s.Tokens} {
		w := &grokQuotaWindow{Limit: quotaHeaderNumber(quotaHeader(h, "limit-"+name)), Remaining: quotaHeaderNumber(quotaHeader(h, "remaining-"+name)), ResetUnix: quotaReset(quotaHeader(h, "reset-"+name), now)}
		if w.ResetUnix != nil {
			w.ResetAt = time.Unix(*w.ResetUnix, 0).UTC().Format(time.RFC3339)
		}
		if w.Limit != nil || w.Remaining != nil || w.ResetUnix != nil {
			*target = w
			s.Observed = true
		}
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if len(raw) <= 128 {
		if n := quotaHeaderNumber(raw); n != nil {
			s.RetryAfter = n
		} else if at, err := http.ParseTime(raw); err == nil {
			n := max(int64(at.Sub(now)/time.Second), 0)
			s.RetryAfter = &n
		}
	}
	s.Observed = s.Observed || s.RetryAfter != nil
	if s.Observed {
		s.HeadersSeen = s.UpdatedAt
	} else if status != 401 && status != 403 && status != 429 {
		return nil
	}
	return s
}

func (a *App) recordGrokQuota(ctx context.Context, u *upstreamAccount, resp *http.Response) {
	if a.DB == nil || u.ID <= 0 || u.Platform != "grok" || u.Type != "apikey" || resp == nil {
		return
	}
	snapshot := observeGrokQuota(resp.Header, resp.StatusCode, time.Now())
	if snapshot == nil {
		return
	}
	raw, _ := json.Marshal(snapshot)
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// A diagnostic must not change the account revision used by admission,
	// cooldowns and admin edits. Their writes invalidate an in-flight snapshot.
	_, err := a.DB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{grok_usage_snapshot}',$3::jsonb)
 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL
 AND COALESCE(extra->'grok_usage_snapshot'->>'updated_at','')<$4`, u.ID, u.UpdatedAt, string(raw), snapshot.UpdatedAt)
	if err != nil && ctx.Err() == nil {
		slog.Warn("account quota snapshot write failed", "account_id", u.ID)
	}
}

func (a *App) accountUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if source := r.URL.Query().Get("source"); source != "" && source != "active" && source != "passive" {
		return bad("source must be active or passive")
	}
	if force := r.URL.Query().Get("force"); force != "" && force != "true" && force != "false" {
		return bad("force must be true or false")
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	if u.Platform == "gemini" {
		return a.geminiAccountUsage(w, r, u)
	}
	if u.Platform != "grok" {
		return bad("upstream usage is unavailable for this API key account; use today-stats for local usage")
	}
	out := map[string]any{"source": "passive", "five_hour": nil, "grok_quota_snapshot_state": "unknown_until_first_response", "error_code": "quota_unknown", "error": "upstream quota has not been observed"}
	var snapshot grokQuotaSnapshot
	if raw := u.Extra["grok_usage_snapshot"]; json.Unmarshal(raw, &snapshot) == nil && snapshot.UpdatedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, snapshot.UpdatedAt); err == nil {
			out["updated_at"], out["grok_last_status_code"] = snapshot.UpdatedAt, snapshot.Status
			out["grok_quota_snapshot_state"] = "no_headers"
			if snapshot.Observed {
				out["grok_quota_snapshot_state"] = "observed"
				delete(out, "error_code")
				delete(out, "error")
			}
			if snapshot.Requests != nil {
				out["grok_request_quota"] = snapshot.Requests
			}
			if snapshot.Tokens != nil {
				out["grok_token_quota"] = snapshot.Tokens
			}
			if snapshot.RetryAfter != nil {
				out["grok_retry_after_seconds"] = snapshot.RetryAfter
			}
			if snapshot.HeadersSeen != "" {
				out["grok_last_headers_seen_at"] = snapshot.HeadersSeen
			}
			switch snapshot.Status {
			case 401:
				out["error_code"] = "unauthenticated"
			case 403:
				out["error_code"], out["is_forbidden"], out["forbidden_type"] = "forbidden", true, "forbidden"
			case 429:
				out["error_code"] = "rate_limited"
			}
		}
	}
	now := time.Now()
	day, _ := quotaStarts(now)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	for name, start := range map[string]time.Time{"grok_local_usage": day, "grok_local_usage_24h": now.Add(-24 * time.Hour)} {
		stats, err := a.accountWindowStats(ctx, id, start, now)
		if err != nil {
			return err
		}
		out[name] = stats
	}
	w.Header().Set("Cache-Control", "no-store")
	return reply(w, out)
}

func geminiUsageDay(now time.Time) (time.Time, time.Time) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now = now.In(loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, 0, 1)
}

func (a *App) geminiAccountUsage(w http.ResponseWriter, r *http.Request, u *upstreamAccount) error {
	now := time.Now()
	day, reset := geminiUsageDay(now)
	minute := now.Truncate(time.Minute)
	// Historical API-key defaults are local estimates, never provider promises
	// or additional scheduler limits. Unknown model names retain the Pro bucket.
	proDay, flashDay, proMinute, flashMinute := 50, 1500, 2, 15
	if credentialString(u.Credentials, "tier_id") == "aistudio_paid" {
		proDay, flashDay, proMinute, flashMinute = -1, -1, 1000, 2000
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	raw, err := jsonRow(a.DB.QueryRowContext(ctx, `WITH windows(name,start_at,reset_at,quota,flash) AS (VALUES
 ('gemini_pro_daily',$2::timestamptz,$3::timestamptz,$6::bigint,false),
 ('gemini_flash_daily',$2::timestamptz,$3::timestamptz,$7::bigint,true),
 ('gemini_pro_minute',$4::timestamptz,$4::timestamptz+interval '1 minute',$8::bigint,false),
 ('gemini_flash_minute',$4::timestamptz,$4::timestamptz+interval '1 minute',$9::bigint,true))
 SELECT jsonb_build_object('source','local','five_hour',NULL,'updated_at',$5::timestamptz,
 'quota_basis','compatibility_default','timezone','America/Los_Angeles') || COALESCE(jsonb_object_agg(name,
 jsonb_build_object('utilization',s.requests::numeric*100/quota,'resets_at',reset_at,
 'remaining_seconds',GREATEST(0,trunc(extract(epoch FROM reset_at-$5::timestamptz))),
 'used_requests',s.requests,'limit_requests',quota,'window_stats',jsonb_build_object(
 'requests',s.requests,'tokens',s.tokens,'cost',s.cost,'standard_cost',0,'user_cost',0))) FILTER(WHERE quota>0),'{}'::jsonb)
 FROM windows CROSS JOIN LATERAL (SELECT count(*) AS requests,
 COALESCE(sum(input_tokens::bigint+output_tokens+cache_creation_tokens+cache_read_tokens),0) AS tokens,
 COALESCE(sum(actual_cost),0) AS cost FROM usage_logs WHERE account_id=$1
 AND created_at>=start_at AND created_at<$5
 AND (lower(model) LIKE '%flash%' OR lower(model) LIKE '%lite%')=flash)s`, u.ID, day, reset, minute, now, proDay, flashDay, proMinute, flashMinute))
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return reply(w, raw)
}

func (a *App) checkMixedChannel(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Platform string  `json:"platform"`
		Groups   []int64 `json:"group_ids"`
		Account  *int64  `json:"account_id"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if !supportedPlatform(in.Platform) || len(in.Groups) > 1000 || in.Account != nil && *in.Account <= 0 {
		return bad("invalid platform, account_id or group_ids")
	}
	for _, id := range in.Groups {
		if id <= 0 {
			return bad("invalid group_id")
		}
	}
	// The original warning requires a platform excluded from this deployment.
	// Actual account/group binding validation still runs when saving an account.
	return reply(w, map[string]bool{"has_risk": false})
}
