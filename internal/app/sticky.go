package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var legacySessionUser = regexp.MustCompile(`^user_[a-fA-F0-9]{64}_account_[a-fA-F0-9-]*_session_([a-fA-F0-9-]{36})$`)

func anthropicMetadataSession(body map[string]json.RawMessage) string {
	var metadata struct {
		UserID string `json:"user_id"`
	}
	_ = json.Unmarshal(body["metadata"], &metadata)
	raw := strings.TrimSpace(metadata.UserID)
	var fields struct {
		Device  string `json:"device_id"`
		Session string `json:"session_id"`
	}
	if json.Unmarshal([]byte(raw), &fields) == nil && fields.Device != "" && fields.Session != "" {
		return fields.Session
	}
	if match := legacySessionUser.FindStringSubmatch(raw); match != nil {
		return match[1]
	}
	return ""
}

// This is only a scheduling preference. Responses continuation has its own
// mandatory binding, and every candidate still passes normal admission checks.
func gatewaySessionKey(r *http.Request, g *gatewayIdentity, in textRequest, body map[string]json.RawMessage) (string, error) {
	if audioProtocol(in.Protocol) || in.CountOnly || in.Protocol == "embeddings" || grokSearchProtocol(in.Protocol) {
		return "", nil
	}
	seed := ""
	if in.Protocol == "alpha_search" {
		seed = credentialString(body, "id")
		if seed != "" {
			seed = "explicit:" + seed
		}
	}
	if in.Protocol == "anthropic" {
		if session := anthropicMetadataSession(body); session != "" {
			seed = "explicit:" + session
		}
	} else {
		for _, name := range []string{"Session-Id", "Session_id", "Conversation_id", "X-Session-Affinity", "X-Session-Id", "X-OpenCode-Session", "X-Conversation-Id", "X-Grok-Conv-Id"} {
			if name == "X-Grok-Conv-Id" && g.Group.Platform != "grok" {
				continue
			}
			value := strings.TrimSpace(r.Header.Get(name))
			if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
				return "", bad("invalid session header")
			}
			if seed == "" && value != "" {
				seed = "explicit:" + value
			}
		}
		if seed == "" {
			var value string
			_ = json.Unmarshal(body["prompt_cache_key"], &value)
			if value = strings.TrimSpace(value); value != "" {
				seed = "explicit:" + value
			}
		}
	}
	if seed == "" {
		if in.Protocol == "anthropic" {
			seed = anthropicSessionSeed(body)
		} else {
			seed = contentSessionSeed(in.Model, body)
		}
	}
	if seed == "" {
		return "", nil
	}
	if g.Group.Platform == "grok" {
		seed = in.Model + "\n" + seed
	}
	// No client IDs, prompts or credentials are stored. Tenant, group, platform
	// and protocol boundaries prevent unrelated clients from steering each other.
	if g.RoutingGroup != nil {
		seed = fmt.Sprintf("fallback:%d:%s", g.RoutingGroup.ID, seed)
	}
	return fmt.Sprintf("gateway:session:%d:%d:%s:%s:%s", g.Key.ID, g.Key.GroupID, g.Group.Platform, in.Protocol, digest(seed)), nil
}

func canonicalSessionJSON(raw json.RawMessage) string {
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Only stable conversation fields enter the OpenAI-compatible fallback. Later
// turns, output limits and stream mode must not move an existing conversation.
func contentSessionSeed(model string, body map[string]json.RawMessage) string {
	parts := []string{model}
	for _, field := range []string{"tools", "functions", "instructions", "systemInstruction"} {
		parts = append(parts, canonicalSessionJSON(body[field]))
	}
	raw := body["messages"]
	if raw == nil {
		raw = body["input"]
	}
	if raw == nil {
		raw = body["contents"]
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parts = append(parts, text)
	} else {
		var messages []map[string]json.RawMessage
		_ = json.Unmarshal(raw, &messages)
		prefix := true
		for _, message := range messages {
			role := credentialString(message, "role")
			content := message["content"]
			if content == nil {
				content = message["parts"]
			}
			if prefix && (role == "system" || role == "developer") {
				parts = append(parts, role, canonicalSessionJSON(content))
				continue
			}
			prefix = false
			if role == "user" || credentialString(message, "type") == "input_text" {
				if content == nil {
					content = message["text"]
				}
				parts = append(parts, "user", canonicalSessionJSON(content))
				break
			}
		}
	}
	b, _ := json.Marshal(parts)
	return "content:" + string(b)
}

func sessionText(raw json.RawMessage, cachedOnly bool) string {
	var text string
	if !cachedOnly && json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type, Text string
		Cache      struct{ Type string } `json:"cache_control"`
	}
	_ = json.Unmarshal(raw, &blocks)
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" && (!cachedOnly || block.Cache.Type == "ephemeral") {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func anthropicSessionSeed(body map[string]json.RawMessage) string {
	var messages []struct{ Content json.RawMessage }
	_ = json.Unmarshal(body["messages"], &messages)
	for _, message := range messages {
		if sessionText(message.Content, true) != "" {
			return "cached:" + sessionText(message.Content, false)
		}
	}
	if text := sessionText(body["system"], true); text != "" {
		return "cached:" + text
	}
	parts := []string{sessionText(body["system"], false)}
	for _, message := range messages {
		parts = append(parts, sessionText(message.Content, false))
	}
	b, _ := json.Marshal(parts)
	return "messages:" + string(b)
}

func (a *App) stickySession(ctx context.Context, key string) (*responseBinding, error) {
	if key == "" {
		return nil, nil
	}
	raw, err := a.Redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, &apiError{503, "session affinity is unavailable"}
	}
	var binding responseBinding
	if json.Unmarshal(raw, &binding) != nil || binding.AccountID <= 0 || binding.Target == "" {
		return nil, nil // Corrupt soft affinity cannot authorize or pin an account.
	}
	return &binding, nil
}

func (a *App) bindSession(ctx context.Context, key string, u *upstreamAccount) error {
	if key == "" {
		return nil
	}
	raw, _ := json.Marshal(responseBinding{AccountID: u.ID, Target: responseTarget(u)})
	// ponytail: simultaneous first turns may choose different accounts; add a
	// per-session queue only if serialized conversation dispatch is required.
	if err := a.Redis.Set(ctx, key, raw, time.Hour).Err(); err != nil {
		return &apiError{503, "session affinity could not be saved"}
	}
	return nil
}
