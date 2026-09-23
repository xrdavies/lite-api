package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func supportedPlatform(platform string) bool {
	switch platform {
	case "openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax":
		return true
	}
	return false
}

type groupInput struct {
	Name             *string              `json:"name"`
	Description      *string              `json:"description"`
	Platform         *string              `json:"platform"`
	Status           *string              `json:"status"`
	SubscriptionType *string              `json:"subscription_type"`
	Rate             *json.Number         `json:"rate_multiplier"`
	Exclusive        *bool                `json:"is_exclusive"`
	RPMLimit         *int                 `json:"rpm_limit"`
	SortOrder        *int                 `json:"sort_order"`
	Allowlist        *modelAllowlist      `json:"model_allowlist"`
	LongContext      *bool                `json:"long_context_pricing_enabled"`
	Manifest         *modelManifestConfig `json:"codex_models_manifest_config"`
}

type modelManifestConfig struct {
	Enabled  bool    `json:"enabled"`
	Accounts []int64 `json:"account_ids"`
	Fallback bool    `json:"fallback_to_scheduler"`
}

func (a *App) validateModelManifest(r *http.Request, id int64, platform string, cfg *modelManifestConfig) error {
	if cfg == nil {
		return nil
	}
	if platform != "openai" {
		*cfg = modelManifestConfig{}
		return nil
	}
	seen := map[int64]bool{}
	ids := []int64{}
	for _, id := range cfg.Accounts {
		if id > 0 && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	cfg.Accounts = ids
	if !cfg.Enabled {
		return nil
	}
	if len(ids) == 0 || len(ids) > 10 {
		return bad("enabled model manifest requires 1 to 10 accounts")
	}
	for _, aid := range ids {
		var member bool
		if err := a.DB.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE a.id=$1 AND ag.group_id=$2 AND a.platform='openai' AND a.type='apikey' AND a.status='active' AND a.deleted_at IS NULL)`, aid, id).Scan(&member); err != nil {
			return err
		}
		if !member {
			return bad("model manifest accounts must be active OpenAI members of the group")
		}
	}
	return nil
}

func (in *groupInput) validate(create bool) error {
	if create && in.Name == nil {
		return bad("name is required")
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid name")
	}
	if in.Platform != nil && !supportedPlatform(*in.Platform) && *in.Platform != "composite" {
		return bad("unsupported platform")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "inactive" {
		return bad("invalid status")
	}
	if in.SubscriptionType != nil && *in.SubscriptionType != "standard" {
		return bad("subscription groups are unsupported")
	}
	if in.Rate != nil && !validDecimal(*in.Rate, 6, 4) {
		return bad("invalid rate_multiplier")
	}
	if in.RPMLimit != nil && (*in.RPMLimit < 0 || *in.RPMLimit > 1000000) {
		return bad("invalid rpm_limit")
	}
	if in.Allowlist != nil {
		if err := in.Allowlist.validate(); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) createGroup(w http.ResponseWriter, r *http.Request) error {
	var in groupInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	platform, status, description, rate, exclusive, rpm, order := "openai", "active", "", "1", false, 0, 0
	if in.Platform != nil {
		platform = *in.Platform
	}
	if in.Status != nil {
		status = *in.Status
	}
	if in.Description != nil {
		description = *in.Description
	}
	if in.Rate != nil {
		rate = in.Rate.String()
	}
	if in.Exclusive != nil {
		exclusive = *in.Exclusive
	}
	if in.RPMLimit != nil {
		rpm = *in.RPMLimit
	}
	if in.SortOrder != nil {
		order = *in.SortOrder
	}
	allowlist := "{}"
	longContext := true
	if in.Allowlist != nil {
		b, _ := json.Marshal(in.Allowlist)
		allowlist = string(b)
	}
	if in.LongContext != nil {
		longContext = *in.LongContext
	}
	if err := a.validateModelManifest(r, 0, platform, in.Manifest); err != nil {
		return err
	}
	manifest := "{}"
	if in.Manifest != nil {
		raw, _ := json.Marshal(in.Manifest)
		manifest = string(raw)
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), `WITH created AS (INSERT INTO groups(name,description,platform,status,rate_multiplier,is_exclusive,rpm_limit,sort_order,model_allowlist,long_context_pricing_enabled,codex_models_manifest_config) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING *) SELECT to_jsonb(created)-'deleted_at' FROM created`, *in.Name, description, platform, status, rate, exclusive, rpm, order, allowlist, longContext, manifest))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) updateGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in groupInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(false); err != nil {
		return err
	}
	sets := []string{"updated_at=now()"}
	args := []any{id}
	add := func(field string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s=$%d", field, len(args)))
	}
	if in.Allowlist != nil {
		b, _ := json.Marshal(in.Allowlist)
		args = append(args, string(b))
		sets = append(sets, fmt.Sprintf("model_allowlist=model_allowlist || $%d::jsonb", len(args)))
	}
	if in.LongContext != nil {
		add("long_context_pricing_enabled", *in.LongContext)
	}
	if in.Name != nil {
		add("name", *in.Name)
	}
	if in.Description != nil {
		add("description", *in.Description)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.Rate != nil {
		add("rate_multiplier", in.Rate.String())
	}
	if in.Exclusive != nil {
		add("is_exclusive", *in.Exclusive)
	}
	if in.RPMLimit != nil {
		add("rpm_limit", *in.RPMLimit)
	}
	if in.SortOrder != nil {
		add("sort_order", *in.SortOrder)
	}
	if in.Platform != nil {
		var platform string
		if err = a.DB.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL", id).Scan(&platform); err != nil {
			return err
		}
		if platform != *in.Platform {
			return bad("platform cannot change for an existing group")
		}
	}
	if in.Manifest != nil {
		var platform string
		if err = a.DB.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL", id).Scan(&platform); err != nil {
			return err
		}
		if err = a.validateModelManifest(r, id, platform, in.Manifest); err != nil {
			return err
		}
		raw, _ := json.Marshal(in.Manifest)
		args = append(args, string(raw))
		sets = append(sets, fmt.Sprintf("codex_models_manifest_config=codex_models_manifest_config || $%d::jsonb", len(args)))
	}
	result, err := a.DB.ExecContext(r.Context(), "UPDATE groups SET "+strings.Join(sets, ",")+" WHERE id=$1 AND deleted_at IS NULL", args...)
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
	return a.getGroup(w, r)
}
func (a *App) getGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(g)-'deleted_at' FROM groups g WHERE id=$1 AND deleted_at IS NULL", id))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) listGroups(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	platform := r.URL.Query().Get("platform")
	status := r.URL.Query().Get("status")
	search := "%" + r.URL.Query().Get("search") + "%"
	where := ` WHERE deleted_at IS NULL AND ($1='' OR platform=$1) AND ($2='' OR status=$2) AND name ILIKE $3`
	if strings.HasSuffix(r.URL.Path, "/all") {
		rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(g)-'deleted_at' FROM groups g"+where+" ORDER BY sort_order,id", platform, status, search)
		if err != nil {
			return err
		}
		items, err := jsonRows(rows)
		if err != nil {
			return err
		}
		return reply(w, items)
	}
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM groups"+where, platform, status, search).Scan(&total); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(g)-'deleted_at' FROM groups g"+where+" ORDER BY sort_order,id LIMIT $4 OFFSET $5", platform, status, search, size, (page-1)*size)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) deleteGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	result, err := a.DB.ExecContext(r.Context(), "UPDATE groups SET status='inactive',deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL", id)
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
func (a *App) availableGroups(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.DB.QueryContext(r.Context(), `SELECT jsonb_build_object('id',g.id,'name',g.name,'description',g.description,'platform',g.platform,'rate_multiplier',COALESCE(m.rate_multiplier,g.rate_multiplier),'is_exclusive',g.is_exclusive,'status',g.status,'subscription_type',g.subscription_type) FROM groups g JOIN users u ON u.id=$1 LEFT JOIN user_group_rate_multipliers m ON m.user_id=u.id AND m.group_id=g.id WHERE g.deleted_at IS NULL AND g.status='active' AND g.subscription_type='standard' AND NOT g.require_oauth_only AND g.platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite') AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id)) ORDER BY g.sort_order,g.id`, current(r).ID)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, items)
}
func (a *App) groupRates(w http.ResponseWriter, r *http.Request) error {
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT COALESCE(jsonb_object_agg(group_id::text,rate_multiplier),'{}'::jsonb) FROM user_group_rate_multipliers WHERE user_id=$1 AND rate_multiplier IS NOT NULL", current(r).ID))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) replaceGroup(w http.ResponseWriter, r *http.Request) error {
	uid, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Old int64 `json:"old_group_id"`
		New int64 `json:"new_group_id"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Old <= 0 || in.New <= 0 || in.Old == in.New {
		return bad("invalid group replacement")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(r.Context(), "SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", uid).Scan(&id); err != nil {
		return err
	}
	var platform string
	if err = tx.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL AND status='active' AND subscription_type='standard' AND NOT require_oauth_only", in.New).Scan(&platform); err != nil {
		return err
	}
	if !supportedPlatform(platform) && platform != "composite" {
		return bad("unsupported platform")
	}
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING", uid, in.New); err != nil {
		return err
	}
	result, err := tx.ExecContext(r.Context(), "UPDATE api_keys SET group_id=$1,updated_at=now() WHERE user_id=$2 AND group_id=$3 AND deleted_at IS NULL", in.New, uid, in.Old)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM user_allowed_groups WHERE user_id=$1 AND group_id=$2", uid, in.Old); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]any{"migrated_keys": count})
}
func (a *App) groupRoutes() {
	a.route("GET /api/v1/admin/groups/{id}/composite-routes", "admin", a.compositeRoutes)
	a.route("POST /api/v1/admin/groups/{id}/composite-routes", "admin", a.compositeRoutes)
	a.route("POST /api/v1/admin/groups/{id}/composite-routes/preview", "admin", a.compositeRoutes)
	a.route("PUT /api/v1/admin/groups/{id}/composite-routes/{route_id}", "admin", a.compositeRoutes)
	a.route("DELETE /api/v1/admin/groups/{id}/composite-routes/{route_id}", "admin", a.compositeRoutes)
	a.route("GET /api/v1/admin/groups/{id}/rate-multipliers", "admin", a.groupOverrides)
	a.route("PUT /api/v1/admin/groups/{id}/rate-multipliers", "admin", a.saveGroupOverrides)
	a.route("DELETE /api/v1/admin/groups/{id}/rate-multipliers", "admin", a.saveGroupOverrides)
	a.route("PUT /api/v1/admin/groups/{id}/rpm-overrides", "admin", a.saveGroupOverrides)
	a.route("DELETE /api/v1/admin/groups/{id}/rpm-overrides", "admin", a.saveGroupOverrides)
	a.route("GET /api/v1/admin/groups", "admin", a.listGroups)
	a.route("GET /api/v1/admin/groups/all", "admin", a.listGroups)
	a.route("POST /api/v1/admin/groups", "admin", a.createGroup)
	a.route("GET /api/v1/admin/groups/{id}", "admin", a.getGroup)
	a.route("PUT /api/v1/admin/groups/{id}", "admin", a.updateGroup)
	a.route("DELETE /api/v1/admin/groups/{id}", "admin", a.deleteGroup)
	a.route("GET /api/v1/groups/available", "user", a.availableGroups)
	a.route("GET /api/v1/groups/rates", "user", a.groupRates)
	a.route("POST /api/v1/admin/users/{id}/replace-group", "admin", a.replaceGroup)
}
