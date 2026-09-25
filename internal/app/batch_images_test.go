package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestBatchImageInputAndEnvelope(t *testing.T) {
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
	unknownCreate, badOutput, changedOutput, cancelFailure := false, false, false, false
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
				if i > 0 || changedOutput {
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
	archive := must("GET", path+"/download", key, nil, "")
	z, err := zip.NewReader(bytes.NewReader(archive.Body.Bytes()), int64(archive.Body.Len()))
	if err != nil || len(z.File) != 1 || z.File[0].Name != "000001_01.png" {
		t.Fatal("batch ZIP", err)
	}
	items := must("GET", path+"/items?limit=1", key, nil, "")
	if !strings.Contains(items.Body.String(), `"has_more":true`) {
		t.Fatal("item pagination")
	}
	if w := call("GET", path+"/items?limit=0", key, nil, ""); w.Code != 400 {
		t.Fatal("invalid pagination", w.Code)
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
}
