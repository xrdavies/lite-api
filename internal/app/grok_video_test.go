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
	"time"
)

func TestGrokVideoContracts(t *testing.T) {
	app := &App{mux: http.NewServeMux()}
	app.videoRoutes() // All legacy aliases must coexist in the standard router.
	for _, raw := range []string{`null`, `{}`, `{"model":"m","prompt":"p","duration":0}`, `{"model":"m","prompt":"p","duration":16}`, `{"model":"m","prompt":"p","duration":2.5}`, `{"model":"m","prompt":"p","resolution":"4k"}`, `{"model":"m","prompt":"p","Model":"other"}`, `{"model":"m","prompt":"p","callback_url":"https://example.test"}`} {
		if _, _, _, _, err := parseGrokVideo([]byte(raw), "generations"); err == nil {
			t.Fatal("accepted invalid video", raw)
		}
	}
	if _, _, _, _, err := parseGrokVideo([]byte(`{"model":"m","prompt":"p"}`), "edits"); err == nil {
		t.Fatal("missing edit video")
	}
	task := &videoTask{ID: "task_public", UpstreamID: "native", Seconds: 8, Resolution: "720p"}
	for _, raw := range []string{`{}`, `{"status":"done"}`, `{"request_id":"foreign","status":"pending"}`, `{"status":"done","video":{"url":"https://example.test","duration":-1}}`} {
		if _, _, _, _, err := grokVideoStatus([]byte(raw), task); err == nil {
			t.Fatal("invalid result", raw)
		}
	}
	out, status, u, _, err := grokVideoStatus([]byte(`{"status":"done","video":{"url":"https://example.test/v.mp4","duration":8.7},"secret":"redact"}`), task)
	if err != nil || status != "succeeded" || u.VideoSeconds != 8 || bytes.Contains(out, []byte("redact")) {
		t.Fatal(status, u, err)
	}
	s := &gatewaySelection{Account: &upstreamAccount{Platform: "grok"}}
	g := gatewayGroup{Rate: "2"}
	u = priceUsage{VideoCount: 1, VideoSeconds: 5, VideoResolution: "720p"}
	check := func(want string) {
		t.Helper()
		c, err := s.videoCost(g, "grok-imagine-video-1.5", u, time.Now())
		if err != nil || c.Actual != want {
			t.Fatal(c, err, want)
		}
	}
	check("1.4000000000")
	s.Pricing = []modelPrice{{Platform: "grok", Models: []string{"*"}, BillingMode: "per_request", PerRequest: number("0.3")}}
	check("0.6000000000") // channel per_request does not multiply seconds
	g.Video720 = number("0.1")
	check("1.0000000000")
	independent := true
	g.IndependentVideo = &independent
	g.VideoRate = number("0.5")
	check("0.2500000000")
	models := map[string]map[string]json.Number{"grok-imagine-video-1.5": {"720p": "0"}}
	g.VideoModels = &models
	check("0.0000000000")
	s.GroupPricing = []modelPrice{{Platform: "openai", Models: []string{"*"}, BillingMode: "video", PerRequest: number("0.2")}}
	check("0.5000000000") // group price matching ignores platform metadata
	s.Restrict = true
	s.Pricing = nil
	if _, err := s.videoCost(g, "grok-imagine-video-1.5", u, time.Now()); err == nil {
		t.Fatal("group price bypassed channel restriction")
	}
}

func testGrokVideo(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.142:1234"
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
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Video", "platform": "grok", "allow_image_generation": true, "rate_multiplier": 2, "video_price_720p": "0.01", "video_rate_independent": true, "video_rate_multiplier": "0.5"}))
	gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "grok-video@example.test", "password": "video-password", "balance": 100, "concurrency": 5}))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "grok-video@example.test", "password": "video-password"})["access_token"].(string)
	kobj := manage("POST", "/api/v1/keys", token, map[string]any{"name": "video", "group_id": gid, "quota": 100})
	key, kid := kobj["key"].(string), id(kobj)
	other := manage("POST", "/api/v1/keys", token, map[string]any{"name": "other-video", "group_id": gid})["key"].(string)
	var mu sync.Mutex
	states := map[string]string{}
	operations := map[string]int{}
	creates, downloads := 0, 0
	ambiguous := false
	var providerURL string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/download" {
			downloads++
			if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
				t.Error("download credential leak")
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte{0, 1, 2, 3, 4})
			return
		}
		if r.Header.Get("Authorization") != "Bearer video-provider-secret" {
			t.Error("upstream credential isolation")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			creates++
			operations[r.URL.Path]++
			if ambiguous {
				ambiguous = false
				w.WriteHeader(503)
				fmt.Fprint(w, `{"error":"private-provider-error"}`)
				return
			}
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if credentialString(body, "model") != "grok-imagine-video-1.5" {
				t.Error("video model mapping")
			}
			native := fmt.Sprintf("native_%d", creates)
			states[native] = "pending"
			json.NewEncoder(w).Encode(map[string]string{"request_id": native})
			return
		}
		native := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
		status, ok := states[native]
		if !ok {
			w.WriteHeader(404)
			return
		}
		out := map[string]any{"status": status, "model": "grok-imagine-video-1.5"}
		if status == "done" {
			out["video"] = map[string]any{"url": providerURL + "/download", "duration": 6, "respect_moderation": true}
		}
		if status == "failed" {
			out["error"] = map[string]string{"message": "private-provider-error"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer provider.Close()
	providerURL = provider.URL
	aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "video", "platform": "grok", "type": "apikey", "group_ids": []int64{gid}, "concurrency": 3, "rate_multiplier": 3, "credentials": map[string]any{"api_key": "video-provider-secret", "base_url": provider.URL, "model_mapping": map[string]string{"team-video": "grok-imagine-video-1.5"}}, "extra": map[string]any{"quota_limit": 100}}))
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "team-video", "prompt": "private video prompt", "duration": 5, "resolution": "720p"}
	create := func(path, k, idem string) string {
		t.Helper()
		w := call("POST", path, k, body, idem)
		var out struct {
			RequestID string `json:"request_id"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !strings.HasPrefix(out.RequestID, "task_") {
			t.Fatalf("create video %d %s", w.Code, w.Body.String())
		}
		return out.RequestID
	}
	lookup := func(task, k string) *httptest.ResponseRecorder { return call("GET", "/v1/videos/"+task, k, nil, "") }
	first := create("/videos", key, "grok-video-one")
	task, err := a.loadVideoTask(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/v1/videos/generations", key, body, "grok-video-one"); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("video alias replay", w.Code, w.Body.String())
	}
	if w := lookup(first, other); w.Code != 404 {
		t.Fatal("foreign video", w.Code)
	}
	if w := lookup(first, token); w.Code != 401 {
		t.Fatal("login token accepted", w.Code)
	}
	if w := call("GET", "/v1/contents/generations/tasks/"+first, key, nil, ""); w.Code != 403 {
		t.Fatal("cross protocol task", w.Code)
	}
	if w := call("GET", "/videos/"+first+"/content", key, nil, ""); w.Code != 409 {
		t.Fatal("pending content", w.Code)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE users SET balance=0 WHERE id=$1", uid)
	if w := lookup(first, key); w.Code != 200 {
		t.Fatal("zero balance lookup", w.Code)
	}
	if w := call("POST", "/videos", key, body, "empty"); w.Code != 402 {
		t.Fatal("zero balance create", w.Code)
	}
	exec("UPDATE users SET balance=100 WHERE id=$1", uid)
	manage("PUT", gpath, admin, map[string]any{"video_price_720p": "9", "video_rate_multiplier": "7"})
	exec("ALTER TABLE usage_logs ADD CONSTRAINT test_grok_video_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID")
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_grok_video_failure")
	mu.Lock()
	states[task.UpstreamID] = "done"
	mu.Unlock()
	if w := lookup(first, key); w.Code != 503 && w.Code != 409 {
		t.Fatal("unsettled content exposed", w.Code, w.Body.String())
	}
	exec("ALTER TABLE usage_logs DROP CONSTRAINT test_grok_video_failure")
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	for i := 0; i < 2; i++ {
		if err := fresh.runVideoTasks(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var count, seconds int
	var actual, mode, resolution, balance, used, accountUsed string
	if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text,min(billing_mode),min(video_resolution),min(video_duration_seconds) FROM usage_logs WHERE request_id=$1", first).Scan(&count, &actual, &mode, &resolution, &seconds); err != nil || count != 1 || actual != "0.0300000000" || mode != "video" || resolution != "720p" || seconds != 6 {
		t.Fatal("video receipt", count, actual, mode, resolution, seconds, err)
	}
	if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text,a.extra->>'quota_used' FROM users u JOIN api_keys k ON k.user_id=u.id CROSS JOIN accounts a WHERE u.id=$1 AND k.id=$2 AND a.id=$3", uid, kid, aid).Scan(&balance, &used, &accountUsed); err != nil || balance != "99.97000000" || used != "0.03000000" || accountUsed != "0.18000000" {
		t.Fatal("video balances", balance, used, accountUsed, err)
	}
	var usageID int64
	if err := a.DB.QueryRow("SELECT id FROM usage_logs WHERE request_id=$1", first).Scan(&usageID); err != nil {
		t.Fatal(err)
	}
	visible := manage("GET", fmt.Sprintf("/api/v1/usage/%d", usageID), token, nil)
	if visible["video_count"] != float64(1) || visible["video_duration_seconds"] != float64(6) || visible["video_resolution"] != "720p" {
		t.Fatal("video usage fields not visible", visible)
	}
	for _, prefix := range []string{"/v1", ""} {
		for _, op := range []string{"", "/generations", "/edits", "/extensions"} {
			path := prefix + "/videos" + op + "/" + first
			if w := call("GET", path, key, nil, ""); w.Code != 200 || strings.Contains(w.Body.String(), task.UpstreamID) {
				t.Fatal("status alias", path, w.Code)
			}
			if w := call("GET", path+"/content", key, nil, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte{0, 1, 2, 3, 4}) {
				t.Fatal("content alias", path, w.Code, w.Body.String())
			}
		}
	}
	// The authenticated task cannot be used to send credentials or requests to
	// a provider-supplied internal metadata URL.
	settled, err := a.loadVideoTask(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	originalResult := settled.Result
	settled.Result = json.RawMessage(`{"status":"done","video":{"url":"http://169.254.169.254/latest/meta-data"}}`)
	if err = a.saveVideoTask(context.Background(), settled); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/videos/"+first+"/content", key, nil, ""); w.Code != 502 {
		t.Fatal("unsafe video URL allowed", w.Code)
	}
	settled.Result = originalResult
	if err = a.saveVideoTask(context.Background(), settled); err != nil {
		t.Fatal(err)
	}
	manage("PUT", gpath, admin, map[string]any{"video_price_720p": "0.01", "video_rate_multiplier": "0.5"})
	// Generation, editing and extension share ownership and durable settlement.
	body["video"] = map[string]string{"url": "https://example.test/input.mp4"}
	for _, op := range []string{"edits", "extensions"} {
		job := create("/v1/videos/"+op, key, op)
		v, _ := a.loadVideoTask(context.Background(), job)
		mu.Lock()
		states[v.UpstreamID] = "failed"
		mu.Unlock()
		if w := lookup(job, key); w.Code != 200 || strings.Contains(w.Body.String(), "private-provider-error") {
			t.Fatal("video failure", w.Code, w.Body.String())
		}
		job = create("/v1/videos/"+op, key, op+"-done")
		v, err := a.loadVideoTask(context.Background(), job)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		states[v.UpstreamID] = "done"
		mu.Unlock()
		for i := 0; i < 2; i++ {
			if w := lookup(job, key); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"done"`) {
				t.Fatal("video operation completion", op, w.Code, w.Body.String())
			}
		}
		if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text,min(video_duration_seconds) FROM usage_logs WHERE request_id=$1", job).Scan(&count, &actual, &seconds); err != nil || count != 1 || actual != "0.0300000000" || seconds != 6 {
			t.Fatal("video operation settlement", op, count, actual, seconds, err)
		}
		if w := call("GET", "/videos/"+op+"/"+job+"/content", key, nil, ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), []byte{0, 1, 2, 3, 4}) {
			t.Fatal("video operation download", op, w.Code, w.Body.String())
		}
	}
	manage("PUT", gpath, admin, map[string]any{"allow_image_generation": false})
	if w := call("POST", "/videos", key, body, "disabled"); w.Code != 403 {
		t.Fatal("video group gate", w.Code)
	}
	manage("PUT", gpath, admin, map[string]any{"allow_image_generation": true})
	// Composite creation and lookup stay on the original upstream account.
	cgid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite video", "platform": "composite", "allow_image_generation": true, "video_price_720p": "0.01"}))
	manage("PUT", apath, admin, map[string]any{"group_ids": []int64{gid, cgid}})
	manage("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", cgid), admin, map[string]any{"public_model": "team-video", "match_type": "exact", "target_platform": "grok", "upstream_model": "team-video", "endpoint": "any", "enabled": true})
	ckey := manage("POST", "/api/v1/keys", token, map[string]any{"name": "composite-video", "group_id": cgid})["key"].(string)
	cjob := create("/videos/extensions", ckey, "composite")
	ctask, _ := a.loadVideoTask(context.Background(), cjob)
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	if w := lookup(cjob, ckey); w.Code != 409 {
		t.Fatal("rotated source", w.Code)
	}
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": "video-provider-secret"}})
	mu.Lock()
	states[ctask.UpstreamID] = "done"
	mu.Unlock()
	if w := lookup(cjob, ckey); w.Code != 200 {
		t.Fatal("composite completion", w.Code, w.Body.String())
	}
	mu.Lock()
	ambiguous = true
	before := creates
	mu.Unlock()
	if w := call("POST", "/videos", key, body, "ambiguous-video"); w.Code != 503 {
		t.Fatal("unknown submission", w.Code)
	}
	if w := call("POST", "/v1/videos", key, body, "ambiguous-video"); w.Code != 409 {
		t.Fatal("unknown submission replay", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != before+1 || downloads != 10 || operations["/v1/videos/edits"] != 2 || operations["/v1/videos/extensions"] != 3 {
		t.Fatal("video dispatch counts", creates, before, downloads, operations)
	}
}
