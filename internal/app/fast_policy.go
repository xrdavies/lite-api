package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

const fastPolicySetting = "openai_fast_policy_settings"

type fastPolicyRule struct {
	Tier            string   `json:"service_tier"`
	Action          string   `json:"action"`
	Scope           string   `json:"scope"`
	Users           []int64  `json:"user_ids,omitempty"`
	Message         string   `json:"error_message,omitempty"`
	Models          []string `json:"model_whitelist,omitempty"`
	Fallback        string   `json:"fallback_action,omitempty"`
	FallbackMessage string   `json:"fallback_error_message,omitempty"`
}

type fastPolicySettings struct {
	Rules []fastPolicyRule `json:"rules"`
}

func (s *fastPolicySettings) validate() error {
	if len(s.Rules) > 100 {
		return bad("too many fast policy rules")
	}
	if s.Rules == nil {
		s.Rules = []fastPolicyRule{}
	}
	actions := []string{"pass", "filter", "block", "force_priority"}
	for i := range s.Rules {
		r := &s.Rules[i]
		r.Tier = strings.ToLower(strings.TrimSpace(r.Tier))
		if r.Tier == "" {
			r.Tier = "all"
		}
		if !slices.Contains([]string{"all", "priority", "ultrafast", "flex", "missing"}, r.Tier) ||
			!slices.Contains(actions, r.Action) || r.Fallback != "" && !slices.Contains(actions, r.Fallback) ||
			!slices.Contains([]string{"all", "apikey"}, r.Scope) || len(r.Users) > 1000 || len(r.Models) > 100 || len(r.Message) > 2000 || len(r.FallbackMessage) > 2000 {
			return bad("invalid fast policy rule")
		}
		seen := map[int64]bool{}
		for _, user := range r.Users {
			if user <= 0 || seen[user] {
				return bad("fast policy user_ids must be unique positive IDs")
			}
			seen[user] = true
		}
		for j, pattern := range r.Models {
			pattern = strings.TrimSpace(pattern)
			if !validModelPattern(pattern) {
				return bad("invalid fast policy model pattern")
			}
			r.Models[j] = pattern
		}
	}
	return nil
}

func (a *App) loadFastPolicy(ctx context.Context) (*fastPolicySettings, error) {
	s := &fastPolicySettings{Rules: []fastPolicyRule{}}
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", fastPolicySetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(raw) == "" {
		return s, nil
	}
	if err != nil {
		return nil, &apiError{503, "fast policy unavailable"}
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), s) != nil || s.validate() != nil {
		// A corrupt stored rule must not silently disable an administrator block.
		return nil, &apiError{503, "invalid stored fast policy"}
	}
	return s, nil
}

func (s *fastPolicySettings) action(user int64, model, tier string) (string, string) {
	for _, scoped := range []bool{true, false} {
		for _, rule := range s.Rules {
			if (len(rule.Users) > 0) != scoped || scoped && !slices.Contains(rule.Users, user) || rule.Scope != "all" && rule.Scope != "apikey" {
				continue
			}
			if tier == "" {
				if rule.Tier != "missing" {
					continue
				}
			} else if rule.Tier != "all" && rule.Tier != tier {
				continue
			}
			if len(rule.Models) == 0 || slices.ContainsFunc(rule.Models, func(p string) bool {
				return p == model || strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*"))
			}) {
				return rule.Action, rule.Message
			}
			if rule.Fallback != "" {
				return rule.Fallback, rule.FallbackMessage
			}
			return "pass", ""
		}
	}
	return "pass", ""
}

func (s *fastPolicySettings) apply(body map[string]json.RawMessage, user int64, model, platform, tier string) (string, error) {
	action, message := s.action(user, model, tier)
	if tier == "" {
		// Legacy all rules never upgrade omitted tiers. Missing only opts an
		// OpenAI target into priority; block/filter on missing remain no-ops.
		if platform != "openai" || action != "force_priority" {
			return "", nil
		}
	}
	switch action {
	case "block":
		if message == "" {
			message = "service_tier=" + tier + " is not allowed for model " + model
		}
		return "", &apiError{403, message}
	case "filter":
		delete(body, "service_tier")
		return "", nil
	case "force_priority":
		body["service_tier"] = json.RawMessage(`"priority"`)
		return "priority", nil
	default:
		return tier, nil
	}
}

// HTTP attempts share one snapshot; WebSocket turns inherit the snapshot taken
// at authenticated connection setup. Background tasks retain the resulting tier.
type fastPolicyKey struct{}

func fastPolicyProtocol(u *upstreamAccount, in textRequest) bool {
	return !in.CountOnly && (in.Protocol == "responses" || in.Protocol == "chat_completions" || in.Protocol == "anthropic") && (u.protocol() == "responses" || u.protocol() == "chat_completions")
}
