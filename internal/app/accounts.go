package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type accountInput struct {
	Name        *string                    `json:"name"`
	Platform    *string                    `json:"platform"`
	Type        *string                    `json:"type"`
	Notes       *string                    `json:"notes"`
	Status      *string                    `json:"status"`
	Credentials map[string]json.RawMessage `json:"credentials"`
	Extra       map[string]json.RawMessage `json:"extra"`
	ProxyID     *int64                     `json:"proxy_id"`
	Concurrency *int                       `json:"concurrency"`
	Priority    *int                       `json:"priority"`
	Rate        *json.Number               `json:"rate_multiplier"`
	LoadFactor  *int                       `json:"load_factor"`
	Groups      *[]int64                   `json:"group_ids"`
	ExpiresAt   *int64                     `json:"expires_at"`
	AutoPause   *bool                      `json:"auto_pause_on_expired"`
}

func (in *accountInput) validate(create bool) error {
	if create && (in.Name == nil || in.Platform == nil || in.Type == nil || in.Credentials == nil) {
		return bad("name, platform, type and credentials are required")
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid account name")
	}
	if in.Platform != nil && !supportedPlatform(*in.Platform) {
		return bad("unsupported platform")
	}
	if in.Type != nil && *in.Type != "apikey" {
		return bad("only API key accounts are supported")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "inactive" && *in.Status != "error" {
		return bad("invalid account status")
	}
	if in.Concurrency != nil && (*in.Concurrency < 1 || *in.Concurrency > 10000) {
		return bad("invalid concurrency")
	}
	if in.Priority != nil && (*in.Priority < 0 || *in.Priority > 1000000) {
		return bad("invalid priority")
	}
	if in.LoadFactor != nil && (*in.LoadFactor < 1 || *in.LoadFactor > 10000) {
		return bad("invalid load_factor")
	}
	if in.ProxyID != nil && *in.ProxyID < 0 {
		return bad("invalid proxy_id")
	}
	if in.Rate != nil && !validDecimal(*in.Rate, 6, 4) {
		return bad("invalid rate_multiplier")
	}
	if in.ExpiresAt != nil && (*in.ExpiresAt < 0 || *in.ExpiresAt > 253402300799) {
		return bad("invalid expiry")
	}
	for key, value := range in.Credentials {
		switch key {
		case "api_key":
			var s string
			if json.Unmarshal(value, &s) != nil || s == "" || len(s) > 8192 || strings.ContainsAny(s, "\r\n") {
				return bad("invalid upstream API key")
			}
		case "base_url":
			var s string
			if json.Unmarshal(value, &s) != nil {
				return bad("invalid base_url")
			}
			if s != "" {
				if _, err := parseUpstreamURL(s); err != nil {
					return err
				}
			}
		case "account_mode":
			if credentialString(in.Credentials, key) != "payg" {
				return bad("only pay-as-you-go accounts are supported")
			}
		case "api_protocol":
			switch credentialString(in.Credentials, key) {
			case "chat_completions", "responses", "anthropic":
			default:
				return bad("unsupported API protocol")
			}
		case "model_mapping":
			var mapping map[string]string
			if json.Unmarshal(value, &mapping) != nil || mapping == nil || len(mapping) > 10000 {
				return bad("invalid model_mapping")
			}
			for k, v := range mapping {
				if k == "" || len(k) > 200 || len(v) > 200 || strings.ContainsAny(k+v, "\r\n") {
					return bad("invalid model mapping entry")
				}
			}
		default:
			return bad("unsupported credential field: " + key)
		}
	}
	for key, value := range in.Extra {
		switch key {
		case "quota_daily_reset_mode", "quota_weekly_reset_mode":
			mode := credentialString(in.Extra, key)
			if mode != "rolling" && mode != "fixed" {
				return bad("invalid quota reset mode")
			}
		case "quota_reset_timezone":
			zone := credentialString(in.Extra, key)
			if zone == "" || zone == "Local" {
				return bad("invalid quota timezone")
			}
			if _, err := time.LoadLocation(zone); err != nil {
				return bad("invalid quota timezone")
			}
		case "quota_daily_reset_hour", "quota_weekly_reset_hour", "quota_weekly_reset_day":
			var n int
			if json.Unmarshal(value, &n) != nil || n < 0 || n > 23 || key == "quota_weekly_reset_day" && n > 6 {
				return bad("invalid quota reset schedule")
			}
		case "quota_limit", "quota_daily_limit", "quota_weekly_limit":
			var n json.Number
			if json.Unmarshal(value, &n) != nil || !validDecimal(n, 12, 8) {
				return bad("invalid account quota")
			}
		default:
			return bad("unsupported account setting: " + key)
		}
	}
	return nil
}
func accountJSON(ctx context.Context, q queryer, id int64) (json.RawMessage, error) {
	return jsonRow(q.QueryRowContext(ctx, `SELECT (to_jsonb(a)-'deleted_at'-'credentials') || jsonb_build_object('credentials',jsonb_strip_nulls(jsonb_build_object('base_url',credentials->'base_url','account_mode',credentials->'account_mode','api_protocol',credentials->'api_protocol','model_mapping',credentials->'model_mapping')),'has_api_key',credentials ? 'api_key','group_ids',COALESCE((SELECT jsonb_agg(group_id ORDER BY group_id) FROM account_groups WHERE account_id=a.id),'[]'::jsonb)) FROM accounts a WHERE id=$1 AND deleted_at IS NULL`, id))
}
func setAccountGroups(ctx context.Context, tx *sql.Tx, id int64, platform string, groups []int64, priority int) error {
	if len(groups) > 1000 {
		return bad("too many groups")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id=$1", id); err != nil {
		return err
	}
	for _, gid := range groups {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1 AND deleted_at IS NULL AND platform=$2 AND subscription_type='standard' AND NOT require_oauth_only)`, gid, platform).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return bad("account and group platforms must match")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO account_groups(account_id,group_id,priority) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", id, gid, priority); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) createAccount(w http.ResponseWriter, r *http.Request) error {
	var in accountInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	if credentialString(in.Credentials, "api_key") == "" {
		return bad("credentials.api_key is required")
	}
	u := &upstreamAccount{Platform: *in.Platform, Credentials: in.Credentials}
	base, err := u.baseURL()
	if err != nil {
		return err
	}
	if err = a.validateUpstream(r.Context(), base); err != nil {
		return err
	}
	var proxy, expiry, load any
	if in.ProxyID != nil && *in.ProxyID > 0 {
		if _, err = a.resolveProxy(r.Context(), *in.ProxyID, map[int64]bool{}); err != nil {
			return err
		}
		proxy = *in.ProxyID
	}
	if in.ExpiresAt != nil && *in.ExpiresAt > 0 {
		expiry = time.Unix(*in.ExpiresAt, 0)
	}
	if in.LoadFactor != nil {
		load = *in.LoadFactor
	}
	concurrency, priority, rate, status, notes, pause := 3, 50, "1", "active", "", true
	if in.Concurrency != nil {
		concurrency = *in.Concurrency
	}
	if in.Priority != nil {
		priority = *in.Priority
	}
	if in.Rate != nil {
		rate = in.Rate.String()
	}
	if in.Status != nil {
		status = *in.Status
	}
	if in.Notes != nil {
		notes = *in.Notes
	}
	if in.AutoPause != nil {
		pause = *in.AutoPause
	}
	credentials, _ := json.Marshal(in.Credentials)
	extra := []byte("{}")
	if in.Extra != nil {
		extra, _ = json.Marshal(in.Extra)
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateProxyAssignment(r.Context(), tx, in.ProxyID); err != nil {
		return err
	}
	var id int64
	err = tx.QueryRowContext(r.Context(), `INSERT INTO accounts(name,platform,type,credentials,extra,proxy_id,concurrency,priority,rate_multiplier,status,notes,expires_at,auto_pause_on_expired,load_factor) VALUES($1,$2,'apikey',$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) RETURNING id`, *in.Name, *in.Platform, string(credentials), string(extra), proxy, concurrency, priority, rate, status, notes, expiry, pause, load).Scan(&id)
	if err != nil {
		return err
	}
	if in.Groups != nil {
		if err = setAccountGroups(r.Context(), tx, id, *in.Platform, *in.Groups, priority); err != nil {
			return err
		}
	}
	raw, err := accountJSON(r.Context(), tx, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) updateAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in accountInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(false); err != nil {
		return err
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	if in.Platform != nil && *in.Platform != u.Platform {
		return bad("account platform is immutable")
	}
	oldTarget := responseTarget(u)
	for key, value := range in.Credentials {
		u.Credentials[key] = value
	}
	if len(in.Credentials) > 0 {
		base, err := u.baseURL()
		if err != nil {
			return err
		}
		if err = a.validateUpstream(r.Context(), base); err != nil {
			return err
		}
	}
	if in.ProxyID != nil && *in.ProxyID > 0 {
		if _, err = a.resolveProxy(r.Context(), *in.ProxyID, map[int64]bool{}); err != nil {
			return err
		}
	}
	sets := []string{"updated_at=now()"}
	args := []any{id}
	add := func(field string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s=$%d", field, len(args)))
	}
	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.Notes != nil {
		add("notes", *in.Notes)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.Concurrency != nil {
		add("concurrency", *in.Concurrency)
	}
	if in.Priority != nil {
		add("priority", *in.Priority)
	}
	if in.Rate != nil {
		add("rate_multiplier", in.Rate.String())
	}
	if in.LoadFactor != nil {
		add("load_factor", *in.LoadFactor)
	}
	if in.AutoPause != nil {
		add("auto_pause_on_expired", *in.AutoPause)
	}
	if in.ProxyID != nil {
		var proxy any
		if *in.ProxyID > 0 {
			proxy = *in.ProxyID
		}
		add("proxy_id", proxy)
	}
	if in.ExpiresAt != nil {
		var expiry any
		if *in.ExpiresAt > 0 {
			expiry = time.Unix(*in.ExpiresAt, 0)
		}
		add("expires_at", expiry)
	}
	// Merge only submitted JSON fields atomically; never round-trip consumption counters.
	for _, entry := range []struct {
		name  string
		value map[string]json.RawMessage
	}{{"credentials", in.Credentials}, {"extra", in.Extra}} {
		if entry.value != nil {
			b, _ := json.Marshal(entry.value)
			args = append(args, string(b))
			expression := fmt.Sprintf("%s || $%d::jsonb", entry.name, len(args))
			if entry.name == "extra" && oldTarget != responseTarget(u) {
				expression = "(" + expression + ") - 'upstream_model_metadata'"
			}
			sets = append(sets, entry.name+"="+expression)
		}
	}
	if oldTarget != responseTarget(u) && in.Extra == nil {
		// Capability snapshots belong to the previous upstream credential/root.
		// Configuration patches cannot retain them after the upstream changes.
		sets = append(sets, "extra=extra - 'upstream_model_metadata'")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = validateProxyAssignment(r.Context(), tx, in.ProxyID); err != nil {
		return err
	}
	var priority int
	if err = tx.QueryRowContext(r.Context(), "UPDATE accounts SET "+strings.Join(sets, ",")+" WHERE id=$1 AND deleted_at IS NULL RETURNING priority", args...).Scan(&priority); err != nil {
		return err
	}
	if in.Groups != nil {
		if err = setAccountGroups(r.Context(), tx, id, u.Platform, *in.Groups, priority); err != nil {
			return err
		}
	}
	raw, err := accountJSON(r.Context(), tx, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) getAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := accountJSON(r.Context(), a.DB, id)
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) listAccounts(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	search, platform, status := "%"+r.URL.Query().Get("search")+"%", r.URL.Query().Get("platform"), r.URL.Query().Get("status")
	where := ` WHERE deleted_at IS NULL AND name ILIKE $1 AND ($2='' OR platform=$2) AND ($3='' OR status=$3)`
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM accounts"+where, search, platform, status).Scan(&total); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT id FROM accounts"+where+" ORDER BY priority,id LIMIT $4 OFFSET $5", search, platform, status, size, (page-1)*size)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	items := []json.RawMessage{}
	for _, id := range ids {
		raw, err := accountJSON(r.Context(), a.DB, id)
		if err != nil {
			return err
		}
		items = append(items, raw)
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) deleteAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(r.Context(), "UPDATE accounts SET deleted_at=now(),status='inactive',schedulable=false,updated_at=now() WHERE id=$1 AND deleted_at IS NULL", id)
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
	if _, err = tx.ExecContext(r.Context(), "UPDATE scheduled_test_plans SET enabled=false,updated_at=now() WHERE account_id=$1", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM account_groups WHERE account_id=$1", id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]bool{"deleted": true})
}
func (a *App) accountState(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if _, err = a.loadAccount(r.Context(), id); err != nil {
		return err
	}
	var clause string
	switch {
	case strings.HasSuffix(r.URL.Path, "/schedulable"):
		var in struct {
			Schedulable *bool `json:"schedulable"`
		}
		if err = decode(w, r, &in); err != nil {
			return err
		}
		if in.Schedulable == nil {
			return bad("schedulable is required")
		}
		if _, err = a.DB.ExecContext(r.Context(), "UPDATE accounts SET schedulable=$1,updated_at=now() WHERE id=$2", *in.Schedulable, id); err != nil {
			return err
		}
		return a.getAccount(w, r)
	case strings.HasSuffix(r.URL.Path, "/temp-unschedulable") && r.Method == "GET":
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT jsonb_build_object('temp_unschedulable_until',temp_unschedulable_until,'temp_unschedulable_reason',temp_unschedulable_reason) FROM accounts WHERE id=$1", id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	case strings.HasSuffix(r.URL.Path, "/temp-unschedulable"):
		clause = "temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,extra=extra - 'model_rate_limits'"
	case strings.HasSuffix(r.URL.Path, "/clear-rate-limit"):
		clause = "rate_limited_at=NULL,rate_limit_reset_at=NULL,overload_until=NULL,temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,extra=extra - 'model_rate_limits'"
	case strings.HasSuffix(r.URL.Path, "/reset-quota"):
		clause = `extra=(extra || '{"quota_used":0,"quota_daily_used":0,"quota_weekly_used":0}'::jsonb) - 'quota_daily_start' - 'quota_weekly_start' - 'quota_daily_reset_at' - 'quota_weekly_reset_at'`
	case strings.HasSuffix(r.URL.Path, "/clear-error"):
		clause = "status=CASE WHEN status='error' THEN 'active' ELSE status END,error_message=NULL"
	default:
		clause = "status=CASE WHEN status='error' THEN 'active' ELSE status END,error_message=NULL,rate_limited_at=NULL,rate_limit_reset_at=NULL,overload_until=NULL,temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,extra=extra - 'model_rate_limits'"
	}
	if _, err = a.DB.ExecContext(r.Context(), "UPDATE accounts SET "+clause+",updated_at=now() WHERE id=$1 AND deleted_at IS NULL", id); err != nil {
		return err
	}
	return a.getAccount(w, r)
}
func (a *App) accountRoutes() {
	a.route("GET /api/v1/admin/accounts", "admin", a.listAccounts)
	a.route("POST /api/v1/admin/accounts", "admin", a.createAccount)
	a.route("GET /api/v1/admin/accounts/{id}", "admin", a.getAccount)
	a.route("PUT /api/v1/admin/accounts/{id}", "admin", a.updateAccount)
	a.route("DELETE /api/v1/admin/accounts/{id}", "admin", a.deleteAccount)
	for _, path := range []string{"recover-state", "clear-error", "clear-rate-limit", "reset-quota", "schedulable"} {
		a.route("POST /api/v1/admin/accounts/{id}/"+path, "admin", a.accountState)
	}
	a.route("GET /api/v1/admin/accounts/{id}/temp-unschedulable", "admin", a.accountState)
	a.route("DELETE /api/v1/admin/accounts/{id}/temp-unschedulable", "admin", a.accountState)
	a.route("POST /api/v1/admin/accounts/{id}/test", "admin", a.testAccount)
	a.route("GET /api/v1/admin/accounts/{id}/models", "admin", a.accountModels)
}

func validateProxyAssignment(ctx context.Context, tx *sql.Tx, id *int64) error {
	if id == nil || *id == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(720034)"); err != nil {
		return err
	}
	var ok bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM proxies WHERE id=$1 AND deleted_at IS NULL AND status='active')", *id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return bad("proxy is unavailable")
	}
	return nil
}
