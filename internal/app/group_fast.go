package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

type fastGroupInput struct {
	ForceFast *bool `json:"force_openai_fast"`
	FreeFast  *bool `json:"free_openai_fast"`
}

func (in fastGroupInput) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	if in.ForceFast == nil && in.FreeFast == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE groups SET
 force_openai_fast=platform IN ('openai','composite') AND COALESCE($2,force_openai_fast),
 free_openai_fast=platform IN ('openai','composite') AND COALESCE($3,free_openai_fast)
 WHERE id=$1`, id, in.ForceFast, in.FreeFast)
	return err
}

func (g *gatewayIdentity) openAIFastScope() bool {
	platform := g.SourcePlatform
	if platform == "" {
		platform = g.Group.Platform
	}
	return platform == "openai" || platform == "composite"
}

// Only OpenAI text wire protocols accept this policy; native Messages, media
// and token counting retain their own request contracts.
func (g *gatewayIdentity) applyFast(body map[string]json.RawMessage, u *upstreamAccount, in textRequest) (string, error) {
	if u.Platform != "openai" || in.CountOnly || (in.Protocol != "responses" && in.Protocol != "chat_completions" && in.Protocol != "anthropic") || (u.protocol() != "responses" && u.protocol() != "chat_completions") {
		return in.Tier, nil
	}
	tier := strings.ToLower(strings.TrimSpace(in.Tier))
	if g.openAIFastScope() && g.Group.ForceFast || tier == "fast" {
		tier = "priority"
	}
	switch tier {
	case "":
		return tier, nil
	case "priority", "flex", "auto", "default", "scale", "ultrafast":
		body["service_tier"], _ = json.Marshal(tier)
		return tier, nil
	default:
		return "", bad("invalid OpenAI service_tier")
	}
}

// An API Key upstream can lower the billed tier, but cannot increase it merely
// by claiming an upgrade. Unknown or absent values keep the outbound request
// tier. The original response itself is forwarded unchanged.
func openAIBillingTier(requested, observed string) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	observed = strings.ToLower(strings.TrimSpace(observed))
	rank := func(tier string) (int, bool) {
		switch tier {
		case "flex":
			return 0, true
		case "", "default", "standard", "auto", "scale":
			return 1, true
		case "priority", "fast":
			return 2, true
		default:
			return 1, false
		}
	}
	want, _ := rank(requested)
	got, known := rank(observed)
	if observed != "" && known && got < want {
		return observed
	}
	return requested
}
