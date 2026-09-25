package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestSeedanceContracts(t *testing.T) {
	for _, base := range []string{"https://example.test", "https://example.test/prefix", "https://example.test/api/v3", "https://example.test/v3"} {
		u := &upstreamAccount{Credentials: map[string]json.RawMessage{"base_url": json.RawMessage(fmt.Sprintf("%q", base))}}
		target, err := upstreamURL(base, seedancePath(u, "original"))
		root := base
		if !strings.HasSuffix(root, "/v3") {
			root += "/api/v3"
		}
		if err != nil || target != root+"/contents/generations/tasks/original" {
			t.Fatal(target, err)
		}
	}
	for _, raw := range []string{`{}`, `null`, `{"model":"a","content":[]}`, `{"model":"a","content":[{"type":"text","text":"hi"}],"Model":"b"}`, `{"model":"a","content":[{"type":"draft_task","draft_task":{"id":"upstream-foreign"}}]}`, `{"model":"a","content":[{"type":"text","text":"hi"}],"callback_url":"https://example.test"}`} {
		if _, _, err := parseSeedance([]byte(raw)); err == nil {
			t.Fatal("accepted invalid video", raw)
		}
	}
	for _, caps := range []string{`["seedance"]`, ` {"seedance":true} `} {
		u := &upstreamAccount{Platform: "openai", Type: "apikey", Credentials: map[string]json.RawMessage{"base_url": json.RawMessage(`"https://example.test"`), "openai_capabilities": json.RawMessage(caps)}}
		if !u.supportsSeedance() || u.allowsOpenAIProtocol("responses") || u.allowsOpenAIProtocol("embeddings") {
			t.Fatal("capability boundary")
		}
	}
	task := &videoTask{ID: "task_public", UpstreamID: "native"}
	for _, raw := range []string{`{"id":"other","status":"queued"}`, `{"id":"native","status":"succeeded"}`, `{"id":"native","status":"succeeded","content":{"video_url":"https://example.test/v.mp4"},"usage":{"completion_tokens":-1}}`, `{"id":"native","status":"succeeded","content":{"video_url":"https://example.test/v.mp4"},"usage":{"completion_tokens":2147483648}}`, `{"id":"native","status":"surprise"}`} {
		if _, _, _, _, err := seedanceStatus([]byte(raw), task); err == nil {
			t.Fatal("invalid upstream status", raw)
		}
	}
	for _, content := range []string{`null`, `{}`, `[]`, `{"video_url":null}`, `{"video_url":42}`, `{"video_url":""}`, `{"video_url":"file:///video.mp4"}`, `{"video_url":"https://secret@example.test/video.mp4"}`} {
		raw := `{"id":"native","status":"succeeded","content":` + content + `,"usage":{"completion_tokens":11}}`
		if _, _, _, _, err := seedanceStatus([]byte(raw), task); err == nil {
			t.Fatal("accepted completion without valid video output", content)
		}
	}
	for _, draft := range []bool{false, true} {
		raw := fmt.Sprintf(`{"id":"native","status":"succeeded","draft":%t,"content":{"video_url":"https://example.test/video.mp4"},"usage":{"completion_tokens":11}}`, draft)
		_, status, tokens, _, err := seedanceStatus([]byte(raw), task)
		if err != nil || status != "succeeded" || tokens != 11 {
			t.Fatal("valid video or draft rejected", draft, status, tokens, err)
		}
	}
	result, status, _, _, err := seedanceStatus([]byte(`{"id":"native","status":"failed","error":{"message":"upstream-secret"}}`), task)
	if err != nil || status != "failed" || bytes.Contains(result, []byte("upstream-secret")) {
		t.Fatal("provider error redaction", err)
	}
}

func testSeedance(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.141:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Seedance", "platform": "openai", "rate_multiplier": 2}))
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "seedance@example.test", "password": "seedance-password", "balance": 100, "concurrency": 5}))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "seedance@example.test", "password": "seedance-password"})["access_token"].(string)
	keyObj := manage("POST", "/api/v1/keys", token, map[string]any{"name": "video", "group_id": gid, "quota": 100})
	key, kid := keyObj["key"].(string), id(keyObj)
	other := manage("POST", "/api/v1/keys", token, map[string]any{"name": "other-video", "group_id": gid})["key"].(string)
	var mu sync.Mutex
	states := map[string]string{}
	counts := map[string]int{}
	drafts := map[string]bool{}
	nextStatus := "queued"
	creates, deletes := 0, 0
	nextFailure := false
	missingOutput := false
	lastModel, lastDraft := "", ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer seedance-upstream-secret" || r.Header.Get("X-Api-Key") != "" {
			t.Error("video credential isolation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Video-Trace", "seedance-"+r.Method)
		const root = "/api/v3/contents/generations/tasks"
		if r.URL.Path == root && r.Method == "POST" {
			creates++
			if nextFailure {
				nextFailure = false
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":"private-provider-error"}`)
				return
			}
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			lastModel = credentialString(body, "model")
			var content []struct {
				Draft struct{ ID string } `json:"draft_task"`
			}
			_ = json.Unmarshal(body["content"], &content)
			lastDraft = ""
			for _, c := range content {
				if c.Draft.ID != "" {
					lastDraft = c.Draft.ID
				}
			}
			native := fmt.Sprintf("native_%d", creates)
			states[native] = nextStatus
			counts[native] = 11
			var draft bool
			_ = json.Unmarshal(body["draft"], &draft)
			drafts[native] = draft
			_ = json.NewEncoder(w).Encode(map[string]string{"id": native})
			return
		}
		native := strings.TrimPrefix(r.URL.Path, root+"/")
		state, exists := states[native]
		if !exists {
			w.WriteHeader(404)
			return
		}
		if r.Method == "DELETE" {
			deletes++
			if state == "running" {
				w.WriteHeader(409)
				return
			}
			if state == "queued" {
				states[native] = "cancelled"
			} else {
				delete(states, native)
			}
			fmt.Fprint(w, `{}`)
			return
		}
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		result := map[string]any{"id": native, "status": state, "model": "provider-video", "draft": drafts[native]}
		if state == "succeeded" {
			if !missingOutput {
				result["content"] = map[string]string{"video_url": "https://example.test/video.mp4?signature=private"}
			}
			result["usage"] = map[string]int{"completion_tokens": counts[native]}
		}
		if state == "failed" {
			result["error"] = map[string]string{"message": "private-provider-error"}
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer provider.Close()
	account := manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "seedance", "platform": "openai", "type": "apikey", "concurrency": 3, "group_ids": []int64{gid}, "rate_multiplier": 3, "credentials": map[string]any{"api_key": "seedance-upstream-secret", "base_url": provider.URL, "model_mapping": map[string]string{"team-video": "provider-video"}, "openai_capabilities": []string{"seedance"}}, "extra": map[string]any{"quota_limit": 100, upstreamRequestIDHeaderKey: "X-Video-Trace"}})
	aid := id(account)
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	prices := []any{map[string]any{"platform": "openai", "models": []string{"team-video"}, "billing_mode": "token", "output_price": "0.001"}}
	channel := manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "seedance-price", "group_ids": []int64{gid}, "model_pricing": prices, "billing_model_source": "requested"})
	body := map[string]any{"model": "team-video", "content": []any{map[string]any{"type": "text", "text": "private prompt"}}, "duration": 5}
	create := func(prefix, idem string) string {
		t.Helper()
		w := call("POST", prefix+"/contents/generations/tasks", key, body, idem)
		var result struct{ ID string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !strings.HasPrefix(result.ID, "task_") {
			t.Fatalf("video create: %d %s", w.Code, w.Body.String())
		}
		return result.ID
	}
	path := func(task string) string { return "/v1/contents/generations/tasks/" + task }
	first := create("/api/v3", "video-one")
	stored, err := a.loadVideoTask(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := a.Redis.Get(context.Background(), videoTaskKey(first)).Result()
	if strings.Contains(envelope, "private prompt") || strings.Contains(envelope, "seedance-upstream-secret") || stored.Selection.Account.Credentials != nil {
		t.Fatal("video persisted credentials")
	}
	mu.Lock()
	if creates != 1 || lastModel != "provider-video" {
		t.Fatal("video model rewrite", creates, lastModel)
	}
	mu.Unlock()
	replay := call("POST", "/v3/contents/generations/tasks", key, body, "video-one")
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("video replay", replay.Code)
	}
	changed := map[string]any{"model": "team-video", "content": []any{map[string]any{"type": "text", "text": "different"}}}
	if w := call("POST", "/v1/contents/generations/tasks", key, changed, "video-one"); w.Code != 409 {
		t.Fatal("video conflict", w.Code)
	}
	for _, credential := range []string{other, token} {
		w := call("GET", path(first), credential, nil, "")
		if w.Code != 404 && w.Code != 401 {
			t.Fatal("video ownership", w.Code)
		}
	}
	if w := call("GET", path(stored.UpstreamID), key, nil, ""); w.Code != 404 {
		t.Fatal("provider ID accepted")
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE users SET balance=0 WHERE id=$1", uid)
	if w := call("GET", path(first), key, nil, ""); w.Code != 200 {
		t.Fatal("zero balance lookup", w.Code, w.Body.String())
	}
	if w := call("POST", "/v1/contents/generations/tasks", key, body, "empty"); w.Code != 402 {
		t.Fatal("video balance gate", w.Code)
	}
	exec("UPDATE users SET balance=100 WHERE id=$1", uid)
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"extra": map[string]any{upstreamRequestIDHeaderKey: "X-Changed"}})
	// Change live prices after acceptance: reconciliation must use the old snapshot.
	manage("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", id(channel)), admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"team-video"}, "billing_mode": "token", "output_price": "0.5"}}})
	mu.Lock()
	states[stored.UpstreamID] = "succeeded"
	missingOutput = true
	mu.Unlock()
	if w := call("GET", path(first), key, nil, ""); w.Code != 502 {
		t.Fatal("completion without output accepted", w.Code, w.Body.String())
	}
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	if err = fresh.runVideoTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := fresh.loadVideoTask(context.Background(), first)
	if err != nil || pending.Stage != "pending" || pending.Receipt != nil {
		t.Fatal("invalid completion did not remain recoverable", err)
	}
	var unsettled int
	var unchangedBalance, unchangedQuota string
	if err = a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", first).Scan(&unsettled); err != nil || unsettled != 0 {
		t.Fatal("invalid completion billed", unsettled, err)
	}
	if err = a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1", kid).Scan(&unchangedBalance, &unchangedQuota); err != nil || unchangedBalance != "100.00000000" || unchangedQuota != "0.00000000" {
		t.Fatal("invalid completion changed funds", unchangedBalance, unchangedQuota, err)
	}
	exec("ALTER TABLE usage_logs ADD CONSTRAINT test_video_failure CHECK(model<>'team-video') NOT VALID")
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_video_failure")
	mu.Lock()
	missingOutput = false
	mu.Unlock()
	w := call("GET", path(first), key, nil, "")
	if w.Code != 503 && w.Code != 409 {
		t.Fatal("failed settlement published", w.Code, w.Body.String())
	}
	exec("ALTER TABLE usage_logs DROP CONSTRAINT test_video_failure")
	// New application state and repeated recovery use the persisted receipt only.
	for i := 0; i < 2; i++ {
		if err := fresh.runVideoTasks(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	assertUsageRequestID(t, a, first, "seedance-POST")
	var n int
	var actual, balance, used, accountUsed string
	if err = a.DB.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE request_id=$1", first).Scan(&n, &actual); err != nil || n != 1 || actual != "0.0220000000" {
		t.Fatal("video receipt", n, actual, err)
	}
	if err = a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text,a.extra->>'quota_used' FROM users u JOIN api_keys k ON k.user_id=u.id CROSS JOIN accounts a WHERE u.id=$1 AND k.id=$2 AND a.id=$3", uid, kid, aid).Scan(&balance, &used, &accountUsed); err != nil || balance != "99.97800000" || used != "0.02200000" || accountUsed != "0.03300000" {
		t.Fatal("video accounting", balance, used, accountUsed, err)
	}
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		w := call("GET", prefix+"/contents/generations/tasks/"+first, key, nil, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "video_url") || strings.Contains(w.Body.String(), stored.UpstreamID) {
			t.Fatal("video result alias", w.Code, w.Body.String())
		}
	}
	// Confirm queued cancellation; failed/cancelled jobs never add usage.
	queued := create("", "cancel-one")
	if w := call("DELETE", path(queued), key, nil, ""); w.Code != 200 {
		t.Fatal("video cancel", w.Code, w.Body.String())
	}
	if w := call("GET", path(queued), key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "cancelled") {
		t.Fatal("cancel confirmation", w.Code, w.Body.String())
	}
	if w := call("DELETE", path(first), key, nil, ""); w.Code != 200 {
		t.Fatal("delete settled", w.Code)
	}
	for _, prefix := range []string{"/api/v3", "/v3", "/v1", ""} {
		if w := call("DELETE", prefix+"/contents/generations/tasks/"+first, key, nil, ""); w.Code != 200 {
			t.Fatal("repeat delete alias", prefix, w.Code)
		}
	}
	if w := call("GET", path(first), key, nil, ""); w.Code != 404 {
		t.Fatal("deleted task exposed", w.Code)
	}
	// Pending task remains bound to the original Key; rotation is not a fallback.
	rotated := create("/v1", "rotate")
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	if w := call("GET", path(rotated), key, nil, ""); w.Code != 409 {
		t.Fatal("rotated source accepted", w.Code, w.Body.String())
	}
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": "seedance-upstream-secret"}})
	if w := call("DELETE", path(rotated), key, nil, ""); w.Code != 200 {
		t.Fatal("restored source", w.Code)
	}
	// Unknown submission outcome stays processing; retry cannot create twice.
	mu.Lock()
	nextFailure = true
	before := creates
	mu.Unlock()
	if w := call("POST", "/v1/contents/generations/tasks", key, body, "ambiguous"); w.Code != 503 {
		t.Fatal("ambiguous create", w.Code)
	}
	if w := call("POST", "/v1/contents/generations/tasks", key, body, "ambiguous"); w.Code != 409 {
		t.Fatal("ambiguous replay", w.Code)
	}
	mu.Lock()
	if creates != before+1 {
		t.Fatal("ambiguous create duplicated")
	}
	mu.Unlock()
	manage("POST", apath+"/recover-state", admin, map[string]any{})
	// Draft references must be owned, completed and bound to this same source.
	body["draft"] = true
	draft := create("/v1", "draft")
	draftTask, _ := a.loadVideoTask(context.Background(), draft)
	mu.Lock()
	states[draftTask.UpstreamID] = "succeeded"
	mu.Unlock()
	if w := call("GET", path(draft), key, nil, ""); w.Code != 200 {
		t.Fatal("draft completion", w.Code, w.Body.String())
	}
	body = map[string]any{"model": "team-video", "content": []any{map[string]any{"type": "draft_task", "draft_task": map[string]string{"id": draft}}}}
	child := create("/v1", "from-draft")
	mu.Lock()
	if lastDraft != draftTask.UpstreamID {
		t.Fatal("draft ID not translated", lastDraft)
	}
	mu.Unlock()
	if w := call("POST", "/v1/contents/generations/tasks", other, body, "foreign-draft"); w.Code != 404 {
		t.Fatal("foreign draft accepted", w.Code)
	}
	if w := call("DELETE", path(child), key, nil, ""); w.Code != 200 {
		t.Fatal("child cancellation", w.Code)
	}
	// Explicit capability removal prevents future dispatch but does not change schema.
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"openai_capabilities": map[string]bool{"seedance": false}}})
	if w := call("POST", "/v1/contents/generations/tasks", key, body, "disabled-capability"); w.Code != 503 {
		t.Fatal("capability gate", w.Code, w.Body.String())
	}
	// No request body leaks into the encrypted task envelopes or billing rows.
	if err = a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1 AND to_jsonb(usage_logs)::text LIKE '%private prompt%'", kid).Scan(&n); err != nil || n != 0 {
		t.Fatal("video prompt persisted", err)
	}
}
