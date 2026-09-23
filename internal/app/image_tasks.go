package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const imageTaskTTL = 24 * time.Hour
const imageTaskPending = "image_tasks:pending"

type imageTaskRecord struct {
	ID          string          `json:"id"`
	UserID      int64           `json:"user_id"`
	APIKeyID    int64           `json:"api_key_id"`
	GroupID     int64           `json:"group_id"`
	Status      string          `json:"status"`
	HTTPStatus  int             `json:"http_status,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	CreatedAt   int64           `json:"created_at"`
	CompletedAt *int64          `json:"completed_at,omitempty"`
	ExpiresAt   int64           `json:"expires_at"`
	Stage       string          `json:"stage,omitempty"`
	Request     string          `json:"request,omitempty"`
	Receipt     *usageReceipt   `json:"receipt,omitempty"`
}

type imageTaskRequest struct {
	Path, RemoteAddr, UserAgent string
	Body                        json.RawMessage
	Storage                     imageStorageConfig
}

type imageTaskExecution struct {
	Task    *imageTaskRecord
	Storage imageStorageConfig
}
type imageTaskContextKey struct{}

func imageExecution(ctx context.Context) *imageTaskExecution {
	x, _ := ctx.Value(imageTaskContextKey{}).(*imageTaskExecution)
	return x
}

func imageTaskKey(id string) string { return "image_task:" + id }

func (a *App) saveImageTask(ctx context.Context, task imageTaskRecord) error {
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	_, err = a.Redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if task.Status == "processing" {
			// Pending work must survive beyond result retention until reconciled.
			p.Set(ctx, imageTaskKey(task.ID), raw, 0)
			p.SAdd(ctx, imageTaskPending, task.ID)
		} else {
			p.Set(ctx, imageTaskKey(task.ID), raw, time.Until(time.Unix(task.ExpiresAt, 0)))
			p.SRem(ctx, imageTaskPending, task.ID)
		}
		return nil
	})
	return err
}

func (a *App) loadImageTask(ctx context.Context, id string) (imageTaskRecord, error) {
	var task imageTaskRecord
	raw, err := a.Redis.Get(ctx, imageTaskKey(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return task, missing()
	}
	if err != nil {
		return task, &apiError{503, "image task storage is unavailable"}
	}
	if json.Unmarshal(raw, &task) != nil || task.ID != id || task.UserID <= 0 || task.APIKeyID <= 0 || task.GroupID <= 0 {
		return task, &apiError{503, "image task state is invalid"}
	}
	return task, nil
}

func (a *App) sealImageRequest(task *imageTaskRecord, in imageTaskRequest) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	cipher := a.chatHistoryCipher()
	nonce := randomBytes(cipher.NonceSize())
	task.Request = base64.RawStdEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, []byte("lite-api/image-task/"+task.ID)))
	return nil
}

func (a *App) openImageRequest(task imageTaskRecord) (imageTaskRequest, error) {
	var in imageTaskRequest
	cipher := a.chatHistoryCipher()
	raw, err := base64.RawStdEncoding.DecodeString(task.Request)
	if err != nil || len(raw) < cipher.NonceSize() {
		return in, errors.New("invalid image request envelope")
	}
	raw, err = cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], []byte("lite-api/image-task/"+task.ID))
	if err == nil {
		err = json.Unmarshal(raw, &in)
	}
	return in, err
}

func (a *App) asyncImageTask(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fail := func(err error) { textGatewayError(w, "images", err) }
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	if _, err := a.admissionWake(); err != nil {
		fail(err)
		return
	}
	g, err := a.gatewayAuth(r, false)
	if err != nil {
		fail(err)
		return
	}
	if g.Group.Platform != "openai" && g.Group.Platform != "composite" || !g.Group.AllowImage {
		fail(denied())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil || len(body) == 0 {
		fail(bad("image task body is invalid or exceeds the limit"))
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/async")
	probe := r.Clone(r.Context())
	probe.URL.Path = path
	var request map[string]json.RawMessage
	if strings.HasSuffix(path, "/edits") && strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		request, err = parseImageMultipart(body, r.Header.Get("Content-Type"))
	} else {
		err = json.Unmarshal(body, &request)
	}
	if err != nil || request == nil {
		fail(bad("JSON object or image edit multipart form required"))
		return
	}
	in, err := parseTextRequest(probe, "images", request)
	if err != nil {
		fail(err)
		return
	}
	if !g.Group.allows(in.Model) {
		fail(denied())
		return
	}
	body, _ = json.Marshal(request)
	writer, finish, replayed, err := a.claimGatewayRequest(w, r, g, in.Scope+".async", string(body), false)
	if err != nil {
		fail(err)
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
	storage, err := a.activeImageStorage(r.Context())
	if err != nil {
		fail(err)
		return
	}
	now := time.Now().UTC()
	task := imageTaskRecord{ID: "imgtask_" + randomToken(18), UserID: g.UserID, APIKeyID: g.Key.ID, GroupID: g.Key.GroupID, Status: "processing", Stage: "queued", CreatedAt: now.Unix(), ExpiresAt: now.Add(imageTaskTTL).Unix()}
	if err = a.sealImageRequest(&task, imageTaskRequest{Path: path, RemoteAddr: r.RemoteAddr, UserAgent: r.UserAgent(), Body: body, Storage: storage}); err != nil {
		fail(err)
		return
	}
	raw, _ := json.Marshal(task)
	// Bound durable admission before accepting a body; active work remains in this set.
	count, err := a.Redis.Eval(r.Context(), `if redis.call('SCARD',KEYS[1])>=32 then return 0 end redis.call('SET',KEYS[2],ARGV[2]); redis.call('SADD',KEYS[1],ARGV[1]); return 1`, []string{imageTaskPending, imageTaskKey(task.ID)}, task.ID, raw).Int()
	if err != nil {
		fail(&apiError{503, "image task storage is unavailable"})
		return
	}
	if count == 0 {
		fail(&apiError{429, "image task queue is full"})
		return
	}
	poll := strings.TrimSuffix(path, "/"+in.Action) + "/tasks/" + task.ID
	w.Header().Set("Location", poll)
	w.Header().Set("Retry-After", "3")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if writer != nil {
		writer.succeeded = true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": task.ID, "task_id": task.ID, "object": "image.generation.task", "status": task.Status, "created_at": task.CreatedAt, "expires_at": task.ExpiresAt, "poll_url": poll})
}

func (a *App) startImageTasks(ctx context.Context) {
	a.imageWorkerDone = make(chan struct{})
	go func() {
		defer close(a.imageWorkerDone)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			if err := a.runImageTasks(ctx); err != nil && ctx.Err() == nil {
				slog.Error("image task recovery failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

func (a *App) runImageTasks(ctx context.Context) error {
	if !a.imageTaskMu.TryLock() {
		return nil
	}
	defer a.imageTaskMu.Unlock()
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	ids, err := a.Redis.SMembers(ctx, imageTaskPending).Result()
	if err != nil {
		return err
	}
	// ponytail: one image worker and 32 pending tasks; add bounded parallel workers
	// only when measured task latency warrants the extra retained image memory.
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		task, err := a.loadImageTask(ctx, id)
		if err != nil {
			return err
		}
		switch task.Stage {
		case "queued":
			if _, err = a.admissionWake(); err != nil {
				return err
			}
			err = a.runImageTask(ctx, task)
		case "settling":
			err = a.completeImageTask(ctx, task)
		case "running":
			// A process died after dispatch. An upstream without a resumable job ID
			// cannot safely be called again; retain a visible indeterminate outcome.
			task.HTTPStatus, task.Error = 503, imageTaskError("execution interrupted; upstream outcome requires review; request will not be resubmitted")
			err = a.finishImageTask(ctx, task)
		default:
			return errors.New("invalid pending image task stage")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) runImageTask(ctx context.Context, task imageTaskRecord) error {
	in, err := a.openImageRequest(task)
	if err != nil {
		task.HTTPStatus, task.Error = 503, imageTaskError("stored image request cannot be decrypted")
		return a.finishImageTask(ctx, task)
	}
	var token string
	err = a.DB.QueryRowContext(ctx, `SELECT key FROM api_keys WHERE id=$1 AND user_id=$2 AND group_id=$3 AND deleted_at IS NULL`, task.APIKeyID, task.UserID, task.GroupID).Scan(&token)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		task.HTTPStatus, task.Error = 403, imageTaskError("image task API key is no longer available")
		return a.finishImageTask(ctx, task)
	}
	task.Stage = "running"
	if err = a.saveImageTask(ctx, task); err != nil {
		return err
	}
	execution := &imageTaskExecution{Task: &task, Storage: in.Storage}
	ctx = context.WithValue(ctx, imageTaskContextKey{}, execution)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.Path, bytes.NewReader(in.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", in.UserAgent)
	req.RemoteAddr = in.RemoteAddr
	recorder := newCaptureResponse()
	a.textGateway(recorder, req, "images")
	// The gateway checkpoints compact results and a receipt before publication.
	// Recover from that checkpoint even if settlement returned 503.
	finishCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stored, err := a.loadImageTask(finishCtx, task.ID)
	if err != nil {
		return err
	}
	if stored.Stage == "settling" {
		return a.completeImageTask(finishCtx, stored)
	}
	task.HTTPStatus = recorder.status
	if task.HTTPStatus < 400 {
		task.HTTPStatus = 503
	}
	var body struct {
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(recorder.Bytes(), &body)
	task.Error = body.Error
	if !json.Valid(task.Error) || string(task.Error) == "null" {
		task.Error = imageTaskError("image generation failed; upstream outcome may require review")
	}
	return a.finishImageTask(finishCtx, task)
}

// Save only object URLs plus billing metadata, never image blobs. This checkpoint
// allows a restarted process to finish billing and publish results without another
// upstream request. The existing receipt transaction deduplicates repeated recovery.
func (a *App) checkpointImageTask(ctx context.Context, execution *imageTaskExecution, result []byte, receipt *usageReceipt, failure error) error {
	task := execution.Task
	task.Stage, task.Receipt, task.Request = "settling", receipt, ""
	task.HTTPStatus = 200
	if failure == nil {
		var err error
		result, err = a.storeImageResult(ctx, execution.Storage, task.ID, result)
		if err != nil {
			failure = &apiError{502, "failed to store generated image to object storage"}
		}
	}
	if failure != nil {
		task.HTTPStatus, task.Error = 502, imageTaskError(safeGatewayError(failure))
	} else {
		task.Result = json.RawMessage(result)
	}
	return a.saveImageTask(ctx, *task)
}

func (a *App) completeImageTask(ctx context.Context, task imageTaskRecord) error {
	if task.Receipt == nil || task.Receipt.RequestID != task.ID || task.Receipt.KeyID != task.APIKeyID || task.Receipt.UserID != task.UserID || task.Receipt.GroupID != task.GroupID || task.Receipt.Fingerprint != task.Receipt.fingerprint() {
		return errors.New("invalid image task billing checkpoint")
	}
	if err := a.saveReceipt(ctx, task.Receipt); err != nil {
		return err
	}
	return a.finishImageTask(ctx, task)
}

func imageTaskError(message string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"type": "api_error", "message": message})
	return raw
}

func (a *App) finishImageTask(ctx context.Context, task imageTaskRecord) error {
	task.Status = "completed"
	if task.HTTPStatus < 200 || task.HTTPStatus >= 300 {
		task.Status, task.Result = "failed", nil
	}
	now := time.Now().UTC()
	completed := now.Unix()
	task.CompletedAt, task.ExpiresAt = &completed, now.Add(imageTaskTTL).Unix()
	task.Stage, task.Request, task.Receipt = "", "", nil
	return a.saveImageTask(ctx, task)
}

type captureResponse struct {
	bytes.Buffer
	status int
	header http.Header
}

func newCaptureResponse() *captureResponse     { return &captureResponse{header: make(http.Header)} }
func (w *captureResponse) Header() http.Header { return w.header }
func (w *captureResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *captureResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.Buffer.Write(data)
}
func (w *captureResponse) Flush() {}

func (a *App) getImageTask(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := a.checkInstance(r.Context()); err != nil {
		textGatewayError(w, "images", err)
		return
	}
	g, err := a.gatewayAuth(r, false)
	if err != nil {
		textGatewayError(w, "images", err)
		return
	}
	id := r.PathValue("task_id")
	if !strings.HasPrefix(id, "imgtask_") || !validNativeModel(id) {
		textGatewayError(w, "images", missing())
		return
	}
	task, err := a.loadImageTask(r.Context(), id)
	if err != nil {
		textGatewayError(w, "images", err)
		return
	}
	if task.UserID != g.UserID || task.APIKeyID != g.Key.ID {
		textGatewayError(w, "images", missing())
		return
	}
	if task.Status == "processing" {
		w.Header().Set("Retry-After", "3")
	}
	public := map[string]any{"id": task.ID, "task_id": task.ID, "object": "image.generation.task", "status": task.Status, "created_at": task.CreatedAt, "expires_at": task.ExpiresAt}
	if task.CompletedAt != nil {
		public["completed_at"] = *task.CompletedAt
	}
	if task.HTTPStatus != 0 {
		public["http_status"] = task.HTTPStatus
	}
	if task.Status == "completed" {
		public["result"] = task.Result
		var result struct {
			Data []struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		_ = json.Unmarshal(task.Result, &result)
		if len(result.Data) > 0 {
			public["image_url"] = result.Data[0].URL
		}
	}
	if task.Status == "failed" {
		public["error"] = task.Error
	}
	_ = json.NewEncoder(w).Encode(public)
}

func (a *App) imageTaskRoutes() {
	for _, suffix := range []string{"/v1/images/generations/async", "/images/generations/async", "/v1/images/edits/async", "/images/edits/async"} {
		a.mux.HandleFunc("POST "+suffix, a.asyncImageTask)
	}
	for _, suffix := range []string{"/v1/images/tasks/{task_id}", "/images/tasks/{task_id}"} {
		a.mux.HandleFunc("GET "+suffix, a.getImageTask)
	}
}
