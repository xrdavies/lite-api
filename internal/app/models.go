package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

type modelMetadata struct {
	ID                       string   `json:"id"`
	DisplayName              string   `json:"display_name,omitempty"`
	Description              string   `json:"description,omitempty"`
	Reasoning                *bool    `json:"reasoning,omitempty"`
	DefaultReasoningLevel    string   `json:"default_reasoning_level,omitempty"`
	SupportedReasoningLevels []string `json:"supported_reasoning_levels,omitempty"`
	InputModalities          []string `json:"input_modalities,omitempty"`
	ContextWindow            int64    `json:"context_window,omitempty"`
	MaxContextWindow         int64    `json:"max_context_window,omitempty"`
	MaxOutputTokens          int64    `json:"max_output_tokens,omitempty"`
}

func (m modelMetadata) complete() bool {
	return m.Reasoning != nil && len(m.InputModalities) > 0 && m.ContextWindow > 0 && (!*m.Reasoning || len(m.SupportedReasoningLevels) > 0)
}

type discoveredModel struct {
	modelMetadata
	Created int64    `json:"created,omitempty"`
	Owner   string   `json:"owned_by,omitempty"`
	Methods []string `json:"supportedGenerationMethods,omitempty"`
	Version string   `json:"version,omitempty"`
}

type modelSnapshot struct {
	Source   string                   `json:"source"`
	SyncedAt string                   `json:"synced_at"`
	Models   map[string]modelMetadata `json:"models"`
}

func concreteModel(id string) bool {
	return validModelPattern(id) && len(id) <= 100 && !strings.ContainsAny(id, "*\t")
}

func decodeModel(raw json.RawMessage) (discoveredModel, error) {
	var fields map[string]json.RawMessage
	var m discoveredModel
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return m, &apiError{502, "invalid upstream model entry"}
	}
	// Normalize only documented catalog fields; internal keys and arbitrary
	// provider extensions never enter the account snapshot or client response.
	for _, pair := range [][2]string{{"slug", "id"}, {"name", "id"}, {"displayName", "display_name"}, {"inputTokenLimit", "context_window"}, {"outputTokenLimit", "max_output_tokens"}, {"thinking", "reasoning"}} {
		if fields[pair[1]] == nil && fields[pair[0]] != nil {
			fields[pair[1]] = fields[pair[0]]
		}
	}
	if raw := fields["supported_reasoning_levels"]; raw != nil {
		var levels []json.RawMessage
		if json.Unmarshal(raw, &levels) != nil {
			return m, &apiError{502, "invalid upstream reasoning metadata"}
		}
		names := []string{}
		for _, level := range levels {
			var name string
			if json.Unmarshal(level, &name) != nil {
				var object struct{ Effort string }
				if json.Unmarshal(level, &object) != nil {
					return m, &apiError{502, "invalid upstream reasoning level"}
				}
				name = object.Effort
			}
			switch name {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max":
				names = append(names, name)
			default:
				return m, &apiError{502, "unknown upstream reasoning level"}
			}
		}
		fields["supported_reasoning_levels"], _ = json.Marshal(names)
	}
	encoded, _ := json.Marshal(fields)
	if json.Unmarshal(encoded, &m) != nil {
		return m, &apiError{502, "invalid upstream model metadata"}
	}
	if len(m.SupportedReasoningLevels) > 0 && m.DefaultReasoningLevel != "" {
		valid := false
		for _, level := range m.SupportedReasoningLevels {
			valid = valid || level == m.DefaultReasoningLevel
		}
		if !valid {
			return m, &apiError{502, "invalid upstream default reasoning level"}
		}
	}
	if len(m.Methods) > 100 || len(m.Version) > 200 {
		return m, &apiError{502, "upstream model metadata exceeds limits"}
	}
	for _, method := range m.Methods {
		if len(method) > 100 || strings.ContainsAny(method, "\r\n\x00") {
			return m, &apiError{502, "invalid model generation method"}
		}
	}
	m.ID = strings.TrimPrefix(m.ID, "models/")
	if !concreteModel(m.ID) || len(m.DisplayName) > 1000 || len(m.Description) > 10000 || len(m.Owner) > 200 || m.ContextWindow < 0 || m.MaxContextWindow < 0 || m.MaxOutputTokens < 0 || m.ContextWindow > 2147483647 || m.MaxContextWindow > 2147483647 || m.MaxOutputTokens > 2147483647 {
		return m, &apiError{502, "upstream model metadata exceeds limits"}
	}
	if len(m.SupportedReasoningLevels) > 0 && m.Reasoning == nil {
		enabled := false
		for _, level := range m.SupportedReasoningLevels {
			enabled = enabled || level != "none"
		}
		m.Reasoning = &enabled
	}
	for _, modality := range m.InputModalities {
		if modality != "text" && modality != "image" && modality != "audio" && modality != "video" && modality != "file" {
			return m, &apiError{502, "invalid upstream input modality"}
		}
	}
	return m, nil
}

func modelCacheKey(u *upstreamAccount) string {
	return fmt.Sprintf("gateway:models:%d:%s:%d", u.ID, responseTarget(u), u.UpdatedAt.UnixNano())
}

func (a *App) fetchModels(ctx context.Context, u *upstreamAccount, fresh bool) ([]discoveredModel, error) {
	key := modelCacheKey(u)
	if !fresh {
		if raw, err := a.Redis.Get(ctx, key).Bytes(); err == nil {
			var cached []discoveredModel
			if json.Unmarshal(raw, &cached) == nil && cached != nil {
				return cached, nil
			}
		}
	}
	path := "/v1/models"
	if u.protocol() == "gemini" {
		path = "/v1beta/models"
	}
	models := []discoveredModel{}
	seen, cursors := map[string]bool{}, map[string]bool{}
	query := url.Values{}
	for page := 0; page < 100; page++ {
		endpoint := path
		if len(query) > 0 {
			endpoint += "?" + query.Encode()
		}
		resp, err := a.upstreamRequest(ctx, u, "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		data, err := readUpstreamJSON(resp)
		resp.Body.Close()
		if err != nil {
			return nil, &apiError{502, "upstream model discovery failed"}
		}
		raw := data["data"]
		if raw == nil {
			raw = data["models"]
		}
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			return nil, &apiError{502, "upstream model list is invalid"}
		}
		for _, entry := range entries {
			m, err := decodeModel(entry)
			if err != nil {
				return nil, err
			}
			secret, _ := json.Marshal(credentialString(u.Credentials, "api_key"))
			normalized, _ := json.Marshal(m)
			if len(secret) > 2 && strings.Contains(string(normalized), string(secret[1:len(secret)-1])) {
				return nil, &apiError{502, "upstream model catalog contains credentials"}
			}
			if !seen[m.ID] {
				models = append(models, m)
				seen[m.ID] = true
			}
			if len(models) > 10000 {
				return nil, &apiError{502, "upstream model list exceeds limit"}
			}
		}
		next := credentialString(data, "nextPageToken")
		if next != "" {
			query = url.Values{"pageToken": {next}}
		} else {
			var more bool
			if raw := data["has_more"]; raw != nil && json.Unmarshal(raw, &more) != nil {
				return nil, &apiError{502, "invalid model pagination"}
			}
			if more {
				next = credentialString(data, "last_id")
				if next == "" && len(entries) > 0 {
					m, _ := decodeModel(entries[len(entries)-1])
					next = m.ID
				}
				if next == "" {
					return nil, &apiError{502, "missing model pagination cursor"}
				}
				query = url.Values{"after_id": {next}}
			}
		}
		if next == "" {
			sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
			raw, _ := json.Marshal(models)
			_ = a.Redis.Set(ctx, key, raw, time.Minute).Err()
			return models, nil
		}
		if len(next) > 4096 || cursors[next] {
			return nil, &apiError{502, "invalid model pagination cursor"}
		}
		cursors[next] = true
	}
	return nil, &apiError{502, "upstream model pagination exceeds limit"}
}

func (a *App) syncAccountModels(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models, err := a.fetchModels(ctx, u, true)
	if err != nil {
		return err
	}
	metadata, complete := map[string]modelMetadata{}, map[string]modelMetadata{}
	ids := []string{}
	var old modelSnapshot
	_ = json.Unmarshal(u.Extra["upstream_model_metadata"], &old)
	for _, m := range models {
		ids = append(ids, m.ID)
		metadata[m.ID] = m.modelMetadata
		if m.complete() {
			complete[m.ID] = m.modelMetadata
		} else if previous, ok := old.Models[m.ID]; ok && previous.complete() {
			complete[m.ID] = previous
		}
	}
	warnings := []map[string]string{}
	if len(complete) > 0 {
		snapshot, _ := json.Marshal(modelSnapshot{"upstream", time.Now().UTC().Format(time.RFC3339), complete})
		result, err := a.DB.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(extra,'{upstream_model_metadata}',$3::jsonb),updated_at=now() WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL`, u.ID, u.UpdatedAt, string(snapshot))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return conflict("account changed during model sync; retry with current configuration")
		}
	}
	if len(complete) < len(models) {
		warnings = append(warnings, map[string]string{"code": "upstream_model_metadata_incomplete", "message": "Model IDs were fetched; some capability metadata remains incomplete."})
	}
	return reply(w, map[string]any{"models": ids, "metadata": metadata, "warnings": warnings})
}

func (a *App) groupModels(ctx context.Context, g *gatewayIdentity, native bool) ([]discoveredModel, error) {
	var composite *compositeConfig
	if g.Group.Platform == "composite" {
		var err error
		composite, err = a.loadComposite(ctx, g.Key.GroupID)
		if err != nil {
			return nil, err
		}
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT a.id FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND (a.platform=$2 OR $2='composite') AND a.type='apikey' AND a.status='active' AND a.deleted_at IS NULL AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()) ORDER BY ag.priority,a.priority,a.id`, g.Key.GroupID, g.Group.Platform)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if g.Group.Manifest.Enabled {
		members := map[int64]bool{}
		for _, id := range ids {
			members[id] = true
		}
		ordered := []int64{}
		for _, id := range g.Group.Manifest.Accounts {
			if members[id] {
				ordered = append(ordered, id)
				delete(members, id)
			}
		}
		if g.Group.Manifest.Fallback {
			for _, id := range ids {
				if members[id] {
					ordered = append(ordered, id)
				}
			}
		}
		ids = ordered
	}
	var channelMapping map[string]map[string]string
	var raw []byte
	err = a.DB.QueryRowContext(ctx, `SELECT c.model_mapping FROM channels c JOIN channel_groups cg ON cg.channel_id=c.id WHERE cg.group_id=$1 AND c.status='active'`, g.Key.GroupID).Scan(&raw)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if len(raw) > 0 && json.Unmarshal(raw, &channelMapping) != nil {
		return nil, &apiError{503, "invalid channel model mapping"}
	}
	byID := map[string]discoveredModel{}
	routedByID := map[string]discoveredModel{}
	routedNames := map[string]bool{}
	available := false
	for _, id := range ids {
		if g.Group.Manifest.Enabled && available {
			pinned := false
			for _, account := range g.Group.Manifest.Accounts {
				pinned = pinned || account == id
			}
			if !pinned {
				break
			}
		}
		u, err := a.loadAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		if native && u.Platform != "gemini" {
			continue
		}
		mapping := channelMapping[u.Platform]
		live, fetchErr := a.fetchModels(ctx, u, false)
		var current bool
		if err := a.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE a.id=$1 AND a.updated_at=$2 AND ag.group_id=$3 AND a.status='active' AND a.deleted_at IS NULL AND a.schedulable AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()))`, u.ID, u.UpdatedAt, g.Key.GroupID).Scan(&current); err != nil {
			return nil, err
		}
		if !current {
			return nil, conflict("account configuration changed during discovery")
		}
		var accountMapping map[string]string
		if raw := u.Credentials["model_mapping"]; raw != nil && json.Unmarshal(raw, &accountMapping) != nil {
			return nil, &apiError{503, "invalid account model mapping"}
		}
		pinned := false
		for _, account := range g.Group.Manifest.Accounts {
			pinned = pinned || account == id
		}
		if g.Group.Manifest.Enabled && pinned && fetchErr != nil {
			continue
		}
		available = available || fetchErr == nil || len(accountMapping) > 0
		liveByID := map[string]discoveredModel{}
		candidates := map[string]bool{}
		for _, m := range live {
			liveByID[m.ID] = m
			candidates[m.ID] = true
		}
		for name := range accountMapping {
			if concreteModel(name) {
				candidates[name] = true
			}
		}
		for name := range mapping {
			if concreteModel(name) {
				candidates[name] = true
			}
		}
		if g.Group.RoutingEnabled {
			for name := range g.Group.ModelRouting {
				if concreteModel(name) {
					candidates[name] = true
				}
			}
		}
		if composite != nil {
			for _, route := range composite.Routes {
				if route.Enabled && route.MatchType == "exact" && concreteModel(route.PublicModel) {
					candidates[route.PublicModel] = true
				}
			}
		}
		// Exact allowlist entries can make a wildcard account mapping enumerable.
		if g.Group.Allowlist.Enabled && (len(accountMapping) > 0 || composite != nil) {
			for _, name := range g.Group.Allowlist.Models {
				if concreteModel(name) {
					candidates[name] = true
				}
			}
		}
		var snapshot modelSnapshot
		_ = json.Unmarshal(u.Extra["upstream_model_metadata"], &snapshot)
		for name := range candidates {
			for _, routed := range compositeModelTargets(composite, g.Key.GroupID, name, u.Platform, u.protocol(), native) {
				mapped := routed
				for pattern, target := range mapping {
					if patternMatches(pattern, routed) {
						if target != "" && target != "*" {
							mapped = target
						}
						break
					}
				}
				target, err := u.mappedModel(mapped)
				if err != nil {
					continue
				}
				m, found := liveByID[strings.TrimPrefix(target, "models/")]
				// Explicit account mappings remain usable when a relay has no list API.
				if !found && len(accountMapping) == 0 {
					continue
				}
				if !g.Group.allows(name) {
					continue
				}
				if old, ok := snapshot.Models[target]; ok && !m.complete() {
					m.modelMetadata = old
				}
				m.ID = name
				if name != target || m.DisplayName == "" {
					m.DisplayName = name
				}
				if name != target {
					m.Description = ""
					m.Version = ""
				}
				if composite != nil {
					m.Owner = u.Platform
				}
				if preferred := g.Group.routingAccounts(mapped, u.Platform); len(preferred) > 0 && !g.Group.Manifest.Enabled {
					routedNames[name] = true
					if slices.Contains(preferred, u.ID) {
						if previous, exists := routedByID[name]; exists {
							routedByID[name] = commonModelCapabilities(previous, m)
						} else {
							routedByID[name] = m
						}
					}
				}
				if previous, exists := byID[name]; exists {
					// Pinned catalogs use explicit first-account precedence. Otherwise a
					// model can route to any account, so advertise only common capabilities.
					if !g.Group.Manifest.Enabled {
						byID[name] = commonModelCapabilities(previous, m)
						if composite != nil && previous.Owner != m.Owner {
							merged := byID[name]
							merged.Owner = "composite"
							byID[name] = merged
						}
					}
				} else {
					byID[name] = m
				}
			}
		}
	}
	if composite != nil {
		current, err := a.loadComposite(ctx, g.Key.GroupID)
		if err != nil {
			return nil, err
		}
		before, _ := json.Marshal(composite)
		after, _ := json.Marshal(current)
		if string(before) != string(after) {
			return nil, conflict("composite routes changed during discovery")
		}
	}
	if !available {
		return nil, &apiError{503, "no model catalog is available; configure account model mappings or enable upstream discovery"}
	}
	models := []discoveredModel{}
	for _, m := range byID {
		if routedNames[m.ID] {
			if routed, ok := routedByID[m.ID]; ok {
				if m.Owner == "composite" {
					// Other endpoint platforms remain callable and constrain the
					// shared manifest even when one platform has a preferred pool.
					routed = commonModelCapabilities(routed, m)
				}
				m.modelMetadata = routed.modelMetadata
			} else {
				// Keep the callable fallback name, but do not invent capabilities
				// for a missing preferred account or borrow another account's limits.
				m.modelMetadata = modelMetadata{ID: m.ID, DisplayName: m.DisplayName}
			}
		}
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	if g.Group.Allowlist.Enabled {
		ordered := []discoveredModel{}
		seen := map[string]bool{}
		for _, pattern := range g.Group.Allowlist.Models {
			for _, m := range models {
				if patternMatches(pattern, m.ID) && !seen[m.ID] {
					ordered = append(ordered, m)
					seen[m.ID] = true
				}
			}
		}
		models = ordered
	}
	return models, nil
}

func commonModelCapabilities(first, other discoveredModel) discoveredModel {
	intersect := func(a, b []string) []string {
		out := []string{}
		for _, value := range a {
			for _, candidate := range b {
				if value == candidate {
					out = append(out, value)
					break
				}
			}
		}
		return out
	}
	first.InputModalities = intersect(first.InputModalities, other.InputModalities)
	first.SupportedReasoningLevels = intersect(first.SupportedReasoningLevels, other.SupportedReasoningLevels)
	first.Methods = intersect(first.Methods, other.Methods)
	first.ContextWindow = min(first.ContextWindow, other.ContextWindow)
	first.MaxContextWindow = min(first.MaxContextWindow, other.MaxContextWindow)
	first.MaxOutputTokens = min(first.MaxOutputTokens, other.MaxOutputTokens)
	if first.Reasoning == nil || other.Reasoning == nil {
		first.Reasoning = nil
	} else {
		enabled := *first.Reasoning && *other.Reasoning
		first.Reasoning = &enabled
	}
	first.DefaultReasoningLevel = ""
	if len(first.SupportedReasoningLevels) > 0 {
		first.DefaultReasoningLevel = first.SupportedReasoningLevels[0]
	}
	return first
}

func (a *App) gatewayModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	protocol := "chat_completions"
	native := strings.HasPrefix(r.URL.Path, "/v1beta/")
	if native {
		protocol = "gemini"
	}
	fail := func(err error) { textGatewayError(w, protocol, err) }
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	g, err := a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	if native && g.Group.Platform != "gemini" && g.Group.Platform != "composite" {
		fail(bad("Gemini discovery requires a Gemini group"))
		return
	}
	model := r.PathValue("model")
	if model != "" && (!concreteModel(model) || !g.Group.allows(model)) {
		fail(missing())
		return
	}
	models, err := a.groupModels(ctx, g, native)
	if err != nil {
		fail(err)
		return
	}
	before, _ := json.Marshal(g.Group)
	current, err := a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	if current.Key.GroupID != g.Key.GroupID {
		fail(conflict("API key group changed during discovery"))
		return
	}
	g = current
	after, _ := json.Marshal(g.Group)
	if string(before) != string(after) {
		fail(conflict("group configuration changed during discovery"))
		return
	}
	codex := model == "" && !native && (r.URL.Query().Get("client_version") != "" || strings.HasPrefix(r.URL.Path, "/backend-api/codex/"))
	entries := []any{}
	for _, m := range models {
		if !g.Group.allows(m.ID) {
			continue
		}
		if model != "" && m.ID != model {
			continue
		}
		if codex {
			entries = append(entries, codexModel(m))
			continue
		}
		if native {
			entry := map[string]any{"name": "models/" + m.ID, "displayName": m.DisplayName}
			if m.Description != "" {
				entry["description"] = m.Description
			}
			if m.ContextWindow > 0 {
				entry["inputTokenLimit"] = m.ContextWindow
			}
			if m.MaxOutputTokens > 0 {
				entry["outputTokenLimit"] = m.MaxOutputTokens
			}
			if len(m.Methods) > 0 {
				entry["supportedGenerationMethods"] = m.Methods
			}
			if m.Version != "" {
				entry["version"] = m.Version
			}
			entries = append(entries, entry)
		} else {
			owner := m.Owner
			if owner == "" {
				owner = g.Group.Platform
			}
			entries = append(entries, map[string]any{"id": m.ID, "object": "model", "created": m.Created, "owned_by": owner, "type": "model", "display_name": m.DisplayName})
		}
	}
	var result any
	if model != "" {
		if len(entries) == 0 {
			fail(missing())
			return
		}
		result = entries[0]
	} else if codex {
		result = map[string]any{"models": entries}
	} else if native {
		start, size := 0, 50
		if raw := r.URL.Query().Get("pageSize"); raw != "" {
			size, err = strconv.Atoi(raw)
			if err != nil || size < 1 || size > 1000 {
				fail(bad("invalid pageSize"))
				return
			}
		}
		if raw := r.URL.Query().Get("pageToken"); raw != "" {
			start, err = strconv.Atoi(raw)
			if err != nil || start < 0 || start > len(entries) {
				fail(bad("invalid pageToken"))
				return
			}
		}
		end := min(start+size, len(entries))
		body := map[string]any{"models": entries[start:end]}
		if end < len(entries) {
			body["nextPageToken"] = strconv.Itoa(end)
		}
		result = body
	} else {
		result = map[string]any{"object": "list", "data": entries}
	}
	body, err := json.Marshal(result)
	if err != nil {
		fail(err)
		return
	}
	etag := `"` + digest(string(body)) + `"`
	w.Header().Set("ETag", etag)
	for _, tag := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func codexModel(m discoveredModel) map[string]any {
	levels := []map[string]string{}
	for _, effort := range m.SupportedReasoningLevels {
		levels = append(levels, map[string]string{"effort": effort, "description": effort})
	}
	defaultLevel := m.DefaultReasoningLevel
	if len(levels) == 0 {
		levels = append(levels, map[string]string{"effort": "none", "description": "Use the upstream default"})
		defaultLevel = "none"
	} else if defaultLevel == "" {
		defaultLevel = levels[0]["effort"]
	}
	modalities := m.InputModalities
	if len(modalities) == 0 {
		modalities = []string{"text"}
	}
	// Unknown context size is null; the client can use its configured fallback.
	var contextWindow, maxContext any
	if m.ContextWindow > 0 {
		contextWindow = m.ContextWindow
		maxContext = m.ContextWindow
	}
	if m.MaxContextWindow > 0 {
		maxContext = m.MaxContextWindow
	}
	return map[string]any{"slug": m.ID, "display_name": m.DisplayName, "description": m.Description, "default_reasoning_level": defaultLevel, "supported_reasoning_levels": levels, "shell_type": "unified_exec", "visibility": "list", "supported_in_api": true, "priority": 100, "additional_speed_tiers": []string{}, "service_tiers": []any{}, "default_service_tier": nil, "availability_nux": nil, "upgrade": nil, "model_messages": map[string]any{"instructions_template": "", "instructions_variables": nil, "approvals": nil, "collaboration_modes": nil, "auto_review": nil, "permissions": nil, "multi_agent": nil, "token_budget": nil, "guardian_v2": nil}, "include_skills_usage_instructions": false, "include_plugin_usage_instructions": false, "include_apps_usage_instructions": false, "supports_reasoning_summary_parameter": false, "default_reasoning_summary": "none", "support_verbosity": false, "default_verbosity": nil, "apply_patch_tool_type": nil, "web_search_tool_type": "text", "truncation_policy": map[string]any{"mode": "bytes", "limit": 10000}, "supports_image_detail_original": false, "supports_parallel_tool_calls": false, "context_window": contextWindow, "max_context_window": maxContext, "auto_compact_token_limit": nil, "comp_hash": nil, "effective_context_window_percent": 95, "experimental_supported_tools": []string{}, "input_modalities": modalities, "supports_search_tool": false, "use_responses_lite": false, "node_repl_auto_review_required": false, "node_repl_disabled": false, "auto_review_model_override": nil, "model_specialty": nil, "tool_mode": nil, "multi_agent_version": nil}
}

func (a *App) modelRoutes() {
	for _, path := range []string{"/v1/models", "/models", "/v1beta/models"} {
		a.mux.HandleFunc("GET "+path, a.gatewayModels)
		a.mux.HandleFunc("GET "+path+"/{model}", a.gatewayModels)
	}
	a.mux.HandleFunc("GET /backend-api/codex/models", a.gatewayModels)
	a.route("POST /api/v1/admin/accounts/{id}/models/sync-upstream", "admin", a.syncAccountModels)
}
