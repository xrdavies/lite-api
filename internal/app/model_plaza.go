package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// plazaPrice resolves absolute tier prices with the same rules as settlement.
// User/group, time and service-tier multipliers remain separate display fields.
func plazaPrice(p modelPrice, longContext bool) modelPrice {
	if p.BillingMode != "token" {
		return p
	}
	resolve := func(iv *priceInterval) priceInterval {
		out := priceInterval{}
		if iv != nil {
			out.Min, out.Max, out.Label = iv.Min, iv.Max, iv.Label
		}
		fields := []**json.Number{&out.Input, &out.Output, &out.CacheWrite, &out.CacheWrite1h, &out.CacheRead}
		for i, value := range tokenPrices(p, iv) {
			if value != nil {
				// Schema prices have 12 decimals and multipliers have at most 6.
				n := json.Number(strings.TrimRight(strings.TrimRight(value.FloatString(18), "0"), "."))
				*fields[i] = &n
			}
		}
		return out
	}
	base := resolve(p.interval(1))
	intervals := []priceInterval{}
	if longContext && len(p.Intervals) > 0 {
		// Include unconfigured gaps, where billing falls back to base prices.
		boundaries := map[int64]bool{0: true}
		for _, iv := range p.Intervals {
			boundaries[iv.Min] = true
			if iv.Max != nil {
				boundaries[*iv.Max] = true
			}
		}
		starts := []int64{}
		for start := range boundaries {
			starts = append(starts, start)
		}
		sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
		for i, start := range starts {
			iv := resolve(p.interval(start + 1))
			iv.Min, iv.Max = start, nil
			if i+1 < len(starts) {
				end := starts[i+1]
				iv.Max = &end
			}
			intervals = append(intervals, iv)
		}
	}
	p.Input, p.Output, p.CacheWrite, p.CacheWrite1h, p.CacheRead = base.Input, base.Output, base.CacheWrite, base.CacheWrite1h, base.CacheRead
	p.Intervals = intervals
	return p
}

func (a *App) modelPlaza(w http.ResponseWriter, r *http.Request) error {
	catalog := a.prices.Load()
	// Invalid, expired or disabled identities must not fall back to an anonymous view.
	var userID int64
	if r.Header.Get("Authorization") != "" {
		u, err := a.authenticate(r)
		if err != nil {
			return err
		}
		userID = u.ID
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled, requireAuth bool
	var description string
	if err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM settings WHERE key='model_plaza_enabled' AND value='true'),EXISTS(SELECT 1 FROM settings WHERE key='model_plaza_require_auth' AND value='true'),COALESCE((SELECT value FROM settings WHERE key='model_plaza_description'),'')`).Scan(&enabled, &requireAuth, &description); err != nil {
		return err
	}
	if !enabled {
		return missing()
	}
	if requireAuth && userID == 0 {
		return unauthorized()
	}
	if userID != 0 {
		var active bool
		if err = tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active' AND deleted_at IS NULL)", userID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return unauthorized()
		}
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT c.id,g.platform,g.long_context_pricing_enabled,g.model_allowlist,g.model_pricing,jsonb_build_object(
'id',g.id,'name',g.name,'description',g.description,'platform',g.platform,'subscription_type',g.subscription_type,'rate_multiplier',g.rate_multiplier,'is_exclusive',g.is_exclusive,'peak_rate_enabled',g.peak_rate_enabled,'peak_start',g.peak_start,'peak_end',g.peak_end,'peak_rate_multiplier',g.peak_rate_multiplier,'image_rate_independent',g.image_rate_independent,'image_rate_multiplier',g.image_rate_multiplier,'image_price_1k',g.image_price_1k,'image_price_2k',g.image_price_2k,'image_price_4k',g.image_price_4k,'long_context_pricing_enabled',g.long_context_pricing_enabled) || CASE WHEN m.rate_multiplier IS NOT NULL THEN jsonb_build_object('user_rate_multiplier',m.rate_multiplier) ELSE '{}'::jsonb END
FROM groups g JOIN channel_groups cg ON cg.group_id=g.id JOIN channels c ON c.id=cg.channel_id LEFT JOIN users u ON u.id=$1 LEFT JOIN user_group_rate_multipliers m ON m.user_id=u.id AND m.group_id=g.id
WHERE g.status='active' AND g.deleted_at IS NULL AND g.subscription_type='standard' AND NOT g.require_oauth_only AND c.status='active' AND ((NOT g.is_exclusive AND NOT COALESCE(u.restrict_public_groups,false)) OR EXISTS(SELECT 1 FROM user_allowed_groups WHERE user_id=u.id AND group_id=g.id)) ORDER BY g.rate_multiplier,g.name,g.id`, userID)
	if err != nil {
		return err
	}
	type group struct {
		channel     int64
		platform    string
		longContext bool
		allowlist   modelAllowlist
		pricing     []modelPrice
		visible     map[string]json.RawMessage
	}
	groups := []group{}
	for rows.Next() {
		g := group{}
		var allowlist, pricing, visible []byte
		if err = rows.Scan(&g.channel, &g.platform, &g.longContext, &allowlist, &pricing, &visible); err != nil {
			rows.Close()
			return err
		}
		if !supportedPlatform(g.platform) && g.platform != "composite" {
			continue
		}
		if err = json.Unmarshal(allowlist, &g.allowlist); err == nil {
			err = json.Unmarshal(visible, &g.visible)
		}
		if err == nil && len(pricing) > 0 {
			err = json.Unmarshal(pricing, &g.pricing)
		}
		if err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	out := []any{}
	configs := map[int64]json.RawMessage{}
	for _, g := range groups {
		raw := configs[g.channel]
		if raw == nil {
			raw, err = channelJSON(r.Context(), tx, g.channel)
			if err != nil {
				return err
			}
			configs[g.channel] = raw
		}
		var c struct {
			Pricing  []modelPrice                 `json:"model_pricing"`
			Mapping  map[string]map[string]string `json:"model_mapping"`
			Source   string                       `json:"billing_model_source"`
			Restrict bool                         `json:"restrict_models"`
		}
		if err = json.Unmarshal(raw, &c); err != nil {
			return err
		}
		models := []any{}
		for _, model := range supportedModels(c.Pricing, c.Mapping) {
			if g.platform != "composite" && g.platform != model.Platform || !(&gatewayGroup{Allowlist: g.allowlist}).allows(model.Name) {
				continue
			}
			entry := map[string]any{"name": model.Name, "platform": model.Platform, "pricing": nil, "official_pricing": nil}
			if reference, ok := catalog.lookup("", model.Name); ok {
				entry["official_pricing"] = publicPricing(plazaPrice(reference, true))
			}
			// Upstream/response-model prices depend on the execution result; an
			// alias cannot claim its mapped channel price under those policies.
			if c.Source == "requested" || c.Source == "channel_mapped" || c.Source == "" {
				name := model.Name
				if c.Source != "requested" {
					for pattern, target := range c.Mapping[model.Platform] {
						if patternMatches(pattern, name) {
							if target != "" && target != "*" {
								name = target
							}
							break
						}
					}
				}
				if price, err := effectiveModelPrice(catalog, g.pricing, c.Pricing, model.Platform, name, c.Restrict); err == nil {
					resolved := plazaPrice(price, g.longContext)
					entry["pricing"] = publicPricing(resolved)
					if price.BillingMode == "token" && len(resolved.Intervals) > 1 {
						entry["long_context_basis"] = "whole_request"
					}
					if price.TimePricing != nil {
						entry["time_pricing"] = price.TimePricing
					}
				}
				if model.Platform == "gemini" {
					// Separate image tariffs from text prices: aliases may generate
					// either depending on the requested modalities and actual output.
					var imageGroup gatewayGroup
					raw, _ := json.Marshal(g.visible)
					if err := json.Unmarshal(raw, &imageGroup); err != nil {
						return err
					}
					selection := gatewaySelection{Account: &upstreamAccount{Platform: model.Platform}, Catalog: catalog, GroupPricing: g.pricing, Pricing: c.Pricing, Restrict: c.Restrict}
					prices := map[string]any{}
					for _, size := range []string{"1K", "2K", "4K"} {
						if price, _, err := selection.geminiImagePrice(imageGroup, name, size); err == nil {
							prices[size] = publicPricing(plazaPrice(price, g.longContext))
						}
					}
					if len(prices) > 0 {
						entry["image_pricing"] = prices
					}
				}
			}
			models = append(models, entry)
		}
		if len(models) > 0 {
			sort.Slice(models, func(i, j int) bool {
				left, right := models[i].(map[string]any), models[j].(map[string]any)
				if left["name"] != right["name"] {
					return left["name"].(string) < right["name"].(string)
				}
				return left["platform"].(string) < right["platform"].(string)
			})
			g.visible["models"], _ = json.Marshal(models)
			out = append(out, g.visible)
		}
	}
	return reply(w, map[string]any{"description": description, "groups": out, "pricing_as_of": catalog.AsOf, "pricing_checksum": catalog.hash})
}
