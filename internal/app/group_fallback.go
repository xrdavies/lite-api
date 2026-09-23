package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
)

// The caller serializes graph edits in the same transaction as the write.
func validateGroupFallback(ctx context.Context, tx *sql.Tx, id int64, checkTarget bool) error {
	var codeOnly, referenced bool
	var next *int64
	if err := tx.QueryRowContext(ctx, `SELECT claude_code_only,fallback_group_id,EXISTS(SELECT 1 FROM groups WHERE fallback_group_id=$1 AND deleted_at IS NULL) FROM groups WHERE id=$1`, id).Scan(&codeOnly, &next, &referenced); err != nil {
		return err
	}
	if codeOnly && referenced {
		return bad("a fallback target cannot enable claude_code_only")
	}
	if !checkTarget {
		return nil
	}
	seen := map[int64]bool{id: true}
	for next != nil {
		if seen[*next] || len(seen) > 32 {
			return bad("fallback group cycle or excessive chain")
		}
		seen[*next] = true
		var platform, subscription string
		var oauth bool
		err := tx.QueryRowContext(ctx, `SELECT platform,subscription_type,require_oauth_only,claude_code_only,fallback_group_id FROM groups WHERE id=$1 AND deleted_at IS NULL`, *next).Scan(&platform, &subscription, &oauth, &codeOnly, &next)
		if errors.Is(err, sql.ErrNoRows) {
			return bad("fallback group does not exist")
		}
		if err != nil {
			return err
		}
		if (!supportedPlatform(platform) && platform != "composite") || subscription != "standard" || oauth || codeOnly {
			return bad("fallback must be a supported API key group without claude_code_only")
		}
	}
	return nil
}

type clientPolicyKey struct{}
type clientPolicy struct {
	Protocol string
	Code     bool
}

func (g *gatewayIdentity) dispatchGroup() gatewayGroup {
	if g.RoutingGroup != nil {
		return *g.RoutingGroup
	}
	return g.Group
}

// A configured fallback delegates scheduling, not ownership or billing. It does
// not grant permission to create keys in the target (including private groups).
func (a *App) resolveClientGroup(r *http.Request, g *gatewayIdentity) error {
	policy, ok := r.Context().Value(clientPolicyKey{}).(clientPolicy)
	if !ok || !g.Group.ClaudeCodeOnly {
		return nil
	}
	if policy.Protocol == "chat_completions" || policy.Protocol == "responses" || policy.Protocol == "embeddings" || policy.Protocol == "alpha_search" || grokSearchProtocol(policy.Protocol) || audioProtocol(policy.Protocol) {
		return &apiError{403, "this group requires the Claude Code Messages endpoint"}
	}
	if policy.Code {
		return nil
	}
	group := g.Group
	seen := map[int64]bool{group.ID: true}
	for group.ClaudeCodeOnly {
		if group.FallbackGroupID == nil {
			return &apiError{403, "this group only allows Claude Code clients"}
		}
		id := *group.FallbackGroupID
		if seen[id] || len(seen) > 32 {
			return &apiError{503, "invalid fallback group chain"}
		}
		seen[id] = true
		var raw []byte
		err := a.DB.QueryRowContext(r.Context(), `SELECT to_jsonb(g) FROM groups g WHERE id=$1 AND deleted_at IS NULL AND status='active' AND subscription_type='standard' AND NOT require_oauth_only`, id).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return &apiError{503, "fallback group is unavailable"}
		}
		if err != nil {
			return err
		}
		group = gatewayGroup{}
		if err = json.Unmarshal(raw, &group); err != nil {
			return err
		}
		if !supportedPlatform(group.Platform) && group.Platform != "composite" {
			return &apiError{503, "unsupported fallback group platform"}
		}
	}
	g.RoutingGroup = &group
	return nil
}

func sameRoutingGroup(a, b *gatewayIdentity) bool {
	return reflect.DeepEqual(a.RoutingGroup, b.RoutingGroup)
}

var claudeClientUA = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+`)

// Client classification is a compatibility policy, not a second credential.
// Headers and prompts are client supplied; API key authorization always applies.
func claudeCodeClient(r *http.Request, body map[string]json.RawMessage) bool {
	if !claudeClientUA.MatchString(r.UserAgent()) {
		return false
	}
	if !strings.Contains(r.URL.Path, "messages") || strings.HasSuffix(r.URL.Path, "/messages/count_tokens") {
		return true
	}
	var maxTokens float64
	if json.Unmarshal(body["max_tokens"], &maxTokens) == nil && maxTokens == 1 {
		return true
	}
	if r.Header.Get("X-App") == "" || r.Header.Get("Anthropic-Beta") == "" || r.Header.Get("Anthropic-Version") == "" || anthropicMetadataSession(body) == "" {
		return false
	}
	var model string
	if json.Unmarshal(body["model"], &model) != nil {
		return false
	}
	var entries []struct{ Type, Text string }
	if json.Unmarshal(body["system"], &entries) != nil {
		return false
	}
	for _, entry := range entries {
		text := entry.Text
		if strings.HasPrefix(text, "x-anthropic-billing-header") && strings.Contains(text, "cc_entrypoint=") {
			return true
		}
		if entry.Type == "text" && len(text) >= 10000 && strings.HasPrefix(text, "You are a security monitor for autonomous AI coding agents.") {
			all := true
			for _, marker := range []string{"## Threat Model", "- `<transcript>`:", "## HARD BLOCK", "## SOFT BLOCK", "## Classification Process", "## Output Format", "<block>yes</block>", "<block>no</block>"} {
				all = all && strings.Contains(text, marker)
			}
			if all {
				return true
			}
		}
		for _, template := range []string{
			"You are Claude Code, Anthropic's official CLI for Claude.",
			"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
			"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
			"You are a file search specialist for Claude Code, Anthropic's official CLI for Claude.",
			"You are a helpful AI assistant tasked with summarizing conversations.",
			"You are an interactive CLI tool that helps users",
		} {
			if promptSimilarity(text, template) >= 0.5 {
				return true
			}
		}
	}
	return false
}

func promptSimilarity(a, b string) float64 {
	left := []rune(strings.ToLower(strings.Join(strings.Fields(a), " ")))
	right := []rune(strings.ToLower(strings.Join(strings.Fields(b), " ")))
	if len(left) < 2 || len(right) < 2 {
		return 0
	}
	pairs := map[[2]rune]int{}
	for i := 1; i < len(left); i++ {
		pairs[[2]rune{left[i-1], left[i]}]++
	}
	intersection := 0
	for i := 1; i < len(right); i++ {
		pair := [2]rune{right[i-1], right[i]}
		if pairs[pair] > 0 {
			pairs[pair]--
			intersection++
		}
	}
	return float64(2*intersection) / float64(len(left)+len(right)-2)
}

// Target-channel restrictions guard scheduling only. The source channel still
// supplies forwarding aliases, the user's prices and usage attribution.
func (a *App) fallbackChannelRestriction(ctx context.Context, gid int64, platform, model string) ([]modelPrice, string, error) {
	var id int64
	err := a.DB.QueryRowContext(ctx, `SELECT c.id FROM channels c JOIN channel_groups cg ON cg.channel_id=c.id WHERE cg.group_id=$1 AND c.status='active' AND c.restrict_models`, gid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	raw, err := channelJSON(ctx, a.DB, id)
	if err != nil {
		return nil, "", err
	}
	var config struct {
		Pricing []modelPrice                 `json:"model_pricing"`
		Source  string                       `json:"billing_model_source"`
		Mapping map[string]map[string]string `json:"model_mapping"`
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		return nil, "", err
	}
	if config.Source == "upstream" {
		return config.Pricing, config.Source, nil
	}
	if config.Source != "requested" {
		for pattern, target := range config.Mapping[platform] {
			if patternMatches(pattern, model) {
				if target != "" && target != "*" {
					model = target
				}
				break
			}
		}
	}
	if _, ok := matchPrice(config.Pricing, platform, model); !ok {
		return nil, "", denied()
	}
	return nil, "", nil
}
