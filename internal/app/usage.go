package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

func usageFilters(r *http.Request) (string, []any, error) {
	uid := current(r).ID
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		uid = 0
		if s := r.URL.Query().Get("user_id"); s != "" {
			var err error
			uid, err = strconv.ParseInt(s, 10, 64)
			if err != nil || uid < 1 {
				return "", nil, bad("invalid user_id")
			}
		}
	}
	kid := int64(0)
	if s := r.URL.Query().Get("api_key_id"); s != "" {
		var err error
		kid, err = strconv.ParseInt(s, 10, 64)
		if err != nil || kid < 1 {
			return "", nil, bad("invalid api_key_id")
		}
	}
	var from, to any
	for i, key := range []string{"start_date", "end_date"} {
		if s := r.URL.Query().Get(key); s != "" {
			v, err := time.Parse(time.RFC3339, s)
			if err != nil {
				v, err = time.Parse("2006-01-02", s)
				if err == nil && i == 1 {
					v = v.AddDate(0, 0, 1)
				}
			}
			if err != nil {
				return "", nil, bad("invalid date filter")
			}
			if i == 0 {
				from = v
			} else {
				to = v
			}
		}
	}
	return ` WHERE ($1::bigint=0 OR user_id=$1) AND ($2::bigint=0 OR api_key_id=$2) AND ($3='' OR model=$3) AND ($4::timestamptz IS NULL OR created_at>=$4) AND ($5::timestamptz IS NULL OR created_at<$5)`, []any{uid, kid, r.URL.Query().Get("model"), from, to}, nil
}

// User views expose usage and price snapshots, not upstream account/channel identities.
const userUsageColumns = `id,request_id,api_key_id,model,requested_model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,input_cost,output_cost,cache_creation_cost,cache_read_cost,total_cost,actual_cost,rate_multiplier,stream,duration_ms,first_token_ms,created_at,group_id,billing_type,billing_mode,request_type,image_input_tokens,image_output_tokens,image_input_cost,image_output_cost,service_tier,reasoning_effort,requested_reasoning_effort,video_count,video_resolution,video_duration_seconds,image_count,image_size,image_size_source,image_input_size,image_output_size,image_size_breakdown`

func (a *App) listUsage(w http.ResponseWriter, r *http.Request) error {
	where, args, err := usageFilters(r)
	if err != nil {
		return err
	}
	page, size := pagination(r)
	var total int
	if err = a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM usage_logs"+where, args...).Scan(&total); err != nil {
		return err
	}
	columns := userUsageColumns
	if current(r).Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		columns = "*"
	}
	args = append(args, size, (page-1)*size)
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(u) FROM(SELECT "+columns+" FROM usage_logs"+where+" ORDER BY id DESC LIMIT $6 OFFSET $7)u", args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) getUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(u) FROM(SELECT "+userUsageColumns+" FROM usage_logs WHERE id=$1 AND user_id=$2)u", id, current(r).ID))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) usageStats(w http.ResponseWriter, r *http.Request) error {
	where, args, err := usageFilters(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT jsonb_build_object('total_requests',count(*),'input_tokens',COALESCE(sum(input_tokens),0),'output_tokens',COALESCE(sum(output_tokens),0),'cache_creation_tokens',COALESCE(sum(cache_creation_tokens),0),'cache_read_tokens',COALESCE(sum(cache_read_tokens),0),'total_cost',COALESCE(sum(total_cost),0),'actual_cost',COALESCE(sum(actual_cost),0)) FROM usage_logs`+where, args...))
	if err != nil {
		return err
	}
	return reply(w, raw)
}

// Reporting periods use calendar boundaries in the same timezone as daily
// quotas; they do not change the rolling windows used for spending limits.
func usagePeriodStart(period string, now time.Time) (time.Time, error) {
	day, week := quotaStarts(now)
	switch period {
	case "day":
		return day, nil
	case "week":
		return week, nil
	case "month":
		return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location()), nil
	default:
		return time.Time{}, bad("period must be day, week or month")
	}
}

func (a *App) adminUserUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	period := r.URL.Query().Get("period")
	if period == "" {
		period = "month"
	}
	now := time.Now()
	start, err := usagePeriodStart(period, now)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT jsonb_build_object(
 'period',$4::text,'start_date',$2::timestamptz,'end_date',$3::timestamptz,'timezone','Asia/Shanghai',
 'total_requests',s.requests,'total_tokens',s.tokens,'total_cost',s.cost,'avg_duration_ms',s.duration)
 FROM users u CROSS JOIN LATERAL (
 SELECT count(*) AS requests,COALESCE(sum(input_tokens::bigint+output_tokens+cache_creation_tokens+cache_read_tokens),0) AS tokens,
 COALESCE(sum(actual_cost),0) AS cost,COALESCE(avg(duration_ms),0) AS duration
 FROM usage_logs WHERE user_id=u.id AND created_at >= $2 AND created_at < $3
 )s WHERE u.id=$1 AND u.deleted_at IS NULL`, id, start, now, period))
	if err != nil {
		return err
	}
	return reply(w, raw)
}

func (a *App) accountTodayStats(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	now := time.Now()
	start, _ := quotaStarts(now)
	raw, err := a.accountWindowStats(r.Context(), id, start, now)
	if err != nil {
		return err
	}
	return reply(w, raw)
}

func (a *App) accountWindowStats(ctx context.Context, id int64, start, end time.Time) (json.RawMessage, error) {
	// Account cost uses the historical cost override and multiplier, never the
	// current account configuration. An explicit zero override remains zero.
	return jsonRow(a.DB.QueryRowContext(ctx, `SELECT to_jsonb(s) FROM accounts a CROSS JOIN LATERAL (
 SELECT count(*) AS requests,COALESCE(sum(input_tokens::bigint+output_tokens+cache_creation_tokens+cache_read_tokens),0) AS tokens,
 COALESCE(sum(COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1)),0) AS cost,
 COALESCE(sum(total_cost),0) AS standard_cost,COALESCE(sum(actual_cost),0) AS user_cost
 FROM usage_logs WHERE account_id=a.id AND created_at >= $2 AND created_at < $3
 )s WHERE a.id=$1 AND a.deleted_at IS NULL`, id, start, end))
}

func (a *App) usageErrors(w http.ResponseWriter, r *http.Request) error {
	uid := current(r).ID
	page, size := pagination(r)
	var total int
	const where = " FROM ops_error_logs WHERE user_id=$1 AND status_code>=400 AND error_phase NOT IN ('upstream','account_auth')"
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*)"+where, uid).Scan(&total); err != nil {
		return err
	}
	const columns = "id,request_id,api_key_id,model,request_path,stream,error_phase,error_type,status_code,error_message,is_business_limited,duration_ms,created_at"
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(e) FROM(SELECT "+columns+where+" AND id=$2)e", uid, id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(e) FROM(SELECT "+columns+where+" ORDER BY id DESC LIMIT $2 OFFSET $3)e", uid, size, (page-1)*size)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) usageRoutes() {
	a.route("GET /api/v1/admin/users/{id}/usage", "admin", a.adminUserUsage)
	a.route("GET /api/v1/admin/accounts/{id}/today-stats", "admin", a.accountTodayStats)
	a.route("POST /api/v1/usage/dashboard/api-keys-usage", "user", a.keyUsageCosts)
	a.route("GET /api/v1/user/api-keys/{id}/usage/daily", "user", a.keyDailyUsage)
	a.route("GET /api/v1/admin/usage/search-users", "admin", a.usageSearch)
	a.route("GET /api/v1/admin/usage/search-api-keys", "admin", a.usageSearch)
	a.route("GET /api/v1/admin/audit-logs", "admin", a.auditLogs)
	a.route("GET /api/v1/admin/audit-logs/{id}", "admin", a.auditLogs)
	a.route("GET /api/v1/usage", "user", a.listUsage)
	a.route("GET /api/v1/usage/{id}", "user", a.getUsage)
	a.route("GET /api/v1/usage/stats", "user", a.usageStats)
	a.route("GET /api/v1/usage/errors", "user", a.usageErrors)
	a.route("GET /api/v1/usage/errors/{id}", "user", a.usageErrors)
	a.route("GET /api/v1/admin/usage", "admin", a.listUsage)
	a.route("GET /api/v1/admin/usage/stats", "admin", a.usageStats)
}

func (a *App) keyUsageCosts(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		IDs *[]int64 `json:"api_key_ids"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.IDs == nil || len(*in.IDs) > 100 {
		return bad("api_key_ids must contain at most 100 IDs")
	}
	for _, id := range *in.IDs {
		if id < 1 {
			return bad("invalid API key ID")
		}
	}
	now := time.Now()
	day, _ := quotaStarts(now)
	// The existing total field is the trailing 30 days, not lifetime spend.
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT jsonb_build_object('stats',COALESCE(jsonb_object_agg(k.id::text,jsonb_build_object('api_key_id',k.id,'today_actual_cost',c.today,'total_actual_cost',c.total)),'{}'::jsonb)) FROM api_keys k CROSS JOIN LATERAL (SELECT COALESCE(sum(actual_cost) FILTER (WHERE created_at >= $3),0) AS today,COALESCE(sum(actual_cost) FILTER (WHERE created_at >= $4 AND created_at < $5),0) AS total FROM usage_logs WHERE api_key_id=k.id AND user_id=$1 AND created_at >= LEAST($3,$4)) c WHERE k.user_id=$1 AND k.id=ANY($2) AND k.deleted_at IS NULL`, current(r).ID, pq.Array(*in.IDs), day, now.AddDate(0, 0, -30), now))
	if err != nil {
		return err
	}
	return reply(w, raw)
}

func (a *App) keyDailyUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	days, start, end, err := dailyUsageRange(r, time.Now())
	if err != nil {
		return err
	}
	var owner int64
	if err = a.DB.QueryRowContext(r.Context(), "SELECT user_id FROM api_keys WHERE id=$1 AND deleted_at IS NULL", id).Scan(&owner); err != nil {
		return err
	}
	if owner != current(r).ID {
		return denied()
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT to_jsonb(d) FROM (SELECT to_char(created_at AT TIME ZONE $5,'YYYY-MM-DD') AS date,count(*) AS requests,sum(input_tokens) AS input_tokens,sum(output_tokens) AS output_tokens,sum(cache_read_tokens) AS cache_read_tokens,sum(cache_creation_tokens) AS cache_write_tokens,sum(input_tokens::bigint+output_tokens+cache_read_tokens+cache_creation_tokens) AS total_tokens,sum(total_cost) AS cost,sum(actual_cost) AS actual_cost FROM usage_logs WHERE user_id=$1 AND api_key_id=$2 AND created_at >= $3 AND created_at < $4 GROUP BY date ORDER BY date)d`, owner, id, start, end, start.Location().String())
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, map[string]any{"items": items, "days": days, "start_date": start.Format("2006-01-02"), "end_date": end.AddDate(0, 0, -1).Format("2006-01-02")})
}

func dailyUsageRange(r *http.Request, now time.Time) (int, time.Time, time.Time, error) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		var err error
		days, err = strconv.Atoi(raw)
		if err != nil || days < 1 || days > 90 {
			return 0, time.Time{}, time.Time{}, bad("days must be between 1 and 90")
		}
	}
	zone := r.URL.Query().Get("timezone")
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return 0, time.Time{}, time.Time{}, bad("invalid timezone")
	}
	now = now.In(loc)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return days, day.AddDate(0, 0, 1-days), day.AddDate(0, 0, 1), nil
}

func (a *App) usageSearch(w http.ResponseWriter, r *http.Request) error {
	query := r.URL.Query().Get("q")
	if len(query) > 255 {
		return bad("search query is too long")
	}
	statement := "SELECT jsonb_build_object('id',id,'email',email,'deleted',deleted_at IS NOT NULL) FROM users WHERE $1<>'' AND (strpos(lower(email),lower($1))>0 OR strpos(lower(username),lower($1))>0) ORDER BY email,id LIMIT 30"
	args := []any{query}
	if strings.HasSuffix(r.URL.Path, "search-api-keys") {
		uid := int64(0)
		if raw := r.URL.Query().Get("user_id"); raw != "" {
			var err error
			uid, err = strconv.ParseInt(raw, 10, 64)
			if err != nil || uid < 1 {
				return bad("invalid user_id")
			}
		}
		statement = "SELECT jsonb_build_object('id',id,'name',name,'user_id',user_id) FROM api_keys WHERE ($2::bigint=0 OR user_id=$2) AND ($1='' OR strpos(lower(name),lower($1))>0) ORDER BY id DESC LIMIT 30"
		args = append(args, uid)
	}
	rows, err := a.DB.QueryContext(r.Context(), statement, args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, items)
}
func (a *App) gatewayBilling(w http.ResponseWriter, r *http.Request) {
	if err := a.checkInstance(r.Context()); err != nil {
		gatewayError(w, err)
		return
	}
	g, err := a.gatewayAuth(r, false)
	if err != nil {
		gatewayError(w, err)
		return
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT jsonb_build_object('balance',u.balance,'quota',k.quota,'quota_used',k.quota_used,'rate_limit_5h',k.rate_limit_5h,'rate_limit_1d',k.rate_limit_1d,'rate_limit_7d',k.rate_limit_7d,'usage_5h',CASE WHEN k.window_5h_start+interval '5 hours'<=now() THEN 0 ELSE k.usage_5h END,'usage_1d',CASE WHEN k.window_1d_start+interval '24 hours'<=now() THEN 0 ELSE k.usage_1d END,'usage_7d',CASE WHEN k.window_7d_start+interval '168 hours'<=now() THEN 0 ELSE k.usage_7d END) FROM users u JOIN api_keys k ON k.user_id=u.id WHERE u.id=$1 AND k.id=$2`, g.UserID, g.Key.ID))
	if err != nil {
		gatewayError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(raw)
}

func (a *App) gatewayUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	err := func() error {
		if err := a.checkInstance(r.Context()); err != nil {
			return err
		}
		g, err := a.gatewayAuth(r, false)
		if err != nil {
			return err
		}
		now := time.Now()
		_, dailyStart, dailyEnd, err := dailyUsageRange(r, now)
		if err != nil {
			return err
		}
		today, _ := quotaStarts(now)
		start, end := now.AddDate(0, 0, -30), now
		for i, name := range []string{"start_date", "end_date"} {
			if value := r.URL.Query().Get(name); value != "" {
				date, err := time.ParseInLocation("2006-01-02", value, today.Location())
				if err != nil {
					return bad("invalid " + name)
				}
				if i == 0 {
					start = date
				} else {
					end = date.AddDate(0, 0, 1)
				}
			}
		}
		if !start.Before(end) {
			return bad("start_date must precede the end of end_date")
		}
		if err = a.gatewayRPM(r.Context(), g); err != nil {
			return err
		}
		if !a.takeSlot("user", g.UserID, g.Concurrency) {
			return &apiError{429, "user concurrency limit reached"}
		}
		defer a.releaseSlot("user", g.UserID)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		// One statement gives balance, windows and raw usage the same snapshot.
		// Monetary sums remain PostgreSQL NUMERIC all the way to the JSON response.
		raw, err := jsonRow(a.DB.QueryRowContext(ctx, `WITH key_state AS (
 SELECT k.*,u.balance,(k.quota>0 OR k.rate_limit_5h>0 OR k.rate_limit_1d>0 OR k.rate_limit_7d>0) AS limited
 FROM api_keys k JOIN users u ON u.id=k.user_id WHERE k.id=$2 AND u.id=$1
), windows AS (
 SELECT w.*,CASE WHEN window_start+duration>$3 THEN consumed ELSE 0 END AS used
 FROM key_state k CROSS JOIN LATERAL (VALUES
 (1,'5h',k.rate_limit_5h,k.usage_5h,k.window_5h_start,interval '5 hours'),
 (2,'1d',k.rate_limit_1d,k.usage_1d,k.window_1d_start,interval '24 hours'),
 (3,'7d',k.rate_limit_7d,k.usage_7d,k.window_7d_start,interval '168 hours')
 )w(position,name,lim,consumed,window_start,duration) WHERE lim>0
), scoped AS NOT MATERIALIZED (
 SELECT *,input_tokens::bigint+output_tokens+cache_creation_tokens+cache_read_tokens AS tokens
 FROM usage_logs WHERE user_id=$1 AND api_key_id=$2
), summaries AS (
 SELECT p.name,jsonb_build_object('requests',count(s.id),'input_tokens',COALESCE(sum(input_tokens),0),
 'output_tokens',COALESCE(sum(output_tokens),0),'cache_creation_tokens',COALESCE(sum(cache_creation_tokens),0),
 'cache_read_tokens',COALESCE(sum(cache_read_tokens),0),'total_tokens',COALESCE(sum(tokens),0),
 'cost',COALESCE(sum(total_cost),0),'actual_cost',COALESCE(sum(actual_cost),0)) AS value
 FROM (VALUES ('total','-infinity'::timestamptz),('today',$4::timestamptz))p(name,start)
 LEFT JOIN scoped s ON s.created_at>=p.start AND s.created_at<$3 GROUP BY p.name
), daily AS (
 SELECT to_char(created_at AT TIME ZONE $9,'YYYY-MM-DD') AS date,count(*) AS requests,
 sum(input_tokens) AS input_tokens,sum(output_tokens) AS output_tokens,sum(cache_read_tokens) AS cache_read_tokens,
 sum(cache_creation_tokens) AS cache_write_tokens,sum(tokens) AS total_tokens,sum(total_cost) AS cost,sum(actual_cost) AS actual_cost
 FROM scoped WHERE created_at >= $7 AND created_at < $8 GROUP BY date
), models AS (
 SELECT model,count(*) AS requests,sum(input_tokens) AS input_tokens,sum(output_tokens) AS output_tokens,
 sum(cache_creation_tokens) AS cache_creation_tokens,sum(cache_read_tokens) AS cache_read_tokens,
 sum(tokens) AS total_tokens,sum(total_cost) AS cost,sum(actual_cost) AS actual_cost
 FROM scoped WHERE created_at >= $5 AND created_at < $6 GROUP BY model
)
SELECT CASE WHEN k.limited THEN
 jsonb_build_object('mode','quota_limited','isValid',true,'status',k.status)
 || CASE WHEN k.quota>0 THEN jsonb_build_object('quota',jsonb_build_object('limit',k.quota,'used',k.quota_used,
 'remaining',GREATEST(0,k.quota-k.quota_used),'unit','USD'),'remaining',GREATEST(0,k.quota-k.quota_used),'unit','USD') ELSE '{}'::jsonb END
 || CASE WHEN EXISTS(SELECT 1 FROM windows) THEN jsonb_build_object('rate_limits',(
 SELECT jsonb_agg(jsonb_build_object('window',name,'limit',lim,'used',used,'remaining',GREATEST(0,lim-used),'window_start',window_start)
 || CASE WHEN window_start+duration>$3 THEN jsonb_build_object('reset_at',window_start+duration) ELSE '{}'::jsonb END ORDER BY position) FROM windows)) ELSE '{}'::jsonb END
 || CASE WHEN k.expires_at IS NOT NULL THEN jsonb_build_object('expires_at',k.expires_at,
 'days_until_expiry',GREATEST(0,floor(extract(epoch FROM(k.expires_at-$3))/86400))) ELSE '{}'::jsonb END
 ELSE jsonb_build_object('mode','unrestricted','isValid',true,'planName','钱包余额','remaining',k.balance,'unit','USD','balance',k.balance) END
 || jsonb_build_object('usage',(SELECT jsonb_object_agg(name,value) FROM summaries) || jsonb_build_object(
 'average_duration_ms',(SELECT COALESCE(avg(duration_ms),0) FROM scoped WHERE created_at<$3),
 'rpm',(SELECT count(*)/5 FROM scoped WHERE created_at >= $3::timestamptz-interval '5 minutes' AND created_at<$3),
 'tpm',(SELECT COALESCE(sum(tokens),0)::bigint/5 FROM scoped WHERE created_at >= $3::timestamptz-interval '5 minutes' AND created_at<$3)),
 'daily_usage',(SELECT COALESCE(jsonb_agg(to_jsonb(daily) ORDER BY date),'[]'::jsonb) FROM daily),
 'model_stats',(SELECT COALESCE(jsonb_agg(to_jsonb(models) ORDER BY total_tokens DESC,model),'[]'::jsonb) FROM models))
 FROM key_state k`, g.UserID, g.Key.ID, now, today, start, end, dailyStart, dailyEnd, dailyStart.Location().String()))
		if err != nil {
			return err
		}
		return rawReply(w, raw)
	}()
	if err != nil {
		gatewayError(w, err)
	}
}
