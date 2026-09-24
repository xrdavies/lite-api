package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"time"
)

type usageReceipt struct {
	WebSocket                                                                          bool
	RequestID, PayloadHash, Fingerprint                                                string
	UserID, KeyID, AccountID, GroupID                                                  int64
	ChannelID                                                                          *int64
	Platform, Model, RequestedModel, UpstreamModel, ResponseModel, ServiceTier, Effort string
	Cost                                                                               priceCost
	UserRate, AccountRate, AccountRaw, AccountDebit                                    string
	AccountStats                                                                       *string
	KeyQuota, KeyWindows, AccountQuota                                                 bool
	Usage                                                                              priceUsage
	Stream                                                                             bool
	NativeCompaction                                                                   bool
	Duration, FirstToken                                                               int64
	At                                                                                 time.Time
	IP, UserAgent, Inbound, Upstream, UpstreamRequestID                                string
	BillingMode                                                                        string
	RequestedEffort                                                                    *string
}

func (a *App) makeReceipt(id string, g *gatewayIdentity, s *gatewaySelection, requested, response, tier, effort string, u priceUsage, stream bool, duration time.Duration, first int64, at time.Time, payload, ip, agent, inbound, upstreamID string) (*usageReceipt, error) {
	model := s.ChannelModel
	switch s.BillingSource {
	case "requested":
		model = requested
	case "upstream":
		model = s.UpstreamModel
	case "response_model":
		if response != "" {
			model = response
		}
	}
	var p modelPrice
	var cost priceCost
	var err error
	rate := g.Group.Rate
	if u.ImageCount > 0 && s.ResponseImage != nil && s.Account.Platform == "openai" {
		model = s.responseImageModel(requested, response)
		cost, p.BillingMode, rate, err = s.generatedImageCost(g.Group, model, u, tier, effort, at)
	} else if (u.ImageCount > 0 || u.ImageRequest) && s.Account.Platform == "gemini" {
		cost, p.BillingMode, rate, err = s.generatedImageCost(g.Group, model, u, tier, effort, at)
	} else if u.VideoCount > 0 {
		p.BillingMode = "video"
		cost, err = s.videoCost(g.Group, model, u, at)
	} else if s.Audio != "" {
		p.BillingMode = "per_request"
		cost, err = s.audioCost(g.Group, model, u.AudioUnits, at)
	} else if s.Search != "" {
		p.BillingMode = "per_request"
		cost, err = g.Group.searchCost(s.Search)
		if grokSearchProtocol(s.Search) {
			model = "grok-" + strings.ReplaceAll(s.Search, "_", "-")
		}
	} else {
		p, err = s.price(model)
		if err == nil {
			cost, err = calculatePrice(p, u, g.Group.Rate, tier, effort, "", at, g.Group.LongContext)
		}
	}
	if err != nil {
		return nil, err
	}
	if u.SearchCalls > 0 {
		if s.Account.Platform != "grok" && s.Account.Platform != "openai" || s.Search != "" {
			return nil, bad("unexpected hosted search usage")
		}
		if err = addHostedSearchCost(&cost, g.Group, u.SearchCalls, rate); err != nil {
			return nil, err
		}
	}
	r := &usageReceipt{RequestID: id, PayloadHash: payload, UserID: g.UserID, KeyID: g.Key.ID, AccountID: s.Account.ID, GroupID: g.Key.GroupID, ChannelID: s.ChannelID, Platform: s.Account.Platform, Model: model, RequestedModel: requested, UpstreamModel: s.UpstreamModel, ResponseModel: response, ServiceTier: tier, Effort: effort, Cost: cost, UserRate: g.Group.Rate.String(), AccountRate: s.Rate.String(), Usage: u, Stream: stream, Duration: duration.Milliseconds(), FirstToken: first, At: at, IP: ip, UserAgent: truncate(agent, 512), Inbound: inbound, UpstreamRequestID: truncate(upstreamID, 128), BillingMode: p.BillingMode}
	if u.VideoCount > 0 {
		r.UserRate = g.Group.videoRate().String()
	} else {
		r.UserRate = rate.String()
	}
	if g.RoutingGroup != nil && g.SourcePlatform != "composite" {
		r.Platform = g.SourcePlatform
	}
	if r.BillingMode == "" {
		r.BillingMode = "token"
	}
	r.KeyQuota = rat(g.Key.Quota).Sign() > 0
	r.KeyWindows = rat(g.Key.Limit5h).Sign() > 0 || rat(g.Key.Limit1d).Sign() > 0 || rat(g.Key.Limit7d).Sign() > 0
	for _, key := range []string{"quota_limit", "quota_daily_limit", "quota_weekly_limit"} {
		r.AccountQuota = r.AccountQuota || rat(extraNumber(s.Account.Extra, key)).Sign() > 0
	}
	r.AccountRaw = new(big.Rat).Mul(cost.totalValue, rat(s.Rate)).FloatString(10)
	r.AccountDebit = new(big.Rat).Mul(cost.totalValue, rat(s.Rate)).FloatString(8)
	if s.ApplyStats {
		statsCost := cost.Total
		matched := false
		for _, rule := range s.StatsRules {
			applies := false
			for _, aid := range rule.Accounts {
				applies = applies || aid == s.Account.ID
			}
			for _, gid := range rule.Groups {
				applies = applies || gid == g.Key.GroupID
			}
			if !applies {
				continue
			}
			for _, price := range rule.Pricing {
				if price.Platform != s.Account.Platform {
					continue
				}
				for _, pattern := range price.Models {
					if patternMatches(pricingName(pattern), pricingName(model)) {
						label := u.VideoResolution
						if u.ImageCount > 0 {
							label = u.ImageSize
						}
						stats, err := calculatePrice(price, u, "1", "", effort, label, at, g.Group.LongContext)
						if err != nil {
							return nil, err
						}
						statsCost = stats.Total
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				break
			}
		}
		r.AccountStats = &statsCost
	}
	if len(model) > 100 || len(response) > 200 || len(tier) > 16 || len(effort) > 20 {
		return nil, &apiError{502, "upstream billing metadata exceeds limits"}
	}
	r.Fingerprint = r.fingerprint()
	return r, nil
}

func alphaSearchCost(price *json.Number, rate json.Number) (priceCost, error) {
	return standaloneSearchCost(price, rate, false)
}

func (g gatewayGroup) searchCost(protocol string) (priceCost, error) {
	if grokSearchProtocol(protocol) {
		return standaloneSearchCost(g.SearchPrice, g.Rate, true)
	}
	return alphaSearchCost(g.WebSearchPrice, g.Rate)
}

func standaloneSearchCost(price *json.Number, rate json.Number, perThousand bool) (priceCost, error) {
	fallback := "0.01"
	if perThousand {
		fallback = "5"
	}
	if !validPrice(price, 12, 8) || !validPrice(&rate, 6, 4) {
		return priceCost{}, bad("invalid search price or multiplier")
	}
	total := decimalOr(price, fallback)
	if perThousand {
		total.Quo(total, big.NewRat(1000, 1))
	}
	actual := new(big.Rat).Mul(total, rat(rate))
	return priceCost{Input: "0", Output: "0", CacheWrite: "0", CacheRead: "0", ImageInput: "0", ImageOutput: "0", Total: total.FloatString(10), Actual: actual.FloatString(10), Debit: actual.FloatString(8), totalValue: total}, nil
}
func truncate(s string, n int) string {
	r := []rune(strings.ToValidUTF8(s, ""))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}
func (r *usageReceipt) fingerprint() string {
	keyCost, windowCost, accountCost := "0.0000000000", "0.0000000000", "0.0000000000"
	if r.KeyQuota {
		keyCost = r.Cost.Actual
	}
	if r.KeyWindows {
		windowCost = r.Cost.Actual
	}
	if r.AccountQuota {
		accountCost = r.AccountRaw
	}
	raw := fmt.Sprintf("%d|%d|%d|apikey|%s|%s|%s|0|%d|%d|%d|%d|0||0|%s|0.0000000000|%s|%s|%s", r.UserID, r.AccountID, r.KeyID, strings.TrimSpace(r.Model), strings.TrimSpace(r.ServiceTier), strings.TrimSpace(r.Effort), r.Usage.Input, r.Usage.Output, r.Usage.CacheWrite, r.Usage.CacheRead, r.Cost.Actual, keyCost, windowCost, accountCost)
	if r.PayloadHash != "" {
		raw += "|" + r.PayloadHash
	}
	if r.Usage.VideoCount > 0 {
		raw += fmt.Sprintf("|video|%d|%d|%s", r.Usage.VideoCount, r.Usage.VideoSeconds, r.Usage.VideoResolution)
	}
	if r.Usage.ImageCount > 0 {
		raw += fmt.Sprintf("|image|%d|%s|%d|%d", r.Usage.ImageCount, r.Usage.ImageSize, r.Usage.ImageInput, r.Usage.ImageOutput)
	}
	if r.Usage.ImageOutputSize != "" || r.Usage.ImageSizes != [3]int64{} {
		raw += fmt.Sprintf("|image-output|%s|%v", r.Usage.ImageOutputSize, r.Usage.ImageSizes)
	}
	if r.Usage.SearchCalls > 0 {
		raw += fmt.Sprintf("|search|%d", r.Usage.SearchCalls)
	}
	return digest(raw)
}
func (a *App) saveReceipt(ctx context.Context, r *usageReceipt) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	// A receipt contains billing metadata, never credentials or message content. Redis
	// AOF retains pending settlement across restarts; PostgreSQL dedup makes replay safe.
	pendingErr := a.Redis.HSet(ctx, "gateway:pending-billing", r.RequestID, string(raw)).Err()
	if err = a.applyReceipt(ctx, r); err != nil {
		return err
	}
	if pendingErr == nil {
		_ = a.Redis.HDel(ctx, "gateway:pending-billing", r.RequestID).Err()
	}
	return nil
}
func (a *App) recoverReceipts(ctx context.Context) error {
	var cursor uint64
	for {
		values, next, err := a.Redis.HScan(ctx, "gateway:pending-billing", cursor, "*", 100).Result()
		if err != nil {
			return err
		}
		for i := 0; i+1 < len(values); i += 2 {
			var r usageReceipt
			if json.Unmarshal([]byte(values[i+1]), &r) != nil || r.RequestID != values[i] || r.Fingerprint != r.fingerprint() {
				return errors.New("invalid pending billing receipt")
			}
			if err = a.applyReceipt(ctx, &r); err != nil {
				return err
			}
			if err = a.Redis.HDel(ctx, "gateway:pending-billing", r.RequestID).Err(); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}
func (a *App) applyReceipt(ctx context.Context, r *usageReceipt) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint) VALUES($1,$2,$3) ON CONFLICT(request_id,api_key_id) DO NOTHING RETURNING id`, r.RequestID, r.KeyID, r.Fingerprint).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		var fingerprint string
		if err = tx.QueryRowContext(ctx, "SELECT request_fingerprint FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2", r.RequestID, r.KeyID).Scan(&fingerprint); err != nil {
			return err
		}
		if fingerprint != r.Fingerprint {
			return conflict("billing fingerprint conflict")
		}
		return nil
	}
	var archived string
	err = tx.QueryRowContext(ctx, "SELECT request_fingerprint FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$2", r.RequestID, r.KeyID).Scan(&archived)
	if err == nil {
		if archived != r.Fingerprint {
			return conflict("archived billing fingerprint conflict")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	// The upstream consumption happened before settlement. Soft deletion or a
	// concurrent balance adjustment cannot erase that debt; lock user before key.
	result, err := tx.ExecContext(ctx, "UPDATE users SET balance=balance-$2::numeric,last_active_at=now(),updated_at=now() WHERE id=$1", r.UserID, r.Cost.Debit)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("billing user missing")
	}
	if r.KeyQuota {
		if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET quota_used=quota_used+$2::numeric,status=CASE WHEN status='active' AND quota>0 AND quota_used+$2::numeric>=quota THEN 'quota_exhausted' ELSE status END,updated_at=now() WHERE id=$1`, r.KeyID, r.Cost.Debit); err != nil {
			return err
		}
	}
	if r.KeyWindows {
		if _, err = tx.ExecContext(ctx, `UPDATE api_keys SET
 usage_5h=CASE WHEN window_5h_start+interval '5 hours'<=now() THEN $2::numeric ELSE usage_5h+$2::numeric END,
 usage_1d=CASE WHEN window_1d_start+interval '24 hours'<=now() THEN $2::numeric ELSE usage_1d+$2::numeric END,
 usage_7d=CASE WHEN window_7d_start+interval '168 hours'<=now() THEN $2::numeric ELSE usage_7d+$2::numeric END,
 window_5h_start=CASE WHEN window_5h_start IS NULL OR window_5h_start+interval '5 hours'<=now() THEN now() ELSE window_5h_start END,
 window_1d_start=CASE WHEN window_1d_start IS NULL OR window_1d_start+interval '24 hours'<=now() THEN date_trunc('day',now()) ELSE window_1d_start END,
 window_7d_start=CASE WHEN window_7d_start IS NULL OR window_7d_start+interval '168 hours'<=now() THEN date_trunc('day',now()) ELSE window_7d_start END WHERE id=$1`, r.KeyID, r.Cost.Debit); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE api_keys SET last_used_at=now() WHERE id=$1", r.KeyID); err != nil {
		return err
	}
	if r.AccountQuota {
		var extraRaw []byte
		if err = tx.QueryRowContext(ctx, "SELECT extra FROM accounts WHERE id=$1 FOR UPDATE", r.AccountID).Scan(&extraRaw); err != nil {
			return err
		}
		var extra map[string]json.RawMessage
		if err = json.Unmarshal(extraRaw, &extra); err != nil {
			return err
		}
		delta, err := accountQuotaDelta(extra, r.AccountDebit, time.Now())
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(delta)
		if _, err = tx.ExecContext(ctx, "UPDATE accounts SET extra=extra || $2::jsonb,last_used_at=now() WHERE id=$1", r.AccountID, string(raw)); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(ctx, "UPDATE accounts SET last_used_at=now() WHERE id=$1", r.AccountID); err != nil {
			return err
		}
	}
	if err = incrementPlatformQuota(ctx, tx, r.UserID, r.Platform, r.Cost.Actual, time.Now()); err != nil {
		return err
	}
	if r.Upstream == "" {
		r.Upstream = "/v1/chat/completions"
	}
	requestType := 1
	if r.Stream {
		requestType = 2
	}
	if r.WebSocket {
		requestType = 3
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,cache_creation_5m_tokens,cache_creation_1h_tokens,input_cost,output_cost,cache_creation_cost,cache_read_cost,total_cost,actual_cost,stream,duration_ms,created_at,group_id,rate_multiplier,first_token_ms,user_agent,ip_address,account_rate_multiplier,reasoning_effort,request_type,service_tier,inbound_endpoint,upstream_endpoint,upstream_model,requested_model,channel_id,billing_mode,image_input_tokens,image_output_tokens,image_input_cost,image_output_cost,account_stats_cost,upstream_response_model,upstream_model_mismatch,upstream_request_id,native_compaction_v2,requested_reasoning_effort,openai_ws_mode,video_count,video_resolution,video_duration_seconds,image_count,image_size,image_size_source,image_input_size,image_output_size,image_size_breakdown)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$43,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$44,$45,$46,$47,NULLIF($48,''),NULLIF($49,0),$50,NULLIF($51,''),NULLIF($52,''),NULLIF($53,''),NULLIF($54,''),$55::jsonb)`, r.UserID, r.KeyID, r.AccountID, r.RequestID, r.Model, r.Usage.Input, r.Usage.Output, r.Usage.CacheWrite, r.Usage.CacheRead, r.Usage.CacheWrite5m, r.Usage.CacheWrite1h, r.Cost.Input, r.Cost.Output, r.Cost.CacheWrite, r.Cost.CacheRead, r.Cost.Total, r.Cost.Actual, r.Stream, r.Duration, r.At, r.GroupID, r.UserRate, r.FirstToken, r.UserAgent, r.IP, r.AccountRate, r.Effort, requestType, r.ServiceTier, r.Inbound, r.UpstreamModel, r.RequestedModel, r.ChannelID, r.BillingMode, r.Usage.ImageInput, r.Usage.ImageOutput, r.Cost.ImageInput, r.Cost.ImageOutput, r.AccountStats, r.ResponseModel, r.ResponseModel != "" && r.ResponseModel != r.UpstreamModel, r.UpstreamRequestID, r.Upstream, r.NativeCompaction, r.RequestedEffort, r.WebSocket, r.Usage.VideoCount, r.Usage.VideoResolution, r.Usage.VideoSeconds, r.Usage.ImageCount, r.Usage.ImageSize, r.Usage.ImageSizeSource, r.Usage.ImageInputSize, r.Usage.ImageOutputSize, imageSizeBreakdown(r.Usage.ImageSizes))
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) markGatewayFailure(ctx context.Context, s *gatewaySelection, status int, retry string, body []byte) {
	if balanceFailure(s.Account.Platform, status, body) {
		a.markBalanceFailure(ctx, s.Account)
		return
	}
	seconds := retryAfterSeconds(retry, time.Now(), 7200)
	var clause string
	switch status {
	case 401, 403:
		clause = "status='error',error_message='upstream authentication rejected'"
	case 429:
		if s.Account.Platform == "gemini" && s.Account.Type == "apikey" {
			seconds = geminiRateLimitSeconds(body, retry, time.Now())
		} else if seconds == 0 {
			settings, err := a.loadRate429Settings(ctx)
			if err != nil {
				slog.Warn("429 policy unavailable; using default")
			}
			if !settings.Enabled {
				return
			}
			seconds = int64(settings.Seconds)
		}
		clause = "rate_limited_at=now(),rate_limit_reset_at=GREATEST(rate_limit_reset_at,now()+$3::bigint*interval '1 second')"
	case 529:
		settings, err := a.loadOverloadSettings(ctx)
		if err != nil {
			slog.Warn("529 policy unavailable; using default")
		}
		if !settings.Enabled {
			return
		}
		seconds = int64(settings.Minutes) * 60
		clause = "overload_until=GREATEST(overload_until,now()+$3::bigint*interval '1 second')"
	case 502, 503, 504:
		if seconds == 0 {
			seconds = 30
		}
		seconds = min(seconds, 3600)
		clause = "overload_until=GREATEST(overload_until,now()+$3::bigint*interval '1 second')"
	default:
		return
	}
	args := []any{s.Account.ID, s.Account.UpdatedAt}
	if status != 401 && status != 403 {
		args = append(args, seconds)
	}
	if _, err := a.DB.ExecContext(ctx, "UPDATE accounts SET "+clause+",updated_at=now() WHERE id=$1 AND updated_at=$2 AND status='active' AND deleted_at IS NULL", args...); err != nil {
		slog.Error("account failure state update failed", "account_id", s.Account.ID)
	}
}
func (a *App) recordGatewayError(id string, g *gatewayIdentity, s *gatewaySelection, r *http.Request, cause error, started time.Time) {
	// Invalid unauthenticated requests do not create an unbounded database audit stream.
	if g == nil || skipErrorMonitoring(cause) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status := 502
	var e *apiError
	if errors.As(cause, &e) {
		status = e.status
	}
	var aid any
	if s != nil && s.Account != nil {
		aid = s.Account.ID
	}
	message := safeGatewayError(cause)
	var passthrough *passthroughError
	if errors.As(cause, &passthrough) {
		// Client-authorized message passthrough does not authorize storing the
		// provider body in operational diagnostics.
		message = fmt.Sprintf("upstream returned HTTP %d (error rule applied)", passthrough.UpstreamStatus)
	}
	_, err := a.DB.ExecContext(ctx, `INSERT INTO ops_error_logs(request_id,user_id,api_key_id,account_id,group_id,platform,request_path,error_phase,error_type,status_code,error_message,error_source,error_owner,is_business_limited,duration_ms) VALUES($1,$2,$3,$4,$5,$6,$7,'gateway','request_failed',$8,$9,'gateway','gateway',$10,$11)`, id, g.UserID, g.Key.ID, aid, g.Key.GroupID, g.Group.Platform, r.URL.Path, status, message, status == 429 || status == 402, time.Since(started).Milliseconds())
	if err != nil {
		slog.Error("gateway error record failed", "request_id", id)
	}
}
