package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

type messagesDispatchConfig struct {
	Opus   string            `json:"opus_mapped_model,omitempty"`
	Sonnet string            `json:"sonnet_mapped_model,omitempty"`
	Haiku  string            `json:"haiku_mapped_model,omitempty"`
	Exact  map[string]string `json:"exact_model_mappings,omitempty"`
}

type messagesDispatchInput struct {
	AllowMessages *bool                   `json:"allow_messages_dispatch"`
	DefaultModel  *string                 `json:"default_mapped_model"`
	MessagesModel *messagesDispatchConfig `json:"messages_dispatch_model_config"`
}

// Compatibility normalization applies only to explicit GPT reasoning suffixes;
// arbitrary relay model IDs and ordinary model requests retain their spelling.
func normalizeDispatchModel(model string) string {
	model = strings.TrimSpace(model)
	id := model[strings.LastIndex(model, "/")+1:]
	parts := strings.FieldsFunc(strings.ToLower(id), func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	if !strings.HasPrefix(strings.ToLower(id), "gpt-") || len(parts) < 2 {
		return model
	}
	suffix := parts[len(parts)-1]
	if suffix != "none" && suffix != "minimal" && suffix != "low" && suffix != "medium" && suffix != "high" && suffix != "xhigh" && suffix != "extrahigh" {
		return model
	}
	id = strings.Join(parts, "-")
	id = strings.NewReplacer("gpt-5.4mini", "gpt-5.4-mini", "gpt-5.4nano", "gpt-5.4-nano", "gpt-5.3-codexspark", "gpt-5.3-codex-spark", "gpt-5.3codexspark", "gpt-5.3-codex-spark", "gpt-5.3codex", "gpt-5.3-codex").Replace(id)
	for _, family := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5-pro", "gpt-5.5", "gpt-5.4-mini", "gpt-5.4-nano", "gpt-5.4", "gpt-5.2", "gpt-5.3-codex-spark", "gpt-5.3-codex"} {
		if strings.Contains(id, family) {
			return family
		}
	}
	if strings.TrimSuffix(id, "-"+suffix) == "gpt-5.6" {
		return "gpt-5.6-sol"
	}
	if strings.HasPrefix(id, "gpt-5.6-") {
		return model
	}
	if strings.Contains(id, "gpt-5.3") || strings.Contains(id, "codex") {
		return "gpt-5.3-codex"
	}
	if strings.Contains(id, "gpt-5") {
		return "gpt-5.4"
	}
	return model
}

func (c *messagesDispatchConfig) normalize() error {
	valid := func(v string) bool {
		return v == "" || len(v) <= 100 && validModelPattern(v) && !strings.Contains(v, "*")
	}
	for _, field := range []*string{&c.Opus, &c.Sonnet, &c.Haiku} {
		*field = normalizeDispatchModel(*field)
		if !valid(*field) {
			return bad("invalid Messages dispatch model")
		}
	}
	if len(c.Exact) > 1000 {
		return bad("too many Messages dispatch mappings")
	}
	exact := map[string]string{}
	for from, to := range c.Exact {
		from, to = strings.TrimSpace(from), normalizeDispatchModel(to)
		if from == "" || to == "" {
			continue
		}
		if !valid(from) || !valid(to) {
			return bad("invalid Messages dispatch mapping")
		}
		if previous, ok := exact[from]; ok && previous != to {
			return bad("conflicting Messages dispatch mappings")
		}
		exact[from] = to
	}
	c.Exact = exact
	return nil
}

func (in messagesDispatchInput) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	if in.AllowMessages == nil && in.DefaultModel == nil && in.MessagesModel == nil {
		return nil
	}
	if in.DefaultModel != nil && (len(*in.DefaultModel) > 100 || strings.ContainsAny(*in.DefaultModel, "\r\n\x00")) {
		return bad("invalid default_mapped_model")
	}
	var config any
	if in.MessagesModel != nil {
		if err := in.MessagesModel.normalize(); err != nil {
			return err
		}
		raw, _ := json.Marshal(in.MessagesModel)
		config = string(raw)
	}
	_, err := tx.ExecContext(ctx, `UPDATE groups SET
 allow_messages_dispatch=platform IN ('openai','composite') AND COALESCE($2,allow_messages_dispatch),
 default_mapped_model=CASE WHEN platform='openai' THEN COALESCE($3,default_mapped_model) ELSE '' END,
 messages_dispatch_model_config=CASE WHEN platform='openai' THEN COALESCE($4::jsonb,messages_dispatch_model_config) ELSE '{}'::jsonb END
 WHERE id=$1`, id, in.AllowMessages, in.DefaultModel, config)
	return err
}

func (c messagesDispatchConfig) resolve(model string) string {
	model = strings.TrimSpace(model)
	if target := c.Exact[model]; target != "" {
		return target
	}
	lower := strings.ToLower(model)
	if strings.HasPrefix(lower, "claude") {
		for _, family := range []struct{ name, configured, fallback string }{
			{"opus", c.Opus, "gpt-5.4"}, {"sonnet", c.Sonnet, "gpt-5.3-codex"}, {"haiku", c.Haiku, "gpt-5.4-mini"},
		} {
			if strings.Contains(lower, family.name) {
				if family.configured != "" {
					return family.configured
				}
				return family.fallback
			}
		}
	}
	return ""
}

func (g *gatewayIdentity) messagesDispatch(in textRequest) (string, error) {
	if in.Protocol != "anthropic" || g.Group.Platform != "openai" {
		return "", nil
	}
	group := g.dispatchGroup()
	if !group.AllowMessages {
		return "", &apiError{403, "this group does not allow Messages dispatch"}
	}
	// default_mapped_model is a stored legacy field, not a forwarding fallback.
	return group.MessagesModel.resolve(in.Model), nil
}
