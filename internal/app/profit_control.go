package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
)

type profitPolicy struct {
	Enabled bool        `json:"profit_control_enabled"`
	Margin  json.Number `json:"profit_min_margin"`
	Buffer  json.Number `json:"profit_safety_buffer"`
}

type profitInput struct {
	Enabled *bool        `json:"profit_control_enabled"`
	Margin  *json.Number `json:"profit_min_margin"`
	Buffer  *json.Number `json:"profit_safety_buffer"`
}

// Only client text execution installs this marker. Internal callers using the
// Gemini wire for batch images or model discovery must not inherit the gate.
type profitRequestKey struct{}

func profitPlatform(platform string) bool {
	return platform == "openai" || platform == "anthropic" || platform == "gemini" || platform == "grok"
}

func validProfitRatio(n json.Number) bool {
	return validPrice(&n, 1, 4) && rat(n).Cmp(big.NewRat(1, 1)) < 0
}

func (in profitInput) apply(ctx context.Context, tx *sql.Tx, id int64, create bool) error {
	if in.Enabled == nil && in.Margin == nil && in.Buffer == nil {
		return nil
	}
	var platform string
	var p profitPolicy
	err := tx.QueryRowContext(ctx, `SELECT platform,COALESCE($2,profit_control_enabled),
 COALESCE($3::text,profit_min_margin::text),COALESCE($4::text,profit_safety_buffer::text)
 FROM groups WHERE id=$1`, id, in.Enabled, in.Margin, in.Buffer).Scan(&platform, &p.Enabled, &p.Margin, &p.Buffer)
	if err != nil {
		return err
	}
	if !profitPlatform(platform) {
		if create && p.Enabled {
			return bad("profit control requires an OpenAI, Anthropic, Gemini or Grok group")
		}
		p = profitPolicy{Margin: "0", Buffer: "0"}
	} else if p.Enabled {
		if !validProfitRatio(p.Margin) || !validProfitRatio(p.Buffer) || new(big.Rat).Add(rat(p.Margin), rat(p.Buffer)).Cmp(big.NewRat(1, 1)) >= 0 {
			return bad("profit margin and buffer must be nonnegative decimals with at most four places and sum below one")
		}
	} else {
		if !validProfitRatio(p.Margin) {
			p.Margin = "0"
		}
		if !validProfitRatio(p.Buffer) {
			p.Buffer = "0"
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE groups SET profit_control_enabled=$2,profit_min_margin=$3,profit_safety_buffer=$4 WHERE id=$1", id, p.Enabled, p.Margin, p.Buffer)
	return err
}

// The billing group's effective user rate stays fixed for one admitted request.
// Routing may delegate the policy to a fallback group, never its billing rate.
func (g *gatewayIdentity) profitThreshold(in textRequest) *big.Rat {
	if in.CountOnly || (in.Protocol != "responses" && in.Protocol != "chat_completions" && in.Protocol != "anthropic" && in.Protocol != "gemini") {
		return nil
	}
	routing := g.dispatchGroup()
	platform := routing.Platform
	if g.RoutingGroup == nil && g.SourcePlatform != "" {
		platform = g.SourcePlatform
	}
	if !routing.profitPolicy.Enabled || !profitPlatform(platform) {
		return nil
	}
	margin, buffer, rate := rat(routing.Margin), rat(routing.Buffer), rat(g.Group.Rate)
	if margin == nil || buffer == nil || rate == nil {
		return new(big.Rat)
	}
	threshold := new(big.Rat).Sub(big.NewRat(1, 1), new(big.Rat).Add(margin, buffer))
	threshold.Mul(threshold, rate)
	if threshold.Sign() < 0 {
		threshold.SetInt64(0)
	}
	return threshold
}

func profitAllows(threshold *big.Rat, rate json.Number) bool {
	if threshold == nil {
		return true
	}
	upstream := rat(rate)
	return upstream != nil && upstream.Sign() >= 0 && upstream.Cmp(threshold) <= 0
}
