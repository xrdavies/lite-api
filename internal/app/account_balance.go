package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxBalanceBody = 256 << 10

type balanceCheckPolicy struct {
	Enabled   bool
	Threshold json.Number
	Interval  time.Duration
}

func parseBalancePolicy(cfg Config) (balanceCheckPolicy, error) {
	p := balanceCheckPolicy{Enabled: true, Threshold: "0.5", Interval: 10 * time.Minute}
	var err error
	if cfg.BalanceCheckEnabled != "" {
		p.Enabled, err = strconv.ParseBool(cfg.BalanceCheckEnabled)
		if err != nil {
			return p, errors.New("invalid balance check enabled setting")
		}
	}
	if cfg.BalanceThreshold != "" {
		p.Threshold = json.Number(cfg.BalanceThreshold)
	}
	if !validPrice(&p.Threshold, 16, 12) {
		return p, errors.New("invalid balance threshold")
	}
	if cfg.BalanceCheckIntervalMinutes != "" {
		minutes, err := strconv.Atoi(cfg.BalanceCheckIntervalMinutes)
		if err != nil || minutes < 1 || minutes > 1440 {
			return p, errors.New("balance check interval must be 1 to 1440 minutes")
		}
		p.Interval = time.Duration(minutes) * time.Minute
	}
	return p, nil
}

type providerBalanceEntry struct {
	Currency string      `json:"currency"`
	Balance  json.Number `json:"balance"`
}

type providerBalance struct {
	Provider   string                 `json:"provider"`
	Success    bool                   `json:"success"`
	Balance    json.Number            `json:"balance"`
	Currency   string                 `json:"currency,omitempty"`
	Balances   []providerBalanceEntry `json:"balances,omitempty"`
	Available  bool                   `json:"available"`
	StatusCode int                    `json:"status_code,omitempty"`
	FetchedAt  int64                  `json:"fetched_at"`
	Persisted  bool                   `json:"persisted"`
	Error      string                 `json:"error,omitempty"`
}

func balanceAccount(u *upstreamAccount) bool {
	mode := credentialString(u.Credentials, "account_mode")
	return u.Type == "apikey" && (u.Platform == "kimi" || u.Platform == "deepseek") && (mode == "" || mode == "payg")
}

func balanceEndpoint(u *upstreamAccount) (*upstreamAccount, string, error) {
	if !balanceAccount(u) {
		return nil, "", bad("this account has no supported pay-as-you-go balance endpoint")
	}
	base, err := u.baseURL()
	if err != nil {
		return nil, "", err
	}
	// Keep custom relay credentials on the configured origin. Only the protocol
	// suffix changes when the account's text endpoint uses Messages.
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/anthropic/v1")
	base = strings.TrimSuffix(base, "/anthropic")
	if _, err = parseUpstreamURL(base); err != nil {
		return nil, "", err
	}
	account := *u
	root, _ := json.Marshal(base)
	account.Credentials = map[string]json.RawMessage{"api_key": u.Credentials["api_key"], "base_url": root, "api_protocol": json.RawMessage(`"chat_completions"`)}
	path := "/user/balance"
	if u.Platform == "kimi" {
		path = "/v1/users/me/balance"
	}
	return &account, path, nil
}

func parseProviderBalance(platform string, raw []byte) (providerBalance, error) {
	out := providerBalance{Provider: platform, Balance: "0", Available: true}
	var body map[string]json.RawMessage
	invalid := func() (providerBalance, error) { return out, errors.New("invalid upstream balance response") }
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return invalid()
	}
	parseAmount := func(raw json.RawMessage) (json.Number, bool) {
		var n json.Number
		if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &n) != nil {
			return "", false
		}
		abs := json.Number(strings.TrimPrefix(n.String(), "-"))
		return n, validPrice(&abs, 16, 12)
	}
	switch platform {
	case "kimi":
		var code *int
		var data map[string]json.RawMessage
		if json.Unmarshal(body["code"], &code) != nil || code == nil || *code != 0 || json.Unmarshal(body["data"], &data) != nil {
			return invalid()
		}
		n, ok := parseAmount(data["available_balance"])
		if !ok {
			return invalid()
		}
		out.Balances = []providerBalanceEntry{{"CNY", n}}
	case "deepseek":
		if raw := body["is_available"]; raw != nil {
			if string(raw) == "null" || json.Unmarshal(raw, &out.Available) != nil {
				return invalid()
			}
		}
		var entries []map[string]json.RawMessage
		if json.Unmarshal(body["balance_infos"], &entries) != nil || len(entries) == 0 || len(entries) > 16 {
			return invalid()
		}
		seen := map[string]bool{}
		for _, entry := range entries {
			n, ok := parseAmount(entry["total_balance"])
			if !ok {
				return invalid()
			}
			currency := strings.ToUpper(strings.TrimSpace(credentialString(entry, "currency")))
			if currency == "" {
				currency = "CNY"
			}
			if len(currency) != 3 || seen[currency] {
				return invalid()
			}
			for _, c := range currency {
				if c < 'A' || c > 'Z' {
					return invalid()
				}
			}
			seen[currency] = true
			out.Balances = append(out.Balances, providerBalanceEntry{currency, n})
		}
	default:
		return invalid()
	}
	out.Balance, out.Currency = out.Balances[0].Balance, out.Balances[0].Currency
	out.Success = true
	return out, nil
}

func (b providerBalance) below(threshold json.Number) bool {
	if !b.Available {
		return true
	}
	for _, entry := range b.Balances {
		if rat(entry.Balance).Cmp(rat(threshold)) >= 0 {
			return false
		}
	}
	return true
}

func (a *App) probeAccountBalance(ctx context.Context, u *upstreamAccount, schedule bool) (*providerBalance, error) {
	if err := a.checkInstance(ctx); err != nil {
		return nil, err
	}
	account, path, err := balanceEndpoint(u)
	if err != nil {
		return nil, err
	}
	if !a.takeSlot("balance-probe", u.ID, 1) {
		return nil, conflict("an account balance probe is already running")
	}
	defer a.releaseSlot("balance-probe", u.ID)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := a.upstreamRequest(ctx, account, "GET", path, nil)
	if err != nil {
		return nil, &apiError{502, "upstream balance request failed"}
	}
	defer resp.Body.Close()
	out := providerBalance{Provider: u.Platform, Balance: "0", StatusCode: resp.StatusCode, FetchedAt: time.Now().UTC().Unix()}
	if resp.StatusCode != 200 {
		out.Error = fmt.Sprintf("upstream balance request rejected (HTTP %d)", resp.StatusCode)
		return &out, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBalanceBody+1))
	if err != nil || len(raw) > maxBalanceBody {
		out.Error = "invalid upstream balance response"
		return &out, nil
	}
	parsed, err := parseProviderBalance(u.Platform, raw)
	if err != nil {
		out.Error = "invalid upstream balance response"
		return &out, nil
	}
	parsed.StatusCode, parsed.FetchedAt = out.StatusCode, out.FetchedAt
	out = parsed
	at := time.Now().UTC()
	updates := map[string]any{}
	for key, value := range map[string]any{"balance": out.Balance, "balance_currency": out.Currency, "balance_available": out.Available, "balance_updated_at": at.Format(time.RFC3339Nano), "balances": out.Balances, "balance_low": false} {
		updates[u.Platform+"_"+key] = value
	}
	delta, _ := json.Marshal(updates)
	low := out.below(a.balancePolicy.Threshold)
	cooldown := a.balanceCooldown()
	// A single CAS preserves concurrent admin changes and merges, rather than
	// replacing, unknown JSON and quota counters. Health checks never enable an
	// account or clear unrelated cooldowns, auth errors, RPM, or consumption.
	result, err := a.DB.ExecContext(ctx, `UPDATE accounts SET extra=extra || $3::jsonb,updated_at=clock_timestamp(),
 temp_unschedulable_until=CASE
 WHEN $4 AND status='active' AND schedulable AND (NOT auto_pause_on_expired OR expires_at IS NULL OR expires_at>now())
 AND $5 AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<=now() OR starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low'))
 AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at<=now()) AND (overload_until IS NULL OR overload_until<=now()) THEN now()+$6::bigint*interval '1 second'
 WHEN $4 AND NOT $5 AND status='active' AND schedulable AND starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low') THEN NULL
 ELSE temp_unschedulable_until END,
 temp_unschedulable_reason=CASE
 WHEN $4 AND status='active' AND schedulable AND (NOT auto_pause_on_expired OR expires_at IS NULL OR expires_at>now())
 AND $5 AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until<=now() OR starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low'))
 AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at<=now()) AND (overload_until IS NULL OR overload_until<=now()) THEN 'cn_balance_low: provider balance below configured threshold or unavailable'
 WHEN $4 AND NOT $5 AND status='active' AND schedulable AND starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low') THEN NULL
 ELSE temp_unschedulable_reason END
 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL`, u.ID, u.UpdatedAt, string(delta), schedule, low, int64(cooldown/time.Second))
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	out.Persisted = n == 1
	if !out.Persisted {
		out.Error = "account changed during balance probe; result was not applied"
	}
	return &out, nil
}

func (a *App) balanceCooldown() time.Duration {
	if a.balancePolicy.Interval <= 0 {
		return 20 * time.Minute
	}
	return 2 * a.balancePolicy.Interval
}

func (a *App) accountBalance(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	result, err := a.probeAccountBalance(r.Context(), u, false)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return reply(w, result)
}

func (a *App) runBalanceChecks(ctx context.Context) error {
	if !a.balancePolicy.Enabled || !a.balanceMu.TryLock() {
		return nil
	}
	defer a.balanceMu.Unlock()
	// ponytail: bounded keyset pages and sequential 15s probes; use a small
	// worker pool if a measured cycle cannot finish before the next interval.
	var last int64
	for {
		if err := a.checkInstance(ctx); err != nil {
			return err
		}
		rows, err := a.DB.QueryContext(ctx, `SELECT id FROM accounts WHERE id>$1 AND deleted_at IS NULL AND status='active' AND schedulable AND type='apikey' AND platform IN ('kimi','deepseek') AND COALESCE(credentials->>'account_mode','payg') IN ('','payg') AND (NOT auto_pause_on_expired OR expires_at IS NULL OR expires_at>now()) ORDER BY id LIMIT 100`, last)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err = ctx.Err(); err != nil {
				return err
			}
			u, e := a.loadAccount(ctx, id)
			if e == nil && u.Status == "active" && u.Schedulable {
				_, e = a.probeAccountBalance(ctx, u, true)
			}
			if e != nil && ctx.Err() == nil {
				slog.Warn("account balance probe failed", "account_id", id)
			}
		}
		last = ids[len(ids)-1]
	}
}

func (a *App) startBalanceChecks(ctx context.Context) {
	a.balanceWorkerDone = make(chan struct{})
	go func() {
		defer close(a.balanceWorkerDone)
		if !a.balancePolicy.Enabled {
			return
		}
		ticker := time.NewTicker(a.balancePolicy.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.runBalanceChecks(ctx); err != nil && ctx.Err() == nil {
					slog.Error("balance check cycle failed")
				}
			}
		}
	}()
}

func cnPaygPlatform(platform string) bool {
	return platform == "kimi" || platform == "deepseek" || platform == "zhipu" || platform == "minimax"
}

func balanceFailure(platform string, status int, body []byte) bool {
	if !cnPaygPlatform(platform) {
		return false
	}
	if status == 402 {
		return true
	}
	if status != 429 {
		return false
	}
	message := strings.ToLower(string(body))
	for _, hint := range []string{"余额不足", "insufficient balance", "insufficient_credit", "balance is not enough", "no enough balance"} {
		if strings.Contains(message, hint) {
			return true
		}
	}
	return false
}

func (a *App) markBalanceFailure(ctx context.Context, u *upstreamAccount) {
	delta, _ := json.Marshal(map[string]bool{u.Platform + "_balance_low": true})
	_, err := a.DB.ExecContext(ctx, `UPDATE accounts SET extra=extra || $3::jsonb,updated_at=clock_timestamp(),
 temp_unschedulable_until=CASE WHEN temp_unschedulable_until IS NULL OR temp_unschedulable_until<=now() OR starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low') THEN now()+$4::bigint*interval '1 second' ELSE temp_unschedulable_until END,
 temp_unschedulable_reason=CASE WHEN temp_unschedulable_until IS NULL OR temp_unschedulable_until<=now() OR starts_with(COALESCE(temp_unschedulable_reason,''),'cn_balance_low') THEN 'cn_balance_low: upstream reported insufficient balance' ELSE temp_unschedulable_reason END
 WHERE id=$1 AND updated_at=$2 AND status='active' AND schedulable AND deleted_at IS NULL`, u.ID, u.UpdatedAt, string(delta), int64(a.balanceCooldown()/time.Second))
	if err != nil {
		slog.Error("balance cooldown update failed", "account_id", u.ID)
	}
}
