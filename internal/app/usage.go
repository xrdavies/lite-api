package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
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
const userUsageColumns = `id,request_id,api_key_id,model,requested_model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,input_cost,output_cost,cache_creation_cost,cache_read_cost,total_cost,actual_cost,rate_multiplier,stream,duration_ms,first_token_ms,created_at,group_id,billing_type,billing_mode,request_type,image_input_tokens,image_output_tokens,image_input_cost,image_output_cost,service_tier,reasoning_effort`

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
func (a *App) usageErrors(w http.ResponseWriter, r *http.Request) error {
	uid := current(r).ID
	page, size := pagination(r)
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM ops_error_logs WHERE user_id=$1", uid).Scan(&total); err != nil {
		return err
	}
	const columns = "id,request_id,api_key_id,model,request_path,stream,error_phase,error_type,status_code,error_message,is_business_limited,duration_ms,created_at"
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(e) FROM(SELECT "+columns+" FROM ops_error_logs WHERE user_id=$1 AND id=$2)e", uid, id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(e) FROM(SELECT "+columns+" FROM ops_error_logs WHERE user_id=$1 ORDER BY id DESC LIMIT $2 OFFSET $3)e", uid, size, (page-1)*size)
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
	a.route("GET /api/v1/usage", "user", a.listUsage)
	a.route("GET /api/v1/usage/{id}", "user", a.getUsage)
	a.route("GET /api/v1/usage/stats", "user", a.usageStats)
	a.route("GET /api/v1/usage/errors", "user", a.usageErrors)
	a.route("GET /api/v1/usage/errors/{id}", "user", a.usageErrors)
	a.route("GET /api/v1/admin/usage", "admin", a.listUsage)
	a.route("GET /api/v1/admin/usage/stats", "admin", a.usageStats)
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
