package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

// Voice ownership is durable, like media task ownership. Only metadata and a
// credential fingerprint are stored, never the recording or upstream API key.
type customVoice struct {
	ID                     string
	UserID, KeyID, GroupID int64
	Binding                responseBinding
	UpstreamID             string
	Metadata               json.RawMessage
}

func customVoiceKey(g *gatewayIdentity) string {
	return fmt.Sprintf("gateway:custom-voices:%d:%d", g.Key.ID, g.Key.GroupID)
}
func validVoiceID(id string) bool {
	if len(id) == 0 || len(id) > 160 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func builtInVoice(id string) bool {
	switch strings.ToLower(id) {
	case "carina", "zagan", "helix", "orion", "luna", "iris", "altair", "zenith", "perseus", "helios", "lux", "kepler", "rigel", "cosmo", "celeste", "ursa", "sirius", "lumen", "castor", "naksh", "atlas", "ara", "eve", "leo", "rex", "sal":
		return true
	}
	return false
}
func voiceMetadata(raw []byte, id string) (json.RawMessage, string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || !validVoiceID(credentialString(fields, "voice_id")) {
		return nil, "", &apiError{502, "invalid upstream voice metadata"}
	}
	upstreamID := credentialString(fields, "voice_id")
	public := map[string]json.RawMessage{}
	for _, name := range []string{"name", "description", "gender", "accent", "age", "language", "use_case", "tone", "created_at"} {
		if value, ok := fields[name]; ok {
			var text string
			if string(value) != "null" && (json.Unmarshal(value, &text) != nil || len(text) > 8192) {
				return nil, "", &apiError{502, "invalid upstream voice metadata"}
			}
			public[name] = value
		}
	}
	public["voice_id"], _ = json.Marshal(id)
	out, _ := json.Marshal(public)
	return out, upstreamID, nil
}
func (a *App) loadCustomVoice(ctx context.Context, g *gatewayIdentity, id string) (*customVoice, error) {
	if !validVoiceID(id) || !strings.HasPrefix(id, "voice_") {
		return nil, missing()
	}
	raw, err := a.Redis.HGet(ctx, customVoiceKey(g), id).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, missing()
	}
	if err != nil {
		return nil, &apiError{503, "voice registry unavailable"}
	}
	var v customVoice
	if json.Unmarshal(raw, &v) != nil || v.ID != id || v.UserID != g.UserID || v.KeyID != g.Key.ID || v.GroupID != g.Key.GroupID || v.Binding.AccountID <= 0 || v.Binding.Target == "" || !validVoiceID(v.UpstreamID) {
		return nil, &apiError{503, "invalid voice registry record"}
	}
	return &v, nil
}
func (a *App) saveCustomVoice(ctx context.Context, g *gatewayIdentity, v *customVoice) error {
	raw, err := json.Marshal(v)
	if err == nil {
		err = a.Redis.HSet(ctx, customVoiceKey(g), v.ID, raw).Err()
	}
	if err != nil {
		return &apiError{503, "voice metadata persistence failed; upstream outcome requires review"}
	}
	return nil
}
func customVoiceBody(method, ct string, raw []byte) ([]byte, error) {
	kind, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, bad("invalid voice content type")
	}
	if method == http.MethodPatch {
		var fields map[string]json.RawMessage
		if kind != "application/json" || !utf8.Valid(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
			return nil, bad("voice update requires a JSON object")
		}
		for name, value := range fields {
			switch name {
			case "name", "description", "gender", "accent", "age", "language", "use_case", "tone":
			default:
				return nil, bad("unknown voice metadata field")
			}
			var text string
			if string(value) != "null" && (json.Unmarshal(value, &text) != nil || strings.TrimSpace(text) == "" || len(text) > 8192) {
				return nil, bad("voice metadata must be nonempty text or null")
			}
		}
		return json.Marshal(fields)
	}
	if kind != "multipart/form-data" || params["boundary"] == "" {
		return nil, bad("voice creation requires multipart/form-data")
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	files := 0
	seen := map[string]bool{}
	for {
		p, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad("invalid voice multipart body")
		}
		name := p.FormName()
		if seen[name] {
			return nil, bad("duplicate voice form field")
		}
		seen[name] = true
		if p.FileName() != "" {
			if name != "file" {
				return nil, bad("unexpected voice file field")
			}
			n, err := io.Copy(io.Discard, p)
			if err != nil || n == 0 {
				return nil, bad("empty voice recording")
			}
			files++
		} else {
			switch name {
			case "name", "description", "gender", "accent", "age", "language", "use_case", "tone":
			default:
				return nil, bad("unknown voice form field")
			}
			value, err := io.ReadAll(io.LimitReader(p, 8193))
			if err != nil || len(value) > 8192 || !utf8.Valid(value) {
				return nil, bad("invalid voice form field")
			}
		}
		_ = p.Close()
	}
	if files != 1 {
		return nil, bad("one reference recording is required")
	}
	return raw, nil
}

func (a *App) customVoices(w http.ResponseWriter, r *http.Request) {
	id, started := randomToken(24), time.Now()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(started.Add(30 * time.Second))
	_ = controller.SetWriteDeadline(started.Add(6 * time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("X-Request-ID", id)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var g *gatewayIdentity
	var selected *gatewaySelection
	fail := func(err error) { a.recordGatewayError(id, g, selected, r, err, started); gatewayError(w, err) }
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(context.WithValue(ctx, clientPolicyKey{}, clientPolicy{Protocol: "custom-voices"}))
	var err error
	g, err = a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	if g.Group.Platform != "grok" {
		fail(&apiError{404, "custom voices require a Grok group"})
		return
	}
	if !g.Group.allows("custom-voices") {
		fail(denied())
		return
	}
	voiceID := r.PathValue("voice_id")
	audio := strings.HasSuffix(r.Pattern, "/audio")
	list := r.Method == http.MethodGet && voiceID == ""
	query := r.URL.Query()
	// Reject malformed and unexpected queries instead of forwarding client secrets.
	if r.URL.RawQuery != "" {
		query, err = url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			fail(bad("invalid voice query"))
			return
		}
		for name, values := range query {
			if !list || len(values) != 1 || (name != "limit" && name != "pagination_token") {
				fail(bad("unsupported voice query"))
				return
			}
		}
	}
	var voice *customVoice
	if voiceID != "" && (!validVoiceID(voiceID) || !strings.HasPrefix(voiceID, "voice_")) {
		fail(missing())
		return
	}
	var body []byte
	ct := r.Header.Get("Content-Type")
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
		if err != nil {
			fail(bad("voice request exceeds limit"))
			return
		}
		body, err = customVoiceBody(r.Method, ct, body)
		if err != nil {
			fail(err)
			return
		}
	}
	var writer *gatewayResponse
	var finish func()
	if r.Method != http.MethodGet {
		var replayed bool
		writer, finish, replayed, err = a.claimGatewayRequest(w, r, g, fmt.Sprintf("custom-voices.%s.%d", r.Method, g.Key.GroupID), voiceID+"\n"+ct+"\n"+string(body), false)
		if err != nil {
			fail(err)
			return
		}
		if replayed {
			return
		}
		if writer != nil {
			w = writer
		}
	}
	// Serialize metadata refreshes with mutations so an in-flight GET cannot
	// restore a just-deleted record. Audio downloads use the same bounded slot.
	if !list {
		if !a.takeSlot("voice-management", g.Key.ID, 1) {
			if finish != nil {
				defer finish()
			}
			fail(conflict("another voice operation is in progress"))
			return
		}
		defer a.releaseSlot("voice-management", g.Key.ID)
	}
	// An unknown result after dispatch leaves the durable claim processing.
	dispatched, completed := false, false
	defer func() {
		if finish != nil && (!dispatched || completed) {
			finish()
		}
	}()
	fresh, err := a.gatewayAuth(r, true)
	if err != nil {
		fail(err)
		return
	}
	if fresh.Key.ID != g.Key.ID || fresh.Key.GroupID != g.Key.GroupID || fresh.UserID != g.UserID || fresh.Group.Platform != "grok" {
		fail(conflict("voice API key assignment changed; retry"))
		return
	}
	g = fresh
	g, err = a.acquireGatewayUser(r, g, "custom-voices", nil)
	if err != nil {
		fail(err)
		return
	}
	defer a.releaseSlot("user", g.UserID)
	if voiceID != "" {
		voice, err = a.loadCustomVoice(ctx, g, voiceID)
		if err != nil {
			fail(err)
			return
		}
	}
	if voice != nil && (voice.GroupID != g.Key.GroupID || voice.KeyID != g.Key.ID) {
		fail(denied())
		return
	}
	if list {
		if err = a.gatewayRPM(ctx, g); err != nil {
			fail(err)
			return
		}
		limit := 100
		if query.Has("limit") {
			limit, err = strconv.Atoi(query.Get("limit"))
			if err != nil || limit < 1 || limit > 1000 {
				fail(bad("voice limit must be between 1 and 1000"))
				return
			}
		}
		cursor := query.Get("pagination_token")
		if cursor != "" {
			if _, err = a.loadCustomVoice(ctx, g, cursor); err != nil {
				fail(bad("invalid voice pagination token"))
				return
			}
		}
		// ponytail: one hash scan for the small voice library; use a sorted index if
		// libraries grow beyond the provider's small per-team voice limit.
		all, err := a.Redis.HGetAll(ctx, customVoiceKey(g)).Result()
		if err != nil {
			fail(&apiError{503, "voice registry unavailable"})
			return
		}
		ids := make([]string, 0, len(all))
		for id := range all {
			if id > cursor {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		var next any
		if len(ids) > limit {
			ids = ids[:limit]
			next = ids[len(ids)-1]
		}
		voices := []json.RawMessage{}
		for _, id := range ids {
			var v customVoice
			if json.Unmarshal([]byte(all[id]), &v) != nil || v.ID != id || v.KeyID != g.Key.ID || v.GroupID != g.Key.GroupID || v.UserID != g.UserID || !json.Valid(v.Metadata) {
				fail(&apiError{503, "invalid voice registry record"})
				return
			}
			voices = append(voices, v.Metadata)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"voices": voices, "pagination_token": next})
		return
	}
	in := textRequest{Protocol: "custom-voices", Model: "custom-voices"}
	var binding *responseBinding
	if voice != nil {
		binding = &voice.Binding
	} else {
		binding, err = a.voiceLibraryBinding(ctx, g)
		if err != nil {
			fail(err)
			return
		}
	}
	snapshot := g.Group
	_, err = a.waitAdmission(ctx, "account", g.Key.GroupID, gatewayQueueTimeout, func(waitCtx context.Context) (bool, error) {
		if err := a.revalidateQueuedRequest(r.WithContext(waitCtx), g, snapshot, in, in.Model); err != nil {
			return false, err
		}
		var err error
		selected, err = a.chooseAccount(waitCtx, g, in.Model, in, nil, binding, nil, a.prices.Load())
		if selected != nil && waitCtx.Err() != nil {
			selected.Release()
			selected = nil
			return false, waitCtx.Err()
		}
		var busy *accountBusy
		if errors.As(err, &busy) {
			return false, nil
		}
		return selected != nil, err
	}, nil)
	if err != nil {
		fail(err)
		return
	}
	defer selected.Release()
	if selected.Restrict {
		if _, ok := matchPrice(selected.Pricing, "grok", "custom-voices"); !ok {
			fail(denied())
			return
		}
	}
	if err = a.gatewayRPM(ctx, g); err != nil {
		fail(err)
		return
	}
	path := "/v1/custom-voices"
	if voice != nil {
		path += "/" + voice.UpstreamID
	}
	if audio {
		path += "/audio"
	}
	dispatched = true
	resp, err := a.upstreamRequestHeaders(ctx, selected.Account, r.Method, path, body, http.Header{"Content-Type": []string{ct}, "Accept": []string{"application/json, audio/*"}})
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !(r.Method == http.MethodDelete && resp.StatusCode == 404) {
			a.recordUpstreamFailure(id, g, selected, r, in, path, resp.StatusCode, started)
			// 403 can mean this account lacks voice creation permission, not a bad Key.
			if resp.StatusCode != 403 {
				a.markGatewayFailure(ctx, selected, resp.StatusCode, resp.Header.Get("Retry-After"), nil)
			}
			completed = resp.StatusCode >= 400 && resp.StatusCode < 500
			status := resp.StatusCode
			if status < 400 || status > 599 {
				status = 502
			}
			fail(&apiError{status, "upstream voice request rejected"})
			return
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil || len(data) > 32<<20 {
		fail(&apiError{502, "invalid or oversized voice response"})
		return
	}
	persistCtx, stopPersist := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopPersist()
	contentType := "application/json"
	if audio {
		contentType, err = audioResponseType("tts", resp.Header.Get("Content-Type"))
		if err != nil || len(data) == 0 {
			fail(&apiError{502, "invalid reference audio"})
			return
		}
	} else if r.Method == http.MethodDelete {
		if resp.StatusCode != 404 && resp.StatusCode != 204 {
			var deleted struct{ Deleted bool }
			if json.Unmarshal(data, &deleted) != nil || !deleted.Deleted {
				fail(&apiError{502, "invalid voice deletion response"})
				return
			}
		}
		if err = a.Redis.HDel(persistCtx, customVoiceKey(g), voice.ID).Err(); err != nil {
			fail(&apiError{503, "voice deletion requires reconciliation"})
			return
		}
		data = []byte(`{"deleted":true}`)
	} else {
		if voice == nil {
			voice = &customVoice{ID: "voice_" + randomToken(18), UserID: g.UserID, KeyID: g.Key.ID, GroupID: g.Key.GroupID, Binding: responseBinding{AccountID: selected.Account.ID, Target: responseTarget(selected.Account)}}
		}
		var upstreamID string
		voice.Metadata, upstreamID, err = voiceMetadata(data, voice.ID)
		if err != nil {
			fail(err)
			return
		}
		if voice.UpstreamID != "" && voice.UpstreamID != upstreamID {
			fail(&apiError{502, "upstream voice identity changed"})
			return
		}
		voice.UpstreamID = upstreamID
		if err = a.saveCustomVoice(persistCtx, g, voice); err != nil {
			fail(err)
			return
		}
		data = voice.Metadata
	}
	completed = true
	if writer != nil {
		writer.succeeded = true
	}
	w.Header().Set("Content-Type", contentType)
	status := 200
	if r.Method == http.MethodPost {
		status = 201
	}
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// A local voice ID is the only way to access a custom voice on a shared account.
// Built-in voices require no binding; unknown IDs never reach the upstream.
func (a *App) resolveVoice(ctx context.Context, g *gatewayIdentity, id string) (string, *responseBinding, error) {
	if id == "" || builtInVoice(id) {
		return id, nil, nil
	}
	v, err := a.loadCustomVoice(ctx, g, id)
	if err != nil {
		return "", nil, err
	}
	return v.UpstreamID, &v.Binding, nil
}

func (a *App) customVoiceRoutes() {
	for _, prefix := range []string{"/v1/custom-voices", "/custom-voices"} {
		for _, method := range []string{"GET", "POST"} {
			a.mux.HandleFunc(method+" "+prefix, a.customVoices)
		}
		for _, method := range []string{"GET", "PATCH", "DELETE"} {
			a.mux.HandleFunc(method+" "+prefix+"/{voice_id}", a.customVoices)
		}
		a.mux.HandleFunc("GET "+prefix+"/{voice_id}/audio", a.customVoices)
	}
}

// Keep a Key's custom voices on one upstream so a Realtime session can select
// their source before receiving session.update. Fail closed on source rotation.
func (a *App) voiceLibraryBinding(ctx context.Context, g *gatewayIdentity) (*responseBinding, error) {
	all, err := a.Redis.HGetAll(ctx, customVoiceKey(g)).Result()
	if err != nil {
		return nil, &apiError{503, "voice registry unavailable"}
	}
	var binding *responseBinding
	for id, raw := range all {
		var voice customVoice
		if json.Unmarshal([]byte(raw), &voice) != nil || voice.ID != id || voice.UserID != g.UserID || voice.KeyID != g.Key.ID || voice.GroupID != g.Key.GroupID || voice.Binding.AccountID <= 0 || voice.Binding.Target == "" {
			return nil, &apiError{503, "invalid voice registry record"}
		}
		if binding != nil && (binding.AccountID != voice.Binding.AccountID || binding.Target != voice.Binding.Target) {
			return nil, conflict("voice library has inconsistent upstream bindings")
		}
		binding = &voice.Binding
	}
	return binding, nil
}

// Validate every voice-bearing object in a Realtime event, including the
// compatible audio.output form, before forwarding any frame to a shared account.
func (a *App) realtimeVoices(ctx context.Context, g *gatewayIdentity, u *upstreamAccount, raw []byte, client bool) ([]byte, error) {
	var walk func(map[string]json.RawMessage, int) error
	walk = func(fields map[string]json.RawMessage, depth int) error {
		if depth > 16 {
			return bad("realtime event nesting exceeds limit")
		}
		for name, value := range fields {
			lower := strings.ToLower(name)
			if lower == "voice" || lower == "voice_id" {
				if name != lower {
					return bad("invalid realtime voice field casing")
				}
				var id string
				if json.Unmarshal(value, &id) != nil || !validVoiceID(id) {
					return bad("invalid realtime voice ID")
				}
				if client {
					native, binding, err := a.resolveVoice(ctx, g, id)
					if err != nil {
						return err
					}
					if binding != nil && (binding.AccountID != u.ID || binding.Target != responseTarget(u)) {
						return conflict("voice belongs to another upstream; reconnect")
					}
					fields[name], _ = json.Marshal(native)
				} else if !builtInVoice(id) {
					all, err := a.Redis.HGetAll(ctx, customVoiceKey(g)).Result()
					if err != nil {
						return &apiError{503, "voice registry unavailable"}
					}
					found := ""
					for _, v := range all {
						var voice customVoice
						if json.Unmarshal([]byte(v), &voice) == nil && voice.KeyID == g.Key.ID && voice.GroupID == g.Key.GroupID && voice.UserID == g.UserID && voice.UpstreamID == id && voice.Binding.AccountID == u.ID && voice.Binding.Target == responseTarget(u) {
							found = voice.ID
							break
						}
					}
					if found == "" {
						return &apiError{502, "upstream returned an unknown custom voice"}
					}
					fields[name], _ = json.Marshal(found)
				}
			} else if lower == "session" || lower == "response" || lower == "audio" || lower == "output" {
				if name != lower {
					return bad("invalid realtime voice container casing")
				}
				if len(value) == 0 || value[0] != '{' {
					continue
				}
				var nested map[string]json.RawMessage
				if json.Unmarshal(value, &nested) != nil {
					return bad("invalid realtime voice container")
				}
				if err := walk(nested, depth+1); err != nil {
					return err
				}
				fields[name], _ = json.Marshal(nested)
			}
		}
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, bad("invalid realtime event")
	}
	if err := walk(fields, 0); err != nil {
		return nil, err
	}
	return json.Marshal(fields)
}
