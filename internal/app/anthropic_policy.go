package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
)

const betaSetting = "beta_policy_settings"
const rectifierSetting = "rectifier_settings"

type betaRule struct {
	Token           string   `json:"beta_token"`
	Action          string   `json:"action"`
	Scope           string   `json:"scope"`
	Message         string   `json:"error_message,omitempty"`
	Models          []string `json:"model_whitelist,omitempty"`
	Fallback        string   `json:"fallback_action,omitempty"`
	FallbackMessage string   `json:"fallback_error_message,omitempty"`
}
type betaSettings struct {
	Rules []betaRule `json:"rules"`
}
type rectifierSettings struct {
	Enabled   bool     `json:"enabled"`
	Signature bool     `json:"thinking_signature_enabled"`
	Budget    bool     `json:"thinking_budget_enabled"`
	APIKey    bool     `json:"apikey_signature_enabled"`
	Patterns  []string `json:"apikey_signature_patterns"`
}

// Fixed compatibility defaults; administrators control the actual supplier policy.
func defaultBetaSettings() betaSettings {
	return betaSettings{Rules: []betaRule{
		{Token: "fast-mode-2026-02-01", Action: "filter", Scope: "all"},
		{Token: "context-1m-2025-08-07", Action: "pass", Scope: "all", Fallback: "filter", Models: []string{
			"claude-sonnet-5", "claude-sonnet-5-*", "claude-sonnet-5@*",
			"us.anthropic.claude-sonnet-5*", "eu.anthropic.claude-sonnet-5*", "apac.anthropic.claude-sonnet-5*",
			"jp.anthropic.claude-sonnet-5*", "au.anthropic.claude-sonnet-5*", "us-gov.anthropic.claude-sonnet-5*",
			"global.anthropic.claude-sonnet-5*", "anthropic.claude-sonnet-5*",
		}},
	}}
}
func defaultRectifierSettings() rectifierSettings {
	return rectifierSettings{Enabled: true, Signature: true, Budget: true, Patterns: []string{}}
}

func (s *betaSettings) validate() error {
	if len(s.Rules) > 100 {
		return bad("too many beta policy rules")
	}
	if s.Rules == nil {
		s.Rules = []betaRule{}
	}
	for i := range s.Rules {
		r := &s.Rules[i]
		if r.Token == "" || len(r.Token) > 200 || strings.ContainsAny(r.Token, ", \t\r\n\x00") ||
			!slices.Contains([]string{"pass", "filter", "block"}, r.Action) ||
			!slices.Contains([]string{"", "pass", "filter", "block"}, r.Fallback) ||
			!slices.Contains([]string{"all", "apikey"}, r.Scope) || len(r.Models) > 100 || len(r.Message) > 2000 || len(r.FallbackMessage) > 2000 {
			return bad("invalid beta policy rule")
		}
		for j, p := range r.Models {
			p = strings.TrimSpace(p)
			if !validModelPattern(p) {
				return bad("invalid beta model pattern")
			}
			r.Models[j] = p
		}
	}
	return nil
}
func (s *rectifierSettings) validate() error {
	if len(s.Patterns) > 50 {
		return bad("too many signature patterns (maximum 50)")
	}
	clean := []string{}
	for _, p := range s.Patterns {
		p = strings.TrimSpace(p)
		if len(p) > 500 {
			return bad("signature pattern exceeds 500 bytes")
		}
		if p != "" {
			clean = append(clean, p)
		}
	}
	s.Patterns = clean
	return nil
}

func (a *App) loadBetaSettings(ctx context.Context) (betaSettings, error) {
	var s betaSettings
	ok, err := a.readRuntimeSetting(ctx, betaSetting, &s)
	if !ok {
		return defaultBetaSettings(), err
	}
	if err = s.validate(); err != nil {
		return s, &apiError{503, "invalid stored beta policy"}
	}
	return s, nil
}
func (a *App) loadRectifierSettings(ctx context.Context) (rectifierSettings, error) {
	var s rectifierSettings
	ok, err := a.readRuntimeSetting(ctx, rectifierSetting, &s)
	if !ok {
		return defaultRectifierSettings(), err
	}
	if err = s.validate(); err != nil {
		return s, &apiError{503, "invalid stored rectifier settings"}
	}
	return s, nil
}
func (a *App) betaConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadBetaSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var s *betaSettings
	if err := decode(w, r, &s); err != nil {
		return err
	}
	if s == nil {
		return bad("settings must be an object")
	}
	if err := s.validate(); err != nil {
		return err
	}
	if err := a.writeRuntimeSetting(r.Context(), betaSetting, s); err != nil {
		return err
	}
	return reply(w, s)
}
func (a *App) rectifierConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadRectifierSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var s *rectifierSettings
	if err := decode(w, r, &s); err != nil {
		return err
	}
	if s == nil {
		return bad("settings must be an object")
	}
	if err := s.validate(); err != nil {
		return err
	}
	if err := a.writeRuntimeSetting(r.Context(), rectifierSetting, s); err != nil {
		return err
	}
	return reply(w, s)
}

func (s betaSettings) apply(header, model string) (string, error) {
	tokens := strings.Split(header, ",")
	filtered := map[string]bool{}
	for _, r := range s.Rules {
		if r.Scope != "all" && r.Scope != "apikey" {
			continue
		}
		action, message := r.Action, r.Message
		if len(r.Models) > 0 && !slices.ContainsFunc(r.Models, func(p string) bool {
			return p == model || strings.HasSuffix(p, "*") && strings.HasPrefix(model, strings.TrimSuffix(p, "*"))
		}) {
			action, message = r.Fallback, r.FallbackMessage
		}
		present := slices.ContainsFunc(tokens, func(t string) bool { return strings.TrimSpace(t) == r.Token })
		if action == "block" && present {
			if message == "" {
				message = "beta feature " + r.Token + " is not allowed"
			}
			return "", bad(message)
		}
		if action == "filter" {
			filtered[r.Token] = true
		}
	}
	out := []string{}
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t != "" && !filtered[t] && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return strings.Join(out, ","), nil
}

// All Anthropic HTTP callers (native, converted and health tests) share this
// boundary. Retries stay on the same account, before any successful body is read.
func (a *App) upstreamRequestHeaders(ctx context.Context, u *upstreamAccount, method, path string, body []byte, headers http.Header) (*http.Response, error) {
	anthropic := u.protocol() == "anthropic" && method == "POST" && (path == "/v1/messages" || path == "/v1/messages/count_tokens")
	if !anthropic {
		return a.sendUpstreamRequest(ctx, u, method, path, body, headers)
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, bad("invalid Anthropic request")
	}
	model := credentialString(request, "model")
	if beta := strings.Join(headers.Values("Anthropic-Beta"), ","); beta != "" {
		policy := defaultBetaSettings()
		if a.DB != nil {
			var err error
			policy, err = a.loadBetaSettings(ctx)
			if err != nil {
				return nil, &apiError{503, "beta policy unavailable"}
			}
		}
		filtered, err := policy.apply(beta, model)
		if err != nil {
			return nil, err
		}
		headers = headers.Clone()
		headers.Set("Anthropic-Beta", filtered)
	}
	resp, err := a.sendUpstreamRequest(ctx, u, method, path, body, headers)
	if err != nil || resp.StatusCode != 400 || path != "/v1/messages" {
		return resp, err
	}
	s := defaultRectifierSettings()
	if a.DB != nil {
		s, err = a.loadRectifierSettings(ctx)
		if err != nil {
			return resp, nil // An unavailable switch must never authorize mutation.
		}
	}
	if !s.Enabled || !s.APIKey && !s.Budget {
		return resp, nil
	}
	const limit = 64 << 10
	failure, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(failure) > limit {
		resp.Body.Close()
		return nil, &apiError{502, "invalid upstream error response"}
	}
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(failure))
	var payload struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(failure, &payload) != nil || payload.Error.Message == "" {
		return resp, nil
	}
	modified, changed := s.rectify(body, model, payload.Error.Message)
	if !changed {
		return resp, nil
	}
	resp.Body.Close()
	// One confirmed rejection permits one corrective retry. Never retry a
	// transport failure or successful response, or downgrade tool calls to text.
	return a.sendUpstreamRequest(ctx, u, method, path, modified, headers)
}

func (s rectifierSettings) rectify(raw []byte, model, message string) ([]byte, bool) {
	var body map[string]json.RawMessage
	if !s.Enabled || json.Unmarshal(raw, &body) != nil || body == nil {
		return raw, false
	}
	msg := strings.ToLower(message)
	strict := slices.ContainsFunc([]string{"claude-", "opus-", "sonnet-", "haiku-"}, func(p string) bool { return strings.HasPrefix(strings.ToLower(model), p) })
	signature := strings.Contains(msg, "signature") || strings.Contains(msg, "thinking") && (strings.Contains(msg, "expected") || strings.Contains(msg, "cannot be modified") || strings.Contains(msg, "block must contain")) ||
		strings.Contains(msg, "empty content") || strings.Contains(msg, "content blocks must be non-empty") || slices.ContainsFunc(s.Patterns, func(p string) bool {
		return strings.TrimSpace(p) != "" && strings.Contains(msg, strings.ToLower(strings.TrimSpace(p)))
	})
	changed := false
	if s.APIKey && strict && signature {
		if body["thinking"] != nil {
			delete(body, "thinking")
			changed = true
		}
		var messages []map[string]json.RawMessage
		if json.Unmarshal(body["messages"], &messages) != nil {
			return raw, false
		}
		for _, m := range messages {
			var blocks []json.RawMessage
			if json.Unmarshal(m["content"], &blocks) != nil || string(m["content"]) == "null" {
				continue
			}
			out := []json.RawMessage{}
			for _, b := range blocks {
				var block map[string]json.RawMessage
				if json.Unmarshal(b, &block) == nil {
					kind := credentialString(block, "type")
					if kind == "thinking" || kind == "" && block["thinking"] != nil {
						changed = true
						if text := credentialString(block, "thinking"); text != "" {
							value, _ := json.Marshal(map[string]string{"type": "text", "text": text})
							out = append(out, value)
						}
						continue
					}
					if kind == "redacted_thinking" || kind == "text" && credentialString(block, "text") == "" {
						changed = true
						continue
					}
				}
				out = append(out, b)
			}
			if len(out) == 0 {
				out = []json.RawMessage{json.RawMessage(`{"type":"text","text":"(content removed)"}`)}
				changed = true
			}
			m["content"], _ = json.Marshal(out)
		}
		body["messages"], _ = json.Marshal(messages)
		var cm map[string]json.RawMessage
		var edits []json.RawMessage
		if json.Unmarshal(body["context_management"], &cm) == nil && cm != nil && json.Unmarshal(cm["edits"], &edits) == nil {
			kept := []json.RawMessage{}
			for _, e := range edits {
				var edit map[string]json.RawMessage
				_ = json.Unmarshal(e, &edit)
				if credentialString(edit, "type") == "clear_thinking_20251015" {
					changed = true
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 {
				delete(cm, "edits")
			} else {
				cm["edits"], _ = json.Marshal(kept)
			}
			body["context_management"], _ = json.Marshal(cm)
		}
	} else if s.Budget && ((strings.Contains(msg, "budget_tokens") || strings.Contains(msg, "budget tokens")) && strings.Contains(msg, "thinking") &&
		(strings.Contains(msg, ">= 1024") || strings.Contains(msg, "greater than or equal to 1024") || strings.Contains(msg, "1024") && strings.Contains(msg, "input should be")) ||
		strings.Contains(msg, "must be greater than 1024 to reserve tokens for a final answer") && strings.Contains(msg, "baseten reasoning is enabled")) {
		thinking := map[string]json.RawMessage{}
		if body["thinking"] != nil && (json.Unmarshal(body["thinking"], &thinking) != nil || thinking == nil) {
			return raw, false
		}
		if credentialString(thinking, "type") == "adaptive" {
			return raw, false
		}
		var budget, maximum int64
		_ = json.Unmarshal(thinking["budget_tokens"], &budget)
		_ = json.Unmarshal(body["max_tokens"], &maximum)
		changed = credentialString(thinking, "type") != "enabled" || budget != 32000 || maximum < 32001
		thinking["type"], thinking["budget_tokens"] = json.RawMessage(`"enabled"`), json.RawMessage("32000")
		body["thinking"], _ = json.Marshal(thinking)
		if maximum < 32001 {
			body["max_tokens"] = json.RawMessage("64000")
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(body)
	return out, err == nil
}
