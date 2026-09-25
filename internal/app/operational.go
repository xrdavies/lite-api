package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// These queries read current account state and raw usage, without rollup jobs.
type operationalAccount struct {
	ID          int64                      `json:"id"`
	Name        string                     `json:"name"`
	Platform    string                     `json:"platform"`
	Status      string                     `json:"status"`
	Error       string                     `json:"error_message"`
	Schedulable bool                       `json:"schedulable"`
	Concurrency int                        `json:"concurrency"`
	AutoPause   bool                       `json:"auto_pause_on_expired"`
	Expires     *time.Time                 `json:"expires_at"`
	RateReset   *time.Time                 `json:"rate_limit_reset_at"`
	Overload    *time.Time                 `json:"overload_until"`
	Temporary   *time.Time                 `json:"temp_unschedulable_until"`
	Extra       map[string]json.RawMessage `json:"extra"`
	Groups      []struct {
		ID                     int64
		Name, Platform, Status string
	} `json:"groups"`
}

func operationalScope(r *http.Request) (string, int64, error) {
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if platform != "" && !supportedPlatform(platform) {
		return "", 0, bad("invalid platform")
	}
	var gid int64
	if raw := strings.TrimSpace(r.URL.Query().Get("group_id")); raw != "" {
		var err error
		gid, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || gid <= 0 {
			return "", 0, bad("invalid group_id")
		}
	}
	return platform, gid, nil
}

func (a *App) operatingAccounts(r *http.Request) ([]operationalAccount, error) {
	platform, gid, err := operationalScope(r)
	if err != nil {
		return nil, err
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT jsonb_build_object(
 'id',a.id,'name',a.name,'platform',a.platform,'status',a.status,'error_message',a.error_message,'schedulable',a.schedulable,
 'concurrency',a.concurrency,'auto_pause_on_expired',a.auto_pause_on_expired,'expires_at',a.expires_at,
 'rate_limit_reset_at',a.rate_limit_reset_at,'overload_until',a.overload_until,
 'temp_unschedulable_until',a.temp_unschedulable_until,'extra',a.extra,
 'groups',COALESCE((SELECT jsonb_agg(jsonb_build_object('id',g.id,'name',g.name,'platform',g.platform,'status',g.status) ORDER BY g.id)
 FROM account_groups ag JOIN groups g ON g.id=ag.group_id WHERE ag.account_id=a.id AND g.deleted_at IS NULL),'[]'::jsonb)),COALESCE(a.credentials->>'api_key','')
 FROM accounts a WHERE a.deleted_at IS NULL AND a.type='apikey'
 AND COALESCE(a.credentials->>'account_mode','') IN ('','payg') AND ($1='' OR a.platform=$1)
 AND ($2::bigint=0 OR EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=a.id AND ag.group_id=$2)) ORDER BY a.id`, platform, gid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []operationalAccount{}
	for rows.Next() {
		var b []byte
		var key string
		if err := rows.Scan(&b, &key); err != nil {
			return nil, err
		}
		var acc operationalAccount
		if err := json.Unmarshal(b, &acc); err != nil {
			return nil, err
		}
		if supportedPlatform(acc.Platform) {
			acc.Error = cleanErrorMessage(acc.Error, key)
			result = append(result, acc)
		}
	}
	return result, rows.Err()
}
func (u operationalAccount) available(now time.Time) bool {
	return u.Status == "active" && u.Schedulable && u.Concurrency > 0 &&
		!(u.AutoPause && u.Expires != nil && !now.Before(*u.Expires)) &&
		!futureTime(u.RateReset, now) && !futureTime(u.Overload, now) && !futureTime(u.Temporary, now) && accountQuotaAvailable(u.Extra, now)
}
func futureTime(at *time.Time, now time.Time) bool { return at != nil && now.Before(*at) }

type availabilityCount struct {
	Platform  string `json:"platform"`
	GroupID   int64  `json:"group_id,omitempty"`
	GroupName string `json:"group_name,omitempty"`
	Total     int    `json:"total_accounts"`
	Available int    `json:"available_count"`
	Limited   int    `json:"rate_limit_count"`
	Errors    int    `json:"error_count"`
}

func (a *App) accountAvailability(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.operatingAccounts(r)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	accounts := map[int64]any{}
	groups := map[int64]*availabilityCount{}
	platforms := map[string]*availabilityCount{}
	for _, acc := range rows {
		available, limited, hasError := acc.available(now), acc.Status != "error" && futureTime(acc.RateReset, now), acc.Status == "error"
		overloaded := !hasError && futureTime(acc.Overload, now)
		item := map[string]any{"account_id": acc.ID, "account_name": acc.Name, "platform": acc.Platform, "status": acc.Status,
			"schedulable": acc.Schedulable, "is_available": available, "is_rate_limited": limited, "has_error": hasError,
			"is_overloaded": overloaded, "rate_limit_reset_at": nil, "rate_limit_remaining_sec": nil,
			"overload_until": nil, "overload_remaining_sec": nil, "expires_at": acc.Expires, "error_message": acc.Error,
			"quota_available": accountQuotaAvailable(acc.Extra, now), "group_id": int64(0), "group_name": ""}
		if limited {
			item["rate_limit_reset_at"] = acc.RateReset
			if seconds := int64(acc.RateReset.Sub(now) / time.Second); seconds > 0 {
				item["rate_limit_remaining_sec"] = seconds
			}
		}
		if overloaded {
			item["overload_until"] = acc.Overload
			if seconds := int64(acc.Overload.Sub(now) / time.Second); seconds > 0 {
				item["overload_remaining_sec"] = seconds
			}
		}
		if futureTime(acc.Temporary, now) {
			item["temp_unschedulable_until"] = acc.Temporary
		}
		if len(acc.Groups) > 0 {
			item["group_id"], item["group_name"] = acc.Groups[0].ID, acc.Groups[0].Name
		}
		accounts[acc.ID] = item
		add := func(c *availabilityCount) {
			c.Total++
			if available {
				c.Available++
			}
			if limited {
				c.Limited++
			}
			if hasError {
				c.Errors++
			}
		}
		if platforms[acc.Platform] == nil {
			platforms[acc.Platform] = &availabilityCount{Platform: acc.Platform}
		}
		add(platforms[acc.Platform])
		for _, g := range acc.Groups {
			if groups[g.ID] == nil {
				groups[g.ID] = &availabilityCount{Platform: g.Platform, GroupID: g.ID, GroupName: g.Name}
			}
			add(groups[g.ID])
		}
	}
	return reply(w, map[string]any{"enabled": true, "timestamp": now, "account": accounts, "group": groups, "platform": platforms})
}

func (a *App) realtimeTraffic(w http.ResponseWriter, r *http.Request) error {
	platform, gid, err := operationalScope(r)
	if err != nil {
		return err
	}
	label := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("window")))
	minutes := map[string]int{"": 1, "1min": 1, "1m": 1, "5min": 5, "5m": 5, "30min": 30, "30m": 30, "1h": 60, "60m": 60, "60min": 60}[label]
	if minutes == 0 {
		return bad("invalid window")
	}
	label = strconv.Itoa(minutes) + "min"
	if minutes == 60 {
		label = "1h"
	}
	end := time.Now().UTC()
	start := end.Add(-time.Duration(minutes) * time.Minute)
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `WITH events AS (
 SELECT u.created_at,1 AS requests,(u.input_tokens::bigint+u.output_tokens+u.cache_creation_tokens+u.cache_read_tokens) AS tokens
 FROM usage_logs u JOIN accounts a ON a.id=u.account_id
 WHERE u.created_at >= $1 AND u.created_at < $2 AND ($3='' OR a.platform=$3) AND ($4::bigint=0 OR u.group_id=$4)
 UNION ALL
 SELECT created_at,1,0 FROM ops_error_logs
 WHERE created_at >= $1 AND created_at < $2 AND ($3='' OR platform=$3) AND ($4::bigint=0 OR group_id=$4) AND status_code>=400
 AND error_phase NOT IN ('upstream','account_auth')
 AND NOT EXISTS (SELECT 1 FROM usage_logs u WHERE u.request_id=ops_error_logs.request_id AND u.api_key_id=ops_error_logs.api_key_id)
 ), buckets AS (SELECT date_trunc('minute',created_at) AS bucket,sum(requests) AS requests,sum(tokens) AS tokens FROM events GROUP BY 1)
 SELECT jsonb_build_object('window',$5::text,'start_time',$1::timestamptz,'end_time',$2::timestamptz,'platform',$3::text,'group_id',NULLIF($4::bigint,0),
 'qps',jsonb_build_object('avg',round(COALESCE(sum(requests),0)/$6::numeric,1),'peak',round(COALESCE(max(requests),0)/60,1),'current',
 (SELECT round(COALESCE(sum(requests),0)/60::numeric,1) FROM events WHERE created_at >= $2::timestamptz-interval '1 minute')),
 'tps',jsonb_build_object('avg',round(COALESCE(sum(tokens),0)/$6::numeric,1),'peak',round(COALESCE(max(tokens),0)/60,1),'current',
 (SELECT round(COALESCE(sum(tokens),0)/60::numeric,1) FROM events WHERE created_at >= $2::timestamptz-interval '1 minute')))
 FROM buckets`, start, end, platform, gid, label, minutes*60))
	if err != nil {
		return err
	}
	return reply(w, map[string]any{"enabled": true, "timestamp": end, "summary": raw})
}

func opsErrorColumns() string {
	return `id,request_id,client_request_id,user_id,api_key_id,account_id,group_id,platform,model,request_path,stream,
		error_phase,error_type,severity,status_code,is_business_limited,error_message,error_owner,error_source,
		account_status,upstream_status_code,provider_error_code,network_error_type,retry_after_seconds,
		duration_ms,time_to_first_token_ms,created_at,is_count_tokens,resolved,inbound_endpoint,
		upstream_endpoint,requested_model,upstream_model,request_type,api_key_prefix`
}

func (a *App) opsErrors(w http.ResponseWriter, r *http.Request) error {
	linked := r.PathValue("id") != "" && strings.HasSuffix(r.URL.Path, "/upstream-errors")
	if r.PathValue("id") != "" && !linked {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(x) FROM (SELECT "+opsErrorColumns()+" FROM ops_error_logs WHERE id=$1)x", id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	where := []string{"true"}
	q := r.URL.Query()
	upstream := strings.HasSuffix(r.URL.Path, "/upstream-errors")
	if upstream {
		where = append(where, "error_phase IN ('upstream','account_auth') AND error_owner='provider'")
	} else {
		where = append(where, "status_code>=400 AND error_phase NOT IN ('upstream','account_auth')")
	}

	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if linked {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var requestID, clientID string
		if err := tx.QueryRowContext(r.Context(), "SELECT COALESCE(request_id,''),COALESCE(client_request_id,'') FROM ops_error_logs WHERE id=$1", id).Scan(&requestID, &clientID); err != nil {
			return err
		}
		if requestID != "" {
			add("request_id=$%d", requestID)
		} else if clientID != "" {
			add("client_request_id=$%d", clientID)
		} else {
			where = append(where, "false")
		}
	}
	// Linked diagnostics include business-limited attempts; ordinary lists use
	// the configured view without changing the persisted error classification.
	if !linked {
		switch strings.ToLower(strings.TrimSpace(q.Get("view"))) {
		case "all":
		case "excluded":
			where = append(where, "COALESCE(is_business_limited,false)")
		default:
			where = append(where, "NOT COALESCE(is_business_limited,false)")
		}
	}
	for _, column := range []string{"user_id", "api_key_id", "account_id", "group_id"} {
		if value := r.URL.Query().Get(column); value != "" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return bad("invalid " + column)
			}
			add(column+"=$%d", id)
		}
	}
	if value := strings.ToLower(strings.TrimSpace(q.Get("resolved"))); value != "" {
		if value == "yes" {
			value = "true"
		} else if value == "no" {
			value = "false"
		}
		resolved, err := strconv.ParseBool(value)
		if err != nil {
			return bad("invalid resolved")
		}
		add("COALESCE(resolved,false)=$%d", resolved)
	}
	for _, filter := range [][2]string{{"model", usageRequestedModel}, {"phase", "error_phase"}, {"error_phase", "error_phase"}, {"error_type", "error_type"}, {"error_source", "LOWER(COALESCE(error_source,''))"}, {"error_owner", "LOWER(COALESCE(error_owner,''))"}} {
		if value := strings.TrimSpace(q.Get(filter[0])); value != "" {
			if len(value) > 100 || strings.ContainsRune(value, 0) {
				return bad("invalid " + filter[0])
			}
			if filter[0] != "model" && filter[0] != "error_type" {
				value = strings.ToLower(value)
			}
			add(filter[1]+"=$%d", value)
		}
	}
	switch category := strings.TrimSpace(q.Get("category")); category {
	case "auth", "service_unavailable", "upstream", "internal", "rate_limit", "quota", "invalid_request":
		add("("+userErrorCategory+")=$%d", category)
	}
	window := time.Hour
	if linked {
		window = 30 * 24 * time.Hour
	}
	from, to, err := operationalTimes(r, window)
	if err != nil {
		return err
	}
	add("created_at >= $%d", from)
	add("created_at < $%d", to)
	if raw := strings.TrimSpace(q.Get("status_codes")); raw != "" {
		if len(raw) > 4096 {
			return bad("too many status_codes")
		}
		codes := []int{}
		for _, part := range strings.Split(raw, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n > 599 {
				return bad("invalid status_codes")
			}
			codes = append(codes, n)
		}
		if len(codes) > 0 {
			add("COALESCE(upstream_status_code,status_code,0)=ANY($%d)", pq.Array(codes))
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("status_code")); value != "" {
		status, err := strconv.Atoi(value)
		if err != nil || status < 100 || status > 599 {
			return bad("invalid status_code")
		}
		if upstream {
			add("COALESCE(upstream_status_code,status_code)=$%d", status)
		} else {
			add("status_code=$%d", status)
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("platform")); value != "" {
		if !supportedPlatform(value) {
			return bad("invalid platform")
		}
		add("platform=$%d", value)
	}
	for _, column := range []string{"request_id", "client_request_id"} {
		if value := strings.TrimSpace(q.Get(column)); value != "" {
			if len(value) > 255 || strings.ContainsRune(value, 0) {
				return bad("invalid " + column)
			}
			add(column+"=$%d", value)
		}
	}
	if value := strings.TrimSpace(r.URL.Query().Get("q")); value != "" {
		if len(value) > 255 || strings.ContainsRune(value, 0) {
			return bad("q is too long")
		}
		args = append(args, value)
		last := len(args)
		where = append(where, fmt.Sprintf("(position(lower($%d) in lower(COALESCE(error_message,'')))>0 OR position(lower($%d) in lower("+usageRequestedModel+"))>0 OR position(lower($%d) in lower(COALESCE(request_id,'')))>0 OR position(lower($%d) in lower(COALESCE(client_request_id,'')))>0)", last, last, last, last))
	}
	if value := strings.TrimSpace(q.Get("user_query")); value != "" {
		if len(value) > 255 || strings.ContainsRune(value, 0) {
			return bad("invalid user_query")
		}
		add("EXISTS(SELECT 1 FROM users u WHERE u.id=e.user_id AND position(lower($%d) in lower(u.email))>0)", value)
	}
	field := "created_at"
	switch strings.ToLower(strings.TrimSpace(q.Get("sort_by"))) {
	case "model":
		field = usageRequestedModel
	case "status_code":
		field = "COALESCE(upstream_status_code,status_code,0)"
	}
	direction := " DESC"
	if strings.EqualFold(strings.TrimSpace(q.Get("sort_order")), "asc") {
		direction = " ASC"
	}
	base := " FROM ops_error_logs e WHERE " + strings.Join(where, " AND ")
	var total int
	if err := tx.QueryRowContext(r.Context(), "SELECT count(*)"+base, args...).Scan(&total); err != nil {
		return err
	}
	page, size := pagination(r)
	size = min(size, 500)
	args = append(args, size, (page-1)*size)
	rows, err := tx.QueryContext(r.Context(), "SELECT to_jsonb(x) FROM (SELECT "+opsErrorColumns()+base+" ORDER BY "+field+direction+",id"+direction+" LIMIT $"+strconv.Itoa(len(args)-1)+" OFFSET $"+strconv.Itoa(len(args))+")x", args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}

func operationalTimes(r *http.Request, window time.Duration) (time.Time, time.Time, error) {
	q := r.URL.Query()
	end := time.Now().UTC()
	var start time.Time
	for _, name := range []string{"start_time", "end_time"} {
		if raw := strings.TrimSpace(q.Get(name)); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return start, end, bad("invalid " + name)
			}
			if name == "start_time" {
				start = at
			} else {
				end = at
			}
		}
	}
	if strings.TrimSpace(q.Get("start_time")) == "" && strings.TrimSpace(q.Get("end_time")) == "" {
		if duration := map[string]time.Duration{"5m": 5 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}[strings.TrimSpace(q.Get("time_range"))]; duration != 0 {
			window = duration
		}
	}
	if start.IsZero() {
		start = end.Add(-window)
	}
	if start.After(end) || end.Sub(start) > 30*24*time.Hour {
		return start, end, bad("invalid time range (maximum 30 days)")
	}
	return start, end, nil
}

func (a *App) ingressRejections(w http.ResponseWriter, r *http.Request) error {
	start, end, err := operationalTimes(r, time.Hour)
	if err != nil {
		return err
	}
	where := []string{"bucket_start >= $1", "bucket_start < $2"}
	args := []any{start, end}
	for _, pair := range [][2]string{{"reason", "reject_reason"}, {"route_family", "route_family"}, {"protocol", "protocol"}, {"client_ip", "client_ip"}, {"user_id", "user_id"}, {"api_key_id", "api_key_id"}} {
		value := strings.TrimSpace(r.URL.Query().Get(pair[0]))
		if value == "" {
			continue
		}
		var v any = value
		if len(value) > 64 {
			return bad("invalid " + pair[0])
		}
		var allowed string
		switch pair[0] {
		case "reason":
			allowed = " query_api_key_deprecated api_key_required invalid_api_key invalid_auth_rate_limited api_key_auth_overloaded api_key_disabled ip_restricted user_inactive group_deleted group_disabled group_not_allowed group_unassigned other "
		case "route_family":
			allowed = " gemini codex messages responses chat_completions images videos embeddings models other "
		case "protocol":
			allowed = " google anthropic openai gateway other "
		}
		if allowed != "" && (strings.ContainsAny(value, " \t\r\n") || !strings.Contains(allowed, " "+value+" ")) {
			return bad("invalid " + pair[0])
		}
		if pair[0] == "client_ip" {
			ip, err := netip.ParseAddr(value)
			if err != nil {
				return bad("invalid client_ip")
			}
			ip = ip.Unmap()
			if ip.Is6() {
				ip = netip.PrefixFrom(ip, 64).Masked().Addr()
			}
			v = ip.String()
		}
		if pair[0] == "user_id" || pair[0] == "api_key_id" {
			id, err := strconv.ParseInt(value, 10, 64)
			if err != nil || id <= 0 {
				return bad("invalid " + pair[0])
			}
			v = id
		}
		args = append(args, v)
		where = append(where, fmt.Sprintf("%s=$%d", pair[1], len(args)))
	}
	base := " FROM ops_ingress_reject_aggregates WHERE " + strings.Join(where, " AND ")
	page, size := pagination(r)
	size = min(size, 200)
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var total int
	if err := tx.QueryRowContext(r.Context(), "SELECT count(*)"+base, args...).Scan(&total); err != nil {
		return err
	}
	args = append(args, size, (page-1)*size)
	rows, err := tx.QueryContext(r.Context(), fmt.Sprintf(`SELECT jsonb_strip_nulls(to_jsonb(x)) FROM(SELECT id,bucket_start,reject_reason,route_family,protocol,
 host(client_ip) AS client_ip,NULLIF(user_id,0) AS user_id,NULLIF(api_key_id,0) AS api_key_id,request_count,first_seen,last_seen%s
 ORDER BY bucket_start DESC,id DESC LIMIT $%d OFFSET $%d)x`, base, len(args)-1, len(args)), args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}

func (a *App) ingressHealth(w http.ResponseWriter, r *http.Request) error {
	var count int64
	if err := a.DB.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(request_count),0) FROM ops_ingress_reject_aggregates WHERE bucket_start>=now()-interval '5 minutes'").Scan(&count); err != nil {
		return err
	}
	return reply(w, map[string]any{"enabled": true, "healthy": a.ingressFailures.Load() == 0, "recent_rejections": count, "write_failures": a.ingressFailures.Load(), "dropped": a.ingressDropped.Load(), "counter_scope": "process"})
}

func (a *App) authCacheHealth(w http.ResponseWriter, r *http.Request) error {
	var pending int64
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM auth_cache_invalidation_outbox").Scan(&pending); err != nil {
		return err
	}
	// Key/user/group authorization always reads PostgreSQL; no stale auth cache
	// or invalidation subscriber exists in this single-instance implementation.
	return reply(w, map[string]any{"enabled": false, "mode": "database", "healthy": true, "outbox_required": false, "stored_events": pending})
}

// Log authentication denials in bounded per-minute dimensions, never credentials.
// ponytail: synchronous writes capped at 100/s; batch flush only if measured latency warrants it.
func (a *App) recordIngressRejection(r *http.Request, cause error, reason string, userID, keyID int64) {
	var failure *apiError
	if !errors.As(cause, &failure) || failure.status != 400 && failure.status != 401 && failure.status != 403 {
		return
	}
	ip, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return
	}
	ip = ip.Unmap()
	if ip.Is6() {
		ip = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	now := time.Now().UTC()
	n, err := a.Redis.Eval(ctx, `local n=redis.call('INCR',KEYS[1]);if n==1 then redis.call('EXPIRE',KEYS[1],2) end;return n`, []string{fmt.Sprintf("ops:ingress:rate:%d", now.Unix())}).Int()
	if err != nil {
		a.ingressFailures.Add(1)
		return
	}
	if n > 100 {
		a.ingressDropped.Add(1)
		return
	}
	family, protocol := "other", "gateway"
	for _, name := range []string{"messages", "responses", "chat/completions", "images", "videos", "embeddings", "models"} {
		if strings.Contains(r.URL.Path, "/"+name) {
			family = strings.ReplaceAll(name, "/", "_")
			protocol = "openai"
			break
		}
	}
	if family == "messages" {
		protocol = "anthropic"
	}
	if strings.HasPrefix(r.URL.Path, "/v1beta/") {
		family, protocol = "gemini", "google"
	}
	if strings.HasPrefix(r.URL.Path, "/backend-api/codex/") {
		family = "codex"
	}
	_, err = a.DB.ExecContext(ctx, `INSERT INTO ops_ingress_reject_aggregates(bucket_start,reject_reason,route_family,protocol,client_ip,request_count,first_seen,last_seen,user_id,api_key_id)
 VALUES($1,$2,$3,$4,$5,1,$6,$6,$7,$8) ON CONFLICT ON CONSTRAINT ops_ingress_reject_aggregates_dimensions_unique
 DO UPDATE SET request_count=ops_ingress_reject_aggregates.request_count+1,
 first_seen=LEAST(ops_ingress_reject_aggregates.first_seen,EXCLUDED.first_seen),
 last_seen=GREATEST(ops_ingress_reject_aggregates.last_seen,EXCLUDED.last_seen),updated_at=now()`, now.Truncate(time.Minute), reason, family, protocol, ip.String(), now, userID, keyID)
	if err != nil {
		a.ingressFailures.Add(1)
	}
}

// Each failed upstream attempt is separate from the final client-visible error,
// so recovered retries remain diagnosable without counting as failed requests.
func (a *App) recordUpstreamFailure(id string, g *gatewayIdentity, s *gatewaySelection, r *http.Request, in textRequest, path string, status int, started time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	path, _, _ = strings.Cut(path, "?")
	_, err := a.DB.ExecContext(ctx, `INSERT INTO ops_error_logs(request_id,user_id,api_key_id,account_id,group_id,platform,model,requested_model,upstream_model,request_path,stream,error_phase,error_type,error_owner,error_source,error_message,upstream_status_code,inbound_endpoint,upstream_endpoint,duration_ms,is_count_tokens,request_type,client_ip,user_agent)
 VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($7,''),NULLIF(NULLIF($8,''),$7),$9,$10,'upstream','upstream_rejected','provider','upstream',$11,$12,$9,$13,$14,$15,$16,NULLIF($17,'')::inet,$18)`,
		id, g.UserID, g.Key.ID, s.Account.ID, g.Key.GroupID, s.Account.Platform, errorModel(in.Model), errorModel(s.UpstreamModel), truncate(r.URL.Path, 256), in.Stream, fmt.Sprintf("upstream returned HTTP %d", status), status, truncate(path, 256), time.Since(started).Milliseconds(), in.CountOnly, errorRequestType(r, in), clientIP(r), truncate(r.UserAgent(), 512))
	if err != nil {
		slog.Error("upstream error record failed", "request_id", id)
	}
}

func (a *App) groupUsageSummary(w http.ResponseWriter, r *http.Request) error {
	today, _ := quotaStarts(time.Now())
	rows, err := a.DB.QueryContext(r.Context(), `SELECT to_jsonb(x) FROM (
 SELECT g.id AS group_id,COALESCE(SUM(u.actual_cost),0) AS total_cost,
 COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $1),0) AS today_cost,
 COALESCE(SUM(u.actual_cost) FILTER(WHERE u.created_at >= $2 AND u.created_at < $1),0) AS yesterday_cost
 FROM groups g LEFT JOIN usage_logs u ON u.group_id=g.id
 WHERE g.deleted_at IS NULL GROUP BY g.id ORDER BY g.id)x`, today, today.AddDate(0, 0, -1))
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, items)
}

func (a *App) groupCapacitySummary(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.operatingAccounts(r)
	if err != nil {
		return err
	}
	active, _ := a.concurrencySnapshot()
	now := time.Now()
	type capacity struct {
		GroupID int64 `json:"group_id"`
		Used    int   `json:"concurrency_used"`
		Max     int   `json:"concurrency_max"`
	}
	groups := map[int64]*capacity{}
	ids, err := a.DB.QueryContext(r.Context(), "SELECT id FROM groups WHERE deleted_at IS NULL AND status='active' ORDER BY id")
	if err != nil {
		return err
	}
	defer ids.Close()
	result := []*capacity{}
	for ids.Next() {
		c := &capacity{}
		if err := ids.Scan(&c.GroupID); err != nil {
			return err
		}
		groups[c.GroupID] = c
		result = append(result, c)
	}
	if err := ids.Err(); err != nil {
		return err
	}
	for _, acc := range rows {
		if !acc.available(now) {
			continue
		}
		for _, g := range acc.Groups {
			if c := groups[g.ID]; c != nil {
				c.Max += acc.Concurrency
				c.Used += active[fmt.Sprintf("account:%d", acc.ID)]
			}
		}
	}
	return reply(w, result)
}

func (a *App) operationalRoutes() {
	a.route("GET /api/v1/admin/ops/account-availability", "admin", a.accountAvailability)
	a.route("GET /api/v1/admin/ops/realtime-traffic", "admin", a.realtimeTraffic)
	for _, path := range []string{"/api/v1/admin/ops/errors", "/api/v1/admin/ops/request-errors", "/api/v1/admin/ops/upstream-errors"} {
		a.route("GET "+path, "admin", a.opsErrors)
		a.route("GET "+path+"/{id}", "admin", a.opsErrors)
	}
	a.route("GET /api/v1/admin/ops/request-errors/{id}/upstream-errors", "admin", a.opsErrors)
	a.route("GET /api/v1/admin/ops/ingress-rejections", "admin", a.ingressRejections)
	a.route("GET /api/v1/admin/ops/ingress-rejections/health", "admin", a.ingressHealth)
	a.route("GET /api/v1/admin/ops/auth-cache-invalidation/health", "admin", a.authCacheHealth)
	a.route("GET /api/v1/admin/groups/usage-summary", "admin", a.groupUsageSummary)
	a.route("GET /api/v1/admin/groups/capacity-summary", "admin", a.groupCapacitySummary)
}
