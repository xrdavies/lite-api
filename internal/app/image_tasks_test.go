package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestImageTaskEnvelopeAndCapture(t *testing.T) {
	a := &App{secret: []byte(strings.Repeat("s", 32))}
	task := imageTaskRecord{ID: "imgtask_test"}
	in := imageTaskRequest{Path: "/v1/images/generations", Body: json.RawMessage(`{"prompt":"private"}`), Storage: imageStorageConfig{Secret: "private-secret"}}
	if err := a.sealImageRequest(&task, in); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(task.Request, "private") {
		t.Fatal("plaintext image request")
	}
	out, err := a.openImageRequest(task)
	if err != nil || string(out.Body) != string(in.Body) || out.Storage.Secret != in.Storage.Secret {
		t.Fatal("request envelope", err)
	}
	task.ID = "imgtask_other"
	if _, err := a.openImageRequest(task); err == nil {
		t.Fatal("request envelope not bound to task")
	}
	writer := newCaptureResponse()
	if err := http.NewResponseController(writer).SetWriteDeadline(time.Now()); !errors.Is(err, http.ErrNotSupported) {
		t.Fatal("capture deadline", err)
	}
	writer.Write([]byte("response"))
	if writer.status != 200 || writer.String() != "response" {
		t.Fatal("capture response")
	}
}

func testImageTasks(t *testing.T, a *App, admin string) {
	t.Helper()
	ctx := context.Background()
	locked := false
	defer func() {
		if locked {
			a.imageTaskMu.Unlock()
		}
	}()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.188:1234"
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	var storageDown atomic.Bool
	var uploads, upstreamCalls atomic.Int32
	s3 := testS3Storage(t, a)
	storageURL, _ := url.Parse(s3.Endpoint)
	proxy := httputil.NewSingleHostReverseProxy(storageURL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	proxy.Transport = transport
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.Method == "PUT" && r.StatusCode == 200 {
			uploads.Add(1)
		}
		return nil
	}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.Method != "GET" && !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")) || strings.Contains(r.Header.Get("Authorization"), s3.Secret) || r.Header.Get("Cookie") != "" {
			t.Error("storage credential isolation")
		}
		if storageDown.Load() {
			w.WriteHeader(503)
			return
		}
		if r.Method == "HEAD" || r.Method == "GET" {
			proxy.ServeHTTP(w, r)
			return
		}
		if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/"+s3.Bucket+"/generated/imgtask_") {
			t.Error("invalid image object path", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		if http.DetectContentType(data) != "image/png" {
			t.Error("image bytes changed")
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		proxy.ServeHTTP(w, r)
	}))
	defer storage.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer async-upstream" || r.Header.Get("Cookie") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("upstream credential isolation")
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if credentialString(body, "model") != "async-upstream-model" || r.URL.Path != "/v1/images/generations" && r.URL.Path != "/v1/images/edits" {
			t.Error("image dispatch", r.URL.Path)
		}
		if r.URL.Path == "/v1/images/edits" {
			var mask map[string]string
			var images []map[string]string
			_ = json.Unmarshal(body["mask"], &mask)
			_ = json.Unmarshal(body["images"], &images)
			if len(images) != 1 || images[0]["image_url"] != "data:image/png;base64,c291cmNl" || mask["image_url"] != "data:image/png;base64,bWFzaw==" {
				t.Error("async edit references or mask lost", body)
			}
		}
		if credentialString(body, "prompt") == "upstream failure" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Image-Trace", "image-create-trace")
		png := base64.StdEncoding.EncodeToString(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...))
		_, _ = fmt.Fprintf(w, `{"created":1710000000,"data":[{"b64_json":%q}]}`, png)
	}))
	defer upstream.Close()
	storageConfig := map[string]any{"enabled": true, "bucket": s3.Bucket, "prefix": "generated/", "region": s3.Region, "endpoint": storage.URL, "force_path_style": true, "access_key_id": s3.AccessKey, "secret_access_key": s3.Secret}
	manage("PUT", "/api/v1/admin/backups/image-storage", admin, storageConfig)
	if !manage("POST", "/api/v1/admin/backups/image-storage/test", admin, storageConfig)["ok"].(bool) {
		t.Fatal("object storage test")
	}
	if w := call("GET", "/api/v1/admin/backups/image-storage", admin, nil, ""); strings.Contains(w.Body.String(), s3.Secret) || strings.Contains(w.Body.String(), "enc:v1:") {
		t.Fatal("storage secret disclosed")
	}
	uid := int64(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "async-images@example.test", "password": "async-images-password", "balance": 10})["id"].(float64))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "async-images@example.test", "password": "async-images-password"})["access_token"].(string)
	gid := int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Async images", "platform": "openai", "allow_image_generation": true, "image_price_2k": "0.04", "image_rate_independent": true, "image_rate_multiplier": "0.5"})["id"].(float64))
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Async image prices", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"async-image"}, "billing_mode": "image", "per_request_price": json.Number("0.02")}}})
	manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Async image provider", "platform": "openai", "type": "apikey", "extra": map[string]any{upstreamRequestIDHeaderKey: "X-Image-Trace"}, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "async-upstream", "base_url": upstream.URL, "model_mapping": map[string]string{"async-image": "async-upstream-model"}}})
	keyData := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Async image key", "group_id": gid, "quota": 100})
	key := keyData["key"].(string)
	kid := int64(keyData["id"].(float64))
	otherKey := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Other image key", "group_id": gid})["key"].(string)
	body := map[string]any{"model": "async-image", "prompt": "private lighthouse"}
	accepted := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		var result struct{ ID, Object, Status string }
		if w.Code != 202 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ID == "" || result.Status != "processing" || result.Object != "image.generation.task" {
			t.Fatalf("image task acceptance: %d %s", w.Code, w.Body.String())
		}
		return result.ID
	}
	poll := func(id string, want string) imageTaskRecord {
		t.Helper()
		var result imageTaskRecord
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			w := call("GET", "/images/tasks/"+id, key, nil, "")
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("poll task: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "user_id") || strings.Contains(w.Body.String(), "api_key_id") || strings.Contains(w.Body.String(), "Receipt") || strings.Contains(w.Body.String(), "b64_json") || strings.Contains(w.Body.String(), "private lighthouse") {
				t.Fatal("private task data exposed")
			}
			if result.Status == want {
				return result
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("task did not reach %s: %+v", want, result)
		return result
	}
	// Stop worker admission while inspecting durable acceptance and replay.
	a.imageTaskMu.Lock()
	locked = true
	first := call("POST", "/v1/images/generations/async", key, body, "async-same")
	id := accepted(first)
	second := call("POST", "/images/generations/async", key, body, "async-same")
	if accepted(second) != id || second.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("async alias replay")
	}
	if w := call("POST", "/images/generations/async", key, map[string]any{"model": "async-image", "prompt": "changed"}, "async-same"); w.Code != 409 {
		t.Fatal("async payload conflict", w.Code)
	}
	raw, err := a.Redis.Get(ctx, imageTaskKey(id)).Result()
	if err != nil || strings.Contains(raw, "private lighthouse") || strings.Contains(raw, s3.Secret) || strings.Contains(raw, key) {
		t.Fatal("task request not encrypted", err)
	}
	if w := call("GET", "/v1/images/tasks/"+id, otherKey, nil, ""); w.Code != 404 {
		t.Fatal("cross-key image task leak", w.Code)
	}
	if w := call("GET", "/v1/images/tasks/"+id, user, nil, ""); w.Code != 401 {
		t.Fatal("JWT used as gateway key", w.Code)
	}
	a.imageTaskMu.Unlock()
	locked = false
	result := poll(id, "completed")
	if uploads.Load() != 1 || upstreamCalls.Load() != 1 || !strings.Contains(string(result.Result), "X-Amz-Signature=") || result.CompletedAt == nil || result.ExpiresAt < *result.CompletedAt+86399 {
		t.Fatal("image task completion", uploads.Load(), upstreamCalls.Load(), result)
	}
	var storedImages struct{ Data []map[string]json.RawMessage }
	if json.Unmarshal(result.Result, &storedImages) != nil || len(storedImages.Data) != 1 {
		t.Fatal("missing stored image")
	}
	if data, err := a.imageBytes(ctx, s3.MaxDownloadBytes, storedImages.Data[0]); err != nil || http.DetectContentType(data) != "image/png" {
		t.Fatal("completed task image could not be downloaded", err)
	}
	var cost, balance, quota string
	if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", id).Scan(&cost); err != nil || cost != "0.0200000000" {
		t.Fatal("async cost", cost, err)
	}
	var countImages int
	var imageSize, imageSource, rate string
	if err := a.DB.QueryRow("SELECT image_count,image_size,image_size_source,rate_multiplier::text FROM usage_logs WHERE request_id=$1", id).Scan(&countImages, &imageSize, &imageSource, &rate); err != nil || countImages != 1 || imageSize != "2K" || imageSource != "default" || rate != "0.5000" {
		t.Fatal("async image accounting", countImages, imageSize, imageSource, rate, err)
	}
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE id=$1", uid).Scan(&balance); err != nil || balance != "9.98000000" {
		t.Fatal("async balance", balance, err)
	}
	if err := a.DB.QueryRow("SELECT quota_used::text FROM api_keys WHERE id=$1", kid).Scan(&quota); err != nil || quota != "0.02000000" {
		t.Fatal("async key count", quota, err)
	}
	// Poll and idempotent acceptance replay remain available after exhaustion and disabling storage.
	_, _ = a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid)
	storageConfig["enabled"] = false
	manage("PUT", "/api/v1/admin/backups/image-storage", admin, storageConfig)
	poll(id, "completed")
	if accepted(call("POST", "/images/generations/async", key, body, "async-same")) != id {
		t.Fatal("exhausted replay changed task")
	}
	if w := call("POST", "/images/generations/async", key, body, ""); w.Code != 402 {
		t.Fatal("accepted task without balance", w.Code)
	}
	_, _ = a.DB.Exec("UPDATE users SET balance=10 WHERE id=$1", uid)
	if w := call("POST", "/images/generations/async", key, body, ""); w.Code != 404 {
		t.Fatal("accepted task without storage", w.Code)
	}
	storageConfig["enabled"] = true
	manage("PUT", "/api/v1/admin/backups/image-storage", admin, storageConfig)
	for _, invalid := range []map[string]any{{"model": "async-image", "prompt": "x", "stream": true}, {"model": "async-image", "prompt": ""}} {
		if w := call("POST", "/images/generations/async", key, invalid, ""); w.Code != 400 {
			t.Fatal("invalid async request accepted", w.Code)
		}
	}
	// Multipart edits use the same validated normalized request as synchronous edits.
	var edit bytes.Buffer
	form := multipart.NewWriter(&edit)
	_ = form.WriteField("model", "async-image")
	_ = form.WriteField("prompt", "replace the sky")
	part, _ := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image"; filename="source.png"`}, "Content-Type": {"image/png"}})
	_, _ = part.Write([]byte("source"))
	part, _ = form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="mask"; filename="mask.png"`}, "Content-Type": {"image/png"}})
	_, _ = part.Write([]byte("mask"))
	_ = form.Close()
	r := httptest.NewRequest("POST", "/images/edits/async", &edit)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Authorization", "Bearer "+key)
	r.RemoteAddr = "192.0.2.188:1234"
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	poll(accepted(w), "completed")
	if upstreamCalls.Load() != 2 {
		t.Fatal("edit was not generated exactly once")
	}
	// Storage failure remains billable consumption, but never exposes base64.
	storageDown.Store(true)
	failedID := accepted(call("POST", "/images/generations/async", key, body, "storage-failure"))
	failed := poll(failedID, "failed")
	if failed.HTTPStatus != 502 || len(failed.Result) != 0 || !strings.Contains(string(failed.Error), "object storage") {
		t.Fatal("storage failure result", failed)
	}
	if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", failedID).Scan(&cost); err != nil || cost != "0.0200000000" {
		t.Fatal("storage failure lost consumption", cost, err)
	}
	storageDown.Store(false)
	upstreamFailure := accepted(call("POST", "/images/generations/async", key, map[string]any{"model": "async-image", "prompt": "upstream failure"}, ""))
	poll(upstreamFailure, "failed")
	var logged int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", upstreamFailure).Scan(&logged); err != nil || logged != 0 {
		t.Fatal("rejected upstream charged", logged, err)
	}
	// Keep a real post-generation checkpoint by failing the billing transaction.
	if _, err := a.DB.Exec(`CREATE FUNCTION reject_async_bill() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.model='async-image' THEN RAISE EXCEPTION 'test settlement failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_async_bill BEFORE INSERT ON usage_logs FOR EACH ROW EXECUTE FUNCTION reject_async_bill()`); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec(`DROP TRIGGER IF EXISTS reject_async_bill ON usage_logs; DROP FUNCTION IF EXISTS reject_async_bill()`)
	recoverID := accepted(call("POST", "/images/generations/async", key, body, "recover"))
	var checkpoint imageTaskRecord
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		checkpoint, err = a.loadImageTask(ctx, recoverID)
		if err == nil && checkpoint.Stage == "settling" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if checkpoint.Stage != "settling" || checkpoint.Receipt == nil || len(checkpoint.Result) == 0 {
		t.Fatal("missing recovery checkpoint", checkpoint.Stage, err)
	}
	a.imageTaskMu.Lock()
	locked = true
	manage("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"image_price_2k": 9, "image_rate_multiplier": 7})
	if _, err := a.DB.Exec(`DROP TRIGGER reject_async_bill ON usage_logs; DROP FUNCTION reject_async_bill()`); err != nil {
		t.Fatal(err)
	}
	count := upstreamCalls.Load()
	// A new process-equivalent App has no execution memory. Recovery only needs persisted data.
	restarted := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	if err := restarted.runImageTasks(ctx); err != nil {
		t.Fatal("restart recovery", err)
	}
	if err := restarted.completeImageTask(ctx, checkpoint); err != nil {
		t.Fatal("repeat checkpoint recovery", err)
	}
	poll(recoverID, "completed")
	assertUsageRequestID(t, a, recoverID, "image-create-trace")
	if upstreamCalls.Load() != count {
		t.Fatal("recovery resubmitted generation")
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", recoverID).Scan(&logged); err != nil || logged != 1 {
		t.Fatal("recovery charged twice", logged, err)
	}
	if err := a.DB.QueryRow("SELECT actual_cost::text,image_count,image_size,rate_multiplier::text FROM usage_logs WHERE request_id=$1", recoverID).Scan(&cost, &countImages, &imageSize, &rate); err != nil || cost != "0.0200000000" || countImages != 1 || imageSize != "2K" || rate != "0.5000" {
		t.Fatal("recovery changed image price snapshot", cost, countImages, imageSize, rate, err)
	}
	manage("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"image_price_2k": "0.04", "image_rate_multiplier": "0.5"})
	// Interrupted dispatch is terminal without unsafe replay. A queued request can resume.
	orphan := imageTaskRecord{ID: "imgtask_interrupted", UserID: uid, APIKeyID: kid, GroupID: gid, Status: "processing", Stage: "running", CreatedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(imageTaskTTL).Unix()}
	if err := a.saveImageTask(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	if err := restarted.runImageTasks(ctx); err != nil {
		t.Fatal(err)
	}
	poll(orphan.ID, "failed")
	if upstreamCalls.Load() != count {
		t.Fatal("interrupted task was resent")
	}
	queuedID := accepted(call("POST", "/images/generations/async", key, body, "queued-restart"))
	restarted.mux = a.mux
	restarted.prices.Store(a.prices.Load())
	if err := restarted.runImageTasks(ctx); err != nil {
		t.Fatal("queued recovery", err)
	}
	poll(queuedID, "completed")
	if upstreamCalls.Load() != count+1 {
		t.Fatal("queued task not resumed once")
	}
	// API key group changes after admission are rejected before upstream dispatch.
	changedID := accepted(call("POST", "/images/generations/async", key, body, "changed-key"))
	_, _ = a.DB.Exec("UPDATE api_keys SET group_id=NULL WHERE id=$1", kid)
	if err := restarted.runImageTasks(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = a.DB.Exec("UPDATE api_keys SET group_id=$2 WHERE id=$1", kid, gid)
	poll(changedID, "failed")
	if upstreamCalls.Load() != count+1 {
		t.Fatal("changed key task dispatched")
	}
	if n, err := a.Redis.SCard(ctx, imageTaskPending).Result(); err != nil || n != 0 {
		t.Fatal("pending tasks not cleared", n, err)
	}
	// Durable admission has a hard bound; a full queue cannot retain another body.
	for i := 0; i < 32; i++ {
		if err := a.Redis.SAdd(ctx, imageTaskPending, fmt.Sprintf("queue-test-%d", i)).Err(); err != nil {
			t.Fatal(err)
		}
	}
	w = call("POST", "/images/generations/async", key, body, "")
	_ = a.Redis.Del(ctx, imageTaskPending).Err()
	if w.Code != 429 || upstreamCalls.Load() != count+1 {
		t.Fatal("full image queue accepted a request", w.Code)
	}
	// Missing the storage secret on update preserves it and unknown settings.
	if _, err := a.DB.Exec(`UPDATE settings SET value=(value::jsonb || '{"future_option":true}'::jsonb)::text WHERE key=$1`, imageStorageSetting); err != nil {
		t.Fatal(err)
	}
	delete(storageConfig, "secret_access_key")
	manage("PUT", "/api/v1/admin/backups/image-storage", admin, storageConfig)
	var setting string
	if err := a.DB.QueryRow("SELECT value FROM settings WHERE key=$1", imageStorageSetting).Scan(&setting); err != nil || strings.Contains(setting, s3.Secret) {
		t.Fatal("storage settings preservation", err)
	}
	var storedConfig map[string]json.RawMessage
	if json.Unmarshal([]byte(setting), &storedConfig) != nil || string(storedConfig["future_option"]) != "true" {
		t.Fatal("unknown storage setting lost")
	}
	if c, err := a.activeImageStorage(ctx); err != nil || c.Secret != s3.Secret {
		t.Fatal("storage secret not preserved", err)
	}
}
