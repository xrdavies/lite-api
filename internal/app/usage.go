package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

const usageRequestedModel = "COALESCE(NULLIF(TRIM(requested_model),''),model)"

func (a *App) usageFilters(r *http.Request) (string, []any, error) {
	q := r.URL.Query()
	admin := current(r).Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/v1/admin/")
	conditions := []string{"TRUE"}
	args := []any{}
	add := func(expression string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(expression, len(args)))
	}
	if !admin {
		add("user_id=$%d", current(r).ID)
	}
	for _, field := range []string{"user_id", "api_key_id", "account_id", "group_id"} {
		if !admin && (field == "user_id" || field == "account_id") {
			continue
		}
		if raw := strings.TrimSpace(q.Get(field)); raw != "" {
			id, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || id < 0 || id == 0 && field == "api_key_id" && !admin {
				return "", nil, bad("invalid " + field)
			}
			if field == "api_key_id" && !admin {
				var owner int64
				if err := a.DB.QueryRowContext(r.Context(), "SELECT user_id FROM api_keys WHERE id=$1 AND deleted_at IS NULL", id).Scan(&owner); err != nil {
					return "", nil, err
				}
				if owner != current(r).ID {
					return "", nil, denied()
				}
			}
			if id > 0 {
				add(field+"=$%d", id)
			}
		}
	}
	if model := strings.TrimSpace(q.Get("model")); model != "" {
		if len([]rune(model)) > 100 {
			return "", nil, bad("model filter too long")
		}
		add(usageRequestedModel+"=$%d", model)
	}
	if admin {
		if id := strings.TrimSpace(q.Get("request_id")); id != "" {
			if len(id) > 64 {
				return "", nil, bad("request_id filter too long")
			}
			add("request_id=$%d", id)
		}
		if raw := q.Get("exact_total"); raw != "" {
			if _, err := strconv.ParseBool(strings.TrimSpace(raw)); err != nil {
				return "", nil, bad("invalid exact_total")
			}
		}
	}
	if raw := strings.ToLower(strings.TrimSpace(q.Get("request_type"))); raw != "" {
		kind, ok := map[string]int{"unknown": 0, "sync": 1, "stream": 2, "ws_v2": 3, "cyber": 4, "live": 5}[raw]
		if !ok {
			return "", nil, bad("invalid request_type")
		}
		expression := "request_type=$%d"
		switch kind {
		case 1:
			expression = "(request_type=$%d OR (request_type=0 AND NOT stream AND NOT openai_ws_mode))"
		case 2:
			expression = "(request_type=$%d OR (request_type=0 AND stream AND NOT openai_ws_mode))"
		case 3:
			expression = "(request_type=$%d OR (request_type=0 AND openai_ws_mode))"
		}
		add(expression, kind)
	}
	for _, field := range []string{"stream", "native_compaction_v2", "upstream_model_mismatch"} {
		if field == "stream" && strings.TrimSpace(q.Get("request_type")) != "" || field == "upstream_model_mismatch" && !admin {
			continue
		}
		if raw := strings.TrimSpace(q.Get(field)); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				return "", nil, bad("invalid " + field)
			}
			// Equality preserves unknown (NULL) model observations as unknown.
			add(field+"=$%d", value)
		}
	}
	if raw := strings.TrimSpace(q.Get("billing_type")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 8)
		if err != nil {
			return "", nil, bad("invalid billing_type")
		}
		add("billing_type=$%d", value)
	}
	if mode := strings.TrimSpace(q.Get("billing_mode")); mode != "" {
		expression := "billing_mode=$%d"
		switch mode {
		case "token":
			expression = "(billing_mode=$%d OR (COALESCE(billing_mode,'')='' AND COALESCE(image_count,0)<=0))"
		case "image":
			expression = "(billing_mode=$%d OR (COALESCE(billing_mode,'')='' AND COALESCE(image_count,0)>0))"
		case "video", "per_request":
		default:
			return "", nil, bad("invalid billing_mode")
		}
		add(expression, mode)
	}
	start, end, err := usageFilterRange(r, admin, time.Now())
	if err != nil {
		return "", nil, err
	}
	if !start.IsZero() {
		add("created_at>=$%d", start)
	}
	if !end.IsZero() {
		add("created_at<$%d", end)
	}
	return " WHERE " + strings.Join(conditions, " AND "), args, nil
}

func usageFilterRange(r *http.Request, admin bool, now time.Time) (time.Time, time.Time, error) {
	q := r.URL.Query()
	zone := strings.TrimSpace(q.Get("timezone"))
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "Local" {
		return time.Time{}, time.Time{}, bad("invalid timezone")
	}
	var dates [2]time.Time
	for i, field := range []string{"start_date", "end_date"} {
		if raw := strings.TrimSpace(q.Get(field)); raw != "" {
			value, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				value, err = time.ParseInLocation("2006-01-02", raw, loc)
				if err == nil && i == 1 {
					value = value.AddDate(0, 0, 1)
				}
			}
			if err != nil {
				return time.Time{}, time.Time{}, bad("invalid " + field)
			}
			dates[i] = value
		}
	}
	if strings.HasSuffix(r.URL.Path, "/stats") {
		now = now.In(loc)
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		period := strings.TrimSpace(q.Get("period"))
		if period == "" && admin {
			period = "today"
		}
		start, end := day.AddDate(0, 0, -7), day.AddDate(0, 0, 1)
		switch period {
		case "today":
			start, end = day, now
		case "week":
			start, end = now.AddDate(0, 0, -7), now
		case "month":
			start, end = now.AddDate(0, -1, 0), now
		case "":
		default:
			return time.Time{}, time.Time{}, bad("period must be today, week or month")
		}
		if dates[0].IsZero() {
			dates[0] = start
		}
		if dates[1].IsZero() {
			dates[1] = end
		}
	}
	if !dates[0].IsZero() && !dates[1].IsZero() && !dates[0].Before(dates[1]) {
		return time.Time{}, time.Time{}, bad("start_date must precede the end of end_date")
	}
	return dates[0], dates[1], nil
}

func usageOrder(r *http.Request) string {
	field := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort_by")))
	switch field {
	case "", "created_at":
		field = "created_at"
	case "model":
		field = usageRequestedModel
	default:
		field = "id"
	}
	direction := " DESC"
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("sort_order")), "asc") {
		direction = " ASC"
	}
	if field == "id" {
		return field + direction
	}
	return field + direction + ",id" + direction
}

// User views expose usage and price snapshots, not upstream account/channel identities.
const userUsageColumns = `user_id,native_compaction_v2,ip_address,inbound_endpoint,user_agent,session_id,cache_ttl_overridden,long_context_billing_applied,openai_ws_mode,id,request_id,api_key_id,model,requested_model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,input_cost,output_cost,cache_creation_cost,cache_read_cost,total_cost,actual_cost,rate_multiplier,stream,duration_ms,first_token_ms,created_at,group_id,billing_type,billing_mode,request_type,image_input_tokens,image_output_tokens,image_input_cost,image_output_cost,service_tier,reasoning_effort,requested_reasoning_effort,video_count,video_resolution,video_duration_seconds,image_count,image_size,image_size_source,image_input_size,image_output_size,image_size_breakdown`

// Public fields describe the client's request; stored model and request_type
// retain their billing and numeric meanings in the database.
const usageView = `to_jsonb(l) || jsonb_build_object('model',` + usageRequestedModel + `,
 'request_type',CASE request_type WHEN 1 THEN 'sync' WHEN 2 THEN 'stream' WHEN 3 THEN 'ws_v2'
 WHEN 4 THEN 'cyber' WHEN 5 THEN 'live' ELSE CASE WHEN openai_ws_mode THEN 'ws_v2' WHEN stream THEN 'stream' ELSE 'sync' END END,
 'stream',CASE request_type WHEN 1 THEN false WHEN 2 THEN true WHEN 3 THEN true ELSE stream END,
 'openai_ws_mode',CASE request_type WHEN 1 THEN false WHEN 2 THEN false WHEN 3 THEN true ELSE openai_ws_mode END)`

// Usage binds relations to the historical row, not the Key's current group.
// Profiles are public projections; credentials never belong in a usage report.
func usageRelations(admin bool) string {
	view := `jsonb_build_object(
 'user',(SELECT ` + userView(false) + ` || CASE WHEN u.deleted_at IS NOT NULL THEN jsonb_build_object('deleted_at',u.deleted_at) ELSE '{}'::jsonb END FROM users u WHERE u.id=l.user_id),
 'api_key',(SELECT (` + keyView + `)-'key' FROM api_keys k WHERE k.id=l.api_key_id AND k.user_id=l.user_id AND k.deleted_at IS NULL),
 'group',(SELECT ` + publicGroupView + ` FROM groups g WHERE g.id=l.group_id AND g.deleted_at IS NULL))`
	if admin {
		view += ` || jsonb_build_object('account',(SELECT jsonb_build_object('id',a.id,'name',a.name) FROM accounts a WHERE a.id=l.account_id AND a.deleted_at IS NULL))`
	}
	return view
}

func usageEffortView(raw json.RawMessage, admin bool) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	forwarded := credentialString(fields, "reasoning_effort")
	requested := strings.TrimSpace(credentialString(fields, "requested_reasoning_effort"))
	if requested == "" {
		requested = forwarded
	} else {
		fields["reasoning_effort"], _ = json.Marshal(requested)
	}
	if effective := strings.TrimSpace(forwarded); admin && effective != "" && canonicalEffort(effective) != canonicalEffort(requested) {
		fields["upstream_reasoning_effort"], _ = json.Marshal(effective)
	}
	return json.Marshal(fields)
}

func (a *App) listUsage(w http.ResponseWriter, r *http.Request) error {
	where, args, err := a.usageFilters(r)
	if err != nil {
		return err
	}
	page, size := pagination(r)
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var total int
	if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM usage_logs"+where, args...).Scan(&total); err != nil {
		return err
	}
	columns := userUsageColumns
	admin := current(r).Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/v1/admin/")
	if admin {
		columns = "*"
	}
	args = append(args, size, (page-1)*size)
	rows, err := tx.QueryContext(r.Context(), "SELECT "+usageView+" || "+usageRelations(admin)+" FROM(SELECT "+columns+" FROM usage_logs"+where+" ORDER BY "+usageOrder(r)+fmt.Sprintf(" LIMIT $%d OFFSET $%d)l", len(args)-1, len(args)), args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	for i := range items {
		if items[i], err = usageEffortView(items[i], admin); err != nil {
			return err
		}
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) getUsage(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT "+usageView+" || "+usageRelations(false)+" FROM(SELECT "+userUsageColumns+" FROM usage_logs WHERE id=$1 AND user_id=$2)l", id, current(r).ID))
	if err != nil {
		return err
	}
	raw, err = usageEffortView(raw, false)
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) usageStats(w http.ResponseWriter, r *http.Request) error {
	where, args, err := a.usageFilters(r)
	if err != nil {
		return err
	}
	projection := `to_jsonb(s)-'total_account_cost' || jsonb_build_object(
 'input_tokens',s.total_input_tokens,'output_tokens',s.total_output_tokens,
 'cache_creation_tokens',s.total_cache_creation_tokens,'cache_read_tokens',s.total_cache_read_tokens,'actual_cost',s.total_actual_cost)`
	if current(r).Role == "admin" && strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		projection += ` || jsonb_build_object('total_account_cost',s.total_account_cost)`
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT `+projection+` FROM (
 SELECT count(*) AS total_requests,COALESCE(sum(input_tokens),0) AS total_input_tokens,
 COALESCE(sum(output_tokens),0) AS total_output_tokens,COALESCE(sum(cache_creation_tokens),0) AS total_cache_creation_tokens,
 COALESCE(sum(cache_read_tokens),0) AS total_cache_read_tokens,
 COALESCE(sum(cache_creation_tokens::bigint+cache_read_tokens),0) AS total_cache_tokens,
 COALESCE(sum(input_tokens::bigint+output_tokens+cache_creation_tokens+cache_read_tokens),0) AS total_tokens,
 COALESCE(sum(total_cost),0) AS total_cost,COALESCE(sum(actual_cost),0) AS total_actual_cost,
 COALESCE(sum(COALESCE(account_stats_cost,total_cost)*COALESCE(account_rate_multiplier,1)),0) AS total_account_cost,
 COALESCE(avg(duration_ms),0) AS average_duration_ms FROM usage_logs`+where+`)s`, args...))
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

// Preserve the raw phase/type while classifying this gateway's final failures
// for the same user-facing categories as native provider errors.
const userErrorCategory = `CASE error_phase
 WHEN 'auth' THEN 'auth' WHEN 'routing' THEN 'service_unavailable'
 WHEN 'account_auth' THEN 'upstream' WHEN 'upstream' THEN 'upstream' WHEN 'network' THEN 'upstream'
 WHEN 'internal' THEN 'internal'
 WHEN 'request' THEN CASE error_type WHEN 'rate_limit_error' THEN 'rate_limit'
 WHEN 'billing_error' THEN 'quota' WHEN 'subscription_error' THEN 'quota'
 WHEN 'invalid_request_error' THEN 'invalid_request' ELSE 'other' END
 WHEN 'gateway' THEN CASE status_code WHEN 401 THEN 'auth' WHEN 403 THEN 'auth'
 WHEN 429 THEN 'rate_limit' WHEN 402 THEN 'quota' WHEN 400 THEN 'invalid_request'
 WHEN 404 THEN 'invalid_request' WHEN 413 THEN 'invalid_request' WHEN 422 THEN 'invalid_request'
 WHEN 503 THEN 'service_unavailable' WHEN 502 THEN 'upstream' WHEN 504 THEN 'upstream'
 WHEN 500 THEN 'internal' ELSE 'other' END ELSE 'other' END`

func (a *App) usageErrors(w http.ResponseWriter, r *http.Request) error {
	// Both entry points are opt-in. Missing, invalid or unreadable settings never
	// expose records; do not reuse an administrator's monitoring permissions.
	var allowed bool
	if err := a.DB.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM settings WHERE key='allow_user_view_error_requests' AND value='true')").Scan(&allowed); err != nil || !allowed {
		return &apiError{403, "error requests view is disabled"}
	}
	args := []any{current(r).ID}
	where := " FROM ops_error_logs e WHERE user_id=$1 AND status_code>=400 AND error_phase NOT IN ('upstream','account_auth') AND NOT is_count_tokens"
	add := func(expression string, value any) {
		args = append(args, value)
		where += " AND " + fmt.Sprintf(expression, len(args))
	}
	// Provider diagnostics and bodies stay internal; only the normalized gateway
	// message and the authenticated user's own request metadata are projected.
	const columns = `id,request_id,api_key_id,` + usageRequestedModel + ` AS model,request_path,
 stream,error_phase,error_type,status_code,error_message,is_business_limited,duration_ms,created_at,
 ` + userErrorCategory + ` AS category,COALESCE(error_message,'') AS message,
 COALESCE(NULLIF(inbound_endpoint,''),request_path,'') AS inbound_endpoint,platform,client_ip,request_type,user_agent,
 COALESCE((SELECT k.name FROM api_keys k WHERE k.id=e.api_key_id AND k.user_id=e.user_id),'') AS key_name,
 NOT EXISTS(SELECT 1 FROM api_keys k WHERE k.id=e.api_key_id AND k.user_id=e.user_id AND k.deleted_at IS NULL) AS key_deleted,
 COALESCE((SELECT g.name FROM groups g WHERE g.id=e.group_id),'') AS group_name`
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(x) FROM(SELECT "+columns+where+" AND id=$2)x", current(r).ID, id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	q := r.URL.Query()
	for _, field := range []string{"api_key_id", "status_code"} {
		if raw := strings.TrimSpace(q.Get(field)); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 || field == "status_code" && value > 599 {
				return bad("invalid " + field)
			}
			if field == "status_code" || value > 0 {
				add(field+"=$%d", value)
			}
		}
	}
	if model := strings.TrimSpace(q.Get("model")); model != "" {
		if len([]rune(model)) > 100 || strings.ContainsRune(model, 0) {
			return bad("invalid model filter")
		}
		add("position(lower($%d) in lower("+usageRequestedModel+"))>0", model)
	}
	switch category := strings.TrimSpace(q.Get("category")); category {
	case "auth", "service_unavailable", "upstream", "internal", "rate_limit", "quota", "invalid_request":
		add("("+userErrorCategory+")=$%d", category)
	}
	start, end, err := usageFilterRange(r, false, time.Now())
	if err != nil {
		return err
	}
	if !start.IsZero() {
		add("created_at>=$%d", start)
	}
	if !end.IsZero() {
		add("created_at<$%d", end)
	}
	field := "created_at"
	switch strings.ToLower(strings.TrimSpace(q.Get("sort_by"))) {
	case "model":
		field = usageRequestedModel
	case "status_code":
		field = "status_code"
	}
	direction := " DESC"
	if strings.EqualFold(strings.TrimSpace(q.Get("sort_order")), "asc") {
		direction = " ASC"
	}
	page, size := pagination(r)
	size = min(size, 100)
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var total int
	if err = tx.QueryRowContext(r.Context(), "SELECT count(*)"+where, args...).Scan(&total); err != nil {
		return err
	}
	args = append(args, size, (page-1)*size)
	rows, err := tx.QueryContext(r.Context(), "SELECT to_jsonb(x) FROM(SELECT "+columns+where+" ORDER BY "+field+direction+",id"+direction+fmt.Sprintf(" LIMIT $%d OFFSET $%d)x", len(args)-1, len(args)), args...)
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
		defer a.trackKeySlot(g.Key.ID)()
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
