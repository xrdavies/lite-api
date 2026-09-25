package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

type keyInput struct {
	Name           *string      `json:"name"`
	GroupID        *int64       `json:"group_id"`
	CustomKey      *string      `json:"custom_key"`
	Status         *string      `json:"status"`
	Whitelist      *[]string    `json:"ip_whitelist"`
	Blacklist      *[]string    `json:"ip_blacklist"`
	Quota          *json.Number `json:"quota"`
	Limit5h        *json.Number `json:"rate_limit_5h"`
	Limit1d        *json.Number `json:"rate_limit_1d"`
	Limit7d        *json.Number `json:"rate_limit_7d"`
	ExpiresInDays  *int         `json:"expires_in_days"`
	ExpiresAt      *string      `json:"expires_at"`
	ResetQuota     bool         `json:"reset_quota"`
	ResetRateLimit bool         `json:"reset_rate_limit_usage"`
}

func (in *keyInput) validate(create bool) error {
	if create && in.Name == nil {
		return bad("name is required")
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid name")
	}
	if in.GroupID != nil && *in.GroupID < 0 {
		return bad("invalid group_id")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "inactive" {
		return bad("invalid status")
	}
	if !create && (in.CustomKey != nil || in.ExpiresInDays != nil) {
		return bad("credential and expires_in_days cannot be edited")
	}
	if in.ExpiresInDays != nil && (*in.ExpiresInDays < 1 || *in.ExpiresInDays > 36500) {
		return bad("invalid expires_in_days")
	}
	if in.CustomKey != nil {
		key := *in.CustomKey
		if len(key) < 16 || len(key) > 128 {
			return bad("custom_key must contain 16 to 128 characters")
		}
		for _, c := range key {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
				return bad("invalid custom_key characters")
			}
		}
	}
	for _, n := range []*json.Number{in.Quota, in.Limit5h, in.Limit1d, in.Limit7d} {
		if n != nil && !validDecimal(*n, 12, 8) {
			return bad("invalid monetary limit")
		}
	}
	for _, list := range []*[]string{in.Whitelist, in.Blacklist} {
		if list == nil {
			continue
		}
		if len(*list) > 100 {
			return bad("too many IP rules")
		}
		for _, rule := range *list {
			if _, err := netip.ParseAddr(rule); err != nil {
				if _, err = netip.ParsePrefix(rule); err != nil {
					return bad("invalid IP or CIDR rule")
				}
			}
		}
	}
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, *in.ExpiresAt); err != nil {
			return bad("invalid expires_at")
		}
	}
	return nil
}
func (a *App) groupAccess(ctx context.Context, q queryer, uid, gid int64) error {
	if gid == 0 {
		return nil
	}
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups g JOIN users u ON u.id=$1 WHERE g.id=$2 AND g.deleted_at IS NULL AND g.status='active' AND g.subscription_type='standard' AND NOT g.require_oauth_only AND g.platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite') AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id)))`, uid, gid).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return denied()
	}
	return nil
}

// Derive effective windows without changing their stored accounting counters.
const keyView = `to_jsonb(k)-'deleted_at' || jsonb_build_object(
 'last_used_ip',(SELECT ip_address FROM usage_logs WHERE api_key_id=k.id AND ip_address IS NOT NULL AND ip_address<>'' ORDER BY created_at DESC,id DESC LIMIT 1),
 'usage_5h',CASE WHEN k.window_5h_start+interval '5 hours'>now() THEN k.usage_5h ELSE 0 END,
 'usage_1d',CASE WHEN k.window_1d_start+interval '24 hours'>now() THEN k.usage_1d ELSE 0 END,
 'usage_7d',CASE WHEN k.window_7d_start+interval '168 hours'>now() THEN k.usage_7d ELSE 0 END)
 || CASE WHEN k.window_5h_start+interval '5 hours'>now() THEN jsonb_build_object('reset_5h_at',k.window_5h_start+interval '5 hours') ELSE '{}'::jsonb END
 || CASE WHEN k.window_1d_start+interval '24 hours'>now() THEN jsonb_build_object('reset_1d_at',k.window_1d_start+interval '24 hours') ELSE '{}'::jsonb END
 || CASE WHEN k.window_7d_start+interval '168 hours'>now() THEN jsonb_build_object('reset_7d_at',k.window_7d_start+interval '168 hours') ELSE '{}'::jsonb END`

func keyRelations(admin bool) string {
	visibility := `g.status='active' AND g.subscription_type='standard' AND NOT g.require_oauth_only
 AND g.platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite')
 AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id))`
	if admin {
		visibility = "true"
	}
	return `jsonb_build_object('group',(SELECT ` + publicGroupView + ` FROM groups g JOIN users u ON u.id=k.user_id
 WHERE g.id=k.group_id AND g.deleted_at IS NULL AND ` + visibility + `),
 'user',(SELECT ` + userView(false) + ` FROM users u WHERE u.id=k.user_id AND u.deleted_at IS NULL))`
}

func (a *App) keyJSON(ctx context.Context, q queryer, id, uid int64) (json.RawMessage, error) {
	a.gatewayMu.Lock()
	count := a.gatewayActive[fmt.Sprintf("key:%d", id)]
	a.gatewayMu.Unlock()
	return jsonRow(q.QueryRowContext(ctx, "SELECT "+keyView+" || "+keyRelations(uid == 0)+" || jsonb_build_object('current_concurrency',$3::int) FROM api_keys k WHERE id=$1 AND ($2::bigint=0 OR user_id=$2) AND deleted_at IS NULL", id, uid, count))
}
func (a *App) createKey(w http.ResponseWriter, r *http.Request) error {
	var in keyInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	uid := current(r).ID
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	finish, replayed, err := writeIdempotency(w, r, tx, "user.api_keys.create", fmt.Sprintf("user:%d", uid), in)
	if err != nil || replayed {
		return err
	}
	var gid any
	if in.GroupID != nil && *in.GroupID != 0 {
		if err := a.groupAccess(r.Context(), tx, uid, *in.GroupID); err != nil {
			return err
		}
		gid = *in.GroupID
	}
	key := "sk-" + hex.EncodeToString(randomBytes(32))
	if in.CustomKey != nil {
		key = *in.CustomKey
	}
	whitelist, blacklist := "[]", "[]"
	if in.Whitelist != nil {
		b, _ := json.Marshal(in.Whitelist)
		whitelist = string(b)
	}
	if in.Blacklist != nil {
		b, _ := json.Marshal(in.Blacklist)
		blacklist = string(b)
	}
	values := []string{"0", "0", "0", "0"}
	for i, n := range []*json.Number{in.Quota, in.Limit5h, in.Limit1d, in.Limit7d} {
		if n != nil {
			values[i] = n.String()
		}
	}
	var expires any
	if in.ExpiresInDays != nil {
		expires = time.Now().AddDate(0, 0, *in.ExpiresInDays)
	}
	if in.ExpiresAt != nil && *in.ExpiresAt != "" {
		expires, _ = time.Parse(time.RFC3339, *in.ExpiresAt)
	}
	status := "active"
	if in.Status != nil {
		status = *in.Status
	}
	var id int64
	err = tx.QueryRowContext(r.Context(), `INSERT INTO api_keys(user_id,key,name,group_id,status,ip_whitelist,ip_blacklist,quota,rate_limit_5h,rate_limit_1d,rate_limit_7d,expires_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,$9,$10,$11,$12) RETURNING id`, uid, key, *in.Name, gid, status, whitelist, blacklist, values[0], values[1], values[2], values[3], expires).Scan(&id)
	if err != nil {
		return err
	}
	data, err := a.keyJSON(r.Context(), tx, id, uid)
	if err != nil {
		return err
	}
	if finish != nil {
		if err = finish(data); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, data)
}
func (a *App) getKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	data, err := a.keyJSON(r.Context(), a.DB, id, current(r).ID)
	if err != nil {
		return err
	}
	return reply(w, data)
}
func (a *App) listKeys(w http.ResponseWriter, r *http.Request) error {
	return a.keysFor(w, r, current(r).ID, 0)
}
func (a *App) keysFor(w http.ResponseWriter, r *http.Request, uid, gid int64) error {
	page, size := pagination(r)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if len(search) > 100 {
		return bad("search is too long")
	}
	search = "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
	status := r.URL.Query().Get("status")
	var group any
	if raw := r.URL.Query().Get("group_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 0 {
			return bad("invalid group_id")
		}
		group = id
	}
	field := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort_by")))
	switch field {
	case "":
		field = "created_at"
	case "id", "name", "status", "created_at", "expires_at", "last_used_at":
	case "current_concurrency":
		field = "COALESCE(($6::jsonb->>k.id::text)::int,0)"
	default:
		field = "id"
	}
	direction := " DESC"
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("sort_order")), "asc") {
		direction = " ASC"
	}
	order := field + direction
	if field != "id" {
		order += ",id" + direction
	}
	where := ` WHERE deleted_at IS NULL AND ($1::bigint=0 OR user_id=$1) AND ($2::bigint=0 OR group_id=$2)
 AND (name ILIKE $3 OR key ILIKE $3) AND ($4='' OR status=$4)
 AND ($5::bigint IS NULL OR group_id=$5 OR ($5=0 AND group_id IS NULL))`
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM api_keys"+where, uid, gid, search, status, group).Scan(&total); err != nil {
		return err
	}
	counts := map[string]int{}
	a.gatewayMu.Lock()
	for key, count := range a.gatewayActive {
		if strings.HasPrefix(key, "key:") {
			counts[strings.TrimPrefix(key, "key:")] = count
		}
	}
	a.gatewayMu.Unlock()
	snapshot, _ := json.Marshal(counts)
	rows, err := a.DB.QueryContext(r.Context(), "SELECT "+keyView+" || "+keyRelations(strings.HasPrefix(r.URL.Path, "/api/v1/admin/"))+" || jsonb_build_object('current_concurrency',COALESCE(($6::jsonb->>k.id::text)::int,0)) FROM api_keys k"+where+" ORDER BY "+order+" LIMIT $7 OFFSET $8", uid, gid, search, status, group, string(snapshot), size, (page-1)*size)
	if err != nil {
		return err
	}
	data, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, data, total, page, size)
}
func (a *App) updateKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in keyInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(false); err != nil {
		return err
	}
	if in.GroupID != nil {
		if err = a.groupAccess(r.Context(), a.DB, current(r).ID, *in.GroupID); err != nil {
			return err
		}
	}
	sets := []string{"updated_at=now()"}
	args := []any{id, current(r).ID}
	add := func(field string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s=$%d", field, len(args)))
	}
	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.GroupID != nil {
		var gid any
		if *in.GroupID > 0 {
			gid = *in.GroupID
		}
		add("group_id", gid)
	}
	for i, field := range []string{"quota", "rate_limit_5h", "rate_limit_1d", "rate_limit_7d"} {
		n := []*json.Number{in.Quota, in.Limit5h, in.Limit1d, in.Limit7d}[i]
		if n != nil {
			add(field, n.String())
		}
	}
	for i, field := range []string{"ip_whitelist", "ip_blacklist"} {
		list := []*[]string{in.Whitelist, in.Blacklist}[i]
		if list != nil {
			b, _ := json.Marshal(list)
			add(field, string(b))
		}
	}
	if in.ExpiresAt != nil {
		var expiry any
		if *in.ExpiresAt != "" {
			expiry, _ = time.Parse(time.RFC3339, *in.ExpiresAt)
		}
		add("expires_at", expiry)
	}
	if in.ResetQuota {
		sets = append(sets, "quota_used=0")
	}
	if in.Status == nil {
		// SQL evaluates all assignments against the current row. Combine status
		// recovery once, without overwriting concurrent quota increments.
		var recover []string
		if in.ResetQuota {
			recover = append(recover, "status='quota_exhausted'")
		} else if in.Quota != nil {
			args = append(args, in.Quota.String())
			recover = append(recover, fmt.Sprintf("(status='quota_exhausted' AND ($%d::numeric=0 OR $%d::numeric>quota_used))", len(args), len(args)))
		}
		if in.ExpiresAt != nil {
			if *in.ExpiresAt == "" {
				recover = append(recover, "status='expired'")
			} else {
				args = append(args, *in.ExpiresAt)
				recover = append(recover, fmt.Sprintf("(status='expired' AND $%d::timestamptz>now())", len(args)))
			}
		}
		if len(recover) > 0 {
			sets = append(sets, "status=CASE WHEN "+strings.Join(recover, " OR ")+" THEN 'active' ELSE status END")
		}
	}
	if in.ResetRateLimit {
		sets = append(sets, resetKeyWindows)
	}
	// Configuration updates never write usage counters unless explicitly reset.
	result, err := a.DB.ExecContext(r.Context(), "UPDATE api_keys SET "+strings.Join(sets, ",")+" WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL", args...)
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
	return a.getKey(w, r)
}

const resetKeyWindows = "usage_5h=0,usage_1d=0,usage_7d=0,window_5h_start=NULL,window_1d_start=NULL,window_7d_start=NULL"

func (a *App) deleteKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	result, err := a.DB.ExecContext(r.Context(), "UPDATE api_keys SET deleted_at=now(),status='inactive',updated_at=now() WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL", id, current(r).ID)
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
	return reply(w, map[string]bool{"deleted": true})
}
func (a *App) adminKey(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		GroupID *int64 `json:"group_id"`
		Reset   bool   `json:"reset_rate_limit_usage"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.GroupID != nil && *in.GroupID < 0 {
		return bad("invalid group_id")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var uid int64
	if err = tx.QueryRowContext(r.Context(), "SELECT user_id FROM api_keys WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&uid); err != nil {
		return err
	}
	granted := false
	if in.GroupID != nil {
		var gid any
		if *in.GroupID > 0 {
			var ok bool
			if err = tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1 AND deleted_at IS NULL AND status='active' AND subscription_type='standard' AND NOT require_oauth_only AND platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite'))", *in.GroupID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return bad("group unavailable")
			}
			if err = a.groupAccess(r.Context(), tx, uid, *in.GroupID); err != nil {
				if _, yes := err.(*apiError); !yes {
					return err
				}
				if _, err = tx.ExecContext(r.Context(), "INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING", uid, *in.GroupID); err != nil {
					return err
				}
				granted = true
			}
			gid = *in.GroupID
		}
		if _, err = tx.ExecContext(r.Context(), "UPDATE api_keys SET group_id=$1,updated_at=now() WHERE id=$2", gid, id); err != nil {
			return err
		}
	}
	if in.Reset {
		if _, err = tx.ExecContext(r.Context(), "UPDATE api_keys SET "+resetKeyWindows+",updated_at=now() WHERE id=$1", id); err != nil {
			return err
		}
	}
	data, err := a.keyJSON(r.Context(), tx, id, 0)
	if err != nil {
		return err
	}
	result := map[string]any{"api_key": data, "auto_granted_group_access": granted}
	if granted {
		var key struct {
			Group struct{ Name string }
		}
		if err = json.Unmarshal(data, &key); err != nil {
			return err
		}
		result["granted_group_id"], result["granted_group_name"] = *in.GroupID, key.Group.Name
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, result)
}
func (a *App) keyRoutes() {
	a.route("GET /api/v1/keys", "user", a.listKeys)
	a.route("POST /api/v1/keys", "user", a.createKey)
	a.route("GET /api/v1/keys/{id}", "user", a.getKey)
	a.route("PUT /api/v1/keys/{id}", "user", a.updateKey)
	a.route("DELETE /api/v1/keys/{id}", "user", a.deleteKey)
	a.route("PUT /api/v1/admin/api-keys/{id}", "admin", a.adminKey)
	a.route("GET /api/v1/admin/users/{id}/api-keys", "admin", func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return a.keysFor(w, r, id, 0)
	})
	a.route("GET /api/v1/admin/groups/{id}/api-keys", "admin", func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return a.keysFor(w, r, 0, id)
	})
}
