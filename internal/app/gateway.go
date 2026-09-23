package app

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

type gatewayKey struct {
	ID        int64       `json:"id"`
	UserID    int64       `json:"user_id"`
	GroupID   int64       `json:"group_id"`
	Quota     json.Number `json:"quota"`
	Limit5h   json.Number `json:"rate_limit_5h"`
	Limit1d   json.Number `json:"rate_limit_1d"`
	Limit7d   json.Number `json:"rate_limit_7d"`
	Whitelist []string    `json:"ip_whitelist"`
	Blacklist []string    `json:"ip_blacklist"`
}
type gatewayGroup struct {
	ID          int64               `json:"id"`
	Platform    string              `json:"platform"`
	Rate        json.Number         `json:"rate_multiplier"`
	RPM         int                 `json:"rpm_limit"`
	LongContext bool                `json:"long_context_pricing_enabled"`
	Allowlist   modelAllowlist      `json:"model_allowlist"`
	Manifest    modelManifestConfig `json:"codex_models_manifest_config"`
	Pricing     []modelPrice        `json:"model_pricing"`
}
type modelAllowlist struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models"`
}

func (m *modelAllowlist) validate() error {
	if len(m.Models) > 1000 {
		return bad("too many allowlist models")
	}
	seen := map[string]bool{}
	models := []string{}
	for _, model := range m.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if !validModelPattern(model) {
			return bad("invalid model allowlist")
		}
		key := strings.ToLower(model)
		if !seen[key] {
			models = append(models, model)
			seen[key] = true
		}
	}
	if m.Enabled && len(models) == 0 {
		return bad("enabled model allowlist must not be empty")
	}
	m.Models = models
	return nil
}

type gatewayIdentity struct {
	Key              gatewayKey
	Group            gatewayGroup
	UserID           int64
	Concurrency, RPM int
}

func ipMatches(addr netip.Addr, rules []string) bool {
	for _, rule := range rules {
		if v, err := netip.ParseAddr(rule); err == nil && v.Unmap() == addr.Unmap() {
			return true
		}
		if prefix, err := netip.ParsePrefix(rule); err == nil && prefix.Contains(addr.Unmap()) {
			return true
		}
	}
	return false
}
func (a *App) gatewayAuth(r *http.Request, spending bool) (*gatewayIdentity, error) {
	token := bearer(r)
	gemini := strings.HasPrefix(r.URL.Path, "/v1beta/")
	if r.URL.Query().Get("api_key") != "" || !gemini && r.URL.Query().Get("key") != "" {
		return nil, bad("use an API key header")
	}
	if token == "" {
		token = r.Header.Get("X-Api-Key")
	}
	if gemini && r.Header.Get("X-Goog-Api-Key") != "" {
		token = r.Header.Get("X-Goog-Api-Key")
	}
	if token == "" {
		token = r.Header.Get("X-Goog-Api-Key")
	}
	if token == "" && gemini {
		token = r.URL.Query().Get("key")
	}
	if token == "" || len(token) > 128 {
		return nil, unauthorized()
	}
	var key, group []byte
	g := &gatewayIdentity{}
	var userStatus, keyStatus string
	var expires *time.Time
	var balanceOK, quotaOK, windowsOK bool
	err := a.DB.QueryRowContext(r.Context(), `SELECT to_jsonb(k),to_jsonb(g)||jsonb_build_object('rate_multiplier',COALESCE(m.rate_multiplier,g.rate_multiplier),'rpm_limit',COALESCE(m.rpm_override,g.rpm_limit)),u.id,u.status,k.status,k.expires_at,u.concurrency,u.rpm_limit,u.balance>0,
 (k.quota=0 OR k.quota_used<k.quota),
 (k.rate_limit_5h=0 OR k.window_5h_start IS NULL OR k.window_5h_start+interval '5 hours'<=now() OR k.usage_5h<k.rate_limit_5h) AND
 (k.rate_limit_1d=0 OR k.window_1d_start IS NULL OR k.window_1d_start+interval '24 hours'<=now() OR k.usage_1d<k.rate_limit_1d) AND
 (k.rate_limit_7d=0 OR k.window_7d_start IS NULL OR k.window_7d_start+interval '168 hours'<=now() OR k.usage_7d<k.rate_limit_7d)
 FROM api_keys k JOIN users u ON u.id=k.user_id LEFT JOIN groups g ON g.id=k.group_id AND g.deleted_at IS NULL AND g.status='active' LEFT JOIN user_group_rate_multipliers m ON m.user_id=u.id AND m.group_id=g.id WHERE k.key=$1 AND k.deleted_at IS NULL AND u.deleted_at IS NULL`, token).Scan(&key, &group, &g.UserID, &userStatus, &keyStatus, &expires, &g.Concurrency, &g.RPM, &balanceOK, &quotaOK, &windowsOK)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unauthorized()
	}
	if err != nil {
		return nil, err
	}
	if userStatus != "active" || (keyStatus != "active" && keyStatus != "quota_exhausted") {
		return nil, unauthorized()
	}
	if expires != nil && !time.Now().Before(*expires) {
		return nil, unauthorized()
	}
	if err = json.Unmarshal(key, &g.Key); err != nil {
		return nil, err
	}
	if len(group) == 0 || string(group) == "null" || g.Key.GroupID == 0 {
		return nil, denied()
	}
	if err = json.Unmarshal(group, &g.Group); err != nil {
		return nil, err
	}
	if g.Group.ID == 0 {
		return nil, denied()
	}
	if err = a.groupAccess(r.Context(), a.DB, g.UserID, g.Key.GroupID); err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return nil, denied()
	}
	if ipMatches(addr, g.Key.Blacklist) || len(g.Key.Whitelist) > 0 && !ipMatches(addr, g.Key.Whitelist) {
		return nil, denied()
	}
	if spending {
		if !balanceOK {
			return nil, &apiError{402, "insufficient balance"}
		}
		if keyStatus == "quota_exhausted" || !quotaOK || !windowsOK {
			return nil, &apiError{429, "API key quota or spending window exhausted"}
		}
		if err = a.checkPlatformQuota(r.Context(), g.UserID, g.Group.Platform); err != nil {
			return nil, err
		}
	}
	return g, nil
}
func patternMatches(pattern, model string) bool {
	pattern, model = strings.ToLower(pattern), strings.ToLower(model)
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(model, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == model
}
func (g *gatewayGroup) allows(model string) bool {
	if !g.Allowlist.Enabled {
		return true
	}
	for _, pattern := range g.Allowlist.Models {
		if patternMatches(pattern, model) {
			return true
		}
	}
	return false
}

type gatewaySelection struct {
	Account                                    *upstreamAccount
	Rate                                       json.Number
	ChannelID                                  *int64
	ChannelModel, UpstreamModel, BillingSource string
	Pricing                                    []modelPrice
	GroupPricing                               []modelPrice
	Catalog                                    *priceCatalog
	ApplyStats                                 bool
	Restrict                                   bool
	StatsRules                                 []statsPriceRule
	Release                                    func()
}

func (s *gatewaySelection) price(model string) (modelPrice, error) {
	return effectiveModelPrice(s.Catalog, s.GroupPricing, s.Pricing, s.Account.Platform, model, s.Restrict)
}
func (a *App) chooseAccount(ctx context.Context, g *gatewayIdentity, model, protocol string, exclude map[int64]bool, binding *responseBinding, catalog *priceCatalog) (*gatewaySelection, error) {
	s := &gatewaySelection{ChannelModel: model, BillingSource: "channel_mapped", Catalog: catalog, GroupPricing: g.Group.Pricing}
	var channelID int64
	err := a.DB.QueryRowContext(ctx, "SELECT c.id FROM channels c JOIN channel_groups cg ON cg.channel_id=c.id WHERE cg.group_id=$1 AND c.status='active'", g.Key.GroupID).Scan(&channelID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == nil {
		raw, err := channelJSON(ctx, a.DB, channelID)
		if err != nil {
			return nil, err
		}
		var config struct {
			Pricing  []modelPrice                 `json:"model_pricing"`
			Mapping  map[string]map[string]string `json:"model_mapping"`
			Source   string                       `json:"billing_model_source"`
			Restrict bool                         `json:"restrict_models"`
			Apply    bool                         `json:"apply_pricing_to_account_stats"`
			Rules    []statsPriceRule             `json:"account_stats_pricing_rules"`
		}
		if err = json.Unmarshal(raw, &config); err != nil {
			return nil, err
		}
		s.ChannelID = &channelID
		s.Pricing = config.Pricing
		s.ApplyStats = config.Apply
		s.Restrict = config.Restrict
		s.StatsRules = config.Rules
		if config.Source != "" {
			s.BillingSource = config.Source
		}

		for pattern, target := range config.Mapping[g.Group.Platform] {
			if patternMatches(pattern, model) {
				if target != "" && target != "*" {
					s.ChannelModel = target
				}
				break
			}
		}
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT a.id,a.concurrency,a.rate_multiplier::text FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND a.platform=$2 AND a.type='apikey' AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=now()) AND (a.overload_until IS NULL OR a.overload_until<=now()) AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=now()) ORDER BY ag.priority,a.priority,a.last_used_at NULLS FIRST,a.id`, g.Key.GroupID, g.Group.Platform)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		id          int64
		concurrency int
		rate        json.Number
	}
	candidates := []candidate{}
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.concurrency, &c.rate); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, c := range candidates {
		if exclude[c.id] || binding != nil && binding.AccountID != c.id {
			continue
		}
		u, err := a.loadAccount(ctx, c.id)
		if err != nil {
			continue
		}
		if binding != nil && binding.Target != responseTarget(u) {
			continue
		}
		matches := u.protocol() == protocol
		if protocol == "embeddings" {
			matches = u.Platform == "openai" && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if !matches {
			continue
		}
		mapped, err := u.mappedModel(s.ChannelModel)
		if err != nil {
			continue
		}
		if s.Restrict && s.BillingSource == "upstream" {
			s.Account = u
			if _, err = s.price(mapped); err != nil {
				continue
			}
		}
		if !accountQuotaAvailable(u.Extra, time.Now()) {
			continue
		}
		if !a.takeSlot("account", u.ID, c.concurrency) {
			continue
		}
		s.Account = u
		s.Rate = c.rate
		s.UpstreamModel = mapped
		s.Release = func() { a.releaseSlot("account", u.ID) }
		return s, nil
	}
	return nil, &apiError{503, "no available upstream account"}
}
func (a *App) takeSlot(kind string, id int64, limit int) bool {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	if a.gatewayActive == nil {
		a.gatewayActive = map[string]int{}
	}
	key := fmt.Sprintf("%s:%d", kind, id)
	if limit <= 0 || a.gatewayActive[key] >= limit {
		return false
	}
	a.gatewayActive[key]++
	return true
}
func (a *App) releaseSlot(kind string, id int64) {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	key := fmt.Sprintf("%s:%d", kind, id)
	a.gatewayActive[key]--
	if a.gatewayActive[key] <= 0 {
		delete(a.gatewayActive, key)
	}
}
func (a *App) gatewayRPM(ctx context.Context, g *gatewayIdentity) error {
	minute := time.Now().Unix() / 60
	keys := []string{fmt.Sprintf("gateway:rpm:u:%d:%d", g.UserID, minute), fmt.Sprintf("gateway:rpm:g:%d:%d:%d", g.UserID, g.Key.GroupID, minute)}
	result, err := a.Redis.Eval(ctx, `for i=1,2 do local n=tonumber(redis.call('GET',KEYS[i]) or '0');local limit=tonumber(ARGV[i]);if limit>0 and n>=limit then return 0 end end;for i=1,2 do redis.call('INCR',KEYS[i]);redis.call('EXPIRE',KEYS[i],120) end;return 1`, keys, g.RPM, g.Group.RPM).Int()
	if err != nil {
		return &apiError{503, "request limit service unavailable"}
	}
	if result == 0 {
		return &apiError{429, "request rate limit exceeded"}
	}
	return nil
}
func gatewayError(w http.ResponseWriter, err error) {
	status, message := 500, "internal gateway error"
	var e *apiError
	if errors.As(err, &e) {
		status, message = e.status, e.message
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "gateway_error", "code": status}})
}
func (a *App) gatewayRoutes() {
	a.mux.HandleFunc("GET /v1/billing", a.gatewayBilling)
	for _, path := range []string{"/v1/chat/completions", "/chat/completions", "/backend-api/codex/chat/completions"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "chat_completions") })
	}
	a.mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "anthropic") })
	a.mux.HandleFunc("POST /v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "anthropic") })
	a.mux.HandleFunc("POST /v1beta/models/{action}", func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "gemini") })
	for _, path := range []string{"/v1/embeddings", "/embeddings"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "embeddings") })
	}
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, suffix := range []string{"", "/{action...}"} {
			a.mux.HandleFunc("POST "+path+suffix, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "responses") })
		}
	}
}
func (a *App) textGateway(w http.ResponseWriter, r *http.Request, protocol string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id := randomToken(24)
	w.Header().Set("X-Request-ID", id)
	started := time.Now()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(started.Add(30 * time.Second))
	_ = controller.SetWriteDeadline(started.Add(6 * time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	var g *gatewayIdentity
	var selected *gatewaySelection
	committed := false
	fail := func(err error) {
		a.recordGatewayError(id, g, selected, r, err, started)
		if !committed {
			textGatewayError(w, protocol, err)
		} else {
			errorBody := textErrorBody(protocol, err)
			if protocol == "responses" {
				errorBody = map[string]any{"type": "error", "code": "gateway_error", "message": safeGatewayError(err), "param": nil}
			}
			data, _ := json.Marshal(errorBody)
			if protocol == "anthropic" || protocol == "responses" {
				_, _ = io.WriteString(w, "event: error\n")
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			_ = http.NewResponseController(w).Flush()
		}
	}
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	var err error
	if g, err = a.gatewayAuth(r, false); err != nil {
		fail(err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(bad("request body exceeds limit or could not be read"))
		return
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		fail(bad("JSON object required"))
		return
	}
	in, err := parseTextRequest(r, protocol, request)
	if err != nil {
		fail(err)
		return
	}
	model, effort, tier, stream := in.Model, in.Effort, in.Tier, in.Stream
	if protocol == "gemini" && g.Group.Platform != "gemini" && g.Group.Platform != "composite" {
		fail(bad("Gemini native endpoints require a Gemini group"))
		return
	}
	if protocol == "embeddings" && g.Group.Platform != "openai" && g.Group.Platform != "composite" {
		fail(&apiError{404, "embeddings require an OpenAI group"})
		return
	}
	if !g.Group.allows(model) {
		fail(denied())
		return
	}
	canonical, _ := json.Marshal(request)
	payload := string(canonical)
	if protocol == "gemini" {
		payload = model + "\n" + payload
	} else if protocol == "anthropic" {
		payload += "\n" + in.Headers.Get("Anthropic-Version") + "\n" + in.Headers.Get("Anthropic-Beta")
	}
	writer, finish, replayed, claimErr := a.claimGatewayRequest(w, r, g, in.Scope, payload, stream)
	if claimErr != nil {
		fail(claimErr)
		return
	}
	if replayed {
		return
	}
	if writer != nil {
		w = writer
		defer finish()
	}
	g, err = a.gatewayAuth(r, true)
	if err != nil {
		fail(err)
		return
	}
	if !g.Group.allows(model) {
		fail(denied())
		return
	}
	// Look up affinity after the idempotent replay check: replay needs no upstream.
	routingModel := model
	if g.Group.Platform == "composite" {
		config, err := a.loadComposite(ctx, g.Key.GroupID)
		if err != nil {
			fail(err)
			return
		}
		endpoint := protocol
		if protocol == "anthropic" {
			endpoint = "messages"
			if in.CountOnly {
				endpoint = "count_tokens"
			}
		}
		decision := config.resolve(g.Key.GroupID, model, endpoint)
		if !decision.Matched {
			fail(&apiError{404, decision.Reason})
			return
		}
		g.Group.Platform, routingModel = decision.TargetPlatform, decision.UpstreamModel
		if err = a.checkPlatformQuota(ctx, g.UserID, g.Group.Platform); err != nil {
			fail(err)
			return
		}
	}
	if protocol == "gemini" && g.Group.Platform != "gemini" || protocol == "embeddings" && g.Group.Platform != "openai" {
		fail(bad("resolved platform does not support this endpoint"))
		return
	}
	binding, err := a.previousResponse(ctx, g, in.Previous)
	if err != nil {
		fail(err)
		return
	}

	// Single-instance concurrency remains held through settlement, including client cancellation.
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		fail(&apiError{429, "user concurrency limit exceeded"})
		return
	}
	defer a.releaseSlot("user", g.UserID)
	if err = a.gatewayRPM(ctx, g); err != nil {
		fail(err)
		return
	}
	if stream && protocol == "chat_completions" {
		options := map[string]json.RawMessage{}
		if raw := request["stream_options"]; raw != nil && string(raw) != "null" {
			if json.Unmarshal(raw, &options) != nil || options == nil {
				fail(bad("invalid stream_options"))
				return
			}
		}
		options["include_usage"] = json.RawMessage("true")
		request["stream_options"], _ = json.Marshal(options)
	}
	excluded := map[int64]bool{}
	catalog := a.prices.Load()
	var resp *http.Response
	for attempt := 0; attempt < 3; attempt++ {
		selected, err = a.chooseAccount(ctx, g, routingModel, protocol, excluded, binding, catalog)
		if err != nil {
			fail(err)
			return
		}
		if len(selected.UpstreamModel) > 100 {
			selected.Release()
			fail(bad("upstream model exceeds supported length"))
			return
		}
		billingModel := selected.ChannelModel
		if selected.BillingSource == "requested" {
			billingModel = model
		}
		if selected.BillingSource == "upstream" {
			billingModel = selected.UpstreamModel
		}
		if selected.Restrict {
			if _, ok := matchPrice(selected.Pricing, selected.Account.Platform, billingModel); !ok {
				selected.Release()
				fail(denied())
				return
			}
		}
		if !in.CountOnly {
			preflight, priceErr := selected.price(billingModel)
			if priceErr == nil && preflight.BillingMode != "per_request" {
				_, priceErr = calculatePrice(preflight, in.preflightUsage(), g.Group.Rate, tier, effort, "", started, g.Group.LongContext)
			}
			if priceErr != nil {
				selected.Release()
				fail(priceErr)
				return
			}
		}
		path, pathErr := in.upstreamPath(selected.UpstreamModel)
		if pathErr != nil {
			selected.Release()
			fail(pathErr)
			return
		}
		if protocol != "gemini" {
			request["model"], _ = json.Marshal(selected.UpstreamModel)
		} else if in.CountOnly && request["generateContentRequest"] != nil {
			var nested map[string]json.RawMessage
			_ = json.Unmarshal(request["generateContentRequest"], &nested)
			nested["model"], _ = json.Marshal("models/" + strings.TrimPrefix(selected.UpstreamModel, "models/"))
			request["generateContentRequest"], _ = json.Marshal(nested)
		}
		upstreamBody, _ := json.Marshal(request)
		resp, err = a.upstreamRequestHeaders(ctx, selected.Account, "POST", path, upstreamBody, in.Headers)
		if err != nil {
			selected.Release()
			fail(err)
			return
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		status := resp.StatusCode
		resp.Body.Close()
		a.markGatewayFailure(ctx, selected, status, resp.Header.Get("Retry-After"))
		selected.Release()
		if status != 429 && status != 502 && status != 503 && status != 504 {
			fail(&apiError{502, fmt.Sprintf("upstream rejected request (HTTP %d)", status)})
			return
		}
		excluded[selected.Account.ID] = true
		resp = nil
	}
	if resp == nil {
		fail(&apiError{503, "upstream attempts exhausted"})
		return
	}
	defer selected.Release()
	defer resp.Body.Close()
	observation := textObservation{Protocol: protocol, Tier: tier, CountOnly: in.CountOnly, Action: in.Action}
	upstreamID := resp.Header.Get("X-Request-ID")
	if upstreamID == "" {
		upstreamID = resp.Header.Get("Request-Id")
	}
	firstToken := int64(0)
	observe := observation.observe
	var responseBody []byte
	var forwardErr error
	done := false
	terminal := ""
	if !stream {
		responseBody, err = io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		if err != nil || len(responseBody) > 16<<20 {
			forwardErr = &apiError{502, "upstream response interrupted or oversized"}
		} else {
			forwardErr = observe(responseBody)
			if forwardErr == nil && protocol == "responses" && !in.CountOnly && !observation.complete() {
				forwardErr = &apiError{502, "upstream response is not complete"}
			}
		}
	} else {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			fail(&apiError{502, "upstream did not return an event stream"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 4096), 2<<20)
		frame := []string{}
		var size int
		emit := func() error {
			if len(frame) == 0 {
				return nil
			}
			dataLines := []string{}
			for _, line := range frame {
				if strings.HasPrefix(line, "data:") {
					dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			data := strings.Join(dataLines, "\n")
			if protocol == "chat_completions" && data == "[DONE]" {
				terminal = "data: [DONE]\n\n"
				done = true
				return nil
			}
			if data != "" {
				if err := observe([]byte(data)); err != nil {
					return err
				}
				if firstToken == 0 {
					firstToken = time.Since(started).Milliseconds()
				}
			}
			wire := strings.Join(frame, "\n") + "\n\n"
			if protocol != "chat_completions" && (observation.complete() || terminal != "") {
				terminal += wire
				if len(terminal) > 2<<20 {
					return &apiError{502, "upstream terminal frames exceed limit"}
				}
				done = protocol == "anthropic" || protocol == "responses"
				return nil
			}
			committed = true
			if _, err := io.WriteString(w, wire); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		}
		for scanner.Scan() {
			line := scanner.Text()
			size += len(line)
			if size > 2<<20 {
				forwardErr = &apiError{502, "upstream stream frame exceeds limit"}
				break
			}
			if line == "" {
				if err = emit(); err != nil {
					forwardErr = err
					break
				}
				frame = nil
				size = 0
				if done {
					break
				}
			} else {
				frame = append(frame, line)
			}
		}
		if protocol == "gemini" && observation.complete() {
			done = true
		}
		if forwardErr == nil {
			if scanner.Err() != nil {
				forwardErr = &apiError{502, "upstream stream interrupted"}
			} else if !done {
				forwardErr = &apiError{502, "upstream stream ended without completion"}
			}
		}
	}
	if in.CountOnly {
		// Token counts check eligibility but do not create consumption.
	} else if !observation.HasUsage {
		if forwardErr == nil {
			forwardErr = &apiError{502, "upstream usage is missing; billing requires review"}
		}
	} else {
		payloadHash := digest(string(body))
		if protocol == "gemini" {
			payloadHash = digest(model + "\n" + string(body))
		}
		receipt, err := a.makeReceipt(id, g, selected, model, observation.Model, observation.Tier, effort, observation.Usage, stream, time.Since(started), firstToken, started, payloadHash, clientIP(r), r.UserAgent(), r.URL.Path, upstreamID)
		if err == nil {
			receipt.NativeCompaction = in.NativeCompaction
			receipt.Upstream, _ = in.upstreamPath(selected.UpstreamModel)
			receipt.Upstream, _, _ = strings.Cut(receipt.Upstream, "?")
			billingCtx, billingCancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = a.saveReceipt(billingCtx, receipt)
			billingCancel()
		}
		if err != nil {
			forwardErr = &apiError{503, "usage settlement failed; billing requires review"}
			slog.Error("gateway settlement failed", "request_id", id, "error_type", fmt.Sprintf("%T", err))
		}
	}
	if forwardErr != nil {
		fail(forwardErr)
		return
	}
	if protocol == "responses" && !in.CountOnly && in.Action == "" && in.Store {
		bindingCtx, bindingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = a.bindResponse(bindingCtx, g, selected.Account, observation.ResponseID)
		bindingCancel()
		if err != nil {
			fail(err)
			return
		}
	}
	if writer != nil {
		writer.succeeded = true
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	} else {
		committed = true
		_, _ = io.WriteString(w, terminal)
		_ = http.NewResponseController(w).Flush()
	}
}
func safeGatewayError(err error) string {
	var e *apiError
	if errors.As(err, &e) {
		return e.message
	}
	return "gateway request interrupted"
}
func parseChatUsage(raw []byte) (priceUsage, error) {
	var u struct {
		Input        *int64 `json:"prompt_tokens"`
		Output       *int64 `json:"completion_tokens"`
		CacheWrite   int64  `json:"cache_creation_input_tokens"`
		InputDetails struct {
			Cached int64  `json:"cached_tokens"`
			Write  *int64 `json:"cache_write_tokens"`
			Image  int64  `json:"image_tokens"`
		} `json:"prompt_tokens_details"`
		OutputDetails struct {
			Image     int64 `json:"image_tokens"`
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if json.Unmarshal(raw, &u) != nil || u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || *u.Input > 2147483647 || *u.Output > 2147483647 || u.InputDetails.Cached < 0 || u.InputDetails.Cached > *u.Input || u.InputDetails.Image < 0 || u.OutputDetails.Image < 0 || u.OutputDetails.Image > *u.Output {
		return priceUsage{}, &apiError{502, "upstream usage is invalid"}
	}
	if u.InputDetails.Write != nil {
		u.CacheWrite = *u.InputDetails.Write
	}
	if !tokenCountsValid(u.CacheWrite, u.OutputDetails.Reasoning) || u.CacheWrite > *u.Input-u.InputDetails.Cached || u.OutputDetails.Reasoning > *u.Output {
		return priceUsage{}, &apiError{502, "upstream usage is invalid"}
	}
	input := *u.Input - u.InputDetails.Cached - u.CacheWrite
	return priceUsage{Input: input, Output: *u.Output, CacheRead: u.InputDetails.Cached, CacheWrite: u.CacheWrite, ImageInput: min(u.InputDetails.Image, input), ImageOutput: u.OutputDetails.Image}, nil
}
