package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

const geminiQuotaSetting = "gemini_quota_policy"

type geminiQuota struct {
	SharedDay, SharedMinute, ProDay, ProMinute, FlashDay, FlashMinute int64
}
type geminiModelQuota struct {
	Day    *int64 `json:"rpd"`
	Minute *int64 `json:"rpm"`
}
type geminiQuotaRule struct {
	SharedDay    *int64            `json:"shared_rpd"`
	SharedMinute *int64            `json:"rpm"`
	Pro          *geminiModelQuota `json:"gemini_pro"`
	Flash        *geminiModelQuota `json:"gemini_flash"`
	Description  *string           `json:"desc"`
}
type geminiLegacyQuota struct {
	ProDay   *int64 `json:"pro_rpd"`
	FlashDay *int64 `json:"flash_rpd"`
	Cooldown *int64 `json:"cooldown_minutes"`
}
type geminiQuotaPolicy struct {
	Rules map[string]geminiQuotaRule   `json:"quota_rules"`
	Tiers map[string]geminiLegacyQuota `json:"tiers"`
}

func parseGeminiQuotaPolicy(raw []byte, strict bool) (geminiQuotaPolicy, error) {
	var p geminiQuotaPolicy
	if len(raw) > 32<<10 || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return p, bad("Gemini quota policy must be an object of at most 32 KiB")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	if strict {
		d.DisallowUnknownFields()
	}
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, bad("invalid Gemini quota policy")
	}
	if !strict {
		return p, nil
	}
	checkTier := func(name string, seen map[string]bool) error {
		tier := strings.ToLower(strings.TrimSpace(name))
		if tier != "aistudio_free" && tier != "aistudio_paid" || seen[tier] {
			return bad("Gemini quota tiers must be unique aistudio_free or aistudio_paid names")
		}
		seen[tier] = true
		return nil
	}
	valid := func(n *int64, low int64) bool { return n == nil || *n >= low }
	seen := map[string]bool{}
	for tier, r := range p.Rules {
		if err := checkTier(tier, seen); err != nil {
			return p, err
		}
		if !valid(r.SharedDay, -1) || !valid(r.SharedMinute, 0) || r.Description != nil && (len(*r.Description) > 2048 || strings.ContainsRune(*r.Description, 0)) {
			return p, bad("invalid Gemini shared quota or description")
		}
		for _, q := range []*geminiModelQuota{r.Pro, r.Flash} {
			if q != nil && (!valid(q.Day, -1) || !valid(q.Minute, 0)) {
				return p, bad("Gemini rpd must be -1 or nonnegative; rpm must be nonnegative")
			}
		}
	}
	seen = map[string]bool{}
	for tier, r := range p.Tiers {
		if err := checkTier(tier, seen); err != nil {
			return p, err
		}
		if !valid(r.ProDay, -1) || !valid(r.FlashDay, -1) || !valid(r.Cooldown, 0) {
			return p, bad("invalid legacy Gemini quota")
		}
	}
	return p, nil
}

func geminiTier(u *upstreamAccount) string {
	if strings.EqualFold(strings.TrimSpace(credentialString(u.Credentials, "tier_id")), "aistudio_paid") {
		return "aistudio_paid"
	}
	return "aistudio_free"
}

func (p geminiQuotaPolicy) apply(q *geminiQuota, tier string) bool {
	setDay := func(target *int64, n *int64) {
		if n != nil {
			*target = *n
			if *n < -1 {
				*target = 0 // Preserve legacy stored-value normalization.
			}
		}
	}
	setMinute := func(target *int64, n *int64) {
		if n != nil {
			*target = max(0, *n)
		}
	}
	// Each layer prefers nonempty V2 rules over V1 tiers, as in the stored contract.
	if len(p.Rules) > 0 {
		for name, r := range p.Rules {
			if strings.ToLower(strings.TrimSpace(name)) != tier {
				continue
			}
			setDay(&q.SharedDay, r.SharedDay)
			setMinute(&q.SharedMinute, r.SharedMinute)
			if r.Pro != nil {
				setDay(&q.ProDay, r.Pro.Day)
				setMinute(&q.ProMinute, r.Pro.Minute)
			}
			if r.Flash != nil {
				setDay(&q.FlashDay, r.Flash.Day)
				setMinute(&q.FlashMinute, r.Flash.Minute)
			}
			return true
		}
		return false
	}
	for name, r := range p.Tiers {
		if strings.ToLower(strings.TrimSpace(name)) != tier {
			continue
		}
		if q.SharedDay > 0 {
			setDay(&q.SharedDay, r.ProDay)
		} else {
			setDay(&q.ProDay, r.ProDay)
		}
		if q.SharedDay <= 0 {
			setDay(&q.FlashDay, r.FlashDay)
		}
		// cooldown_minutes belongs to excluded authentication branches. API
		// keys use upstream reset hints or the next Los Angeles midnight.
		return true
	}
	return false
}

func (a *App) geminiQuotaOverride(ctx context.Context) (json.RawMessage, error) {
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", geminiQuotaSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || err == nil && strings.TrimSpace(raw) == "" {
		return json.RawMessage(`{}`), nil
	}
	if err != nil {
		return nil, err
	}
	if _, err = parseGeminiQuotaPolicy([]byte(raw), false); err != nil {
		return nil, &apiError{503, "stored Gemini quota policy is invalid"}
	}
	return json.RawMessage(raw), nil
}

func (a *App) accountGeminiQuota(ctx context.Context, u *upstreamAccount) (geminiQuota, string, error) {
	tier := geminiTier(u)
	q := geminiQuota{ProDay: 50, FlashDay: 1500, ProMinute: 2, FlashMinute: 15}
	if tier == "aistudio_paid" {
		q = geminiQuota{ProDay: -1, FlashDay: -1, ProMinute: 1000, FlashMinute: 2000}
	}
	basis := "compatibility_default"
	if a.geminiQuotaPolicy.apply(&q, tier) {
		basis = "deployment_policy"
	}
	raw, err := a.geminiQuotaOverride(ctx)
	if err != nil {
		return q, basis, err
	}
	p, _ := parseGeminiQuotaPolicy(raw, false)
	if p.apply(&q, tier) {
		basis = "configured_policy"
	}
	return q, basis, nil
}

var geminiRetryIn = regexp.MustCompile(`(?i)please retry in ([0-9]+(?:\.[0-9]+)?)s`)

func geminiRateLimitSeconds(body []byte, retry string, now time.Time) int64 {
	_, midnight := geminiUsageDay(now)
	daily := func() int64 { return max(1, int64((midnight.Sub(now)+time.Second-1)/time.Second)) }
	delay := func(raw string, cap time.Duration) int64 {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			d = min(d, cap)
			return int64((d + time.Second - 1) / time.Second)
		}
		return 0
	}
	var response struct {
		Error struct {
			Message string
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string
				Metadata   struct{ QuotaResetDelay string }
			}
		}
	}
	if len(body) <= maxBalanceBody && json.Unmarshal(body, &response) == nil {
		if strings.Contains(strings.ToLower(response.Error.Message), "per day") {
			return daily()
		}
		for _, d := range response.Error.Details {
			if n := delay(d.Metadata.QuotaResetDelay, 24*time.Hour); n > 0 {
				return n
			}
			if d.Type == "type.googleapis.com/google.rpc.RetryInfo" {
				if n := delay(d.RetryDelay, 15*time.Minute); n > 0 {
					return n
				}
			}
		}
		if match := geminiRetryIn.FindStringSubmatch(response.Error.Message); len(match) == 2 {
			if n := delay(match[1]+"s", 24*time.Hour); n > 0 {
				return n
			}
		}
	}
	if n := retryAfterSeconds(retry, now, 7200); n > 0 {
		return n
	}
	return daily()
}
