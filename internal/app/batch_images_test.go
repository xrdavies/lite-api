package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBatchImageInputAndEnvelope(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", `a\b`, "..", "\r\n", strings.Repeat("猫", 80)} {
		name := batchFilename(id, "png", 1)
		if !utf8.ValidString(name) || path.Base(name) != name || strings.ContainsAny(name, "\\\r\n") || strings.Contains(name, "..") || !strings.HasSuffix(name, "_2.png") {
			t.Fatal("unsafe batch filename", name)
		}
	}
	in := batchImageRequest{Model: "gemini-3-pro-image-preview", Items: []batchImageInput{{ID: "a", Prompt: "private scene", Count: 2}}}
	if err := in.normalize(); err != nil || len(in.Items) != 2 || in.Items[1].ID != "a_02" {
		t.Fatal(in, err)
	}
	raw, err := in.jsonl()
	if err != nil || bytes.Count(raw, []byte("\n")) != 2 || !bytes.Contains(raw, []byte(`"imageSize":"1K"`)) {
		t.Fatal(string(raw), err)
	}
	for _, body := range []string{
		`{"model":"m","items":[{"prompt":"x","custom_id":"a","output_count":2},{"prompt":"y","custom_id":"a_02"}]}`,
		`{"model":"m","provider":"vertex","items":[{"prompt":"x"}]}`,
		`{"model":"m","image_size":"4K","items":[{"prompt":"x"}]}`,
		`{"model":"m","items":[{"prompt":"x","output_count":5}]}`,
		`{"model":"gemini-3-pro-image-preview","items":[{"prompt":"x","reference_images":[{"mime_type":"image/png","file_uri":"http://127.0.0.1/private"}]}]}`,
	} {
		var invalid batchImageRequest
		if json.Unmarshal([]byte(body), &invalid) != nil {
			t.Fatal(body)
		}
		if invalid.normalize() == nil {
			t.Fatal("accepted invalid input", body)
		}
	}
	for _, name := range []string{"files/../secret", "files/a/b", "https://files/a", "files/a?key=secret", "files/a#fragment"} {
		if batchResource(name, "files") {
			t.Fatal("unsafe provider resource", name)
		}
	}
	a := &App{secret: []byte(strings.Repeat("b", 32))}
	snap := batchImageSnapshot{Request: in, GroupID: 1, Target: "private target"}
	encoded, err := a.batchEnvelope("job1", &snap, "")
	if err != nil || strings.Contains(encoded, "private") {
		t.Fatal("envelope", err)
	}
	var restored batchImageSnapshot
	if _, err = a.batchEnvelope("job1", &restored, encoded); err != nil || restored.Request.Items[0].Prompt != "private scene" {
		t.Fatal("restore", err)
	}
	if _, err = a.batchEnvelope("job2", &restored, encoded); err == nil {
		t.Fatal("cross-task envelope accepted")
	}
	for _, raw := range []string{`{}`, `{"key":"a","response":{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW52YWxpZA=="}}]}}]}}`} {
		if _, err := parseBatchImageLine([]byte(raw)); err == nil {
			t.Fatal("invalid provider image accepted", raw)
		}
	}
}

func testBatchImages(t *testing.T, a *App, admin string) {
	t.Helper()
	ctx := context.Background()
	// Keep the running worker idle; separate App objects exercise persisted recovery
	// without sharing any task memory or starting a second service instance.
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	fresh := func() *App {
		b := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, privateUpstreams: a.privateUpstreams, instanceLock: a.instanceLock, mux: http.NewServeMux()}
		b.prices.Store(a.prices.Load())
		b.batchImageRoutes()
		return b
	}
	b := fresh()
	request := func(h http.Handler, method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.189:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Cookie", "downstream-private")
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		return request(b.Handler(), method, path, token, body, idem)
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := request(a.Handler(), method, path, token, body, "")
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	type providerJob struct {
		Name, Display, State string
		Keys                 []string
	}
	var mu sync.Mutex
	jobs := map[string]*providerJob{}
	inputs := map[string][]string{}
	uploads, creates, downloads, deletes, cancels := 0, 0, 0, 0, 0
	unknownCreate, badOutput, changedOutput, cancelFailure, allImages := false, false, false, false, false
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-Goog-Api-Key") != "batch-provider-secret" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("batch credential isolation")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/upload/v1beta/files":
			uploads++
			if r.URL.Query().Get("uploadType") != "multipart" {
				t.Error("upload query")
			}
			form, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			meta, err := form.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			var name struct{ File struct{ DisplayName string } }
			if json.NewDecoder(meta).Decode(&name) != nil || name.File.DisplayName == "" {
				t.Error("upload metadata")
			}
			file, err := form.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(file)
			keys := []string{}
			for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
				var item struct {
					Key     string
					Request struct{ Contents []json.RawMessage }
				}
				if json.Unmarshal(line, &item) != nil || item.Key == "" || len(item.Request.Contents) == 0 {
					t.Error("invalid JSONL")
				}
				keys = append(keys, item.Key)
			}
			ref := fmt.Sprintf("files/input%d", uploads)
			inputs[ref] = keys
			_ = json.NewEncoder(w).Encode(map[string]any{"file": map[string]string{"name": ref}})
		case r.Method == "POST" && r.URL.Path == "/v1beta/models/gemini-3-pro-image-preview:batchGenerateContent":
			creates++
			var body struct {
				Batch struct {
					DisplayName string
					InputConfig struct{ FileName string }
				}
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			keys, ok := inputs[body.Batch.InputConfig.FileName]
			if !ok {
				t.Error("missing uploaded input")
			}
			name := fmt.Sprintf("batches/job%d", creates)
			jobs[name] = &providerJob{name, body.Batch.DisplayName, "JOB_STATE_RUNNING", keys}
			if unknownCreate {
				unknownCreate = false
				w.WriteHeader(503)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
		case r.Method == "GET" && r.URL.Path == "/v1beta/batches":
			list := []any{}
			for _, j := range jobs {
				list = append(list, map[string]any{"name": j.Name, "metadata": map[string]string{"displayName": j.Display, "state": j.State}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"operations": list})
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, ":cancel"):
			cancels++
			if cancelFailure {
				w.WriteHeader(503)
				return
			}
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1beta/"), ":cancel")
			if j := jobs[name]; j != nil {
				j.State = "JOB_STATE_CANCELLED"
			}
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1beta/batches/"):
			j := jobs[strings.TrimPrefix(r.URL.Path, "/v1beta/")]
			if j == nil {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": j.Name, "metadata": map[string]string{"state": j.State}, "response": map[string]string{"responsesFile": "files/" + strings.TrimPrefix(j.Name, "batches/")}})
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/download/v1beta/files/"):
			downloads++
			name := "batches/" + strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/download/v1beta/files/"), ":download")
			j := jobs[name]
			if j == nil {
				w.WriteHeader(404)
				return
			}
			enc := json.NewEncoder(w)
			for i, key := range j.Keys {
				if badOutput {
					key = "unexpected"
				}
				if i > 0 && !allImages || changedOutput {
					_ = enc.Encode(map[string]any{"key": key, "error": map[string]string{"message": "private upstream error"}})
					continue
				}
				_ = enc.Encode(map[string]any{"key": key, "response": map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": png}}}}}}}})
			}
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1beta/files/"):
			deletes++
			w.WriteHeader(404) // Already gone is an idempotent cleanup success.
		default:
			t.Error("unexpected provider request", r.Method, r.URL.String())
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	uid := int64(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "batch-images@example.test", "password": "batch-images-password", "balance": 10})["id"].(float64))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "batch-images@example.test", "password": "batch-images-password"})["access_token"].(string)
	gid := int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Batch images", "platform": "gemini", "allow_batch_image_generation": true, "image_price_1k": json.Number("0.1"), "rate_multiplier": 2, "batch_image_discount_multiplier": json.Number("0.5"), "batch_image_hold_multiplier": json.Number("0.6"), "profit_control_enabled": true, "profit_min_margin": "0.9"})["id"].(float64))
	groupPath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	aid := int64(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Batch provider", "platform": "gemini", "type": "apikey", "group_ids": []int64{gid}, "rate_multiplier": json.Number("1.25"), "credentials": map[string]any{"api_key": "batch-provider-secret", "base_url": provider.URL + "/v1beta", "model_mapping": map[string]string{"team-image": "gemini-3-pro-image-preview"}}})["id"].(float64))
	keyData := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Batch key", "group_id": gid, "quota": 100})
	key := keyData["key"].(string)
	kid := int64(keyData["id"].(float64))
	other := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Other batch key", "group_id": gid})["key"].(string)
	body := map[string]any{"model": "team-image", "items": []any{map[string]any{"custom_id": "one", "prompt": "private scene", "output_count": 2}}}
	must := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		t.Helper()
		w := call(method, path, token, body, idem)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w
	}
	submit := func(idem string) string {
		t.Helper()
		w := must("POST", "/v1/images/batches", key, body, idem)
		var out struct{ ID string }
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ID == "" {
			t.Fatal(w.Body)
		}
		return out.ID
	}
	balances := func(want, held string) {
		t.Helper()
		var balance, frozen string
		if err := a.DB.QueryRow("SELECT balance::text,frozen_balance::text FROM users WHERE id=$1", uid).Scan(&balance, &frozen); err != nil || balance != want || frozen != held {
			t.Fatalf("balance %s frozen %s, want %s %s: %v", balance, frozen, want, held, err)
		}
	}
	cycle := func(app *App, id string, want string) {
		t.Helper()
		if err := app.processBatchImage(ctx, id); err != nil {
			t.Fatal("batch cycle", err)
		}
		job, err := app.loadBatchJob(ctx, id)
		if err != nil || job.Status != want {
			t.Fatalf("batch status %s want %s: %v", job.Status, want, err)
		}
	}
	setState := func(id, state string) {
		t.Helper()
		j, err := b.loadBatchJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		jobs[j.ProviderJob].State = state
	}
	usageCount := func(id string, want int) {
		t.Helper()
		var n int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", "batch_image_capture:"+id).Scan(&n); err != nil || n != want {
			t.Fatal("usage count", n, want, err)
		}
	}
	id := submit("same")
	path := "/v1/images/batches/" + id
	balances("9.70000000", "0.30000000")
	if submit("same") != id {
		t.Fatal("replay created new job")
	}
	balances("9.70000000", "0.30000000")
	if w := call("POST", "/v1/images/batches", key, map[string]any{"model": "team-image", "items": []any{map[string]any{"prompt": "changed"}}}, "same"); w.Code != 409 {
		t.Fatal("idempotency conflict", w.Code)
	}
	var payload string
	if err := a.DB.QueryRow("SELECT payload::text FROM batch_image_events WHERE job_id=$1 AND event_type='request_created'", id).Scan(&payload); err != nil || strings.Contains(payload, "private scene") || strings.Contains(payload, "batch-provider-secret") {
		t.Fatal("unencrypted request", err)
	}
	for _, suffix := range []string{"", "/items", "/items/one_01/content", "/download"} {
		if w := call("GET", path+suffix, other, nil, ""); w.Code != 404 {
			t.Fatal("cross-key read", suffix, w.Code)
		}
	}
	for _, method := range []string{"POST", "DELETE"} {
		suffix := ""
		if method == "POST" {
			suffix = "/cancel"
		}
		if w := call(method, path+suffix, other, nil, ""); w.Code != 404 {
			t.Fatal("cross-key write", w.Code)
		}
	}
	if w := call("GET", path, user, nil, ""); w.Code != 401 {
		t.Fatal("JWT accepted", w.Code)
	}
	if w := call("GET", path+"/download", key, nil, ""); w.Code != 409 {
		t.Fatal("download before settlement", w.Code)
	}
	if w := must("GET", "/v1/images/batches/models", key, nil, ""); !strings.Contains(w.Body.String(), `"id":"team-image"`) {
		t.Fatal("batch model alias missing", w.Body.String())
	}
	cycle(b, id, "running")
	// Pricing changes cannot change an accepted hold or its settlement.
	manage("PUT", groupPath, admin, map[string]any{"image_price_1k": 9, "rate_multiplier": 9})
	setState(id, "JOB_STATE_SUCCEEDED")
	badOutput = true
	if err := b.processBatchImage(ctx, id); err == nil {
		t.Fatal("unrecognized output accepted")
	}
	badOutput = false
	balances("9.70000000", "0.30000000")
	usageCount(id, 0)
	cycle(b, id, "settling")
	if _, err := a.DB.Exec(`ALTER TABLE usage_logs ADD CONSTRAINT test_batch_failure CHECK(model<>'team-image') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec(`ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_batch_failure`)
	if err := b.processBatchImage(ctx, id); err == nil {
		t.Fatal("settlement failure not injected")
	}
	balances("9.70000000", "0.30000000")
	usageCount(id, 0)
	if _, err := a.DB.Exec(`ALTER TABLE usage_logs DROP CONSTRAINT test_batch_failure`); err != nil {
		t.Fatal(err)
	}
	recovered := fresh()
	cycle(recovered, id, "completed")
	cycle(recovered, id, "completed")
	balances("9.87500000", "0.00000000")
	usageCount(id, 1)
	var actual, total string
	var count int
	if err := a.DB.QueryRow("SELECT actual_cost::text,total_cost::text,image_count FROM usage_logs WHERE request_id=$1", "batch_image_capture:"+id).Scan(&actual, &total, &count); err != nil || actual != "0.1250000000" || total != actual || count != 1 {
		t.Fatal("batch snapshot billing", actual, total, count, err)
	}
	var quota string
	if err := a.DB.QueryRow("SELECT quota_used::text FROM api_keys WHERE id=$1", kid).Scan(&quota); err != nil || quota != "0.00000000" {
		t.Fatal("batch ledger semantics", quota, err)
	}
	content := must("GET", path+"/items/one_01/content", key, nil, "")
	if !bytes.Equal(content.Body.Bytes(), png) {
		t.Fatal("image changed")
	}
	kind, disposition, err := mime.ParseMediaType(content.Header().Get("Content-Disposition"))
	if err != nil || kind != "attachment" || disposition["filename"] != "one_01.png" {
		t.Fatal("image filename", disposition, err)
	}
	downloadedAt := func() *time.Time {
		t.Helper()
		var at *time.Time
		if err := a.DB.QueryRow("SELECT downloaded_at FROM batch_image_jobs WHERE batch_id=$1", id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	firstDownload := downloadedAt()
	if firstDownload == nil {
		t.Fatal("single image did not mark downloaded")
	}
	if w := must("GET", "/v1/images/batches?downloaded=true", key, nil, ""); !strings.Contains(w.Body.String(), id) {
		t.Fatal("single image missing from downloaded list", w.Body.String())
	}
	archive := must("GET", path+"/download", key, nil, "")
	z, err := zip.NewReader(bytes.NewReader(archive.Body.Bytes()), int64(archive.Body.Len()))
	if err != nil || len(z.File) != 3 || z.File[0].Name != "images/one_01.png" {
		t.Fatal("batch ZIP", err)
	}
	for _, file := range z.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch file.Name {
		case "images/one_01.png":
			if !bytes.Equal(data, png) {
				t.Fatal("ZIP image changed")
			}
		case "manifest.json":
			var manifest struct {
				BatchID string `json:"batch_id"`
				Model   string
				Count   int `json:"item_count"`
				Success int `json:"success_count"`
				Failed  int `json:"fail_count"`
				Files   []map[string]any
			}
			if json.Unmarshal(data, &manifest) != nil || manifest.BatchID != id || manifest.Model != "team-image" || manifest.Count != 2 || manifest.Success != 1 || manifest.Failed != 1 || len(manifest.Files) != 1 || manifest.Files[0]["custom_id"] != "one_01" || manifest.Files[0]["filename"] != "images/one_01.png" || manifest.Files[0]["mime_type"] != "image/png" || manifest.Files[0]["image_index"] != float64(0) {
				t.Fatal("ZIP manifest", string(data))
			}
		case "errors.json":
			var failures []map[string]string
			if json.Unmarshal(data, &failures) != nil || len(failures) != 1 || failures[0]["custom_id"] != "one_02" || failures[0]["code"] != "PROVIDER_ITEM_FAILED" || strings.Contains(string(data), "private upstream error") {
				t.Fatal("ZIP errors", string(data))
			}
		default:
			t.Fatal("unexpected archive file", file.Name)
		}
	}
	if at := downloadedAt(); at == nil || !at.Equal(*firstDownload) {
		t.Fatal("download replaced first timestamp", at)
	}
	for _, value := range []string{"0", "-1", "201", "invalid"} {
		if w := call("GET", path+"/download?max_items="+value, key, nil, ""); w.Code != 400 {
			t.Fatal("invalid ZIP limit", value, w.Code)
		}
	}
	must("GET", path+"/download?max_items=1", key, nil, "")
	// Fail the status write after successful delivery; never append JSON to a file.
	execSQL := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execSQL("UPDATE batch_image_jobs SET downloaded_at=NULL WHERE batch_id=$1", id)
	execSQL("ALTER TABLE batch_image_jobs ADD CONSTRAINT test_batch_download_failure CHECK(batch_id<>'" + id + "' OR downloaded_at IS NULL) NOT VALID")
	defer a.DB.Exec("ALTER TABLE batch_image_jobs DROP CONSTRAINT IF EXISTS test_batch_download_failure")
	for _, suffix := range []string{"/items/one_01/content", "/download"} {
		w := must("GET", path+suffix, key, nil, "")
		if w.Header().Get("Content-Length") != fmt.Sprint(w.Body.Len()) || downloadedAt() != nil {
			t.Fatal("status failure corrupted file", suffix, w.Body.String())
		}
	}
	execSQL("ALTER TABLE batch_image_jobs DROP CONSTRAINT test_batch_download_failure")
	// Interrupted writes must neither mark downloads nor append an error document.
	for _, suffix := range []string{"/items/one_01/content", "/download"} {
		r := httptest.NewRequest("GET", path+suffix, nil)
		r.RemoteAddr = "192.0.2.189:1234"
		r.Header.Set("Authorization", "Bearer "+key)
		w := &batchInterruptedWriter{ResponseRecorder: httptest.NewRecorder()}
		b.Handler().ServeHTTP(w, r)
		if w.writes != 1 || downloadedAt() != nil {
			t.Fatal("interrupted download", suffix, w.writes)
		}
	}
	must("GET", path+"/download", key, nil, "")
	if downloadedAt() == nil {
		t.Fatal("ZIP did not mark downloaded")
	}
	// Distinct custom IDs can collapse to one sanitized name; the manifest must
	// still map each to its own ZIP entry without changing settled billing.
	storedJob, err := b.loadBatchJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	originalKeys := jobs[storedJob.ProviderJob].Keys
	jobs[storedJob.ProviderJob].Keys = []string{"a/b", `a\b`}
	allImages = true
	mu.Unlock()
	execSQL(`UPDATE batch_image_items SET custom_id=CASE custom_id WHEN 'one_01' THEN 'a/b' ELSE $2 END,status='success',image_count=1,mime_type='image/png',file_extension='png',error_code=NULL,error_message=NULL WHERE job_id=$1`, id, `a\b`)
	if w := call("GET", path+"/download?max_items=1", key, nil, ""); w.Code != 400 {
		t.Fatal("ZIP item cap", w.Code)
	}
	collision := must("GET", path+"/download", key, nil, "")
	cz, err := zip.NewReader(bytes.NewReader(collision.Body.Bytes()), int64(collision.Body.Len()))
	if err != nil || len(cz.File) != 4 || cz.File[0].Name == cz.File[1].Name {
		t.Fatal("ZIP filename collision", err)
	}
	reader, err := cz.File[2].Open()
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct{ Files []map[string]any }
	err = json.NewDecoder(reader).Decode(&manifest)
	reader.Close()
	if err != nil || len(manifest.Files) != 2 || manifest.Files[0]["custom_id"] != "a/b" || manifest.Files[1]["custom_id"] != `a\b` || manifest.Files[0]["filename"] != cz.File[0].Name || manifest.Files[1]["filename"] != cz.File[1].Name {
		t.Fatal("collision manifest", manifest, err)
	}
	reader, err = cz.File[3].Open()
	if err != nil {
		t.Fatal(err)
	}
	var noFailures []map[string]string
	err = json.NewDecoder(reader).Decode(&noFailures)
	reader.Close()
	if err != nil || noFailures == nil || len(noFailures) != 0 {
		t.Fatal("successful ZIP error list", noFailures, err)
	}
	execSQL(`UPDATE batch_image_items SET custom_id='one_01' WHERE job_id=$1 AND custom_id='a/b'`, id)
	execSQL(`UPDATE batch_image_items SET custom_id='one_02',status='failed',image_count=0,mime_type=NULL,file_extension=NULL,error_code='PROVIDER_ITEM_FAILED',error_message='provider could not generate this item' WHERE job_id=$1 AND custom_id=$2`, id, `a\b`)
	mu.Lock()
	jobs[storedJob.ProviderJob].Keys = originalKeys
	allImages = false
	mu.Unlock()
	balances("9.87500000", "0.00000000")
	usageCount(id, 1)
	items := must("GET", path+"/items?limit=1", key, nil, "")
	if !strings.Contains(items.Body.String(), `"has_more":true`) {
		t.Fatal("item pagination")
	}
	if w := call("GET", path+"/items?limit=0", key, nil, ""); w.Code != 400 {
		t.Fatal("invalid pagination", w.Code)
	}
	if w := must("GET", path+"/items?status=failed", key, nil, ""); !strings.Contains(w.Body.String(), `"source":"provider"`) || strings.Contains(w.Body.String(), "private upstream error") {
		t.Fatal("indexed provider error source", w.Body.String())
	}
	// A changed provider result must never silently become an empty successful ZIP.
	changedOutput = true
	for _, suffix := range []string{"/download", "/items/one_01/content"} {
		if w := call("GET", path+suffix, key, nil, ""); w.Code != 502 {
			t.Fatal("changed results accepted", suffix, w.Code)
		}
	}
	changedOutput = false
	if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	must("GET", path, key, nil, "")
	must("GET", path+"/download", key, nil, "")
	if submit("same") != id {
		t.Fatal("exhausted replay")
	}
	if w := call("POST", "/v1/images/batches", key, body, "no-balance"); w.Code != 402 {
		t.Fatal("accepted without balance", w.Code)
	}
	if _, err := a.DB.Exec("UPDATE users SET balance=10 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	manage("PUT", groupPath, admin, map[string]any{"image_price_1k": json.Number("0.1"), "rate_multiplier": 2})
	must("DELETE", path+"/outputs", key, nil, "")
	must("DELETE", path+"/outputs", key, nil, "")
	if w := call("GET", path+"/download", key, nil, ""); w.Code != 410 {
		t.Fatal("deleted outputs readable", w.Code)
	}
	must("DELETE", path, key, nil, "")
	if w := call("GET", path, key, nil, ""); w.Code != 404 {
		t.Fatal("deleted record visible", w.Code)
	}
	// Queued cancellation releases its own hold once, without calling the provider.
	queued := submit("queued")
	qpath := "/v1/images/batches/" + queued
	must("POST", qpath+"/cancel", key, nil, "")
	must("POST", qpath+"/cancel", key, nil, "")
	balances("10.00000000", "0.00000000")
	usageCount(queued, 0)
	// Lost create response: recover the provider identity from durable displayName,
	// never resubmit a billable job. Cancellation only refunds confirmed termination.
	unknownCreate = true
	pending := submit("unknown")
	ppath := "/v1/images/batches/" + pending
	if err := b.processBatchImage(ctx, pending); err == nil {
		t.Fatal("expected lost create outcome")
	}
	createsBefore := creates
	cycle(fresh(), pending, "running")
	if creates != createsBefore {
		t.Fatal("recovery resubmitted batch")
	}
	cancelFailure = true
	must("POST", ppath+"/cancel", key, nil, "")
	if err := fresh().processBatchImage(ctx, pending); err == nil {
		t.Fatal("expected cancel failure")
	}
	balances("9.70000000", "0.30000000")
	cancelFailure = false
	cycle(fresh(), pending, "cancelled")
	cycle(fresh(), pending, "cancelled")
	balances("10.00000000", "0.00000000")
	usageCount(pending, 0)
	// A credential change cannot route polling to a different upstream identity.
	rotated := submit("rotated")
	cycle(b, rotated, "running")
	accountPath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	manage("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-key"}})
	if err := fresh().processBatchImage(ctx, rotated); err == nil {
		t.Fatal("rotated provider accepted")
	}
	balances("9.70000000", "0.30000000")
	manage("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"api_key": "batch-provider-secret"}})
	setState(rotated, "JOB_STATE_FAILED")
	cycle(fresh(), rotated, "failed")
	cycle(fresh(), rotated, "failed")
	balances("10.00000000", "0.00000000")
	usageCount(rotated, 0)
	// Before the first provider call, lost credentials can safely release a hold;
	// an already submitted job above must instead retain its original identity.
	stale := submit("stale")
	manage("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-key"}})
	cycle(fresh(), stale, "cancelled")
	balances("10.00000000", "0.00000000")
	manage("PUT", accountPath, admin, map[string]any{"credentials": map[string]any{"api_key": "batch-provider-secret"}})
	// Permission revocation before creation releases the accepted hold.
	revoked := submit("revoked")
	manage("PUT", groupPath, admin, map[string]any{"allow_batch_image_generation": false})
	cycle(fresh(), revoked, "cancelled")
	balances("10.00000000", "0.00000000")
	manage("PUT", groupPath, admin, map[string]any{"allow_batch_image_generation": true})
	gone := submit("deleted-account")
	manage("DELETE", accountPath, admin, nil)
	cycle(fresh(), gone, "cancelled")
	balances("10.00000000", "0.00000000")
	if uploads != 3 || creates != 3 || cancels != 2 || deletes < 4 || downloads < 3 {
		t.Fatal("provider lifecycle", uploads, creates, cancels, deletes, downloads)
	}
	testBatchImageQueries(t, b, uid, kid, key, other, user)
}

type batchInterruptedWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *batchInterruptedWriter) Write(data []byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func testBatchImageQueries(t *testing.T, a *App, uid, kid int64, key, other, login string) {
	t.Helper()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	prefix := fmt.Sprintf("imgbatch_query_%d_", kid)
	defer func() {
		exec("DELETE FROM batch_image_items WHERE job_id LIKE $1", prefix+"%")
		exec("DELETE FROM batch_image_jobs WHERE batch_id LIKE $1", prefix+"%")
	}()
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO batch_image_jobs(batch_id,user_id,api_key_id,provider,model,status,item_count,task_name,created_at,downloaded_at,settled_at)
		SELECT $3||n,$1,$2,'gemini_api','team-image',CASE n WHEN 1 THEN 'created' WHEN 2 THEN 'uploading' WHEN 3 THEN 'submitted' WHEN 4 THEN 'indexing' ELSE 'completed' END,0,'Query fixture', $4::timestamptz + (n/2)*interval '1 second', CASE WHEN n%2=0 THEN $4::timestamptz END,$4 FROM generate_series(0,104) n`, uid, kid, prefix, at)
	// Hidden and foreign-key jobs must stay out of filtered pages, including has_more.
	exec(`INSERT INTO batch_image_jobs(batch_id,user_id,api_key_id,provider,model,status,item_count,task_name,created_at,user_deleted_at)
		VALUES($3||'hidden',$1,$2,'gemini_api','team-image','completed',0,'Query fixture',$4,$4),
		($3||'foreign',$1,(SELECT id FROM api_keys WHERE key=$5),'gemini_api','team-image','completed',0,'Query fixture',$4,NULL)`, uid, kid, prefix, at, other)
	job := prefix + "0"
	exec(`INSERT INTO batch_image_items(job_id,custom_id,status,error_code,error_message,provider_source_object)
		SELECT $1,'item_'||n,CASE WHEN n<=7 THEN 'failed' WHEN n=8 THEN 'pending' ELSE 'success' END,
		CASE n WHEN 0 THEN 'PRIVATE_PROVIDER_ERROR' WHEN 1 THEN 'EMPTY_IMAGE_OUTPUT' WHEN 2 THEN ' PROVIDER_ITEM_FAILED ' WHEN 3 THEN 'INDEX_OUTPUT_MISSING' WHEN 4 THEN 'INDEX_PARSE_FAILED' WHEN 5 THEN 'DUPLICATE_CUSTOM_ID_IN_OUTPUT' WHEN 6 THEN 'UNCLASSIFIED' ELSE NULL END,
		CASE n WHEN 0 THEN 'failed at files/private-output' WHEN 1 THEN 'api_key=private-provider-credential' WHEN 2 THEN repeat('猫',200) ELSE NULL END,
		CASE WHEN n=0 THEN 'files/private-output' WHEN n=7 THEN 'files/no-code' ELSE NULL END
		FROM generate_series(0,500) n`, job)
	type item struct {
		Error *struct{ Code, Message, Source string }
	}
	type page struct {
		Object  string
		Data    []map[string]json.RawMessage
		HasMore bool `json:"has_more"`
	}
	request := func(path, token string, want int) page {
		t.Helper()
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "192.0.2.189:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s: %d want %d: %s", path, w.Code, want, w.Body.String())
		}
		var out page
		if want == 200 {
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Object != "list" || out.Data == nil {
				t.Fatal("invalid batch list", w.Body.String())
			}
			for _, field := range []string{"provider_source_object", "provider_job_name", "user_id", "api_key_id", "account_id", "billed_amount", "request_hash", "private-provider-credential", "files/private-output"} {
				if strings.Contains(w.Body.String(), field) {
					t.Fatal("internal batch data exposed", field)
				}
			}
		}
		return out
	}
	jobs := "/v1/images/batches?task_name=" + url.QueryEscape("  qUeRy fiX  ")
	assertJobs := func(query string, want []string, more bool) {
		t.Helper()
		out := request(jobs+query, key, 200)
		ids := []string{}
		for _, row := range out.Data {
			ids = append(ids, credentialString(row, "id"))
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") || out.HasMore != more {
			t.Fatal("batch filter", query, ids, out.HasMore, want, more)
		}
	}
	financial := func() string {
		t.Helper()
		var snapshot string
		if err := a.DB.QueryRow(`SELECT jsonb_build_array(u.balance::text,u.frozen_balance::text,k.quota_used::text,(SELECT count(*) FROM usage_logs WHERE api_key_id=k.id),(SELECT count(*) FROM usage_billing_dedup WHERE api_key_id=k.id))::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1`, kid).Scan(&snapshot); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	before := financial()
	out := request(jobs, key, 200)
	if len(out.Data) != 20 || !out.HasMore || credentialString(out.Data[0], "id") != prefix+"104" || credentialString(out.Data[19], "id") != prefix+"85" {
		t.Fatal("job default pagination", out)
	}
	out = request(jobs+"&limit=100", key, 200)
	if len(out.Data) != 100 || !out.HasMore {
		t.Fatal("job maximum page", out)
	}
	assertJobs("&limit=5&cursor=100", []string{prefix + "4", prefix + "3", prefix + "2", prefix + "1", prefix + "0"}, false)
	assertJobs("&limit=%205%20&cursor=%20100%20", []string{prefix + "4", prefix + "3", prefix + "2", prefix + "1", prefix + "0"}, false)
	assertJobs("&status=%20queued%20", []string{prefix + "3", prefix + "2", prefix + "1"}, false)
	assertJobs("&status=processing_results", []string{prefix + "4"}, false)
	assertJobs("&status=unknown", nil, false)
	for _, value := range []string{"true", "1", "yes", "downloaded", " YES "} {
		assertJobs("&status=queued&downloaded="+url.QueryEscape(value), []string{prefix + "2"}, false)
	}
	for _, value := range []string{"false", "0", "no", "not_downloaded", " No "} {
		assertJobs("&status=queued&downloaded="+url.QueryEscape(value), []string{prefix + "3", prefix + "1"}, false)
	}
	for _, value := range []string{"", "all", " ALL "} {
		assertJobs("&status=queued&downloaded="+url.QueryEscape(value), []string{prefix + "3", prefix + "2", prefix + "1"}, false)
	}
	for _, from := range []string{fmt.Sprint(at.Unix()), at.Format(time.RFC3339), "2026-09-01", "2026-09-01T09:00:00+09:00"} {
		for _, to := range []string{fmt.Sprint(at.Add(time.Second).Unix()), "2026-09-01T00:00:01Z"} {
			assertJobs("&from="+url.QueryEscape(from)+"&to="+url.QueryEscape(to), []string{prefix + "1", prefix + "0"}, false)
		}
	}
	assertJobs("&from=2026-09-01&to=2026-09-01", nil, false)
	assertJobs("&to=2026-09-01", nil, false)
	assertJobs("&from=2026-09-01T00:00:00.000001Z&to=2026-09-01T00:00:02Z", []string{prefix + "3", prefix + "2"}, false)
	assertJobs("&from=2026-09-01&to=2026-09-01T00:00:01Z&downloaded=true&status=completed", []string{job}, false)
	assertJobs("&cursor=1000000", nil, false)
	for _, query := range []string{"downloaded=maybe", "from=bad", "to=2026-02-30", "from=0", "from=-1", "from=9223372036854775807", "from=253402300800", "from=2026-09-02&to=2026-09-01", "limit=0", "limit=101", "limit=1.5", "cursor=-1", "cursor=1000001", "cursor=no", "task_name=%00", "status=%FF"} {
		request("/v1/images/batches?"+query, key, 400)
	}
	itemsPath := "/v1/images/batches/" + job + "/items"
	out = request(itemsPath, key, 200)
	if len(out.Data) != 100 || !out.HasMore || credentialString(out.Data[0], "custom_id") != "item_0" {
		t.Fatal("item default pagination", out)
	}
	for i, source := range []string{"provider", "provider", "provider", "system", "system", "system", "", ""} {
		var failure item
		encoded, _ := json.Marshal(out.Data[i])
		if json.Unmarshal(encoded, &failure) != nil || failure.Error == nil || failure.Error.Source != source {
			t.Fatal("item error source", i, string(encoded))
		}
		if len(failure.Error.Message) > 500 || !utf8.ValidString(failure.Error.Message) {
			t.Fatal("unsafe error message", i)
		}
		if i == 0 && failure.Error.Message != "upstream provider operation failed" || i == 1 && !strings.Contains(failure.Error.Message, "[redacted]") {
			t.Fatal("error message redaction", i)
		}
	}
	if string(out.Data[8]["error"]) != "null" || string(out.Data[9]["error"]) != "null" {
		t.Fatal("pending or successful item exposed failure")
	}
	out = request(itemsPath+"?limit=500", key, 200)
	if len(out.Data) != 500 || !out.HasMore {
		t.Fatal("item maximum page", len(out.Data), out.HasMore)
	}
	out = request(itemsPath+"?limit=500&cursor=500", key, 200)
	if len(out.Data) != 1 || out.HasMore || credentialString(out.Data[0], "custom_id") != "item_500" {
		t.Fatal("item last page", out)
	}
	for status, count := range map[string]int{"success": 492, "succeeded": 492, " pending ": 1, "failed": 8, "all": 500} {
		out = request(itemsPath+"?limit=500&status="+url.QueryEscape(status), key, 200)
		if len(out.Data) != count || out.HasMore != (status == "all") {
			t.Fatal("item status filter", status, len(out.Data), out.HasMore)
		}
	}
	out = request(itemsPath+"?status=failed&cursor=8", key, 200)
	if len(out.Data) != 0 || out.HasMore {
		t.Fatal("empty item page", out)
	}
	for _, query := range []string{"limit=501", "limit=0", "status=unknown", "status=SUCCESS", "cursor=-1"} {
		request(itemsPath+"?"+query, key, 400)
	}
	request(itemsPath, other, 404)
	request(itemsPath, login, 401)
	request(jobs, login, 401)
	out = request(jobs, other, 200)
	if len(out.Data) != 1 || out.HasMore || credentialString(out.Data[0], "id") != prefix+"foreign" {
		t.Fatal("cross-key job list", out)
	}
	if after := financial(); after != before {
		t.Fatal("query changed billing", before, after)
	}
}
