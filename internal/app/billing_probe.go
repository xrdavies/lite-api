package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const billingProbeSetting = "upstream_billing_probe_settings"
const billingSnapshotKey = "upstream_billing_probe"
const billingEnabledKey = "upstream_billing_probe_enabled"
const billingSyncKey = "upstream_billing_rate_sync_enabled"
const keyBillingPath = "/v1/lite-api/billing"

type billingProbeSettings struct {
	Enabled bool `json:"enabled"`
	Minutes int  `json:"interval_minutes"`
}
type keyBillingInfo struct {
	Object    string       `json:"object"`
	Version   int          `json:"schema_version"`
	Scope     string       `json:"billing_scope"`
	Group     *json.Number `json:"group_rate_multiplier"`
	User      *json.Number `json:"user_rate_multiplier,omitempty"`
	Resolved  *json.Number `json:"resolved_rate_multiplier"`
	Peak      *bool        `json:"peak_rate_enabled"`
	Start     *string      `json:"peak_start,omitempty"`
	End       *string      `json:"peak_end,omitempty"`
	PeakRate  *json.Number `json:"peak_rate_multiplier,omitempty"`
	Applied   *json.Number `json:"applied_peak_multiplier,omitempty"`
	Effective *json.Number `json:"effective_rate_multiplier"`
	Zone      *string      `json:"timezone,omitempty"`
	Observed  time.Time    `json:"observed_at"`
}
type billingSnapshot struct {
	Status     string          `json:"status"`
	Data       *keyBillingInfo `json:"data,omitempty"`
	Received   *time.Time      `json:"received_at,omitempty"`
	Fresh      *time.Time      `json:"fresh_until,omitempty"`
	Attempt    time.Time       `json:"last_attempt_at"`
	Next       time.Time       `json:"next_probe_at"`
	Failures   int             `json:"failure_count,omitempty"`
	HTTPStatus int             `json:"http_status,omitempty"`
	Error      string          `json:"last_error,omitempty"`
	Synced     *json.Number    `json:"synced_rate_multiplier,omitempty"`
}
type billingProbeResult struct {
	ID       int64            `json:"account_id"`
	Snapshot *billingSnapshot `json:"snapshot,omitempty"`
	Error    string           `json:"error,omitempty"`
}

func (a *App) loadBillingProbeSettings(ctx context.Context) (billingProbeSettings, error) {
	s := billingProbeSettings{true, 30}
	ok, err := a.readRuntimeSetting(ctx, billingProbeSetting, &s)
	if !ok {
		return billingProbeSettings{true, 30}, err
	}
	s.Minutes = min(max(s.Minutes, 5), 1440)
	return s, nil
}
func (a *App) billingProbeConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadBillingProbeSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var s *billingProbeSettings
	if err := decode(w, r, &s); err != nil {
		return err
	}
	if s == nil || s.Minutes < 5 || s.Minutes > 1440 {
		return bad("interval_minutes must be between 5 and 1440")
	}
	if err := a.writeRuntimeSetting(r.Context(), billingProbeSetting, s); err != nil {
		return err
	}
	return reply(w, s)
}

// This is a token multiplier declaration, not a model price or wallet balance.
// Standard groups have no subscription peak multiplier.
func (a *App) keyBilling(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	err := func() error {
		if err := a.checkInstance(r.Context()); err != nil {
			return err
		}
		g, err := a.gatewayAuth(r, false)
		if err != nil {
			return err
		}
		if err = a.gatewayRPM(r.Context(), g); err != nil {
			return err
		}
		if !a.takeSlot("user", g.UserID, g.Concurrency) {
			return &apiError{429, "user concurrency limit reached"}
		}
		defer a.releaseSlot("user", g.UserID)
		defer a.trackKeySlot(g.Key.ID)()
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		var group, resolved json.Number
		if err = a.DB.QueryRowContext(ctx, `SELECT g.rate_multiplier::text,COALESCE(m.rate_multiplier,g.rate_multiplier)::text
 FROM groups g LEFT JOIN user_group_rate_multipliers m ON m.group_id=g.id AND m.user_id=$2
 WHERE g.id=$1 AND g.deleted_at IS NULL AND g.status='active' AND g.subscription_type='standard'`, g.Key.GroupID, g.UserID).Scan(&group, &resolved); err != nil {
			return err
		}
		var user *json.Number
		if rat(group).Cmp(rat(resolved)) != 0 {
			user = &resolved
		}
		peak := false
		return rawReply(w, keyBillingInfo{Object: "lite-api.key_billing", Version: 1, Scope: "token", Group: &group,
			User: user, Resolved: &resolved, Peak: &peak, Effective: &resolved, Observed: time.Now().UTC()})
	}()
	if err != nil {
		gatewayError(w, err)
	}
}

func parseBillingInfo(raw []byte) (*keyBillingInfo, error) {
	var b keyBillingInfo
	invalid := func() (*keyBillingInfo, error) { return nil, bad("invalid billing declaration") }
	if json.Unmarshal(raw, &b) != nil || b.Object != "lite-api.key_billing" || b.Version != 1 || b.Scope != "token" ||
		b.Group == nil || b.Resolved == nil || b.Peak == nil || b.Effective == nil || b.Observed.IsZero() || b.Observed.Year() < 1 || b.Observed.Year() > 9999 {
		return invalid()
	}
	for _, value := range []*json.Number{b.Group, b.User, b.Resolved, b.Effective, b.PeakRate, b.Applied} {
		if value != nil && !validPrice(value, 16, 12) {
			return invalid()
		}
	}
	resolved := b.Group
	if b.User != nil {
		resolved = b.User
	}
	if rat(*resolved).Cmp(rat(*b.Resolved)) != 0 {
		return invalid()
	}
	applied, err := b.peakAt(b.Observed)
	if err != nil || b.Applied != nil && rat(*b.Applied).Cmp(applied) != 0 || rat(*b.Effective).Cmp(new(big.Rat).Mul(rat(*b.Resolved), applied)) != 0 {
		return invalid()
	}
	if !*b.Peak {
		b.Start, b.End, b.Zone, b.PeakRate, b.Applied = nil, nil, nil, nil, nil
	}
	b.Observed = b.Observed.UTC()
	return &b, nil
}
func (b *keyBillingInfo) peakAt(at time.Time) (*big.Rat, error) {
	one := big.NewRat(1, 1)
	if b.Peak == nil {
		return nil, bad("missing peak declaration")
	}
	if !*b.Peak {
		return one, nil
	}
	if b.Start == nil || b.End == nil || b.Zone == nil || *b.Zone == "" || *b.Zone == "Local" || b.PeakRate == nil || b.Applied == nil {
		return nil, bad("incomplete peak declaration")
	}
	minute := func(s string) (int, error) {
		parts := strings.Split(s, ":")
		if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 2 || len(parts[1]) != 2 {
			return 0, bad("invalid peak time")
		}
		h, err := strconv.Atoi(parts[0])
		m, e := strconv.Atoi(parts[1])
		if err != nil || e != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, bad("invalid peak time")
		}
		return h*60 + m, nil
	}
	start, err := minute(*b.Start)
	end, e := minute(*b.End)
	zone, z := time.LoadLocation(*b.Zone)
	if err != nil || e != nil || z != nil || start >= end {
		return nil, bad("invalid peak interval")
	}
	local := at.In(zone)
	now := local.Hour()*60 + local.Minute()
	if now >= start && now < end {
		return rat(*b.PeakRate), nil
	}
	return one, nil
}
func billingSyncRate(b *keyBillingInfo) *json.Number {
	if b == nil || b.Resolved == nil {
		return nil
	}
	n := json.Number(rat(*b.Resolved).FloatString(4))
	if rat(n).Sign() <= 0 || rat(n).Cmp(big.NewRat(100, 1)) > 0 {
		return nil
	}
	return &n
}
func decodeBillingSnapshot(raw json.RawMessage) *billingSnapshot {
	var s billingSnapshot
	if json.Unmarshal(raw, &s) != nil || s.Status != "ok" && s.Status != "failed" && s.Status != "unsupported" {
		return nil
	}
	if s.Data != nil {
		data, _ := json.Marshal(s.Data)
		b, err := parseBillingInfo(data)
		if err != nil {
			s.Data, s.Received, s.Fresh, s.Synced = nil, nil, nil, nil
		} else {
			s.Data = b
		}
	}
	return &s
}
func billingFlag(extra map[string]json.RawMessage, key string) bool {
	var enabled bool
	return json.Unmarshal(extra[key], &enabled) == nil && enabled
}

func (in *accountInput) normalizeBillingFlags(create bool) error {
	for _, flag := range []struct {
		key   string
		value **bool
	}{{billingEnabledKey, &in.ProbeEnabled}, {billingSyncKey, &in.RateSync}} {
		if raw, ok := in.Extra[flag.key]; ok {
			var enabled bool
			if create || string(raw) == "null" || json.Unmarshal(raw, &enabled) != nil || *flag.value != nil && **flag.value != enabled {
				return bad("invalid or conflicting billing probe switch")
			}
			*flag.value = &enabled
		}
	}
	if create && in.RateSync != nil && *in.RateSync {
		return bad("enable rate sync after creating the account")
	}
	if in.RateSync != nil && *in.RateSync {
		if in.ProbeEnabled != nil && !*in.ProbeEnabled {
			return bad("billing rate sync requires billing probes")
		}
		enabled := true
		in.ProbeEnabled = &enabled
	}
	if in.ProbeEnabled != nil && !*in.ProbeEnabled {
		disabled := false
		in.RateSync = &disabled
	}
	for _, flag := range []struct {
		key   string
		value *bool
	}{{billingEnabledKey, in.ProbeEnabled}, {billingSyncKey, in.RateSync}} {
		if flag.value != nil {
			if in.Extra == nil {
				in.Extra = map[string]json.RawMessage{}
			}
			in.Extra[flag.key], _ = json.Marshal(*flag.value)
		}
	}
	return nil
}

func billingProbeBase(u *upstreamAccount) (string, bool, error) {
	base, err := u.baseURL()
	if err != nil {
		return "", false, err
	}
	base = strings.TrimRight(base, "/")
	if cnPaygPlatform(u.Platform) {
		base = strings.TrimSuffix(strings.TrimSuffix(base, "/anthropic/v1"), "/anthropic")
	}
	parsed, err := parseUpstreamURL(base)
	if err != nil {
		return "", false, err
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	for _, domain := range []string{"openai.com", "anthropic.com", "googleapis.com", "x.ai", "grok.com", "moonshot.cn", "moonshot.ai", "kimi.com", "bigmodel.cn", "z.ai", "deepseek.com", "minimax.io", "minimaxi.com", "ollama.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return base, true, nil
		}
	}
	return base, false, nil
}

// Include the entire bounded fallback graph and revisions. No proxy secrets
// leave this function; the digest is only held during the probe.
func billingProxyVersion(ctx context.Context, q queryer, id *int64) (string, error) {
	if id == nil {
		return "", nil
	}
	var raw []byte
	err := q.QueryRowContext(ctx, `WITH RECURSIVE chain AS (
 SELECT p.*,ARRAY[id] AS trail FROM proxies p WHERE id=$1
 UNION ALL SELECT p.*,c.trail || p.id FROM chain c JOIN proxies p ON p.id=c.backup_proxy_id WHERE NOT p.id=ANY(c.trail) AND cardinality(c.trail)<10
) SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY id),'[]'::jsonb) FROM chain c`, *id).Scan(&raw)
	return digest(string(raw)), err
}
func billingProbeDelay(minutes int, unsupported bool, retry string, at time.Time) time.Duration {
	delay := time.Duration(minutes) * time.Minute
	jitter := min(delay/5, 5*time.Minute)
	if jitter > 0 {
		delay += time.Duration(rand.Int64N(int64(2*jitter)+1)) - jitter
	}
	if unsupported {
		delay *= 8
	}
	delay = min(delay, 24*time.Hour)
	// Preserve long Retry-After instructions without allowing duration overflow.
	seconds := retryAfterSeconds(retry, at, 315360000)
	return max(delay, time.Duration(seconds)*time.Second)
}

func (a *App) probeBilling(ctx context.Context, id int64, scheduled bool) (*billingSnapshot, error) {
	if err := a.checkInstance(ctx); err != nil {
		return nil, err
	}
	if !a.takeSlot("billing-probe", id, 1) {
		return nil, conflict("an account billing probe is already running")
	}
	defer a.releaseSlot("billing-probe", id)
	if !a.takeSlot("billing-probes", 0, 4) {
		return nil, &apiError{429, "billing probe concurrency limit reached"}
	}
	defer a.releaseSlot("billing-probes", 0)
	s, err := a.loadBillingProbeSettings(ctx)
	if err != nil {
		return nil, err
	}
	u, err := a.loadAccount(ctx, id)
	if err != nil {
		return nil, err
	}
	previous := decodeBillingSnapshot(u.Extra[billingSnapshotKey])
	now := time.Now().UTC()
	if scheduled && (!s.Enabled || u.Status != "active" || !billingFlag(u.Extra, billingEnabledKey) || previous != nil && previous.Next.After(now)) {
		return nil, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	version, err := billingProxyVersion(probeCtx, a.DB, u.ProxyID)
	if err != nil {
		return nil, err
	}
	out := &billingSnapshot{Status: "failed", Attempt: now, Failures: 1}
	if previous != nil {
		out.Data, out.Received, out.Fresh = previous.Data, previous.Received, previous.Fresh
		out.Failures = min(max(previous.Failures, 0), 2147483646) + 1
	}
	base, official, err := billingProbeBase(u)
	retry := ""
	switch {
	case err != nil:
		out.Error = "invalid_base_url"
	case official:
		out.Status, out.Error = "unsupported", "unsupported"
	default:
		account := *u
		// Billing declarations always use Bearer authentication, regardless of
		// the account's inference protocol (including native Gemini/Anthropic).
		account.Platform = "openai"
		root, _ := json.Marshal(base)
		account.Credentials = map[string]json.RawMessage{"api_key": u.Credentials["api_key"], "base_url": root, "api_protocol": json.RawMessage(`"chat_completions"`)}
		resp, e := a.upstreamRequest(probeCtx, &account, "GET", keyBillingPath, nil)
		if e != nil {
			out.Error = "request_failed"
		} else {
			out.HTTPStatus, retry = resp.StatusCode, resp.Header.Get("Retry-After")
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
			resp.Body.Close()
			switch {
			case readErr != nil:
				out.Error = "response_read_failed"
			case len(raw) > 64<<10:
				out.Error = "response_too_large"
			case resp.StatusCode == 404 || resp.StatusCode == 405:
				out.Status, out.Error = "unsupported", "unsupported"
			case resp.StatusCode < 200 || resp.StatusCode >= 300:
				out.Error = "http_error"
			default:
				data, e := parseBillingInfo(raw)
				if e != nil {
					out.Error = "invalid_response"
				} else {
					fresh := now.Add(2 * time.Duration(s.Minutes) * time.Minute)
					out.Status, out.Data, out.Received, out.Fresh, out.Failures = "ok", data, &now, &fresh, 0
					if billingFlag(u.Extra, billingEnabledKey) && billingFlag(u.Extra, billingSyncKey) {
						out.Synced = billingSyncRate(data)
					}
				}
			}
		}
	}
	// Cancellation does not replace a good snapshot with a synthetic failure.
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	ctx, persistCancel := context.WithTimeout(ctx, 5*time.Second)
	defer persistCancel()
	out.Next = now.Add(billingProbeDelay(s.Minutes, out.Status == "unsupported", retry, now))
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if u.ProxyID != nil {
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(720034)"); err != nil {
			return nil, err
		}
		current, err := billingProxyVersion(ctx, tx, u.ProxyID)
		if err != nil {
			return nil, err
		}
		if current != version {
			return nil, conflict("billing probe proxy changed; retry the probe")
		}
	}
	raw, _ := json.Marshal(out)
	var rate any
	if out.Synced != nil {
		rate = out.Synced.String()
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_billing_probe}',$3::jsonb),
 rate_multiplier=COALESCE($4::numeric,rate_multiplier),updated_at=clock_timestamp()
 WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL
 AND ($4::numeric IS NULL OR extra @> '{"upstream_billing_probe_enabled":true,"upstream_billing_rate_sync_enabled":true}'::jsonb)`, u.ID, u.UpdatedAt, string(raw), rate)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, conflict("billing probe account changed; retry the probe")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) accountBillingProbe(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if r.Method == "PUT" {
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if err = decode(w, r, &in); err != nil {
			return err
		}
		if in.Enabled == nil {
			return bad("enabled is required")
		}
		if _, err = a.loadAccount(r.Context(), id); err != nil {
			return err
		}
		flags := map[string]bool{billingEnabledKey: *in.Enabled}
		if !*in.Enabled {
			flags[billingSyncKey] = false
		}
		raw, _ := json.Marshal(flags)
		result, err := a.DB.ExecContext(r.Context(), "UPDATE accounts SET extra=extra || $2::jsonb,updated_at=clock_timestamp() WHERE id=$1 AND deleted_at IS NULL", id, string(raw))
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return missing()
		}
		return reply(w, map[string]any{"account_id": id, "enabled": *in.Enabled})
	}
	snapshot, err := a.probeBilling(r.Context(), id, false)
	if err != nil {
		return err
	}
	return reply(w, billingProbeResult{ID: id, Snapshot: snapshot})
}
func (a *App) probeBillingBatch(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		IDs []int64 `json:"account_ids"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if len(in.IDs) < 1 || len(in.IDs) > 20 {
		return bad("account_ids must contain 1 to 20 IDs")
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, id := range in.IDs {
		if id < 1 {
			return bad("account IDs must be positive")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return reply(w, map[string]any{"results": a.probeBillingIDs(r.Context(), ids, false)})
}
func (a *App) probeBillingIDs(ctx context.Context, ids []int64, scheduled bool) []billingProbeResult {
	results := make([]billingProbeResult, len(ids))
	var wg sync.WaitGroup
	// Four bounded workers also serve the manual batch. The shared slot cap
	// protects against several administrators starting batches simultaneously.
	for worker := 0; worker < min(4, len(ids)); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < len(ids); i += 4 {
				results[i].ID = ids[i]
				var err error
				results[i].Snapshot, err = a.probeBilling(ctx, ids[i], scheduled)
				if err != nil {
					results[i].Error = "probe_failed"
				}
			}
		}()
	}
	wg.Wait()
	return results
}

func (a *App) runBillingProbes(ctx context.Context) error {
	if !a.takeSlot("billing-probe-cycle", 0, 1) {
		return nil
	}
	defer a.releaseSlot("billing-probe-cycle", 0)
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	s, err := a.loadBillingProbeSettings(ctx)
	if err != nil || !s.Enabled {
		return err
	}
	// ponytail: scan enabled snapshots in bounded pages, retain only the earliest
	// 20 due tasks; add a scheduling index if large installations need it.
	type due struct {
		id int64
		at time.Time
	}
	list := []due{}
	now, last := time.Now(), int64(0)
	for {
		rows, err := a.DB.QueryContext(ctx, `SELECT id,extra->'upstream_billing_probe' FROM accounts
 WHERE id>$1 AND deleted_at IS NULL AND status='active' AND type='apikey' AND extra @> '{"upstream_billing_probe_enabled":true}'::jsonb ORDER BY id LIMIT 100`, last)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&last, &raw); err != nil {
				rows.Close()
				return err
			}
			count++
			at := time.Time{}
			if snapshot := decodeBillingSnapshot(raw); snapshot != nil {
				at = snapshot.Next
			}
			if !at.After(now) {
				list = append(list, due{last, at})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].at.Equal(list[j].at) {
				return list[i].id < list[j].id
			}
			return list[i].at.Before(list[j].at)
		})
		list = list[:min(len(list), 20)]
		if count < 100 {
			break
		}
	}
	ids := make([]int64, len(list))
	for i := range list {
		ids[i] = list[i].id
	}
	a.probeBillingIDs(ctx, ids, true)
	return ctx.Err()
}
func (a *App) startBillingProbes(ctx context.Context) {
	a.billingWorkerDone = make(chan struct{})
	go func() {
		defer close(a.billingWorkerDone)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if err := a.runBillingProbes(ctx); err != nil && ctx.Err() == nil {
				slog.Error("billing probe cycle failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func (a *App) billingRates(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	where, args := accountListFilter(r)
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM accounts"+where, args...).Scan(&total); err != nil {
		return err
	}
	args = append(args, size, (page-1)*size)
	rows, err := a.DB.QueryContext(r.Context(), "SELECT id,extra->'upstream_billing_probe' FROM accounts"+where+" ORDER BY priority,id LIMIT $4 OFFSET $5", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return err
		}
		items = append(items, map[string]any{"account_id": id, "snapshot": decodeBillingSnapshot(raw)})
	}
	if err = rows.Err(); err != nil {
		return err
	}
	payload := map[string]any{"items": items, "total": total, "page": page, "page_size": size}
	raw, _ := json.Marshal(payload)
	etag := `"` + digest(string(raw)) + `"`
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
	}
	return reply(w, payload)
}
func (a *App) billingProbeRoutes() {
	const root = "/api/v1/admin/accounts/"
	a.route("GET "+root+"upstream-billing-rates", "admin", a.billingRates)
	a.route("GET "+root+"upstream-billing-probe/settings", "admin", a.billingProbeConfig)
	a.route("PUT "+root+"upstream-billing-probe/settings", "admin", a.billingProbeConfig)
	a.route("POST "+root+"upstream-billing-probe/batch", "admin", a.probeBillingBatch)
	a.route("PUT "+root+"{id}/upstream-billing-probe", "admin", a.accountBillingProbe)
	a.route("POST "+root+"{id}/upstream-billing-probe", "admin", a.accountBillingProbe)
	a.mux.HandleFunc("GET "+keyBillingPath, a.keyBilling)
}
