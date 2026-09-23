package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/lib/pq"
)

type statsPriceRule struct {
	Name     string       `json:"name"`
	Groups   []int64      `json:"group_ids"`
	Accounts []int64      `json:"account_ids"`
	Pricing  []modelPrice `json:"pricing"`
}
type channelInput struct {
	Name           *string                      `json:"name"`
	Description    *string                      `json:"description"`
	Status         *string                      `json:"status"`
	Groups         *[]int64                     `json:"group_ids"`
	Pricing        *[]modelPrice                `json:"model_pricing"`
	Mapping        map[string]map[string]string `json:"model_mapping"`
	BillingSource  *string                      `json:"billing_model_source"`
	RestrictModels *bool                        `json:"restrict_models"`
	Features       *string                      `json:"features"`
	FeaturesConfig map[string]json.RawMessage   `json:"features_config"`
	ApplyToStats   *bool                        `json:"apply_pricing_to_account_stats"`
	StatsRules     *[]statsPriceRule            `json:"account_stats_pricing_rules"`
}

func validIDs(ids []int64) bool {
	if len(ids) > 1000 {
		return false
	}
	seen := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}
func (in *channelInput) validate(create bool) error {
	if create && in.Name == nil {
		return bad("name is required")
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid channel name")
	}
	if in.Description != nil && len(*in.Description) > 10000 {
		return bad("description is too long")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		return bad("invalid channel status")
	}
	if in.BillingSource != nil {
		switch *in.BillingSource {
		case "requested", "upstream", "channel_mapped", "response_model":
		default:
			return bad("invalid billing_model_source")
		}
	}
	if in.Groups != nil && !validIDs(*in.Groups) {
		return bad("invalid group_ids")
	}
	if in.Pricing != nil {
		if err := validateModelPrices(*in.Pricing, false); err != nil {
			return err
		}
	}
	for platform, mapping := range in.Mapping {
		if !supportedPlatform(platform) || len(mapping) > 1000 {
			return bad("invalid model mapping platform or size")
		}
		patterns := []string{}
		for from, to := range mapping {
			if !validModelPattern(from) || to != "" && !validModelPattern(to) {
				return bad("invalid channel model mapping")
			}
			n := strings.ToLower(from)
			for _, prev := range patterns {
				if patternsOverlap(n, prev) {
					return bad("overlapping model mapping patterns")
				}
			}
			patterns = append(patterns, n)
		}
	}
	if in.Features != nil && len(*in.Features) > 10000 {
		return bad("features is too long")
	}
	// Runtime feature switches are accepted only when their execution path exists.
	if len(in.FeaturesConfig) > 0 {
		return bad("channel feature switches are not yet supported")
	}
	if in.StatsRules != nil {
		if len(*in.StatsRules) > 100 {
			return bad("too many account stats pricing rules")
		}
		for i := range *in.StatsRules {
			rule := &(*in.StatsRules)[i]
			if len([]rune(rule.Name)) > 100 || !validIDs(rule.Groups) || !validIDs(rule.Accounts) || len(rule.Groups)+len(rule.Accounts) == 0 || len(rule.Pricing) == 0 {
				return bad("invalid account stats pricing rule")
			}
			if rule.Groups == nil {
				rule.Groups = []int64{}
			}
			if rule.Accounts == nil {
				rule.Accounts = []int64{}
			}
			if err := validateModelPrices(rule.Pricing, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// One statement gives a consistent configuration snapshot across child tables.
const channelSnapshot = `SELECT to_jsonb(c) || jsonb_build_object(
 'group_ids',COALESCE((SELECT jsonb_agg(group_id ORDER BY group_id) FROM channel_groups WHERE channel_id=c.id),'[]'::jsonb),
 'model_pricing',COALESCE((SELECT jsonb_agg(to_jsonb(p) || jsonb_build_object('intervals',COALESCE((SELECT jsonb_agg(to_jsonb(i) ORDER BY sort_order,id) FROM channel_pricing_intervals i WHERE pricing_id=p.id),'[]'::jsonb)) ORDER BY p.id) FROM channel_model_pricing p WHERE channel_id=c.id),'[]'::jsonb),
 'account_stats_pricing_rules',COALESCE((SELECT jsonb_agg(to_jsonb(s) || jsonb_build_object('pricing',COALESCE((SELECT jsonb_agg(to_jsonb(p) || jsonb_build_object('intervals',COALESCE((SELECT jsonb_agg(to_jsonb(i) ORDER BY sort_order,id) FROM channel_account_stats_pricing_intervals i WHERE pricing_id=p.id),'[]'::jsonb)) ORDER BY p.id) FROM channel_account_stats_model_pricing p WHERE rule_id=s.id),'[]'::jsonb)) ORDER BY s.sort_order,s.id) FROM channel_account_stats_pricing_rules s WHERE channel_id=c.id),'[]'::jsonb)) FROM channels c WHERE c.id=$1`

func channelJSON(ctx context.Context, q queryer, id int64) (json.RawMessage, error) {
	return jsonRow(q.QueryRowContext(ctx, channelSnapshot, id))
}

func insertPrices(ctx context.Context, tx *sql.Tx, parent int64, prices []modelPrice, stats bool) error {
	table, parentColumn, intervalTable := "channel_model_pricing", "channel_id", "channel_pricing_intervals"
	columns := []string{"platform", "models", "billing_mode", "input_price", "output_price", "cache_write_price", "cache_write_1h_price", "cache_read_price", "image_output_price", "per_request_price", "reasoning_effort_multipliers"}
	intervalColumns := []string{"min_tokens", "max_tokens", "tier_label", "input_price", "output_price", "cache_write_price", "cache_write_1h_price", "cache_read_price", "per_request_price", "sort_order"}
	if stats {
		table, parentColumn, intervalTable = "channel_account_stats_model_pricing", "rule_id", "channel_account_stats_pricing_intervals"
	} else {
		columns = append(columns, "image_input_price", "time_pricing", "fast_multiplier", "flex_multiplier")
		intervalColumns = append(intervalColumns, "input_multiplier", "output_multiplier", "cache_write_multiplier", "cache_read_multiplier")
	}
	// All identifiers above are constants; JSON values are bound parameters and cast by PostgreSQL.
	for _, price := range prices {
		b, err := json.Marshal(price)
		if err != nil {
			return err
		}
		var id int64
		err = tx.QueryRowContext(ctx, "INSERT INTO "+table+" ("+parentColumn+","+strings.Join(columns, ",")+") SELECT $1,"+strings.Join(columns, ",")+" FROM jsonb_populate_record(NULL::"+table+",$2::jsonb) RETURNING id", parent, string(b)).Scan(&id)
		if err != nil {
			return err
		}
		for _, interval := range price.Intervals {
			b, err = json.Marshal(interval)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO "+intervalTable+" (pricing_id,"+strings.Join(intervalColumns, ",")+") SELECT $1,"+strings.Join(intervalColumns, ",")+" FROM jsonb_populate_record(NULL::"+intervalTable+",$2::jsonb)", id, string(b)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (a *App) saveChannel(w http.ResponseWriter, r *http.Request) error {
	create := r.Method == "POST"
	var in channelInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(create); err != nil {
		return err
	}
	var id int64
	var err error
	if !create {
		id, err = pathID(r)
		if err != nil {
			return err
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if create {
		if err = tx.QueryRowContext(r.Context(), "INSERT INTO channels(name) VALUES($1) RETURNING id", *in.Name).Scan(&id); err != nil {
			return err
		}
	} else {
		if err = tx.QueryRowContext(r.Context(), "SELECT id FROM channels WHERE id=$1 FOR UPDATE", id).Scan(&id); err != nil {
			return err
		}
	}
	sets := []string{"updated_at=now()"}
	args := []any{id}
	add := func(column string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s=$%d", column, len(args)))
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
	if in.BillingSource != nil {
		add("billing_model_source", *in.BillingSource)
	}
	if in.RestrictModels != nil {
		add("restrict_models", *in.RestrictModels)
	}
	if in.Features != nil {
		add("features", *in.Features)
	}
	if in.ApplyToStats != nil {
		add("apply_pricing_to_account_stats", *in.ApplyToStats)
	}
	if in.Mapping != nil {
		b, _ := json.Marshal(in.Mapping)
		add("model_mapping", string(b))
	}

	if _, err = tx.ExecContext(r.Context(), "UPDATE channels SET "+strings.Join(sets, ",")+" WHERE id=$1", args...); err != nil {
		return err
	}
	if in.Groups != nil {
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM channel_groups WHERE channel_id=$1", id); err != nil {
			return err
		}
		for _, gid := range *in.Groups {
			var platform string
			if err = tx.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL AND subscription_type='standard' AND NOT require_oauth_only FOR SHARE", gid).Scan(&platform); err != nil {
				if err == sql.ErrNoRows {
					return bad("group is unavailable")
				}
				return err
			}
			if !supportedPlatform(platform) && platform != "composite" {
				return bad("group platform is unsupported")
			}
			if _, err = tx.ExecContext(r.Context(), "INSERT INTO channel_groups(channel_id,group_id) VALUES($1,$2)", id, gid); err != nil {
				return err
			}
		}
	}
	if in.Pricing != nil {
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM channel_model_pricing WHERE channel_id=$1", id); err != nil {
			return err
		}
		if err = insertPrices(r.Context(), tx, id, *in.Pricing, false); err != nil {
			return err
		}
	}
	if in.StatsRules != nil {
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM channel_account_stats_pricing_rules WHERE channel_id=$1", id); err != nil {
			return err
		}
		for i, rule := range *in.StatsRules {
			for _, gid := range rule.Groups {
				var exists bool
				if err = tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM channel_groups WHERE channel_id=$1 AND group_id=$2)", id, gid).Scan(&exists); err != nil {
					return err
				}
				if !exists {
					return bad("pricing rule group must belong to this channel")
				}
			}
			for _, aid := range rule.Accounts {
				var platform string
				if err = tx.QueryRowContext(r.Context(), "SELECT platform FROM accounts WHERE id=$1 AND deleted_at IS NULL AND type='apikey' FOR SHARE", aid).Scan(&platform); err != nil {
					if err == sql.ErrNoRows {
						return bad("pricing rule account is unavailable")
					}
					return err
				}
				if !supportedPlatform(platform) {
					return bad("pricing rule account platform is unsupported")
				}
			}
			var ruleID int64
			if err = tx.QueryRowContext(r.Context(), "INSERT INTO channel_account_stats_pricing_rules(channel_id,name,group_ids,account_ids,sort_order) VALUES($1,$2,$3,$4,$5) RETURNING id", id, rule.Name, pq.Array(rule.Groups), pq.Array(rule.Accounts), i).Scan(&ruleID); err != nil {
				return err
			}
			if err = insertPrices(r.Context(), tx, ruleID, rule.Pricing, true); err != nil {
				return err
			}
		}
	}
	raw, err := channelJSON(r.Context(), tx, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) getChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := channelJSON(r.Context(), a.DB, id)
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) listChannels(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	search, status := "%"+r.URL.Query().Get("search")+"%", r.URL.Query().Get("status")
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM channels WHERE name ILIKE $1 AND ($2='' OR status=$2)", search, status).Scan(&total); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT id FROM channels WHERE name ILIKE $1 AND ($2='' OR status=$2) ORDER BY id DESC LIMIT $3 OFFSET $4", search, status, size, (page-1)*size)
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
		raw, err := channelJSON(r.Context(), a.DB, id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		items = append(items, raw)
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) deleteChannel(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	result, err := a.DB.ExecContext(r.Context(), "DELETE FROM channels WHERE id=$1", id)
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

func (a *App) availableChannels(w http.ResponseWriter, r *http.Request) error {
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	if err := tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM settings WHERE key='available_channels_enabled' AND value='true')").Scan(&enabled); err != nil {
		return err
	}
	if !enabled {
		return reply(w, []any{})
	}
	// The SQL returns only permitted groups; channel rules, mappings and account IDs never enter the response.
	rows, err := tx.QueryContext(r.Context(), `SELECT c.id,c.name,COALESCE(c.description,''),g.platform,jsonb_build_object('id',g.id,'name',g.name,'platform',g.platform,'subscription_type',g.subscription_type,'rate_multiplier',COALESCE(m.rate_multiplier,g.rate_multiplier),'peak_rate_enabled',g.peak_rate_enabled,'peak_start',g.peak_start,'peak_end',g.peak_end,'peak_rate_multiplier',g.peak_rate_multiplier,'is_exclusive',g.is_exclusive)
 FROM channels c JOIN channel_groups cg ON cg.channel_id=c.id JOIN groups g ON g.id=cg.group_id JOIN users u ON u.id=$1 LEFT JOIN user_group_rate_multipliers m ON m.user_id=u.id AND m.group_id=g.id
 WHERE c.status='active' AND g.status='active' AND g.deleted_at IS NULL AND g.subscription_type='standard' AND NOT g.require_oauth_only AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id)) ORDER BY c.id,g.sort_order,g.id`, current(r).ID)
	if err != nil {
		return err
	}
	type visibleChannel struct {
		id                int64
		name, description string
		groups            map[string][]json.RawMessage
	}
	channels := []visibleChannel{}
	for rows.Next() {
		var id int64
		var name, description, platform string
		var group json.RawMessage
		if err = rows.Scan(&id, &name, &description, &platform, &group); err != nil {
			rows.Close()
			return err
		}
		if !supportedPlatform(platform) && platform != "composite" {
			continue
		}
		if len(channels) == 0 || channels[len(channels)-1].id != id {
			channels = append(channels, visibleChannel{id, name, description, map[string][]json.RawMessage{}})
		}
		ch := &channels[len(channels)-1]
		ch.groups[platform] = append(ch.groups[platform], group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	out := []any{}
	for _, ch := range channels {
		raw, err := channelJSON(r.Context(), tx, ch.id)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		var config struct {
			Pricing []modelPrice                 `json:"model_pricing"`
			Mapping map[string]map[string]string `json:"model_mapping"`
			Status  string                       `json:"status"`
		}
		if err = json.Unmarshal(raw, &config); err != nil {
			return err
		}
		if config.Status != "active" {
			continue
		}
		supported := supportedModels(config.Pricing, config.Mapping)
		if composite := ch.groups["composite"]; len(composite) > 0 {
			platforms := map[string]bool{}
			for _, p := range supported {
				if supportedPlatform(p.Platform) {
					platforms[p.Platform] = true
				}
			}
			if len(platforms) > 0 {
				delete(ch.groups, "composite")
				for platform := range platforms {
					ch.groups[platform] = append(ch.groups[platform], composite...)
				}
			}
		}
		platforms := []string{}
		for platform := range ch.groups {
			platforms = append(platforms, platform)
		}
		sort.Strings(platforms)
		sections := []any{}
		for _, platform := range platforms {
			models := []any{}
			for _, model := range supported {
				if model.Platform != platform {
					continue
				}
				var pricing any
				if model.Pricing != nil {
					pricing = publicPricing(*model.Pricing)
				}
				models = append(models, map[string]any{"name": model.Name, "platform": platform, "pricing": pricing})
			}
			sections = append(sections, map[string]any{"platform": platform, "groups": ch.groups[platform], "supported_models": models})
		}
		out = append(out, map[string]any{"name": ch.name, "description": ch.description, "platforms": sections})
	}
	return reply(w, out)
}

func publicPricing(p modelPrice) map[string]json.RawMessage {
	b, _ := json.Marshal(p)
	var visible map[string]json.RawMessage
	_ = json.Unmarshal(b, &visible)
	delete(visible, "platform")
	delete(visible, "models")
	var intervals []map[string]json.RawMessage
	_ = json.Unmarshal(visible["intervals"], &intervals)
	for _, iv := range intervals {
		delete(iv, "sort_order")
	}
	visible["intervals"], _ = json.Marshal(intervals)
	return visible
}
func (a *App) channelRoutes() {
	a.route("POST /api/v1/admin/channels", "admin", a.saveChannel)
	a.route("GET /api/v1/admin/channels", "admin", a.listChannels)
	a.route("GET /api/v1/admin/channels/{id}", "admin", a.getChannel)
	a.route("PUT /api/v1/admin/channels/{id}", "admin", a.saveChannel)
	a.route("DELETE /api/v1/admin/channels/{id}", "admin", a.deleteChannel)
	a.route("GET /api/v1/channels/available", "user", a.availableChannels)
}

type supportedModel struct {
	Name, Platform string
	Pricing        *modelPrice
}

func supportedModels(prices []modelPrice, mapping map[string]map[string]string) []supportedModel {
	index := map[string]map[string]supportedModel{}
	for i := range prices {
		p := &prices[i]
		if !supportedPlatform(p.Platform) {
			continue
		}
		if index[p.Platform] == nil {
			index[p.Platform] = map[string]supportedModel{}
		}
		for _, name := range p.Models {
			if !strings.HasSuffix(name, "*") {
				index[p.Platform][strings.ToLower(name)] = supportedModel{name, p.Platform, p}
			}
		}
	}
	out := []supportedModel{}
	seen := map[string]bool{}
	add := func(m supportedModel) {
		key := m.Platform + "\x00" + strings.ToLower(m.Name)
		if !seen[key] {
			seen[key] = true
			out = append(out, m)
		}
	}
	for platform, entries := range mapping {
		if !supportedPlatform(platform) {
			continue
		}
		for source, target := range entries {
			if strings.HasSuffix(source, "*") {
				continue
			} // Concrete pricing entries below supply wildcard expansions.
			display := source
			if p, ok := index[platform][strings.ToLower(source)]; ok {
				display = p.Name
			}
			if target == "" || strings.HasSuffix(target, "*") {
				target = source
			}
			add(supportedModel{display, platform, index[platform][strings.ToLower(target)].Pricing})
		}
	}
	for _, entries := range index {
		for _, model := range entries {
			add(model)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Platform != out[j].Platform {
			return out[i].Platform < out[j].Platform
		}
		return out[i].Name < out[j].Name
	})
	return out
}
