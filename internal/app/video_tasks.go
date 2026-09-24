package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const videoPending = "gateway:video:pending"
const videoTaskTTL = 7 * 24 * time.Hour

// These are the existing credential capability meanings. An empty collection
// leaves ordinary protocols unrestricted; Seedance always requires explicit opt-in.
func openAICapabilities(raw json.RawMessage) (map[string]bool, bool, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	result := map[string]bool{}
	if len(raw) == 0 || string(raw) == "null" {
		return result, false, nil
	}
	if raw[0] == '[' {
		var names []string
		if json.Unmarshal(raw, &names) != nil {
			return nil, false, bad("invalid openai_capabilities")
		}
		for _, n := range names {
			result[n] = true
		}
	} else if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, false, bad("invalid openai_capabilities")
	}
	for name := range result {
		switch name {
		case "chat_completions", "embeddings", "alpha_search", "seedance":
		default:
			return nil, false, bad("unsupported OpenAI capability")
		}
	}
	return result, len(result) > 0, nil
}
func (u *upstreamAccount) supportsSeedance() bool {
	caps, _, err := openAICapabilities(u.Credentials["openai_capabilities"])
	return err == nil && caps["seedance"] && u.Platform == "openai" && u.Type == "apikey" && credentialString(u.Credentials, "base_url") != ""
}
func (u *upstreamAccount) allowsOpenAIProtocol(protocol string) bool {
	if u.Platform != "openai" {
		return true
	}
	caps, set, err := openAICapabilities(u.Credentials["openai_capabilities"])
	if err != nil {
		return false
	}
	if !set {
		return protocol != "seedance"
	}
	switch protocol {
	case "chat_completions", "responses", "anthropic":
		return caps["chat_completions"]
	case "embeddings":
		return caps["embeddings"]
	case "alpha_search":
		return caps["alpha_search"] || caps["chat_completions"]
	case "seedance":
		return caps["seedance"]
	default:
		return true
	}
}

func seedancePath(u *upstreamAccount, id string) string {
	base := strings.TrimRight(credentialString(u.Credentials, "base_url"), "/")
	prefix := "/api/v3"
	if strings.HasSuffix(base, "/v3") {
		prefix = ""
	}
	path := prefix + "/contents/generations/tasks"
	if id != "" {
		path += "/" + id
	}
	return path
}

func (a *App) seedanceRequest(ctx context.Context, u *upstreamAccount, method, id string, body []byte) (*http.Response, error) {
	// The native task endpoint always uses Bearer authentication, even when this
	// account's text protocol is configured for Messages compatibility.
	account := *u
	account.Credentials = map[string]json.RawMessage{
		"base_url": u.Credentials["base_url"], "api_key": u.Credentials["api_key"],
		"api_protocol": json.RawMessage(`"chat_completions"`),
	}
	return a.upstreamRequest(ctx, &account, method, seedancePath(u, id), body)
}

func parseSeedance(raw []byte) (map[string]json.RawMessage, string, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, "", bad("Seedance requires a JSON object")
	}
	model := credentialString(body, "model")
	if !validNativeModel(model) || len(model) > 100 {
		return nil, "", bad("invalid Seedance model")
	}
	var content []map[string]json.RawMessage
	if json.Unmarshal(body["content"], &content) != nil || len(content) == 0 || len(content) > 64 {
		return nil, "", bad("Seedance content must contain 1 to 64 items")
	}
	for name := range body {
		if strings.ToLower(name) != name {
			return nil, "", bad("Seedance field names must use lowercase")
		}
	}
	for _, item := range content {
		kind := credentialString(item, "type")
		switch kind {
		case "text":
			if strings.TrimSpace(credentialString(item, "text")) == "" {
				return nil, "", bad("Seedance text is required")
			}
		case "image_url", "video_url", "audio_url":
			var media map[string]json.RawMessage
			if json.Unmarshal(item[kind], &media) != nil {
				return nil, "", bad("invalid Seedance media")
			}
			uri := credentialString(media, "url")
			if !validAudioURL(uri) && !strings.HasPrefix(uri, "data:") {
				return nil, "", bad("invalid Seedance media URL")
			}
		case "draft_task":
			var draft struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(item[kind], &draft) != nil || !validVoiceID(draft.ID) || !strings.HasPrefix(draft.ID, "task_") {
				return nil, "", bad("draft_task requires a gateway task ID")
			}
		default:
			return nil, "", bad("unsupported Seedance content type")
		}
	}
	// A callback can publish the provider result before gateway settlement. Result
	// delivery stays on the authenticated task endpoint.
	for _, name := range []string{"callback_url", "tools"} {
		if value := body[name]; value != nil && string(value) != "null" && string(value) != "[]" && string(value) != `""` {
			return nil, "", bad("unsupported Seedance " + name)
		}
	}
	return body, model, nil
}

// No request content or credentials are retained. The encrypted record protects
// signed result URLs, original identity and the immutable creation-time prices.
type videoTask struct {
	Protocol, Operation, Resolution            string
	Seconds                                    int64
	ID, UpstreamID, Target, Stage              string
	Identity                                   gatewayIdentity
	Selection                                  gatewaySelection
	Requested, Payload, IP, UserAgent, Inbound string
	Created                                    time.Time
	Result                                     json.RawMessage
	Receipt                                    *usageReceipt
}

func videoTaskKey(id string) string { return "gateway:video:task:" + id }
func (a *App) saveVideoTask(ctx context.Context, task *videoTask) error {
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	cipher := a.chatHistoryCipher()
	nonce := randomBytes(cipher.NonceSize())
	encrypted := base64.RawStdEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, []byte(videoTaskKey(task.ID))))
	pipe := a.Redis.TxPipeline()
	ttl := time.Duration(0)
	if task.Stage == "terminal" || task.Stage == "deleted" {
		ttl = videoTaskTTL
	}
	pipe.Set(ctx, videoTaskKey(task.ID), encrypted, ttl)
	if ttl == 0 {
		pipe.SAdd(ctx, videoPending, task.ID)
	} else {
		pipe.SRem(ctx, videoPending, task.ID)
	}
	_, err = pipe.Exec(ctx)
	return err
}
func (a *App) loadVideoTask(ctx context.Context, id string) (*videoTask, error) {
	if !validVoiceID(id) || !strings.HasPrefix(id, "task_") {
		return nil, missing()
	}
	encoded, err := a.Redis.Get(ctx, videoTaskKey(id)).Result()
	if errors.Is(err, redis.Nil) {
		return nil, missing()
	}
	if err != nil {
		return nil, err
	}
	cipher := a.chatHistoryCipher()
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < cipher.NonceSize() {
		return nil, errors.New("invalid video task envelope")
	}
	raw, err = cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], []byte(videoTaskKey(id)))
	if err != nil {
		return nil, errors.New("video task could not be decrypted")
	}
	var task videoTask
	if json.Unmarshal(raw, &task) != nil || task.ID != id || task.Identity.UserID <= 0 || task.Identity.Key.ID <= 0 || task.Selection.Account == nil || !validVoiceID(task.UpstreamID) || task.Target == "" {
		return nil, errors.New("invalid video task record")
	}
	if task.Protocol != "seedance" && task.Protocol != "videos" {
		return nil, errors.New("invalid video task protocol")
	}
	if task.Selection.Catalog != nil {
		raw, err = json.Marshal(task.Selection.Catalog)
		if err != nil {
			return nil, err
		}
		task.Selection.Catalog, err = parsePriceCatalog(raw)
		if err != nil {
			return nil, err
		}
	}
	return &task, nil
}

func (a *App) videoSource(ctx context.Context, t *videoTask) (*upstreamAccount, error) {
	u, err := a.loadAccount(ctx, t.Selection.Account.ID)
	if err != nil {
		return nil, err
	}
	if responseTarget(u) != t.Target || t.Protocol == "seedance" && !u.supportsSeedance() || t.Protocol == "videos" && (u.Platform != "grok" || u.Type != "apikey") {
		return nil, conflict("video task upstream source changed; restore the original account to reconcile")
	}
	// Existing paid work remains readable even when new scheduling is disabled or
	// quota exhausted. Creation alone uses health, concurrency and budget admission.
	return u, nil
}

func seedanceStatus(raw []byte, t *videoTask) (json.RawMessage, string, int64, string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || credentialString(fields, "id") != t.UpstreamID {
		return nil, "", 0, "", &apiError{502, "invalid upstream video task identity"}
	}
	status := credentialString(fields, "status")
	switch status {
	case "queued", "running", "succeeded", "failed", "cancelled", "expired":
	default:
		return nil, "", 0, "", &apiError{502, "invalid upstream video task status"}
	}
	tokens := int64(0)
	if status == "succeeded" {
		var content map[string]json.RawMessage
		if json.Unmarshal(fields["content"], &content) != nil || !validAudioURL(credentialString(content, "video_url")) {
			return nil, "", 0, "", &apiError{502, "video completion output is missing or invalid"}
		}
		var usage map[string]json.RawMessage
		if json.Unmarshal(fields["usage"], &usage) != nil || len(usage["completion_tokens"]) == 0 || string(usage["completion_tokens"]) == "null" || json.Unmarshal(usage["completion_tokens"], &tokens) != nil || tokens < 0 || tokens > 2147483647 {
			return nil, "", 0, "", &apiError{502, "video completion usage is missing or invalid"}
		}
		if total := usage["total_tokens"]; total != nil {
			var n int64
			if string(total) == "null" || json.Unmarshal(total, &n) != nil || n != tokens {
				return nil, "", 0, "", &apiError{502, "inconsistent video token usage"}
			}
		}
		if tools := usage["tool_usage"]; tools != nil && string(tools) != "null" {
			var counts map[string]int64
			if json.Unmarshal(tools, &counts) != nil {
				return nil, "", 0, "", &apiError{502, "invalid video tool usage"}
			}
			for _, n := range counts {
				if n != 0 {
					return nil, "", 0, "", &apiError{502, "unexpected video tool consumption requires review"}
				}
			}
		}
	}
	public := map[string]json.RawMessage{}
	for _, name := range []string{"id", "model", "status", "content", "usage", "draft", "created_at", "updated_at", "seed", "resolution", "ratio", "duration", "framespersecond", "generate_audio", "service_tier", "execution_expires_after"} {
		if value, ok := fields[name]; ok {
			public[name] = value
		}
	}
	fields = public
	// Provider errors may echo the original prompt or credentials. Return a stable
	// failure code; successful native output fields otherwise retain their shape.
	if status == "failed" || status == "expired" {
		fields["error"], _ = json.Marshal(map[string]string{"code": "video_generation_failed", "message": "upstream video task " + status})
	}
	fields["id"], _ = json.Marshal(t.ID)
	out, err := json.Marshal(fields)
	return out, status, tokens, credentialString(fields, "model"), err
}
func (a *App) refreshVideoTask(ctx context.Context, t *videoTask) error {
	if t.Stage == "terminal" || t.Stage == "deleted" {
		return nil
	}
	if t.Stage == "settling" {
		return a.settleVideoTask(ctx, t)
	}
	u, err := a.videoSource(ctx, t)
	if err != nil {
		return err
	}
	release, err := a.acquireAccountSlot(ctx, u.ID)
	if err != nil {
		return err
	}
	defer release()
	var resp *http.Response
	if t.Protocol == "videos" {
		resp, err = a.upstreamRequest(ctx, u, "GET", "/v1/videos/"+t.UpstreamID, nil)
	} else {
		resp, err = a.seedanceRequest(ctx, u, "GET", t.UpstreamID, nil)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return a.upstreamError(ctx, u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "upstream video task query rejected; reconciliation remains pending"})
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return &apiError{502, "invalid video task response"}
	}
	var result json.RawMessage
	var status, model string
	var tokens int64
	usage := priceUsage{}
	if t.Protocol == "videos" {
		result, status, usage, model, err = grokVideoStatus(raw, t)
	} else {
		result, status, tokens, model, err = seedanceStatus(raw, t)
		usage.Output = tokens
	}
	if err != nil {
		return err
	}
	t.Result = result
	switch status {
	case "succeeded":
		at := time.Now().UTC()
		t.Receipt, err = a.makeReceipt(t.ID, &t.Identity, &t.Selection, t.Requested, model, "", "", usage, false, at.Sub(t.Created), 0, t.Created, t.Payload, t.IP, t.UserAgent, t.Inbound, t.UpstreamID)
		if err != nil {
			return err
		}
		t.Receipt.Upstream = "/api/v3/contents/generations/tasks"
		if t.Protocol == "videos" {
			t.Receipt.Upstream = "/v1/videos/" + t.Operation
		}
		t.Receipt.At = at
		t.Stage = "settling"
	case "failed", "cancelled", "expired":
		t.Stage = "terminal"
	default:
		t.Stage = "pending"
	}
	// Detach persistence from client cancellation once provider usage is known.
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err = a.saveVideoTask(persist, t); err != nil {
		return err
	}
	if t.Stage == "settling" {
		return a.settleVideoTask(persist, t)
	}
	return nil
}
func (a *App) settleVideoTask(ctx context.Context, t *videoTask) error {
	r := t.Receipt
	if r == nil || r.RequestID != t.ID || r.UserID != t.Identity.UserID || r.KeyID != t.Identity.Key.ID || r.GroupID != t.Identity.Key.GroupID || r.Fingerprint != r.fingerprint() {
		return errors.New("invalid video billing checkpoint")
	}
	if err := a.saveReceipt(ctx, r); err != nil {
		return &apiError{503, "video settlement pending recovery"}
	}
	t.Stage = "terminal"
	return a.saveVideoTask(ctx, t)
}

func (a *App) videoTasks(w http.ResponseWriter, r *http.Request, protocol, operation string) {
	started := time.Now()
	requestID := randomToken(24)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", requestID)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(started.Add(30 * time.Second))
	_ = controller.SetWriteDeadline(started.Add(2 * time.Minute))
	defer controller.SetReadDeadline(time.Time{})
	defer controller.SetWriteDeadline(time.Time{})
	var g *gatewayIdentity
	var s *gatewaySelection
	fail := func(err error) { a.recordGatewayError(requestID, g, s, r, err, started); gatewayError(w, err) }
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	r = r.WithContext(context.WithValue(ctx, clientPolicyKey{}, clientPolicy{Protocol: protocol}))
	platform := "openai"
	if protocol == "videos" {
		platform = "grok"
	}
	var err error
	g, err = a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	if g.Group.Platform != platform && g.Group.Platform != "composite" {
		fail(denied())
		return
	}
	if r.URL.RawQuery != "" {
		fail(bad("video task queries do not accept query parameters"))
		return
	}
	if r.Method != "POST" {
		if err = a.videoLookup(w, r, g, protocol); err != nil {
			fail(err)
		}
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		kind, _, e := mime.ParseMediaType(ct)
		if e != nil || kind != "application/json" {
			fail(bad("video requests require application/json"))
			return
		}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		fail(bad("video request exceeds limit"))
		return
	}
	var body map[string]json.RawMessage
	var model, resolution string
	var seconds int64
	if protocol == "videos" {
		body, model, resolution, seconds, err = parseGrokVideo(raw, operation)
	} else {
		body, model, err = parseSeedance(raw)
	}
	if err != nil {
		fail(err)
		return
	}
	if !g.Group.allows(model) {
		fail(denied())
		return
	}
	raw, _ = json.Marshal(body)
	writer, finish, replayed, err := a.claimGatewayRequest(w, r, g, fmt.Sprintf("%s.%s.%d", protocol, operation, g.Key.GroupID), string(raw), false)
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
	dispatched, complete := false, false
	defer func() {
		if finish != nil && (!dispatched || complete) {
			finish()
		}
	}()
	original := g
	g, err = a.gatewayAuth(r, true)
	if err != nil {
		fail(err)
		return
	}
	if g.Key.ID != original.Key.ID || g.Key.GroupID != original.Key.GroupID || g.UserID != original.UserID {
		fail(conflict("video key assignment changed"))
		return
	}
	g, err = a.acquireGatewayUser(r, g, model, nil)
	if err != nil {
		fail(err)
		return
	}
	defer a.releaseSlot("user", g.UserID)
	if protocol == "videos" && !g.Group.AllowImage {
		fail(denied())
		return
	}
	snapshot := g.Group
	routingModel := model
	if g.Group.Platform == "composite" {
		config, e := a.loadComposite(ctx, g.Key.GroupID)
		if e != nil {
			fail(e)
			return
		}
		decision := config.resolve(g.Key.GroupID, model, "any")
		if !decision.Matched || decision.TargetPlatform != platform {
			fail(bad("video route does not target the required platform"))
			return
		}
		g.Group.Platform, routingModel = decision.TargetPlatform, decision.UpstreamModel
		if err = a.checkPlatformQuota(ctx, g.UserID, platform); err != nil {
			fail(err)
			return
		}
	}
	in := textRequest{Protocol: protocol, Model: model}
	var binding *responseBinding
	var content []map[string]json.RawMessage
	if protocol == "seedance" {
		_ = json.Unmarshal(body["content"], &content)
	}
	for _, item := range content {
		if credentialString(item, "type") != "draft_task" {
			continue
		}
		var draft struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(item["draft_task"], &draft)
		previous, e := a.loadVideoTask(ctx, draft.ID)
		if e != nil {
			fail(e)
			return
		}
		if previous.Protocol != "seedance" || previous.Identity.UserID != g.UserID || previous.Identity.Key.ID != g.Key.ID || previous.Identity.Key.GroupID != g.Key.GroupID || previous.Stage != "terminal" {
			fail(missing())
			return
		}
		var result struct {
			Status string
			Draft  bool
		}
		if json.Unmarshal(previous.Result, &result) != nil || result.Status != "succeeded" || !result.Draft {
			fail(bad("draft task is not a completed draft"))
			return
		}
		if binding != nil && (binding.AccountID != previous.Selection.Account.ID || binding.Target != previous.Target) {
			fail(bad("draft tasks must share one upstream source"))
			return
		}
		binding = &responseBinding{AccountID: previous.Selection.Account.ID, Target: previous.Target}
		item["draft_task"], _ = json.Marshal(map[string]string{"id": previous.UpstreamID})
	}
	if protocol == "seedance" {
		body["content"], _ = json.Marshal(content)
	}
	_, err = a.waitAdmission(ctx, "account", g.Key.GroupID, gatewayQueueTimeout, func(waitCtx context.Context) (bool, error) {
		if e := a.revalidateQueuedRequest(r.WithContext(waitCtx), g, snapshot, in, routingModel); e != nil {
			return false, e
		}
		var e error
		s, e = a.chooseAccount(waitCtx, g, routingModel, in, nil, binding, nil, a.prices.Load())
		if s != nil && waitCtx.Err() != nil {
			s.Release()
			s = nil
			return false, waitCtx.Err()
		}
		var busy *accountBusy
		if errors.As(e, &busy) {
			return false, nil
		}
		return s != nil, e
	}, nil)
	if err != nil {
		fail(err)
		return
	}
	defer s.Release()
	// Preflight and persisted selection use identical price inputs. Restore the
	// catalog index on recovery so response_model billing also uses this snapshot.
	preflight := priceUsage{Output: 1}
	if protocol == "videos" {
		preflight = priceUsage{VideoCount: 1, VideoSeconds: seconds, VideoResolution: resolution}
	}
	if _, err = a.makeReceipt(requestID, g, s, model, "", "", "", preflight, false, 0, 0, started, "", "", "", "", ""); err != nil {
		fail(err)
		return
	}
	if err = a.gatewayRPM(ctx, g); err != nil {
		fail(err)
		return
	}
	if !a.takeSlot("video-create", 0, 1) {
		fail(&apiError{429, "another video submission is in progress"})
		return
	}
	defer a.releaseSlot("video-create", 0)
	count, err := a.Redis.SCard(ctx, videoPending).Result()
	if err != nil {
		fail(err)
		return
	}
	if count >= 32 {
		fail(&apiError{429, "video task capacity reached"})
		return
	}
	body["model"], _ = json.Marshal(s.UpstreamModel)
	upstreamBody, _ := json.Marshal(body)
	dispatched = true
	var resp *http.Response
	if protocol == "videos" {
		resp, err = a.upstreamRequest(ctx, s.Account, "POST", "/v1/videos/"+operation, upstreamBody)
	} else {
		resp, err = a.seedanceRequest(ctx, s.Account, "POST", "", upstreamBody)
	}
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		complete = resp.StatusCode >= 400 && resp.StatusCode < 500
		a.markGatewayFailure(ctx, s, resp.StatusCode, resp.Header.Get("Retry-After"), nil)
		status := resp.StatusCode
		if status < 400 || status > 599 {
			status = 502
		}
		fail(a.upstreamError(ctx, s.Account, resp.StatusCode, readUpstreamError(resp), &apiError{status, "upstream video submission rejected"}))
		return
	}
	raw, err = io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	var accepted struct {
		ID        string
		RequestID string `json:"request_id"`
	}
	parseErr := json.Unmarshal(raw, &accepted)
	if protocol == "videos" {
		accepted.ID = accepted.RequestID
	}
	if err != nil || len(raw) > 2<<20 || parseErr != nil || !validVoiceID(accepted.ID) {
		fail(&apiError{502, "video submission outcome requires review"})
		return
	}
	task := &videoTask{ID: "task_" + randomToken(18), UpstreamID: accepted.ID, Target: responseTarget(s.Account), Stage: "pending", Identity: *g, Selection: *s, Requested: model, Payload: digest(string(upstreamBody)), IP: clientIP(r), UserAgent: truncate(r.UserAgent(), 512), Inbound: r.URL.Path, Created: started}
	task.Protocol, task.Operation, task.Resolution, task.Seconds = protocol, operation, resolution, seconds
	account := *s.Account
	account.Credentials = nil
	account.Extra = map[string]json.RawMessage{}
	for _, key := range []string{"quota_limit", "quota_daily_limit", "quota_weekly_limit"} {
		if value := s.Account.Extra[key]; value != nil {
			account.Extra[key] = value
		}
	}
	task.Selection.Account = &account
	task.Selection.Release = nil
	task.Result, _ = json.Marshal(map[string]string{"id": task.ID, "status": "queued"})
	persist, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	if err = a.saveVideoTask(persist, task); err != nil {
		fail(&apiError{503, "video task persistence failed; upstream outcome requires review"})
		return
	}
	complete = true
	if writer != nil {
		writer.succeeded = true
	}
	field := "id"
	if protocol == "videos" {
		field = "request_id"
	}
	_ = rawReply(w, map[string]string{field: task.ID})
}

func (a *App) videoLookup(w http.ResponseWriter, r *http.Request, g *gatewayIdentity, protocol string) error {
	id := r.PathValue("task_id")
	if !validVoiceID(id) || !strings.HasPrefix(id, "task_") {
		return missing()
	}
	lock := "video-task:" + id
	if !a.takeSlot(lock, 0, 1) {
		return conflict("video task reconciliation is in progress")
	}
	defer a.releaseSlot(lock, 0)
	t, err := a.loadVideoTask(r.Context(), id)
	if err != nil {
		return err
	}
	if t.Protocol != protocol || t.Identity.UserID != g.UserID || t.Identity.Key.ID != g.Key.ID || t.Identity.Key.GroupID != g.Key.GroupID {
		return missing()
	}
	if t.Stage == "deleted" {
		if r.Method == "DELETE" {
			return rawReply(w, map[string]any{})
		}
		return missing()
	}
	if err = a.gatewayRPM(r.Context(), g); err != nil {
		return err
	}
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		return &apiError{429, "user concurrency limit reached"}
	}
	defer a.releaseSlot("user", g.UserID)
	if err = a.refreshVideoTask(r.Context(), t); err != nil {
		return err
	}
	if r.Method == "GET" {
		if protocol == "videos" && strings.HasSuffix(r.URL.Path, "/content") {
			return a.videoContent(w, r, t)
		}
		return rawReply(w, t.Result)
	}
	// A completed task is settled before deleting its provider record. Deleting a
	// queued task is confirmed by another status read, since completion can race it.
	u, err := a.videoSource(r.Context(), t)
	if err != nil {
		return err
	}
	release, err := a.acquireAccountSlot(r.Context(), u.ID)
	if err != nil {
		return err
	}
	resp, err := a.seedanceRequest(r.Context(), u, "DELETE", t.UpstreamID, nil)
	release()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !(resp.StatusCode == 404 && t.Stage == "terminal") {
			return a.upstreamError(r.Context(), u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "upstream video cancellation or deletion rejected"})
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	if t.Stage != "terminal" {
		t.Stage = "pending"
		if err = a.refreshVideoTask(ctx, t); err != nil {
			return err
		}
		if t.Stage != "terminal" {
			return conflict("video cancellation is not confirmed; poll the task")
		}
		return rawReply(w, map[string]any{})
	}
	t.Stage = "deleted"
	t.Result = nil
	if err = a.saveVideoTask(ctx, t); err != nil {
		return err
	}
	return rawReply(w, map[string]any{})
}

func (a *App) runVideoTasks(ctx context.Context) error {
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	ids, err := a.Redis.SMembers(ctx, videoPending).Result()
	if err != nil {
		return err
	}
	// ponytail: 32 durable tasks, polled sequentially; add bounded parallel polls
	// if measured provider latency makes reconciliation fall behind.
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lock := "video-task:" + id
		if !a.takeSlot(lock, 0, 1) {
			continue
		}
		taskCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		task, e := a.loadVideoTask(taskCtx, id)
		if e == nil {
			e = a.refreshVideoTask(taskCtx, task)
		}
		cancel()
		a.releaseSlot(lock, 0)
		if e != nil && ctx.Err() == nil {
			slog.Error("video task reconciliation pending", "task_id", id)
		}
	}
	return nil
}
func (a *App) startVideoTasks(ctx context.Context) {
	a.videoWorkerDone = make(chan struct{})
	go func() {
		defer close(a.videoWorkerDone)
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			if err := a.runVideoTasks(ctx); err != nil && ctx.Err() == nil {
				slog.Error("video reconciliation cycle failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
func (a *App) videoRoutes() {
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		path := prefix + "/contents/generations/tasks"
		for _, method := range []string{"POST", "GET", "DELETE"} {
			pattern := method + " " + path
			if method != "POST" {
				pattern += "/{task_id}"
			}
			a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { a.videoTasks(w, r, "seedance", "create") })
		}
	}
	for _, prefix := range []string{"/v1", ""} {
		// One lookup pattern avoids overlapping ServeMux wildcards between
		// /videos/{id}/content and /videos/generations/{id}.
		a.mux.HandleFunc("GET "+prefix+"/videos/{video_path...}", func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(r.PathValue("video_path"), "/")
			operation := "generations"
			if parts[0] == "generations" || parts[0] == "edits" || parts[0] == "extensions" {
				operation, parts = parts[0], parts[1:]
			}
			if len(parts) < 1 || len(parts) > 2 || len(parts) == 2 && parts[1] != "content" || !strings.HasPrefix(parts[0], "task_") || !validVoiceID(parts[0]) {
				gatewayError(w, missing())
				return
			}
			r.SetPathValue("task_id", parts[0])
			a.videoTasks(w, r, "videos", operation)
		})
		for _, operation := range []string{"", "generations", "edits", "extensions"} {
			path := prefix + "/videos"
			if operation != "" {
				path += "/" + operation
			}
			op := operation
			if op == "" {
				op = "generations"
			}
			a.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) { a.videoTasks(w, r, "videos", op) })
		}
	}
}
