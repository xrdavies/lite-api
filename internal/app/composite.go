package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type compositeRouteConfig struct {
	PublicModel    string `json:"public_model"`
	MatchType      string `json:"match_type"`
	TargetPlatform string `json:"target_platform"`
	UpstreamModel  string `json:"upstream_model"`
	Endpoint       string `json:"endpoint"`
	Priority       int    `json:"priority"`
	Enabled        bool   `json:"enabled"`
	Notes          string `json:"notes"`
}

type compositeRoute struct {
	compositeRouteConfig
	ID        int64     `json:"id"`
	GroupID   int64     `json:"group_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type compositeDecision struct {
	Matched        bool            `json:"matched"`
	Source         string          `json:"source"`
	GroupID        int64           `json:"group_id"`
	PublicModel    string          `json:"public_model"`
	TargetPlatform string          `json:"target_platform"`
	UpstreamModel  string          `json:"upstream_model"`
	Endpoint       string          `json:"endpoint"`
	Route          *compositeRoute `json:"route,omitempty"`
	Reason         string          `json:"reason,omitempty"`
}

// One request uses one routing snapshot, including account alias ownership.
type compositeConfig struct {
	Routes []compositeRoute
	Owners map[string]string // Empty value means multiple concrete platforms claim the alias.
}

func compositeEndpoint(endpoint string) bool {
	switch endpoint {
	case "any", "messages", "count_tokens", "responses", "chat_completions", "embeddings", "images", "gemini":
		return true
	}
	return false
}

func (a *App) loadComposite(ctx context.Context, gid int64) (*compositeConfig, error) {
	tx, err := a.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	c := &compositeConfig{Routes: []compositeRoute{}, Owners: map[string]string{}}
	rows, err := tx.QueryContext(ctx, "SELECT to_jsonb(r)-'deleted_at' || jsonb_build_object('notes',COALESCE(notes,'')) FROM composite_model_routes r WHERE group_id=$1 AND deleted_at IS NULL ORDER BY priority,id", gid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw []byte
		var route compositeRoute
		if err = rows.Scan(&raw); err == nil {
			err = json.Unmarshal(raw, &route)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		c.Routes = append(c.Routes, route)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT a.platform,COALESCE(a.credentials->'model_mapping','{}'::jsonb) FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
 WHERE ag.group_id=$1 AND a.type='apikey' AND a.status='active' AND a.deleted_at IS NULL AND a.schedulable
 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now())`, gid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var platform string
		var raw []byte
		if err = rows.Scan(&platform, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if !supportedPlatform(platform) {
			continue
		}
		var mapping map[string]string
		if err = json.Unmarshal(raw, &mapping); err != nil {
			rows.Close()
			return nil, &apiError{503, "invalid account model mapping"}
		}
		for model, target := range mapping {
			if !concreteModel(model) || strings.TrimSpace(target) == "" {
				continue
			}
			if previous, exists := c.Owners[model]; exists && previous != platform {
				c.Owners[model] = ""
			} else {
				c.Owners[model] = platform
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return c, tx.Commit()
}

func (c *compositeConfig) resolve(gid int64, model, endpoint string) compositeDecision {
	d := compositeDecision{GroupID: gid, PublicModel: model, Endpoint: endpoint}
	var best *compositeRoute
	for i := range c.Routes {
		r := &c.Routes[i]
		if !r.Enabled || r.Endpoint != "any" && r.Endpoint != endpoint {
			continue
		}
		if r.MatchType == "exact" && r.PublicModel == model || r.MatchType == "prefix" && strings.HasPrefix(model, r.PublicModel) {
			if best == nil || betterCompositeRoute(r, best, endpoint) {
				best = r
			}
		}
	}
	if best != nil {
		if !supportedPlatform(best.TargetPlatform) {
			d.Reason = "route targets an unsupported platform"
			return d
		}
		d.Matched, d.Source, d.TargetPlatform, d.UpstreamModel, d.Route = true, "route", best.TargetPlatform, best.UpstreamModel, best
		if d.UpstreamModel == "" {
			d.UpstreamModel = model
		}
		return d
	}
	if platform, exists := c.Owners[model]; exists {
		if platform == "" {
			d.Reason = "model is exposed by multiple provider platforms"
			return d
		}
		d.Matched, d.Source, d.TargetPlatform, d.UpstreamModel = true, "account_model", platform, model
		return d
	}
	if platform := detectModelPlatform(model); platform != "" {
		d.Matched, d.Source, d.TargetPlatform, d.UpstreamModel = true, "detector", platform, model
		return d
	}
	d.Reason = "no explicit route, account ownership or recognized model platform"
	return d
}

func betterCompositeRoute(a, b *compositeRoute, endpoint string) bool {
	if a.MatchType != b.MatchType {
		return a.MatchType == "exact"
	}
	if (a.Endpoint == endpoint) != (b.Endpoint == endpoint) {
		return a.Endpoint == endpoint
	}
	if len(a.PublicModel) != len(b.PublicModel) {
		return len(a.PublicModel) > len(b.PublicModel)
	}
	if a.Priority != b.Priority {
		return a.Priority < b.Priority
	}
	return a.ID < b.ID
}

func detectModelPlatform(model string) string {
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "models/")
	if provider, rest, ok := strings.Cut(model, "/"); ok {
		switch provider {
		case "antigravity", "opencode", "opencode_go", "bedrock":
			return ""
		case "anthropic", "claude":
			return "anthropic"
		case "openai", "chatgpt":
			return "openai"
		case "google", "google-ai-studio", "gemini":
			return "gemini"
		case "xai", "x-ai", "grok":
			return "grok"
		case "kimi", "moonshot":
			return "kimi"
		case "zhipu", "glm", "bigmodel":
			return "zhipu"
		case "deepseek", "minimax":
			return provider
		}
		model = strings.TrimPrefix(rest, "models/")
	}
	for _, row := range []struct {
		platform string
		prefixes []string
	}{
		{"anthropic", []string{"anthropic.claude-", "claude-"}},
		{"openai", []string{"gpt-", "chatgpt-", "codex-", "text-embedding-", "text-moderation-", "omni-moderation-", "dall-e-", "tts-", "whisper-"}},
		{"gemini", []string{"gemini-", "learnlm-"}},
		{"grok", []string{"grok-"}},
		{"kimi", []string{"kimi-", "moonshot-"}},
		{"zhipu", []string{"glm-"}},
		{"deepseek", []string{"deepseek-"}},
		{"minimax", []string{"minimax-", "abab5", "abab6", "abab7"}},
	} {
		for _, prefix := range row.prefixes {
			if strings.HasPrefix(model, prefix) {
				return row.platform
			}
		}
	}
	for _, name := range []string{"o1", "o3", "o4", "o5"} {
		if model == name || strings.HasPrefix(model, name+"-") {
			return "openai"
		}
	}
	if model == "k3" || model == "k3-256k" {
		return "kimi"
	}
	if model == "grok" {
		return "grok"
	}
	return ""
}

// Catalogs are the union of callable endpoint identities, not every account's
// internal aliases. A route that chooses another provider cannot leak its target.
func compositeModelTargets(c *compositeConfig, gid int64, model, platform, protocol string, native bool) []string {
	if c == nil {
		return []string{model}
	}
	endpoints := []string{protocol}
	if protocol == "responses" && chatResponsesPlatform(platform) {
		endpoints = append(endpoints, "chat_completions")
	}
	if protocol == "anthropic" {
		endpoints = []string{"messages", "count_tokens"}
	}
	if platform == "openai" && (protocol == "responses" || protocol == "chat_completions") {
		endpoints = append(endpoints, "embeddings")
	}
	if native {
		endpoints = []string{"gemini"}
	}
	targets := []string{}
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		d := c.resolve(gid, model, endpoint)
		if d.Matched && d.TargetPlatform == platform && !seen[d.UpstreamModel] {
			targets = append(targets, d.UpstreamModel)
			seen[d.UpstreamModel] = true
		}
	}
	return targets
}

func (a *App) compositeRoutes(w http.ResponseWriter, r *http.Request) error {
	gid, err := pathID(r)
	if err != nil {
		return err
	}
	if r.Method == "GET" || strings.HasSuffix(r.URL.Path, "/preview") {
		var platform string
		if err = a.DB.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL", gid).Scan(&platform); err != nil {
			return err
		}
		if platform != "composite" {
			return bad("a composite group is required")
		}
		c, err := a.loadComposite(r.Context(), gid)
		if err != nil {
			return err
		}
		if r.Method == "GET" {
			return reply(w, c.Routes)
		}
		var in struct{ Model, Endpoint string }
		if err = decode(w, r, &in); err != nil {
			return err
		}
		if in.Endpoint == "" {
			in.Endpoint = "any"
		}
		if !concreteModel(in.Model) || !compositeEndpoint(in.Endpoint) {
			return bad("invalid preview model or endpoint")
		}
		return reply(w, c.resolve(gid, in.Model, in.Endpoint))
	}
	rid := int64(0)
	if r.Method != "POST" {
		rid, err = strconv.ParseInt(r.PathValue("route_id"), 10, 64)
		if err != nil || rid <= 0 {
			return bad("invalid route ID")
		}
	}
	in := compositeRouteConfig{MatchType: "exact", Endpoint: "any", Enabled: true, Priority: 100}
	if r.Method != "DELETE" {
		if err = decode(w, r, &in); err != nil {
			return err
		}
		in.PublicModel, in.UpstreamModel = strings.TrimSpace(in.PublicModel), strings.TrimSpace(in.UpstreamModel)
		if in.MatchType == "" {
			in.MatchType = "exact"
		}
		if in.Endpoint == "" {
			in.Endpoint = "any"
		}
		if in.Priority == 0 {
			in.Priority = 100
		}
		if !concreteModel(in.PublicModel) || in.UpstreamModel != "" && !concreteModel(in.UpstreamModel) || !supportedPlatform(in.TargetPlatform) || !compositeEndpoint(in.Endpoint) || in.MatchType != "exact" && in.MatchType != "prefix" || in.Priority < -2147483648 || in.Priority > 2147483647 || len(in.Notes) > 10000 {
			return bad("invalid composite route")
		}
		if in.MatchType == "exact" && in.UpstreamModel == "" {
			in.UpstreamModel = in.PublicModel
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var platform string
	if err = tx.QueryRowContext(r.Context(), "SELECT platform FROM groups WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE", gid).Scan(&platform); err != nil {
		return err
	}
	if platform != "composite" {
		return bad("a composite group is required")
	}
	var raw json.RawMessage
	if r.Method == "DELETE" {
		err = tx.QueryRowContext(r.Context(), "UPDATE composite_model_routes SET deleted_at=now(),updated_at=now() WHERE id=$1 AND group_id=$2 AND deleted_at IS NULL RETURNING id", rid, gid).Scan(&rid)
		raw = json.RawMessage(`{"message":"Composite route deleted"}`)
	} else {
		args := []any{gid, in.PublicModel, in.MatchType, in.TargetPlatform, in.UpstreamModel, in.Endpoint, in.Priority, in.Enabled, strings.TrimSpace(in.Notes)}
		query := "INSERT INTO composite_model_routes(group_id,public_model,match_type,target_platform,upstream_model,endpoint,priority,enabled,notes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)"
		if r.Method == "PUT" {
			args = append(args, rid)
			query = "UPDATE composite_model_routes SET public_model=$2,match_type=$3,target_platform=$4,upstream_model=$5,endpoint=$6,priority=$7,enabled=$8,notes=$9,updated_at=now() WHERE group_id=$1 AND id=$10 AND deleted_at IS NULL"
		}
		raw, err = jsonRow(tx.QueryRowContext(r.Context(), "WITH changed AS ("+query+" RETURNING *) SELECT to_jsonb(changed)-'deleted_at' FROM changed", args...))
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if r.Method == "POST" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	return reply(w, raw)
}
