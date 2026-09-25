package app

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func supportedPlatform(platform string) bool {
	switch platform {
	case "openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax":
		return true
	}
	return false
}

// Keep the user-facing projection explicit: account routing and administrator
// pricing policies must not become public when the table gains a column.
const publicGroupView = `(SELECT to_jsonb(visible) FROM (SELECT
 g.id,g.name,COALESCE(g.description,'') AS description,g.platform,g.rate_multiplier,g.is_exclusive,g.status,
 g.subscription_type,g.daily_limit_usd,g.weekly_limit_usd,g.monthly_limit_usd,g.long_context_pricing_enabled,
 g.allow_image_generation,g.allow_batch_image_generation,g.image_rate_independent,g.image_rate_multiplier,
 g.image_price_1k,g.image_price_2k,g.image_price_4k,g.batch_image_discount_multiplier,g.batch_image_hold_multiplier,
 g.video_rate_independent,g.video_rate_multiplier,g.video_price_480p,g.video_price_720p,g.video_price_1080p,g.video_model_prices,
 g.web_search_price_per_call,g.search_price_per_1k,g.audio_realtime_price_per_min,g.audio_tts_price_per_million_chars,g.audio_stt_price_per_hour,
 g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier,g.claude_code_only,g.fallback_group_id,g.fallback_group_id_on_invalid_request,
 g.allow_messages_dispatch,g.rpm_limit,g.max_reasoning_effort,g.max_reasoning_effort_over_limit,g.reasoning_effort_mappings,g.created_at,g.updated_at
 ) visible)`

type groupInput struct {
	audioPrices
	videoPrices
	batchGroupInput
	fastGroupInput
	messagesDispatchInput
	profitInput
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
	Pricing          *[]modelPrice        `json:"model_pricing"`
	ModelRouting     *map[string][]int64  `json:"model_routing"`
	RoutingEnabled   *bool                `json:"model_routing_enabled"`
	MaxEffort        *string              `json:"max_reasoning_effort"`
	OverLimit        *string              `json:"max_reasoning_effort_over_limit"`
	EffortMappings   *[]effortMapping     `json:"reasoning_effort_mappings"`
	ClaudeCodeOnly   *bool                `json:"claude_code_only"`
	FallbackGroupID  *int64               `json:"fallback_group_id"`
	WebSearchPrice   *json.Number         `json:"web_search_price_per_call"`
	SearchPrice      *json.Number         `json:"search_price_per_1k"`
	AllowImage       *bool                `json:"allow_image_generation"`
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
	if in.SearchPrice != nil && !validPrice(in.SearchPrice, 12, 8) {
		value := json.Number(strings.TrimPrefix(in.SearchPrice.String(), "-"))
		if !strings.HasPrefix(in.SearchPrice.String(), "-") || !validPrice(&value, 12, 8) {
			return bad("invalid search price per thousand calls")
		}
	}
	if in.WebSearchPrice != nil && !validPrice(in.WebSearchPrice, 12, 8) {
		// A negative value clears an optional group price; null/omission preserves it.
		value := json.Number(strings.TrimPrefix(in.WebSearchPrice.String(), "-"))
		if !strings.HasPrefix(in.WebSearchPrice.String(), "-") || !validPrice(&value, 12, 8) {
			return bad("invalid web search price")
		}
	}
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
	if in.ModelRouting != nil {
		if err := validateModelRouting(*in.ModelRouting); err != nil {
			return err
		}
	}
	return nil
}

func validateModelRouting(routes map[string][]int64) error {
	if len(routes) > 1000 {
		return bad("too many model routing rules")
	}
	count := 0
	for pattern, ids := range routes {
		count += len(ids)
		if !validModelPattern(pattern) || !validIDs(ids) || count > 10000 {
			return bad("invalid model routing pattern or account IDs")
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
	if err := in.validateReasoning(platform); err != nil {
		return err
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
	pricing := "[]"
	if in.Pricing != nil {
		if err := validateGroupPrices(platform, *in.Pricing); err != nil {
			return err
		}
		b, _ := json.Marshal(in.Pricing)
		pricing = string(b)
	}
	manifest := "{}"
	if in.Manifest != nil {
		raw, _ := json.Marshal(in.Manifest)
		manifest = string(raw)
	}
	routing, routingEnabled := "{}", false
	if in.ModelRouting != nil {
		raw, _ := json.Marshal(in.ModelRouting)
		routing = string(raw)
	}
	if in.RoutingEnabled != nil {
		routingEnabled = *in.RoutingEnabled
	}
	maxEffort, overLimit, effortMappings := "", "downgrade", "[]"
	if in.MaxEffort != nil {
		maxEffort = *in.MaxEffort
	}
	if in.OverLimit != nil {
		overLimit = *in.OverLimit
	}
	if in.EffortMappings != nil {
		raw, _ := json.Marshal(in.EffortMappings)
		effortMappings = string(raw)
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720035)"); err != nil {
		return err
	}
	codeOnly := in.ClaudeCodeOnly != nil && *in.ClaudeCodeOnly
	allowImage := in.AllowImage != nil && *in.AllowImage
	var searchPrice any
	var searchPricePerThousand any
	if in.SearchPrice != nil && rat(*in.SearchPrice).Sign() >= 0 {
		searchPricePerThousand = in.SearchPrice.String()
	}
	if in.WebSearchPrice != nil && rat(*in.WebSearchPrice).Sign() >= 0 {
		searchPrice = in.WebSearchPrice.String()
	}
	raw, err := jsonRow(tx.QueryRowContext(r.Context(), `WITH created AS (INSERT INTO groups(name,description,platform,status,rate_multiplier,is_exclusive,rpm_limit,sort_order,model_allowlist,long_context_pricing_enabled,codex_models_manifest_config,model_pricing,model_routing,model_routing_enabled,max_reasoning_effort,max_reasoning_effort_over_limit,reasoning_effort_mappings,claude_code_only,fallback_group_id,web_search_price_per_call,search_price_per_1k,allow_image_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,CASE WHEN $19::bigint>0 THEN $19 ELSE NULL END,$20,$21,$22) RETURNING *) SELECT to_jsonb(created)-'deleted_at' FROM created`, *in.Name, description, platform, status, rate, exclusive, rpm, order, allowlist, longContext, manifest, pricing, routing, routingEnabled, maxEffort, overLimit, effortMappings, codeOnly, in.FallbackGroupID, searchPrice, searchPricePerThousand, allowImage))
	if err != nil {
		return err
	}
	var created struct{ ID int64 }
	if err = json.Unmarshal(raw, &created); err != nil {
		return err
	}
	if err = in.audioPrices.apply(r.Context(), tx, created.ID); err != nil {
		return err
	}
	if err = in.videoPrices.apply(r.Context(), tx, created.ID); err != nil {
		return err
	}
	if err = in.batchGroupInput.apply(r.Context(), tx, created.ID); err != nil {
		return err
	}
	if err = in.fastGroupInput.apply(r.Context(), tx, created.ID); err != nil {
		return err
	}
	if err = in.messagesDispatchInput.apply(r.Context(), tx, created.ID); err != nil {
		return err
	}
	if err = in.profitInput.apply(r.Context(), tx, created.ID, true); err != nil {
		return err
	}
	raw, err = jsonRow(tx.QueryRowContext(r.Context(), "SELECT "+adminGroupView+adminGroupFrom+" WHERE g.id=$1", created.ID))
	if err != nil {
		return err
	}
	if err = validateGroupFallback(r.Context(), tx, created.ID, true); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
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
	if in.WebSearchPrice != nil {
		var price any
		if rat(*in.WebSearchPrice).Sign() >= 0 {
			price = in.WebSearchPrice.String()
		}
		add("web_search_price_per_call", price)
	}
	if in.SearchPrice != nil {
		var price any
		if rat(*in.SearchPrice).Sign() >= 0 {
			price = in.SearchPrice.String()
		}
		add("search_price_per_1k", price)
	}
	if in.AllowImage != nil {
		add("allow_image_generation", *in.AllowImage)
	}
	if in.ClaudeCodeOnly != nil {
		add("claude_code_only", *in.ClaudeCodeOnly)
	}
	if in.FallbackGroupID != nil {
		var fallback any
		if *in.FallbackGroupID > 0 {
			fallback = *in.FallbackGroupID
		}
		add("fallback_group_id", fallback)
	}
	if in.MaxEffort != nil || in.OverLimit != nil || in.EffortMappings != nil {
		var platform string
		if err = a.DB.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL", id).Scan(&platform); err != nil {
			return err
		}
		if err = in.validateReasoning(platform); err != nil {
			return err
		}
		if in.MaxEffort != nil {
			add("max_reasoning_effort", *in.MaxEffort)
		}
		if in.OverLimit != nil {
			add("max_reasoning_effort_over_limit", *in.OverLimit)
		}
		if in.EffortMappings != nil {
			raw, _ := json.Marshal(in.EffortMappings)
			add("reasoning_effort_mappings", string(raw))
		}
	}
	if in.ModelRouting != nil {
		raw, _ := json.Marshal(in.ModelRouting)
		add("model_routing", string(raw))
	}
	if in.RoutingEnabled != nil {
		add("model_routing_enabled", *in.RoutingEnabled)
	}
	if in.Pricing != nil {
		var platform string
		if err = a.DB.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL", id).Scan(&platform); err != nil {
			return err
		}
		if err = validateGroupPrices(platform, *in.Pricing); err != nil {
			return err
		}
		b, _ := json.Marshal(in.Pricing)
		add("model_pricing", string(b))
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
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720035)"); err != nil {
		return err
	}
	result, err := tx.ExecContext(r.Context(), "UPDATE groups SET "+strings.Join(sets, ",")+" WHERE id=$1 AND deleted_at IS NULL", args...)
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
	if err = in.audioPrices.apply(r.Context(), tx, id); err != nil {
		return err
	}
	if err = in.videoPrices.apply(r.Context(), tx, id); err != nil {
		return err
	}
	if err = in.batchGroupInput.apply(r.Context(), tx, id); err != nil {
		return err
	}
	if err = in.fastGroupInput.apply(r.Context(), tx, id); err != nil {
		return err
	}
	if err = in.messagesDispatchInput.apply(r.Context(), tx, id); err != nil {
		return err
	}
	if err = in.profitInput.apply(r.Context(), tx, id, false); err != nil {
		return err
	}
	if err = validateGroupFallback(r.Context(), tx, id, in.FallbackGroupID != nil && *in.FallbackGroupID > 0); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.getGroup(w, r)
}

// These are inventory counts, not model-specific admission or free concurrency.
// Retain the stored health/expiry/cooldown meanings within API Key account scope.
const adminGroupFrom = ` FROM groups g CROSS JOIN LATERAL (
 SELECT count(*) AS account_count,
 count(*) FILTER (WHERE a.status='active' AND a.schedulable
 AND (a.expires_at IS NULL OR a.expires_at>now() OR NOT a.auto_pause_on_expired)
 AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=now())
 AND (a.overload_until IS NULL OR a.overload_until<=now())
 AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=now())) AS active_account_count,
 count(*) FILTER (WHERE a.status='active' AND a.schedulable
 AND (a.expires_at IS NULL OR a.expires_at>now() OR NOT a.auto_pause_on_expired)
 AND (a.rate_limit_reset_at>now() OR a.overload_until>now() OR a.temp_unschedulable_until>now())) AS rate_limited_account_count
 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
 WHERE ag.group_id=g.id AND a.deleted_at IS NULL AND a.type='apikey'
 AND a.platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax')
 AND COALESCE(a.credentials->>'account_mode','') IN ('','payg')
 ) counts`
const adminGroupView = `(to_jsonb(g)-'deleted_at') || to_jsonb(counts)`

func (a *App) getGroup(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT "+adminGroupView+adminGroupFrom+" WHERE g.id=$1 AND g.deleted_at IS NULL", id))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) listGroups(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("search"))
	if len([]rune(search)) > 100 || strings.ContainsRune(search, 0) {
		return bad("invalid group search (maximum 100 characters)")
	}
	var exclusive any
	if raw := q.Get("is_exclusive"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return bad("invalid is_exclusive")
		}
		exclusive = value
	}
	all := strings.HasSuffix(r.URL.Path, "/all")
	status := q.Get("status")
	if all {
		includeInactive := false
		if raw := q.Get("include_inactive"); raw != "" {
			var err error
			if includeInactive, err = strconv.ParseBool(raw); err != nil {
				return bad("invalid include_inactive")
			}
		}
		if !includeInactive {
			status = "active"
		}
	}
	args := []any{q.Get("platform"), status, search, exclusive}
	where := ` WHERE g.deleted_at IS NULL AND ($1='' OR g.platform=$1) AND ($2='' OR g.status=$2)
 AND ($3='' OR position(lower($3) in lower(g.name))>0 OR position(lower($3) in lower(COALESCE(g.description,'')))>0)
 AND ($4::boolean IS NULL OR g.is_exclusive=$4)`
	field := strings.ToLower(strings.TrimSpace(q.Get("sort_by")))
	switch field {
	case "name", "platform", "subscription_type", "rate_multiplier", "is_exclusive", "status", "created_at", "id", "sort_order", "account_count":
	case "billing_type":
		field = "subscription_type"
	default:
		field = "sort_order"
	}
	direction := " ASC"
	if strings.EqualFold(strings.TrimSpace(q.Get("sort_order")), "desc") {
		direction = " DESC"
	}
	order := "g." + field + direction
	if field == "account_count" {
		order = "counts.account_count" + direction + ",g.sort_order ASC,g.id ASC"
	} else if field != "id" {
		order += ",g.id" + direction
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	page, size := pagination(r)
	var total int
	if !all {
		if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM groups g"+where, args...).Scan(&total); err != nil {
			return err
		}
	}
	query := "SELECT " + adminGroupView + adminGroupFrom + where + " ORDER BY " + order
	if !all {
		query += " LIMIT $5 OFFSET $6"
		args = append(args, size, (page-1)*size)
	}
	rows, err := tx.QueryContext(r.Context(), query, args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	if all {
		return reply(w, items)
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
	rows, err := a.DB.QueryContext(r.Context(), `SELECT `+publicGroupView+` FROM groups g JOIN users u ON u.id=$1 WHERE g.deleted_at IS NULL AND g.status='active' AND g.subscription_type='standard' AND NOT g.require_oauth_only AND g.platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite') AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id)) ORDER BY g.sort_order,g.id`, current(r).ID)
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
	a.route("GET /api/v1/admin/groups/{id}/model-allowlist-candidates", "admin", a.groupModelCandidates)
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
