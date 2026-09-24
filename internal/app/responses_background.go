package app

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const backgroundPending = "gateway:background:pending"

// Creation snapshots contain no request body or credentials. Provider results are
// encrypted with the same deployment key as other resumable gateway tasks.
type backgroundResponse struct {
	videoTask
	Store, Stream   bool
	Effort, Tier    string
	RequestedEffort *string
	Items           []string
	MCPTool         bool     `json:",omitempty"`
	CodeTool        bool     `json:",omitempty"`
	Containers      []string `json:",omitempty"`
}

func backgroundKey(id string) string { return "gateway:background:task:" + id }
func backgroundIndex(g *gatewayIdentity, id string) string {
	return "gateway:background:owner:" + strconv.FormatInt(g.Key.ID, 10) + ":" + strconv.FormatInt(g.Key.GroupID, 10) + ":" + digest(id)
}

func (a *App) saveBackgroundResponse(ctx context.Context, t *backgroundResponse) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	cipher := a.chatHistoryCipher()
	nonce := randomBytes(cipher.NonceSize())
	encrypted := base64.RawStdEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, []byte(backgroundKey(t.ID))))
	ttl := 0
	if t.Stage == "terminal" {
		ttl = 600
		if t.Store {
			ttl = 30 * 24 * 60 * 60
		}
	}
	index := backgroundIndex(&t.Identity, t.UpstreamID)
	// A provider ID collision or delayed write must not replace an owned or deleted task.
	ok, err := a.Redis.Eval(ctx, `
if ARGV[4]~='' then
 if redis.call('EXISTS',KEYS[4])~=0 then return 0 end
 local old=redis.call('GET',KEYS[3]);if old and old~=ARGV[1] then return 0 end
end
redis.call('SET',KEYS[1],ARGV[2])
if ARGV[4]~='' then redis.call('SET',KEYS[3],ARGV[1]) end
if tonumber(ARGV[3])>0 then
 redis.call('EXPIRE',KEYS[1],ARGV[3]);redis.call('SREM',KEYS[2],ARGV[1])
 if ARGV[4]~='' then redis.call('EXPIRE',KEYS[3],ARGV[3]) end
else redis.call('SADD',KEYS[2],ARGV[1]) end
return 1`, []string{backgroundKey(t.ID), backgroundPending, index, responseDeletionKey(&t.Identity, t.UpstreamID)}, t.ID, encrypted, ttl, t.UpstreamID).Int()
	if err != nil || ok != 1 {
		return &apiError{503, "background response could not be saved"}
	}
	return nil
}

func (a *App) loadBackgroundResponse(ctx context.Context, id string) (*backgroundResponse, error) {
	encoded, err := a.Redis.Get(ctx, backgroundKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, missing()
	}
	if err != nil {
		return nil, &apiError{503, "background response storage unavailable"}
	}
	cipher := a.chatHistoryCipher()
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < cipher.NonceSize() {
		return nil, errors.New("invalid background response envelope")
	}
	raw, err = cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], []byte(backgroundKey(id)))
	if err != nil {
		return nil, errors.New("background response could not be decrypted")
	}
	var t backgroundResponse
	if json.Unmarshal(raw, &t) != nil || t.ID != id || t.Identity.UserID <= 0 || t.Identity.Key.ID <= 0 || t.Selection.Account == nil || t.Target == "" {
		return nil, errors.New("invalid background response record")
	}
	if t.Selection.Catalog != nil {
		raw, err = json.Marshal(t.Selection.Catalog)
		if err != nil {
			return nil, err
		}
		t.Selection.Catalog, err = parsePriceCatalog(raw)
		if err != nil {
			return nil, err
		}
	}
	return &t, nil
}

func (a *App) submitBackgroundResponse(w http.ResponseWriter, r *http.Request, g *gatewayIdentity, s *gatewaySelection, in textRequest, body []byte, id, payload, effort string, requestedEffort *string, started time.Time) (bool, error) {
	if !a.takeSlot("background-create", 0, 1) {
		return true, &apiError{429, "another background submission is in progress"}
	}
	creating := true
	defer func() {
		if creating {
			a.releaseSlot("background-create", 0)
		}
	}()
	count, err := a.Redis.SCard(r.Context(), backgroundPending).Result()
	if err != nil {
		return true, err
	}
	if count >= 32 {
		return true, &apiError{429, "background response capacity reached"}
	}
	t := &backgroundResponse{videoTask: videoTask{ID: id, Target: responseTarget(s.Account), Stage: "submitting", Identity: *g, Selection: *s, Requested: in.Model, Payload: payload, IP: clientIP(r), UserAgent: truncate(r.UserAgent(), 512), Inbound: r.URL.Path, Created: started}, Store: in.Store, Stream: in.Stream, Effort: effort, Tier: in.Tier, RequestedEffort: requestedEffort}
	t.MCPTool = in.NativeMCP
	t.CodeTool = in.NativeCode
	t.Containers = in.ResponseContainers
	account := *s.Account
	account.Credentials = nil
	account.Extra = map[string]json.RawMessage{}
	for _, key := range []string{"quota_limit", "quota_daily_limit", "quota_weekly_limit"} {
		if value := s.Account.Extra[key]; value != nil {
			account.Extra[key] = value
		}
	}
	t.Selection.Account = &account
	t.Selection.Release = nil
	// The lock covers acceptance and initial stream handling. A restart leaves a
	// submitting record for review; it never resends a potentially paid request.
	if !a.takeSlot("background:"+id, 0, 1) {
		return true, conflict("background submission is busy")
	}
	defer a.releaseSlot("background:"+id, 0)
	if err = a.saveBackgroundResponse(r.Context(), t); err != nil {
		return true, err
	}
	a.releaseSlot("background-create", 0)
	creating = false
	upstreamCtx, stop := context.WithCancel(r.Context())
	defer stop()
	resp, err := a.upstreamRequestHeaders(upstreamCtx, s.Account, "POST", "/v1/responses", body, in.Headers)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw := readUpstreamError(resp)
		a.markGatewayFailure(r.Context(), s, resp.StatusCode, resp.Header.Get("Retry-After"), raw)
		definite := resp.StatusCode >= 400 && resp.StatusCode < 500
		if definite {
			t.Stage = "terminal"
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			defer cancel()
			if err = a.saveBackgroundResponse(ctx, t); err != nil {
				return false, err
			}
		}
		return definite, a.upstreamError(r.Context(), s.Account, resp.StatusCode, raw, &apiError{502, "upstream background submission rejected"})
	}
	if in.Stream {
		resp.Body = &idleStreamBody{ReadCloser: resp.Body, ctx: upstreamCtx, cancel: stop, idle: a.streamIdle}
		err = a.streamBackgroundResponse(w, r, t, resp)
		if errors.Is(err, errStreamIdle) {
			a.markStreamTimeout(r.Context(), s.Account, in.Model)
		}
		return t.UpstreamID != "", err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		return false, &apiError{502, "background submission outcome requires review"}
	}
	if err = a.observeBackgroundResponse(r.Context(), t, raw); err != nil {
		return t.UpstreamID != "", err
	}
	return true, rawReply(w, t.Result)
}

// Persist identity before interpreting final usage: missing/invalid terminal
// usage remains pollable rather than turning an accepted task into a new create.
func (a *App) observeBackgroundResponse(ctx context.Context, t *backgroundResponse, raw []byte) error {
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil || credentialString(result, "object") != "response" {
		return &apiError{502, "invalid background response"}
	}
	id, status := credentialString(result, "id"), credentialString(result, "status")
	if !validResponseID(id) || t.UpstreamID != "" && t.UpstreamID != id {
		return &apiError{502, "background response identity changed"}
	}
	switch status {
	case "queued", "in_progress", "completed", "incomplete", "failed", "cancelled":
	default:
		return &apiError{502, "invalid background response status"}
	}
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if t.UpstreamID == "" {
		previous := *t
		t.UpstreamID = id
		t.Stage = "pending"
		t.Result, _ = json.Marshal(map[string]any{"id": id, "object": "response", "status": "queued", "background": true})
		if err := a.saveBackgroundResponse(persist, t); err != nil {
			*t = previous
			return err
		}
	}
	// Acceptance remains recoverable even if the returned content cannot be
	// sanitized. The initial checkpoint contains only identity, never tool headers.
	clean, err := sanitizeResponseMCP(raw)
	if err != nil {
		return err
	}
	if string(clean) != string(raw) {
		raw = clean
		result = nil
		_ = json.Unmarshal(raw, &result)
	}
	if status == "queued" || status == "in_progress" {
		// Partial output is available over the native stream; polling publishes the
		// final output only after settlement. No cumulative usage is billed twice.
		delete(result, "output")
		delete(result, "output_text")
		delete(result, "usage")
		delete(result, "error")
		t.Result, _ = json.Marshal(result)
		return a.saveBackgroundResponse(persist, t)
	}
	observation := textObservation{Protocol: "responses", Tier: t.Tier}
	err = observation.observe(raw)
	failed := status == "failed" || status == "cancelled"
	if err != nil && !failed {
		return err
	}
	if t.Selection.Account.Platform == "grok" || t.Selection.Account.Platform == "openai" {
		meter := hostedSearchMeter{OpenAI: t.Selection.Account.Platform == "openai"}
		if err = meter.observe(raw); err != nil {
			return err
		}
		observation.Usage.SearchCalls = meter.count()
	}
	if t.Selection.ResponseImage != nil {
		meter := responseImageMeter{}
		if err = meter.observe(raw); err != nil {
			return err
		}
		meter.apply(&observation.Usage, t.Selection.ResponseImage)
	}
	if !observation.HasUsage {
		// Queued work may start between polls or race cancellation. Only explicit
		// provider usage proves consumption, including an explicit zero result.
		return &apiError{502, "background usage is missing or invalid; reconciliation remains pending"}
	}
	if failed {
		result["error"], _ = json.Marshal(map[string]string{"code": "response_failed", "message": "upstream background response " + status})
	}
	t.Result, _ = json.Marshal(result)
	t.Items = observation.ResponseItems
	t.Containers, err = mergeResponseContainers(t.Containers, observation.ResponseContainers)
	if err != nil {
		return err
	}
	t.CodeTool = t.CodeTool || len(t.Containers) > 0
	if !failed || observation.Usage != (priceUsage{}) {
		at := time.Now().UTC()
		t.Receipt, err = a.makeReceipt(t.ID, &t.Identity, &t.Selection, t.Requested, observation.Model, observation.Tier, t.Effort, observation.Usage, t.Stream, at.Sub(t.Created), 0, t.Created, t.Payload, t.IP, t.UserAgent, t.Inbound, t.UpstreamID)
		if err != nil {
			return err
		}
		t.Receipt.RequestedEffort = t.RequestedEffort
		t.Receipt.Upstream = "/v1/responses"
		t.Receipt.At = at
	}
	t.Stage = "settling"
	if err = a.saveBackgroundResponse(persist, t); err != nil {
		return err
	}
	return a.settleBackgroundResponse(persist, t)
}

func (a *App) settleBackgroundResponse(ctx context.Context, t *backgroundResponse) error {
	if receipt := t.Receipt; receipt != nil {
		if receipt.RequestID != t.ID || receipt.UserID != t.Identity.UserID || receipt.KeyID != t.Identity.Key.ID || receipt.GroupID != t.Identity.Key.GroupID || receipt.Fingerprint != receipt.fingerprint() {
			return errors.New("invalid background settlement checkpoint")
		}
		if err := a.saveReceipt(ctx, receipt); err != nil {
			return &apiError{503, "background settlement pending recovery"}
		}
	}
	var result struct{ Status string }
	_ = json.Unmarshal(t.Result, &result)
	if t.Store && (result.Status == "completed" || result.Status == "incomplete") {
		binding := responseBinding{AccountID: t.Selection.Account.ID, Target: t.Target, Items: t.Items, ImageTool: t.Selection.ResponseImage != nil, MCPTool: t.MCPTool, CodeTool: t.CodeTool, Containers: t.Containers}
		if err := a.storeResponseBinding(ctx, &t.Identity, t.UpstreamID, binding); err != nil {
			return err
		}
	}
	t.Stage = "terminal"
	return a.saveBackgroundResponse(ctx, t)
}

func (a *App) backgroundSource(ctx context.Context, t *backgroundResponse) (*upstreamAccount, error) {
	u, err := a.loadAccount(ctx, t.Selection.Account.ID)
	if err != nil {
		return nil, err
	}
	if u.Type != "apikey" || u.protocol() != "responses" || responseTarget(u) != t.Target {
		return nil, conflict("background upstream source changed; restore the original account to reconcile")
	}
	return u, nil
}
func (a *App) refreshBackgroundResponse(ctx context.Context, t *backgroundResponse) error {
	if t.MCPTool {
		ctx = context.WithValue(ctx, mcpRequestKey{}, true)
	}
	if t.Stage == "terminal" {
		return nil
	}
	if t.Stage == "settling" {
		return a.settleBackgroundResponse(ctx, t)
	}
	if t.Stage == "submitting" {
		return conflict("background submission requires review; it will not be sent twice")
	}
	u, err := a.backgroundSource(ctx, t)
	if err != nil {
		return err
	}
	release, err := a.acquireAccountSlot(ctx, u.ID)
	if err != nil {
		return err
	}
	defer release()
	resp, err := a.upstreamRequest(ctx, u, "GET", "/v1/responses/"+t.UpstreamID, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return a.upstreamError(ctx, u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "background query rejected; reconciliation remains pending"})
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		return &apiError{502, "background response interrupted or oversized"}
	}
	return a.observeBackgroundResponse(ctx, t, raw)
}

func (a *App) backgroundResponseLookup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fail := func(err error) { textGatewayError(w, "responses", err) }
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	if err := a.checkInstance(ctx); err != nil {
		fail(err)
		return
	}
	g, err := a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	id := r.PathValue("response_id")
	if !validResponseID(id) {
		fail(missing())
		return
	}
	query, err := responseResourceQuery(r)
	if err != nil {
		fail(err)
		return
	}
	if r.Method == "DELETE" {
		if err = a.deleteResponseResource(w, r, g, id); err != nil {
			fail(err)
		}
		return
	}
	if err = a.responseNotDeleted(ctx, g, id); err != nil {
		fail(err)
		return
	}
	stream := query.Get("stream") == "true"
	taskID, err := a.Redis.Get(ctx, backgroundIndex(g, id)).Result()
	if errors.Is(err, redis.Nil) {
		if r.Method != "GET" {
			fail(missing())
		} else if err = a.storedResponseLookup(w, r, g, id, query); err != nil {
			fail(err)
		}
		return
	}
	if err != nil {
		fail(&apiError{503, "background storage unavailable"})
		return
	}
	if !a.takeSlot("background:"+taskID, 0, 1) {
		fail(conflict("background reconciliation is in progress"))
		return
	}
	defer a.releaseSlot("background:"+taskID, 0)
	t, err := a.loadBackgroundResponse(ctx, taskID)
	if err != nil {
		fail(err)
		return
	}
	if t.UpstreamID != id || t.Identity.UserID != g.UserID || t.Identity.Key.ID != g.Key.ID || t.Identity.Key.GroupID != g.Key.GroupID {
		fail(missing())
		return
	}
	if t.MCPTool {
		ctx = context.WithValue(ctx, mcpRequestKey{}, true)
		r = r.WithContext(ctx)
	}
	if stream && !t.Stream {
		fail(bad("background streaming requires stream=true at creation"))
		return
	}
	if err = a.gatewayRPM(ctx, g); err != nil {
		fail(err)
		return
	}
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		fail(&apiError{429, "user concurrency limit reached"})
		return
	}
	defer a.releaseSlot("user", g.UserID)
	if !stream && r.Method == "GET" || t.Stage == "settling" {
		if err = a.refreshBackgroundResponse(ctx, t); err != nil {
			fail(err)
			return
		}
	}
	if !stream && r.Method == "GET" || r.Method == "POST" && t.Stage == "terminal" {
		if strings.HasSuffix(r.URL.Path, "/input_items") || query.Has("include") || query.Has("include[]") {
			if t.Stage != "terminal" {
				fail(conflict("background response is not settled yet"))
				return
			}
			u, err := a.backgroundSource(ctx, t)
			if err == nil {
				err = a.readResponseResource(w, r, u, id, query)
			}
			if err != nil {
				fail(err)
			}
			return
		}
		_ = rawReply(w, t.Result)
		return
	}
	u, err := a.backgroundSource(ctx, t)
	if err != nil {
		fail(err)
		return
	}
	release, err := a.acquireAccountSlot(ctx, u.ID)
	if err != nil {
		fail(err)
		return
	}
	defer release()
	path := "/v1/responses/" + id
	if r.Method == "POST" {
		path += "/cancel"
	} else {
		path += "?" + query.Encode()
	}
	resp, err := a.upstreamRequest(ctx, u, r.Method, path, nil)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(a.upstreamError(ctx, u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "background operation rejected"}))
		return
	}
	if stream {
		resp.Body = &idleStreamBody{ReadCloser: resp.Body, ctx: ctx, cancel: cancel, idle: a.streamIdle}
		if err = a.streamBackgroundResponse(w, r, t, resp); err != nil {
			if errors.Is(err, errStreamIdle) {
				a.markStreamTimeout(context.WithoutCancel(ctx), u, t.Requested)
			}
			if w.Header().Get("Content-Type") != "text/event-stream" {
				fail(err)
			}
			slog.Error("background stream failed", "task_id", t.ID)
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		fail(&apiError{502, "invalid background cancellation result"})
		return
	}
	if err = a.observeBackgroundResponse(ctx, t, raw); err != nil {
		fail(err)
		return
	}
	_ = rawReply(w, t.Result)
}

func (a *App) streamBackgroundResponse(w http.ResponseWriter, r *http.Request, t *backgroundResponse, resp *http.Response) error {
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return &apiError{502, "upstream background stream is not SSE"}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	scanner := bufio.NewScanner(resp.Body)
	frameLimit := 2 << 20
	if t.Selection.ResponseImage != nil {
		frameLimit = 16 << 20
	}
	scanner.Buffer(make([]byte, 4096), frameLimit)
	var frame []string
	size := 0
	done := false
	emit := func() error {
		var lines []string
		for _, line := range frame {
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(lines) == 0 {
			return nil
		}
		raw := []byte(strings.Join(lines, "\n"))
		var event map[string]json.RawMessage
		if json.Unmarshal(raw, &event) != nil || event == nil {
			return &apiError{502, "invalid background event"}
		}
		kind := credentialString(event, "type")
		if kind == "" || len(kind) > 128 || strings.ContainsAny(kind, "\r\n") {
			return &apiError{502, "invalid background event type"}
		}
		terminal := kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" || kind == "response.cancelled"
		if kind == "error" {
			return &apiError{502, "upstream background stream failed; task remains recoverable"}
		}
		if terminal && (event["response"] == nil || string(event["response"]) == "null") {
			return &apiError{502, "background terminal event has no response"}
		}
		if response := event["response"]; response != nil {
			var envelope struct{ ID, Status string }
			if json.Unmarshal(response, &envelope) != nil {
				return &apiError{502, "invalid background event response"}
			}
			switch kind {
			case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
				if kind != "response."+envelope.Status {
					return &apiError{502, "background event status mismatch"}
				}
			}
			if t.Stage != "terminal" {
				if err := a.observeBackgroundResponse(r.Context(), t, response); err != nil {
					return err
				}
			} else {
				var stored struct{ ID, Status string }
				if json.Unmarshal(t.Result, &stored) != nil || envelope.ID != t.UpstreamID || terminal && envelope.Status != stored.Status {
					return &apiError{502, "background stream identity changed"}
				}
			}
			switch kind {
			case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
				done = true
				event["response"] = t.Result
			}
		}
		if t.UpstreamID == "" {
			return &apiError{502, "background event missing response identity"}
		}
		raw, _ = json.Marshal(event)
		raw, err := sanitizeResponseMCP(raw)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(append([]byte("event: "+kind+"\ndata: "), raw...), '\n', '\n')); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	var err error
	for scanner.Scan() {
		line := scanner.Text()
		size += len(line)
		if size > frameLimit {
			err = &apiError{502, "background event exceeds limit"}
			break
		}
		if line == "" {
			err = emit()
			frame = nil
			size = 0
			if err != nil || done {
				break
			}
		} else {
			frame = append(frame, line)
		}
	}
	if err == nil && !done && len(frame) > 0 {
		err = emit()
	}
	if err == nil {
		err = scanner.Err()
	}
	if err == nil && !done {
		err = &apiError{502, "background stream interrupted; poll the response"}
	}
	if err != nil {
		raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "upstream_error", "message": "background stream interrupted; poll the response for recovery"}})
		_, _ = w.Write(append(append([]byte("event: error\ndata: "), raw...), '\n', '\n'))
		_ = http.NewResponseController(w).Flush()
	}
	return err
}

func (a *App) runBackgroundResponses(ctx context.Context) error {
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	ids, err := a.Redis.SMembers(ctx, backgroundPending).Result()
	if err != nil {
		return err
	}
	// ponytail: 32 tasks with sequential ten-second polls; add bounded parallel
	// polls if measured latency makes the provider retrieval window insufficient.
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !a.takeSlot("background:"+id, 0, 1) {
			continue
		}
		poll, cancel := context.WithTimeout(ctx, 10*time.Second)
		t, e := a.loadBackgroundResponse(poll, id)
		if e == nil && t.Stage != "submitting" {
			e = a.refreshBackgroundResponse(poll, t)
		}
		cancel()
		a.releaseSlot("background:"+id, 0)
		if e != nil && ctx.Err() == nil {
			slog.Error("background response reconciliation pending", "task_id", id)
		}
	}
	return nil
}
func (a *App) startBackgroundResponses(ctx context.Context) {
	a.responseWorkerDone = make(chan struct{})
	go func() {
		defer close(a.responseWorkerDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			if err := a.runBackgroundResponses(ctx); err != nil && ctx.Err() == nil {
				slog.Error("background response recovery failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
