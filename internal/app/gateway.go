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
	"sort"
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
	imagePrices
	audioPrices
	videoPrices
	reasoningPolicy
	ID              int64               `json:"id"`
	Platform        string              `json:"platform"`
	Rate            json.Number         `json:"rate_multiplier"`
	RPM             int                 `json:"rpm_limit"`
	LongContext     bool                `json:"long_context_pricing_enabled"`
	Allowlist       modelAllowlist      `json:"model_allowlist"`
	Manifest        modelManifestConfig `json:"codex_models_manifest_config"`
	Pricing         []modelPrice        `json:"model_pricing"`
	ModelRouting    map[string][]int64  `json:"model_routing"`
	RoutingEnabled  bool                `json:"model_routing_enabled"`
	ClaudeCodeOnly  bool                `json:"claude_code_only"`
	FallbackGroupID *int64              `json:"fallback_group_id"`
	WebSearchPrice  *json.Number        `json:"web_search_price_per_call"`
	SearchPrice     *json.Number        `json:"search_price_per_1k"`
	AllowImage      bool                `json:"allow_image_generation"`
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
	RoutingGroup     *gatewayGroup
	SourcePlatform   string
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
func (a *App) gatewayAuth(r *http.Request, spending bool) (identity *gatewayIdentity, authErr error) {
	defer func() {
		if authErr != nil {
			a.recordIngressRejection(r, authErr)
		}
	}()
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
	g.SourcePlatform = g.Group.Platform
	if execution := imageExecution(r.Context()); execution != nil && (g.Key.ID != execution.Task.APIKeyID || g.Key.GroupID != execution.Task.GroupID || g.UserID != execution.Task.UserID) {
		return nil, conflict("image task API key assignment changed")
	}
	if turn := socketTurn(r.Context()); turn != nil && (g.Key.ID != turn.socket.keyID || g.Key.GroupID != turn.socket.groupID || g.UserID != turn.socket.userID) {
		return nil, conflict("API key assignment changed; reconnect")
	}
	if err = a.resolveClientGroup(r, g); err != nil {
		return nil, err
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

// Rules select a preferred pool, not a whitelist. Preserve case-sensitive
// matching and exact precedence; longest prefix removes map iteration ambiguity.
func (g *gatewayGroup) routingAccounts(model, platform string) []int64 {
	if !g.RoutingEnabled || platform != "openai" && platform != "anthropic" {
		return nil
	}
	if ids := g.ModelRouting[model]; len(ids) > 0 {
		return ids
	}
	best := ""
	for pattern, ids := range g.ModelRouting {
		if len(ids) > 0 && strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, strings.TrimSuffix(pattern, "*")) && len(pattern) > len(best) {
			best = pattern
		}
	}
	return g.ModelRouting[best]
}

type gatewaySelection struct {
	Audio                                      string
	Search                                     string
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
	Release                                    func() `json:"-"`
}

func (s *gatewaySelection) price(model string) (modelPrice, error) {
	return effectiveModelPrice(s.Catalog, s.GroupPricing, s.Pricing, s.Account.Platform, model, s.Restrict)
}
func (a *App) chooseAccount(ctx context.Context, g *gatewayIdentity, model string, in textRequest, exclude map[int64]bool, binding, sticky *responseBinding, catalog *priceCatalog) (*gatewaySelection, error) {
	protocol := in.Protocol
	routing := g.dispatchGroup()
	s := &gatewaySelection{ChannelModel: model, BillingSource: "channel_mapped", Catalog: catalog, GroupPricing: g.Group.Pricing}
	if audioProtocol(protocol) {
		s.Audio = protocol
		s.ChannelModel = protocol
	}
	if protocol == "realtime" {
		s.Audio = protocol
	}
	if protocol == "alpha_search" || grokSearchProtocol(protocol) {
		s.Search = protocol
	}
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

		platform := g.Group.Platform
		if g.RoutingGroup != nil {
			platform = g.SourcePlatform
		}
		for pattern, target := range config.Mapping[platform] {
			if patternMatches(pattern, model) {
				if target != "" && target != "*" {
					s.ChannelModel = target
				}
				break
			}
		}
	}
	var fallbackPrices []modelPrice
	var fallbackSource string
	if g.RoutingGroup != nil {
		fallbackPrices, fallbackSource, err = a.fallbackChannelRestriction(ctx, routing.ID, g.Group.Platform, model)
		if err != nil {
			return nil, err
		}
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT a.id,a.concurrency,a.rate_multiplier::text FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND a.platform=$2 AND a.type='apikey' AND a.deleted_at IS NULL AND a.status='active' AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=now()) AND (a.overload_until IS NULL OR a.overload_until<=now()) AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=now()) ORDER BY ag.priority,a.priority,a.last_used_at NULLS FIRST,a.id`, routing.ID, g.Group.Platform)
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
	preferred := routing.routingAccounts(s.ChannelModel, g.Group.Platform)
	if sticky != nil {
		u, err := a.loadAccount(ctx, sticky.AccountID)
		if err != nil || responseTarget(u) != sticky.Target {
			sticky = nil
		}
	}
	if len(preferred) > 0 || sticky != nil {
		pool := make(map[int64]bool, len(preferred))
		for _, id := range preferred {
			pool[id] = true
		}
		// Explicit model routing outranks soft affinity. Within the preferred
		// pool, keep the session before applying priority/last-used ordering.
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i].id, candidates[j].id
			if pool[left] != pool[right] {
				return pool[left]
			}
			return sticky != nil && (len(preferred) == 0 || pool[left]) && left == sticky.AccountID && right != sticky.AccountID
		})
	}
	var busy *accountBusy
	for _, c := range candidates {
		if exclude[c.id] || binding != nil && binding.AccountID != c.id {
			continue
		}
		u, err := a.loadAccount(ctx, c.id)
		if err != nil {
			continue
		}
		if turn := socketTurn(ctx); turn != nil && !turn.socket.eligible(u) {
			continue
		}
		if binding != nil && binding.Target != responseTarget(u) {
			continue
		}
		matches := u.protocol() == protocol
		if (protocol == "chat_completions" || protocol == "anthropic") && u.protocol() == "gemini" {
			matches = true
		}
		if protocol == "anthropic" && !in.CountOnly && (u.protocol() == "chat_completions" || u.protocol() == "responses") && messagesChatPlatform(u.Platform) {
			matches = true
		}
		if protocol == "chat_completions" && u.protocol() == "responses" && chatResponsesPlatform(u.Platform) {
			matches = true
		}
		if protocol == "chat_completions" && u.protocol() == "anthropic" && chatAnthropicPlatform(u.Platform) {
			matches = true
		}
		if protocol == "responses" && u.protocol() == "chat_completions" && chatResponsesPlatform(u.Platform) && in.Action == "" && !in.NativeCompaction && socketTurn(ctx) == nil {
			matches = true
		}
		if protocol == "responses" && u.protocol() == "gemini" && in.Action == "" && !in.NativeCompaction && socketTurn(ctx) == nil {
			matches = true
		}
		if protocol == "responses" && u.protocol() == "anthropic" && chatAnthropicPlatform(u.Platform) && in.Action == "" && !in.NativeCompaction && socketTurn(ctx) == nil {
			matches = true
		}
		if protocol == "embeddings" {
			matches = u.Platform == "openai" && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if protocol == "seedance" {
			matches = u.supportsSeedance()
		}
		if protocol == "videos" {
			matches = u.Platform == "grok" && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if protocol == "alpha_search" {
			matches = u.Platform == "openai" && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if protocol == "images" {
			matches = (u.Platform == "openai" || u.Platform == "grok") && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if grokSearchProtocol(protocol) || voiceProtocol(protocol) || protocol == "custom-voices" {
			matches = u.Platform == "grok" && (u.protocol() == "chat_completions" || u.protocol() == "responses")
		}
		if !matches {
			continue
		}
		if !u.allowsOpenAIProtocol(protocol) {
			continue
		}
		mapped, err := u.mappedModel(s.ChannelModel)
		if audioProtocol(protocol) || protocol == "custom-voices" {
			mapped, err = protocol, nil
		}
		if protocol == "realtime" {
			mapped, err = in.Model, nil
		}
		if err != nil {
			continue
		}
		if fallbackSource == "upstream" {
			if _, ok := matchPrice(fallbackPrices, u.Platform, mapped); !ok {
				continue
			}
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
			if busy == nil && c.concurrency > 0 {
				busy = &accountBusy{ID: c.id}
			}
			continue
		}
		s.Account = u
		s.Rate = c.rate
		s.UpstreamModel = mapped
		s.Release = func() { a.releaseSlot("account", u.ID) }
		return s, nil
	}
	if busy != nil {
		return nil, busy
	}
	return nil, errNoUpstream
}
func (a *App) takeSlot(kind string, id int64, limit int) bool {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	if a.gatewayActive == nil {
		a.gatewayActive = map[string]int{}
	}
	key := fmt.Sprintf("%s:%d", kind, id)
	if a.gatewayStopped || a.instanceLost.Load() || limit <= 0 || a.gatewayActive[key] >= limit {
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
	a.wakeGatewayLocked()
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
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": gatewayErrorType(err), "code": status}})
}
func (a *App) gatewayRoutes() {
	a.customVoiceRoutes()
	for _, path := range []string{"/v1/realtime", "/realtime"} {
		a.mux.HandleFunc("GET "+path, a.grokRealtime)
	}
	a.mux.HandleFunc("GET /v1/billing", a.gatewayBilling)
	a.mux.HandleFunc("GET /v1/usage", a.gatewayUsage)
	for _, protocol := range []string{"web_search", "x_search", "tts", "stt"} {
		for _, prefix := range []string{"/v1/", "/"} {
			a.mux.HandleFunc("POST "+prefix+protocol, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, protocol) })
		}
	}
	for _, path := range []string{"/v1/alpha/search", "/alpha/search", "/backend-api/codex/alpha/search"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "alpha_search") })
	}
	for _, path := range []string{"/v1/chat/completions", "/chat/completions", "/backend-api/codex/chat/completions"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "chat_completions") })
	}
	a.mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "anthropic") })
	for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "anthropic") })
	}
	a.mux.HandleFunc("POST /v1beta/models/{action}", func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "gemini") })
	for _, path := range []string{"/v1/embeddings", "/embeddings"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "embeddings") })
	}
	for _, path := range []string{"/v1/images/generations", "/images/generations", "/backend-api/codex/images/generations", "/v1/images/edits", "/images/edits", "/backend-api/codex/images/edits"} {
		a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "images") })
	}
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		a.mux.HandleFunc("GET "+path, a.responsesWebSocket)
		for _, suffix := range []string{"", "/{action...}"} {
			a.mux.HandleFunc("POST "+path+suffix, func(w http.ResponseWriter, r *http.Request) { a.textGateway(w, r, "responses") })
		}
	}
}
func (a *App) textGateway(w http.ResponseWriter, r *http.Request, protocol string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id := randomToken(24)
	if execution := imageExecution(r.Context()); execution != nil {
		id = execution.Task.ID
	}
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
				errorBody = map[string]any{"type": "error", "code": gatewayErrorType(err), "message": safeGatewayError(err), "param": nil}
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	var err error
	if g, err = a.gatewayAuth(r, false); err != nil {
		fail(err)
		return
	}
	bodyLimit := int64(4 << 20)
	if protocol == "images" || protocol == "gemini" || audioProtocol(protocol) {
		bodyLimit = 32 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, bodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(bad("request body exceeds limit or could not be read"))
		return
	}
	var request map[string]json.RawMessage
	var audioIn *audioRequest
	if audioProtocol(protocol) {
		audioIn, err = parseAudioRequest(protocol, r.Header.Get("Content-Type"), body)
		request = map[string]json.RawMessage{}
	} else if protocol == "images" && strings.HasSuffix(r.URL.Path, "/edits") && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		request, err = parseImageMultipart(body, r.Header.Get("Content-Type"))
	} else {
		err = json.Unmarshal(body, &request)
	}
	if err != nil || request == nil {
		if audioProtocol(protocol) {
			fail(err)
		} else {
			fail(bad("JSON object or image edit multipart form required"))
		}
		return
	}
	in, err := parseTextRequest(r, protocol, request)
	if err != nil {
		fail(err)
		return
	}
	if in.Stream {
		_ = controller.SetWriteDeadline(started.Add(31 * time.Minute))
	} else if imageExecution(ctx) == nil {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, started.Add(5*time.Minute))
		defer stop()
		r = r.WithContext(ctx)
	}
	if audioIn != nil && audioIn.Model != "" {
		in.Model = audioIn.Model
	}
	r = r.WithContext(context.WithValue(r.Context(), clientPolicyKey{}, clientPolicy{protocol, claudeCodeClient(r, request)}))
	ctx = r.Context()
	if err = a.resolveClientGroup(r, g); err != nil {
		fail(err)
		return
	}
	model, effort, tier, stream := in.Model, in.Effort, in.Tier, in.Stream
	includeChatUsage := false
	if raw := request["stream_options"]; protocol == "chat_completions" && raw != nil {
		var options struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if json.Unmarshal(raw, &options) != nil {
			fail(bad("invalid stream_options"))
			return
		}
		includeChatUsage = options.IncludeUsage
	}
	originalEffort := requestedEffort(request, model)
	if in.Search != nil {
		originalEffort = nil
		if g.Group.Platform != "grok" {
			fail(&apiError{404, "this endpoint requires a Grok group"})
			return
		}
	}
	if audioProtocol(protocol) && g.Group.Platform != "grok" {
		fail(&apiError{404, "voice endpoints require a Grok group"})
		return
	}
	if protocol == "gemini" && g.Group.Platform != "gemini" && g.Group.Platform != "composite" {
		fail(bad("Gemini native endpoints require a Gemini group"))
		return
	}
	if (protocol == "embeddings" || protocol == "alpha_search") && g.Group.Platform != "openai" && g.Group.Platform != "composite" {
		fail(&apiError{404, "this endpoint requires an OpenAI group"})
		return
	}
	if protocol == "images" && g.Group.Platform != "openai" && g.Group.Platform != "grok" && g.Group.Platform != "composite" {
		fail(&apiError{404, "this endpoint requires an OpenAI or Grok group"})
		return
	}
	if protocol == "images" && !g.Group.AllowImage {
		fail(denied())
		return
	}
	if !g.Group.allows(model) {
		fail(denied())
		return
	}
	canonical, _ := json.Marshal(request)
	payload := string(canonical)
	if audioIn != nil {
		payload = audioIn.ContentType + "\n" + string(audioIn.Body)
	}
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
	ping := func() error {
		if !stream {
			return nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		committed = true
		if _, err := io.WriteString(w, ": waiting for concurrency\n\n"); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	// A queued user is reauthorized before routing and pricing are captured.
	g, err = a.acquireGatewayUser(r, g, model, ping)
	if err != nil {
		fail(err)
		return
	}
	defer a.releaseSlot("user", g.UserID)
	groupSnapshot := g.Group
	routingGroup := g.dispatchGroup()
	g.Group.Platform = routingGroup.Platform
	// Look up affinity after the idempotent replay check: replay needs no upstream.
	routingModel := model
	if g.Group.Platform == "composite" {
		config, err := a.loadComposite(ctx, routingGroup.ID)
		if err != nil {
			fail(err)
			return
		}
		decision := config.resolve(routingGroup.ID, model, in.compositeEndpoint())
		if !decision.Matched {
			fail(&apiError{404, decision.Reason})
			return
		}
		g.Group.Platform, routingModel = decision.TargetPlatform, decision.UpstreamModel
	}
	if g.SourcePlatform == "composite" {
		if err = a.checkPlatformQuota(ctx, g.UserID, g.Group.Platform); err != nil {
			fail(err)
			return
		}
	}
	if audioProtocol(protocol) && g.Group.Platform != "grok" || protocol == "gemini" && g.Group.Platform != "gemini" || (protocol == "embeddings" || protocol == "alpha_search") && g.Group.Platform != "openai" || protocol == "images" && g.Group.Platform != "openai" && g.Group.Platform != "grok" {
		fail(bad("resolved platform does not support this endpoint"))
		return
	}
	if in.Search != nil && g.Group.Platform != "grok" {
		fail(bad("this endpoint requires a Grok group"))
		return
	}
	if protocol == "images" && !g.Group.AllowImage {
		fail(denied())
		return
	}
	if socketTurn(ctx) != nil && g.Group.Platform != "openai" && g.Group.Platform != "grok" {
		fail(bad("Responses WebSocket requires an OpenAI or Grok target"))
		return
	}
	session, err := gatewaySessionKey(r, g, in, request)
	if err != nil {
		fail(err)
		return
	}
	if protocol != "gemini" && protocol != "embeddings" && protocol != "alpha_search" && protocol != "images" && in.Search == nil && audioIn == nil {
		if err = g.Group.reasoningPolicy.apply(request, model, g.Group.Platform); err == nil {
			effort, err = requestEffort(request, protocol)
		}
		if err != nil {
			fail(err)
			return
		}
	}
	binding, err := a.previousResponse(ctx, g, in.Previous)
	if err != nil {
		fail(err)
		return
	}
	if audioIn != nil && protocol == "tts" {
		var native string
		native, binding, err = a.resolveVoice(ctx, g, audioIn.Voice)
		if err != nil {
			fail(err)
			return
		}
		if audioIn.Voice != "" {
			var audioBody map[string]json.RawMessage
			_ = json.Unmarshal(audioIn.Body, &audioBody)
			delete(audioBody, "voice")
			audioBody["voice_id"], _ = json.Marshal(native)
			audioIn.Body, _ = json.Marshal(audioBody)
		}
	}
	var reasoningInput map[string]json.RawMessage
	if protocol == "anthropic" {
		reasoningInput, binding, err = a.messagesReasoningInput(g, request)
		if err != nil {
			fail(err)
			return
		}
	}
	history, err := a.chatHistory(g, in.Previous, binding)
	if err != nil {
		fail(err)
		return
	}
	sticky, err := a.stickySession(ctx, session)
	if err != nil {
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
	var upstreamStarted time.Time
	var lastRejection *passthroughError
	var lastRejectedAccount *gatewaySelection
	wireIn := in
	var chatBridge *responseChatStream
	var messagesBridge *chatMessagesStream
	var responsesMessages *responsesMessagesStream
	var responsesGemini *responsesGeminiStream
	var anthropicBridge *anthropicChatStream
	var geminiBridge *geminiChatStream
	var geminiMessages *geminiMessagesStream
	var anthropicResponses *anthropicResponsesStream
	var responsesBridge *chatResponsesStream
	var chatRequest *responsesChatRequest
	maxAttempts := 3
	if in.Search != nil || audioIn != nil {
		maxAttempts = 4
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		selected, err = a.chooseAccount(ctx, g, routingModel, in, excluded, binding, sticky, catalog)
		var busy *accountBusy
		if errors.As(err, &busy) {
			_, err = a.waitAdmission(ctx, "account", busy.ID, gatewayQueueTimeout, func(waitCtx context.Context) (bool, error) {
				if err := a.revalidateQueuedRequest(r.WithContext(waitCtx), g, groupSnapshot, in, routingModel); err != nil {
					return false, err
				}
				var selectErr error
				selected, selectErr = a.chooseAccount(waitCtx, g, routingModel, in, excluded, binding, sticky, catalog)
				if selected != nil && waitCtx.Err() != nil {
					selected.Release()
					selected = nil
					return false, waitCtx.Err()
				}
				var busy *accountBusy
				if errors.As(selectErr, &busy) {
					return false, nil
				}
				return selected != nil, selectErr
			}, ping)
		}
		if err != nil {
			if errors.Is(err, errNoUpstream) && lastRejection != nil {
				selected = lastRejectedAccount
				err = lastRejection
			}
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
		if selected.Search != "" {
			if _, err = g.Group.searchCost(selected.Search); err != nil {
				selected.Release()
				fail(err)
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
		if protocol == "images" && in.Action == "edits" && selected.Account.Platform == "grok" {
			request, err = imagesToGrok(request)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
		}
		upstreamBody, _ := json.Marshal(request)
		wireIn, chatBridge = in, nil
		wireIn.Effort = effort
		anthropicBridge = nil
		geminiBridge = nil
		geminiMessages = nil
		anthropicResponses = nil
		responsesBridge, chatRequest = nil, nil
		messagesBridge = nil
		responsesMessages = nil
		responsesGemini = nil
		if protocol == "anthropic" && selected.Account.protocol() == "gemini" {
			upstreamBody, wireIn.Effort, err = messagesToGemini(request, reasoningInput, in.CountOnly)
			if err == nil {
				wireIn.Protocol, wireIn.Headers, wireIn.Action = "gemini", nil, "generateContent"
				if in.CountOnly {
					wireIn.Action = "countTokens"
				} else if stream {
					wireIn.Action = "streamGenerateContent"
				}
				path, err = wireIn.upstreamPath(selected.UpstreamModel)
			}
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			if !in.CountOnly {
				geminiMessages = newGeminiMessagesStream(model, func(item json.RawMessage) (string, error) {
					return a.sealMessagesReasoning(g, selected.Account, item)
				})
			}
		}
		if protocol == "chat_completions" && selected.Account.protocol() == "gemini" {
			var custom map[string]bool
			upstreamBody, custom, wireIn.Effort, err = chatToGemini(request)
			if err == nil {
				wireIn.Protocol, wireIn.Headers, wireIn.Action = "gemini", nil, "generateContent"
				if stream {
					wireIn.Action = "streamGenerateContent"
				}
				path, err = wireIn.upstreamPath(selected.UpstreamModel)
			}
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			geminiBridge = newGeminiChatStream(model, includeChatUsage, custom)
		}
		if protocol == "anthropic" && selected.Account.protocol() == "responses" {
			upstreamBody, wireIn.Effort, err = messagesToResponses(request, reasoningInput)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			wireIn.Protocol, wireIn.Headers = "responses", nil
			path = "/v1/responses"
			responsesMessages = newResponsesMessagesStream(model, func(item json.RawMessage) (string, error) {
				return a.sealMessagesReasoning(g, selected.Account, item)
			})
		}
		if protocol == "anthropic" && selected.Account.protocol() == "chat_completions" {
			upstreamBody, wireIn.Effort, err = messagesToChat(request)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			wireIn.Protocol, wireIn.Headers = "chat_completions", nil
			path = "/v1/chat/completions"
			messagesBridge = newChatMessagesStream(model)
		}
		if protocol == "chat_completions" && selected.Account.protocol() == "anthropic" {
			var custom map[string]bool
			upstreamBody, custom, wireIn.Effort, err = chatToAnthropic(request)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			wireIn.Protocol = "anthropic"
			path = "/v1/messages"
			anthropicBridge = newAnthropicChatStream(model, includeChatUsage, custom)
		}
		if protocol == "chat_completions" && selected.Account.protocol() == "responses" {
			upstreamBody, err = chatToResponses(request)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			wireIn.Protocol = "responses"
			path = "/v1/responses"
			chatBridge = newResponseChatStream(model, includeChatUsage)
		}
		if protocol == "responses" && selected.Account.protocol() != "responses" {
			if selected.Account.protocol() == "anthropic" {
				chatRequest, wireIn.Effort, err = responsesToAnthropicRequest(request, history)
			} else {
				chatRequest, err = responsesToChatRequest(request, history)
			}
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			upstreamBody, err = json.Marshal(chatRequest.Body)
			if err != nil {
				selected.Release()
				fail(err)
				return
			}
			if in.Store && (len(upstreamBody) > 2<<20 || len(chatRequest.History) >= 256) {
				selected.Release()
				fail(bad("response history exceeds limit; use store=false"))
				return
			}
			wireIn.Protocol = selected.Account.protocol()
			path, _ = wireIn.upstreamPath(selected.UpstreamModel)
			if wireIn.Protocol == "gemini" {
				var custom map[string]bool
				for _, field := range []string{"generationConfig", "modalities"} {
					if raw := request[field]; raw != nil {
						chatRequest.Body[field] = raw
					}
				}
				upstreamBody, custom, wireIn.Effort, err = responsesGeminiRequest(chatRequest.Body)
				if err != nil {
					selected.Release()
					fail(err)
					return
				}
				wireIn.Action = "generateContent"
				if stream {
					wireIn.Action = "streamGenerateContent"
				}
				path, err = wireIn.upstreamPath(selected.UpstreamModel)
				if err != nil {
					selected.Release()
					fail(err)
					return
				}
				responsesGemini = newResponsesGeminiStream(model, custom)
			} else if wireIn.Protocol == "anthropic" {
				anthropicResponses = newAnthropicResponsesStream(model, chatRequest)
			} else {
				responsesBridge = newChatResponsesStream(model, chatRequest.Custom)
				responsesBridge.Namespaces = chatRequest.Namespaces
				responsesBridge.ToolSearch = chatRequest.ToolSearch
			}
		}
		if in.Search != nil {
			upstreamBody = in.Search.upstreamBody(protocol, selected.UpstreamModel)
		}
		if audioIn != nil {
			upstreamBody = audioIn.Body
			wireIn.Headers = audioIn.headers()
			if _, err = selected.audioCost(g.Group, billingModel, "1", started); err != nil {
				selected.Release()
				fail(err)
				return
			}
		}
		if wireIn.Protocol == "gemini" && protocol != "gemini" {
			// Use the converted wire request for both validation and billing size;
			// client aliases and ignored native options must not change the tariff.
			var native map[string]json.RawMessage
			_ = json.Unmarshal(upstreamBody, &native)
			probe := r.Clone(ctx)
			probe.SetPathValue("action", selected.UpstreamModel+":"+wireIn.Action)
			parsed, parseErr := parseTextRequest(probe, "gemini", native)
			if parseErr != nil {
				selected.Release()
				fail(parseErr)
				return
			}
			wireIn.ImageGeneration, wireIn.ImageSize = parsed.ImageGeneration, parsed.ImageSize
			wireIn.ImageSizeSource, wireIn.ImageInputSize = parsed.ImageSizeSource, parsed.ImageInputSize
		}
		wireIn.ImageGeneration = wireIn.ImageGeneration || wireIn.Protocol == "gemini" && geminiImageModel(selected.UpstreamModel)
		if !in.CountOnly && selected.Search == "" && audioIn == nil {
			preflight, priceErr := selected.price(billingModel)
			rate, label := g.Group.Rate, ""
			if wireIn.Protocol == "gemini" && wireIn.ImageGeneration {
				preflight, rate, priceErr = selected.geminiImagePrice(g.Group, billingModel, wireIn.ImageSize)
				label = wireIn.ImageSize
			}
			if priceErr == nil {
				_, priceErr = calculatePrice(preflight, wireIn.preflightUsage(), rate, tier, wireIn.Effort, label, started, g.Group.LongContext)
			}
			if priceErr != nil {
				selected.Release()
				fail(priceErr)
				return
			}
		}
		if _, err = a.admissionWake(); err != nil || ctx.Err() != nil {
			selected.Release()
			if err == nil {
				err = &apiError{499, "request canceled before dispatch"}
			}
			fail(err)
			return
		}
		// Queuing and rejected preflights do not consume RPM. Account retries
		// share the single admission count from the first dispatch.
		if attempt == 0 {
			if err = a.gatewayRPM(ctx, g); err != nil {
				selected.Release()
				fail(err)
				return
			}
		}
		if err = a.bindSession(ctx, session, selected.Account); err != nil {
			selected.Release()
			fail(err)
			return
		}
		upstreamStarted = time.Now()
		if turn := socketTurn(ctx); turn != nil {
			resp, err = a.socketUpstream(ctx, selected.Account, request, turn)
		} else {
			upstreamCtx, upstreamCancel := context.WithCancel(ctx)
			defer upstreamCancel()
			resp, err = a.upstreamRequestHeaders(upstreamCtx, selected.Account, "POST", path, upstreamBody, wireIn.Headers)
			if err == nil && stream && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				idle := a.streamIdle
				if wireIn.ImageGeneration {
					idle = a.imageStreamIdle
				}
				resp.Body = &idleStreamBody{ReadCloser: resp.Body, ctx: upstreamCtx, cancel: upstreamCancel, idle: idle}
			}
		}
		if err != nil {
			selected.Release()
			fail(err)
			return
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			break
		}
		status := resp.StatusCode
		failureBody := readUpstreamError(resp)
		resp.Body.Close()
		failure := a.upstreamError(ctx, selected.Account, status, failureBody, &apiError{502, fmt.Sprintf("upstream rejected request (HTTP %d)", status)})
		lastRejection = nil
		_ = errors.As(failure, &lastRejection)
		lastRejectedAccount = selected
		if !skipErrorMonitoring(failure) {
			a.recordUpstreamFailure(id, g, selected, r, in, path, status, started)
		}
		searchEndpointError := protocol == "alpha_search" && (status == 401 || status == 404 || status == 405)
		if !searchEndpointError {
			a.markGatewayFailure(ctx, selected, status, resp.Header.Get("Retry-After"), failureBody)
		}
		selected.Release()
		grokRetry := (in.Search != nil || audioIn != nil) && (status == 401 || status == 402 || status == 403 || status >= 500)
		if !searchEndpointError && !grokRetry && !balanceFailure(selected.Account.Platform, status, failureBody) && status != 429 && status != 502 && status != 503 && status != 504 && status != 529 {
			fail(failure)
			return
		}
		excluded[selected.Account.ID] = true
		resp = nil
	}
	if resp == nil {
		if lastRejection != nil {
			fail(lastRejection)
			return
		}
		fail(&apiError{503, "upstream attempts exhausted"})
		return
	}
	defer selected.Release()
	defer resp.Body.Close()
	observation := textObservation{Protocol: wireIn.Protocol, Tier: tier, CountOnly: in.CountOnly, Action: in.Action}
	upstreamID := resp.Header.Get("X-Request-ID")
	if upstreamID == "" {
		upstreamID = resp.Header.Get("Xai-Request-Id")
	}
	if upstreamID == "" {
		upstreamID = resp.Header.Get("Request-Id")
	}
	firstToken := int64(0)
	observe := observation.observe
	var responseBody []byte
	var forwardErr error
	responseType := "application/json"
	done := false
	terminal := ""
	if !stream {
		responseBody, err = io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		if err != nil || len(responseBody) > 16<<20 {
			forwardErr = &apiError{502, "upstream response interrupted or oversized"}
			// TTS input is already consumed when audio arrives. Losing the rest
			// of the response cannot erase this known charge.
			if audioIn != nil && protocol == "tts" && len(responseBody) > 0 {
				if _, typeErr := audioResponseType(protocol, resp.Header.Get("Content-Type")); typeErr == nil {
					observation.Usage, _ = audioIn.usage(protocol, responseBody, time.Since(upstreamStarted))
					observation.HasUsage = true
				}
			}
		} else if audioIn != nil {
			responseType, forwardErr = audioResponseType(protocol, resp.Header.Get("Content-Type"))
			if forwardErr == nil {
				observation.Usage, forwardErr = audioIn.usage(protocol, responseBody, time.Since(upstreamStarted))
				observation.HasUsage = forwardErr == nil
			}
		} else if in.Search != nil {
			responseBody, observation.Model, forwardErr = in.Search.response(responseBody)
			if forwardErr == nil {
				observation.Usage, observation.HasUsage = priceUsage{Requests: 1}, true
			}
		} else {
			forwardErr = observe(responseBody)
			if forwardErr == nil && in.CountOnly && protocol == "anthropic" && wireIn.Protocol == "gemini" {
				var count struct {
					Total int64 `json:"totalTokens"`
				}
				_ = json.Unmarshal(responseBody, &count)
				responseBody, _ = json.Marshal(map[string]int64{"input_tokens": count.Total})
			}
			if forwardErr == nil && wireIn.Protocol == "responses" && !in.CountOnly && !observation.complete() {
				forwardErr = &apiError{502, "upstream response is not complete"}
			}
			if forwardErr == nil && chatBridge != nil {
				responseBody, forwardErr = responsesToChat(responseBody, model)
			}
			if forwardErr == nil && messagesBridge != nil {
				_, forwardErr = messagesBridge.observe(responseBody, false)
				if forwardErr == nil {
					_, responseBody, forwardErr = messagesBridge.finish(observation.Usage)
				}
			}
			if forwardErr == nil && responsesMessages != nil {
				responseBody, forwardErr = responsesMessages.response(responseBody, observation.Usage)
			}
			if forwardErr == nil && anthropicBridge != nil {
				responseBody, forwardErr = anthropicBridge.response(responseBody, observation.Usage)
			}
			if forwardErr == nil && geminiBridge != nil {
				_, forwardErr = geminiBridge.observe(responseBody)
				if forwardErr == nil {
					_, responseBody, forwardErr = geminiBridge.finish(observation.Usage)
				}
			}
			if forwardErr == nil && geminiMessages != nil {
				_, forwardErr = geminiMessages.observe(responseBody)
				if forwardErr == nil {
					_, responseBody, forwardErr = geminiMessages.finish(observation.Usage)
				}
			}
			if forwardErr == nil && responsesGemini != nil {
				responseBody, forwardErr = responsesGemini.response(responseBody, observation.Usage)
				observation.ResponseID = responsesGemini.Output.ID
			}
			if forwardErr == nil && anthropicResponses != nil {
				responseBody, forwardErr = anthropicResponses.response(responseBody, observation.Usage)
				observation.ResponseID = anthropicResponses.Output.ID
			}
			if forwardErr == nil && responsesBridge != nil {
				_, forwardErr = responsesBridge.observe(responseBody, false)
				if forwardErr == nil {
					_, responseBody, forwardErr = responsesBridge.finish()
					observation.ResponseID = responsesBridge.ID
				}
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
		frameLimit := 2 << 20
		if wireIn.Protocol == "gemini" {
			frameLimit = 16 << 20
		}
		scanner.Buffer(make([]byte, 4096), frameLimit)
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
			if wireIn.Protocol == "chat_completions" && data == "[DONE]" {
				terminal = "data: [DONE]\n\n"
				if messagesBridge != nil {
					var err error
					terminal, _, err = messagesBridge.finish(observation.Usage)
					if err != nil {
						return err
					}
				}
				if responsesBridge != nil {
					var err error
					terminal, _, err = responsesBridge.finish()
					if err != nil {
						return err
					}
					observation.ResponseID = responsesBridge.ID
				}
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
			if geminiMessages != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = geminiMessages.observe([]byte(data))
				if err != nil {
					return err
				}
			}
			if geminiBridge != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = geminiBridge.observe([]byte(data))
				if err != nil {
					return err
				}
			}
			if responsesMessages != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = responsesMessages.event([]byte(data), observation.Usage)
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if responsesGemini != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = responsesGemini.observe([]byte(data))
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if responsesGemini != nil {
				committed = true
				if _, err := io.WriteString(w, wire); err != nil {
					return err
				}
				return http.NewResponseController(w).Flush()
			}
			if messagesBridge != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = messagesBridge.observe([]byte(data), true)
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if anthropicResponses != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = anthropicResponses.event([]byte(data), observation.Usage)
				if err != nil {
					return err
				}
				observation.ResponseID = anthropicResponses.Output.ID
				if wire == "" {
					return nil
				}
			}
			if anthropicBridge != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = anthropicBridge.event([]byte(data), observation.Usage)
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if responsesBridge != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = responsesBridge.observe([]byte(data), true)
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if chatBridge != nil {
				if data == "" {
					return nil
				}
				var err error
				wire, err = chatBridge.event([]byte(data))
				if err != nil {
					return err
				}
				if wire == "" {
					return nil
				}
			}
			if wireIn.Protocol != "chat_completions" && (observation.complete() || terminal != "") {
				terminal += wire
				if len(terminal) > frameLimit {
					return &apiError{502, "upstream terminal frames exceed limit"}
				}
				done = wireIn.Protocol == "anthropic" || wireIn.Protocol == "responses"
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
			if size > frameLimit {
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
		if wireIn.Protocol == "gemini" && observation.complete() {
			done = true
		}
		if forwardErr == nil {
			if errors.Is(scanner.Err(), errStreamIdle) && ctx.Err() == nil {
				forwardErr = &apiError{504, "upstream stream data interval timed out"}
				a.markStreamTimeout(ctx, selected.Account, model)
			} else if scanner.Err() != nil {
				forwardErr = &apiError{502, "upstream stream interrupted"}
			} else if !done {
				forwardErr = &apiError{502, "upstream stream ended without completion"}
			}
		}
		if forwardErr == nil && geminiBridge != nil {
			var end string
			end, _, forwardErr = geminiBridge.finish(observation.Usage)
			terminal += end
		}
		if forwardErr == nil && geminiMessages != nil {
			var end string
			end, _, forwardErr = geminiMessages.finish(observation.Usage)
			terminal += end
		}
		if forwardErr == nil && responsesGemini != nil {
			var end string
			end, _, forwardErr = responsesGemini.finish(observation.Usage)
			terminal += end
			observation.ResponseID = responsesGemini.Output.ID
		}
	}
	if wireIn.Protocol == "gemini" && !in.CountOnly {
		count := observation.ImageCount
		observation.Usage.ImageRequest = wireIn.ImageGeneration || count > 0
		if count == 0 && (geminiImageModel(model) || geminiImageModel(selected.UpstreamModel)) && forwardErr == nil && observation.complete() && !observation.blocked && !observation.ImageRejected {
			count = 1
		}
		if count > 0 {
			observation.Usage.ImageCount = count
			observation.Usage.ImageSize = wireIn.ImageSize
			observation.Usage.ImageSizeSource = wireIn.ImageSizeSource
			observation.Usage.ImageInputSize = wireIn.ImageInputSize
			observation.Usage.Requests = count
		}
		if observation.Usage.ImageRequest && (count > 0 || forwardErr == nil && observation.complete()) {
			if !observation.HasUsage {
				billingModel := selected.ChannelModel
				switch selected.BillingSource {
				case "requested":
					billingModel = model
				case "upstream":
					billingModel = selected.UpstreamModel
				case "response_model":
					if observation.Model != "" {
						billingModel = observation.Model
					}
				}
				p, _, priceErr := selected.geminiImagePrice(g.Group, billingModel, wireIn.ImageSize)
				observation.HasUsage = priceErr == nil && (p.BillingMode == "image" || p.BillingMode == "per_request")
			}
		}
	}
	if in.CountOnly {
		// Token counts check eligibility but do not create consumption.
	} else if turn := socketTurn(ctx); turn != nil && turn.warmup && !observation.HasUsage {
		// Native generate=false prepares state without model usage.
	} else if !observation.HasUsage {
		if forwardErr == nil {
			forwardErr = &apiError{502, "upstream usage is missing; billing requires review"}
		}
	} else {
		payloadHash := digest(string(body))
		if protocol == "gemini" {
			payloadHash = digest(model + "\n" + string(body))
		}
		billingEffort := effort
		if anthropicBridge != nil || anthropicResponses != nil || messagesBridge != nil || responsesMessages != nil || geminiBridge != nil || geminiMessages != nil {
			billingEffort = wireIn.Effort
		}
		receipt, err := a.makeReceipt(id, g, selected, model, observation.Model, observation.Tier, billingEffort, observation.Usage, stream, time.Since(started), firstToken, started, payloadHash, clientIP(r), r.UserAgent(), r.URL.Path, upstreamID)
		if err == nil {
			receipt.WebSocket = socketTurn(ctx) != nil
			receipt.RequestedEffort = originalEffort
			receipt.NativeCompaction = in.NativeCompaction
			receipt.Upstream, _ = wireIn.upstreamPath(selected.UpstreamModel)
			receipt.Upstream, _, _ = strings.Cut(receipt.Upstream, "?")
			billingCtx, billingCancel := context.WithTimeout(context.Background(), 15*time.Second)
			err = a.saveReceipt(billingCtx, receipt)
			billingCancel()
			if execution := imageExecution(ctx); execution != nil {
				// Persist consumption before slow object I/O. An interrupted upload
				// must not erase an already observed billable result.
				storageCtx, storageCancel := context.WithTimeout(context.Background(), 2*time.Minute)
				checkpointErr := a.checkpointImageTask(storageCtx, execution, responseBody, receipt, forwardErr)
				storageCancel()
				if err == nil {
					err = checkpointErr
				}
			}
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
	if turn := socketTurn(ctx); turn != nil {
		turn.socket.remember(observation.ResponseID, selected.Account)
	}
	if protocol == "responses" && !in.CountOnly && in.Action == "" && in.Store {
		bindingCtx, bindingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if anthropicResponses != nil {
			err = a.bindChatResponse(bindingCtx, g, selected.Account, observation.ResponseID, append(chatRequest.History, anthropicResponses.assistant()))
		} else if responsesBridge != nil {
			err = a.bindChatResponse(bindingCtx, g, selected.Account, observation.ResponseID, append(chatRequest.History, responsesBridge.assistant()))
		} else {
			if responsesGemini != nil {
				err = a.bindChatResponse(bindingCtx, g, selected.Account, observation.ResponseID, append(chatRequest.History, responsesGemini.assistant()))
			} else {
				err = a.bindResponse(bindingCtx, g, selected.Account, observation.ResponseID)
			}
		}
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
		w.Header().Set("Content-Type", responseType)
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
