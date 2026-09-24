package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testImagePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aWQAAAABJRU5ErkJggg=="

func TestAccountMediaProbes(t *testing.T) {
	var calls atomic.Int64
	var behavior atomic.Int64
	var path, model, auth, goog, api string
	var requestBody map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		path, auth, goog, api = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Goog-Api-Key"), r.Header.Get("X-Api-Key")
		if r.Method == "POST" {
			if json.NewDecoder(r.Body).Decode(&requestBody) != nil {
				t.Error("invalid probe body")
			}
			model = credentialString(requestBody, "model")
		}
		w.Header().Set("Content-Type", "application/json")
		switch behavior.Load() {
		case 1:
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"media-test-secret"}`)
			return
		case 2:
			fmt.Fprint(w, `{"data":[{}],"candidates":[{"content":{"parts":[{"text":"no image"}]}}]}`)
			return
		case 3:
			fmt.Fprint(w, `{"data":[{"b64_json":"not-an-image"}]}`)
			return
		case 4:
			if r.Method == "GET" {
				fmt.Fprint(w, `{"request_id":"probe_1","status":"pending"}`)
				return
			}
		case 5:
			if r.Method == "GET" {
				fmt.Fprint(w, `{"request_id":"probe_1","status":"failed","error":"secret"}`)
				return
			}
		case 6:
			fmt.Fprint(w, `{"request_id":"../../escape"}`)
			return
		case 7:
			fmt.Fprint(w, `{"data":[{"url":"https://example.test/media-test-secret"}]}`)
			return
		case 8:
			fmt.Fprint(w, `{"data":[{"b64_json":"`+strings.Repeat("A", 16<<20)+`"}]}`)
			return
		case 9:
			fmt.Fprint(w, `{"data":[`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		switch {
		case strings.Contains(path, "contents/generations/tasks"):
			if r.Method == "POST" {
				fmt.Fprint(w, `{"id":"probe_1"}`)
			} else {
				fmt.Fprint(w, `{"id":"probe_1","status":"succeeded","content":{"video_url":"https://example.test/video.mp4"},"usage":{"completion_tokens":3}}`)
			}
		case strings.HasPrefix(path, "/v1/videos"):
			if r.Method == "POST" {
				fmt.Fprint(w, `{"request_id":"probe_1"}`)
			} else {
				fmt.Fprint(w, `{"request_id":"probe_1","status":"done","video":{"url":"https://example.test/video.mp4","duration":6}}`)
			}
		case strings.HasPrefix(path, "/v1beta/models/"):
			fmt.Fprint(w, `{"candidates":[{"content":{"parts":[{"inline_data":{"mime_type":"image/png","data":"`+testImagePNG+`"}}]}}]}`)
		case path == "/v1/images/generations" || path == "/v1/images/edits":
			fmt.Fprint(w, `{"data":[{"b64_json":"`+testImagePNG+`"}]}`)
		case path == "/v1/chat/completions":
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		default:
			t.Error("unexpected probe path", path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	account := func(platform, mapped string) *upstreamAccount {
		raw, _ := json.Marshal(map[string]any{"api_key": "media-test-secret", "base_url": upstream.URL, "model_mapping": map[string]string{"alias": mapped}, "openai_capabilities": []string{"seedance"}})
		var credentials map[string]json.RawMessage
		_ = json.Unmarshal(raw, &credentials)
		return &upstreamAccount{ID: 1, Platform: platform, Type: "apikey", Credentials: credentials}
	}
	source := "data:image/png;base64," + testImagePNG
	for _, tc := range []struct{ platform, mapped, mode, image, want string }{
		{"openai", "gpt-image-1", "", "", "/v1/images/generations"},
		{"openai", "relay-picture", "image", source, "/v1/images/edits"},
		{"grok", "grok-imagine-image", "default", source, "/v1/images/edits"},
		{"gemini", "gemini-2.5-flash-image", "", source, "/v1beta/models/gemini-2.5-flash-image:generateContent"},
		{"grok", "grok-imagine-video", "", source, "/v1/videos/probe_1"},
		{"openai", "doubao-seedance-2-0", "", source, "/api/v3/contents/generations/tasks/probe_1"},
	} {
		u := account(tc.platform, tc.mapped)
		if tc.platform == "openai" {
			u.Credentials["api_protocol"] = json.RawMessage(`"anthropic"`)
		}
		result := a.runAccountTest(context.Background(), u, accountTestInput{Model: "alias", Mode: tc.mode, Image: tc.image})
		if result.Status != "success" || len(result.Media) != 1 || path != tc.want || strings.Contains(result.Text, "base64") || strings.Contains(result.Text, "https://") {
			t.Fatalf("%s %s: %#v path=%s", tc.platform, tc.mapped, result, path)
		}
		if tc.platform == "gemini" {
			if goog != "media-test-secret" || auth != "" {
				t.Fatal("Gemini credential isolation")
			}
			if !bytes.Contains(requestBody["generationConfig"], []byte(`"IMAGE"`)) || !bytes.Contains(requestBody["contents"], []byte(`"inlineData"`)) {
				t.Fatal("Gemini image request missing modalities/input")
			}
		} else if auth != "Bearer media-test-secret" || goog != "" || api != "" || model != tc.mapped {
			t.Fatal("media credential or model mapping", auth, api, model)
		}

		if tc.want == "/v1/images/edits" {
			if tc.platform == "openai" {
				var images []struct {
					URL string `json:"image_url"`
				}
				if json.Unmarshal(requestBody["images"], &images) != nil || len(images) != 1 || images[0].URL != source {
					t.Fatal("OpenAI JSON edit shape")
				}
			} else {
				var image map[string]string
				if json.Unmarshal(requestBody["image"], &image) != nil || image["url"] != source || image["type"] != "image_url" {
					t.Fatal("Grok edit shape")
				}
			}
		}
		raw, _ := json.Marshal(result)
		if bytes.Contains(raw, []byte("base64")) || bytes.Contains(raw, []byte("video_url")) {
			t.Fatal("media entered persisted summary")
		}
	}
	u := account("grok", "grok-imagine-image")
	explicit := a.runAccountTest(context.Background(), u, accountTestInput{Model: "alias", Mode: "text"})
	if explicit.Status != "success" || path != "/v1/chat/completions" {
		t.Fatal("explicit text mode lost", explicit, path)
	}
	for _, b := range []int64{1, 2, 3, 7, 8} {
		behavior.Store(b)
		result := a.runAccountTest(context.Background(), u, accountTestInput{Model: "alias"})
		if result.Status != "failed" || strings.Contains(result.Error, "media-test-secret") {
			t.Fatal("invalid image reported healthy", b, result)
		}
	}
	behavior.Store(9)
	interrupted, stop := context.WithTimeout(context.Background(), 40*time.Millisecond)
	result := a.runAccountTest(interrupted, u, accountTestInput{Model: "alias"})
	stop()
	if result.Status != "failed" || !strings.Contains(result.Error, "timed out") {
		t.Fatal("interrupted media response", result)
	}
	u = account("grok", "grok-imagine-video")
	for _, b := range []int64{4, 5, 6} {
		behavior.Store(b)
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		result := a.runAccountTest(ctx, u, accountTestInput{Model: "alias"})
		cancel()
		if result.Status != "failed" || len(result.Media) != 0 {
			t.Fatal("unfinished video reported healthy", b, result)
		}
		if b == 4 && (!strings.Contains(result.Error, "timed out") || !strings.Contains(result.Text, "probe_1")) {
			t.Fatal("timeout lost task reference", result)
		}
	}
	before := calls.Load()
	for _, in := range []accountTestInput{{Model: "../bad"}, {Model: "alias", Mode: "unknown"}, {Model: "alias", Image: "https://example.test/source"}, {Model: "alias", Image: "data:image/png;base64,aGVsbG8="}, {Model: "alias", Prompt: strings.Repeat("x", 4001)}} {
		if result := a.runAccountTest(context.Background(), u, in); result.Status != "failed" {
			t.Fatal("invalid probe input allowed", in)
		}
	}
	if calls.Load() != before {
		t.Fatal("invalid input reached upstream")
	}

	for _, platform := range []string{"anthropic", "deepseek", "kimi", "zhipu", "minimax"} {
		for _, mode := range []string{"image", "video"} {
			if _, _, err := prepareAccountTest(account(platform, "custom"), accountTestInput{Model: "alias", Mode: mode}); err == nil {
				t.Fatal("unsupported provider mode", platform, mode)
			}
		}
	}
	disabled := account("openai", "doubao-seedance-2-0")
	disabled.Credentials["openai_capabilities"] = json.RawMessage(`{"seedance":false}`)
	if _, _, err := prepareAccountTest(disabled, accountTestInput{Model: "alias"}); err == nil {
		t.Fatal("Seedance capability bypass")
	}
	disabled.Credentials["openai_capabilities"] = json.RawMessage(`["seedance"]`)
	if _, _, err := prepareAccountTest(disabled, accountTestInput{Model: "alias", Mode: "text"}); err == nil {
		t.Fatal("text capability bypass")
	}
	if !a.takeSlot("account-test", u.ID, 1) {
		t.Fatal("slot leaked")
	}
	if result := a.runAccountTest(context.Background(), u, accountTestInput{Model: "alias"}); result.Status != "failed" || calls.Load() != before {
		t.Fatal("duplicate probe dispatched")
	}
	a.releaseSlot("account-test", u.ID)
	// Large valid inline media works beyond the text JSON limit.
	imageBytes, _ := base64.StdEncoding.DecodeString(testImagePNG)
	imageBytes = append(imageBytes, make([]byte, 4<<20)...)
	if _, _, err := accountTestImageData("data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)); err != nil {
		t.Fatal("large image rejected", err)
	}
}

func testAccountMediaHealth(t *testing.T, a *App, admin, user string) {
	t.Helper()
	var fail atomic.Bool
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer health-secret" {
			t.Error("incorrect media health dispatch", r.URL.Path)
		}
		if fail.Load() {
			w.WriteHeader(429)
			fmt.Fprint(w, "health-secret")
			return
		}
		fmt.Fprint(w, `{"data":[{"b64_json":"`+testImagePNG+`"}]}`)
	}))
	defer upstream.Close()
	request := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.111:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	create := func(path string, body any) int64 {
		t.Helper()
		w := request("POST", path, admin, body)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		var out map[string]json.RawMessage
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		if out["data"] != nil {
			_ = json.Unmarshal(out["data"], &out)
		}
		var id int64
		if json.Unmarshal(out["id"], &id) != nil || id <= 0 {
			t.Fatal("missing ID")
		}
		return id
	}
	id := create("/api/v1/admin/accounts", map[string]any{"name": "Media health", "platform": "grok", "type": "apikey", "concurrency": 1, "credentials": map[string]any{"api_key": "health-secret", "base_url": upstream.URL, "model_mapping": map[string]string{"alias": "grok-imagine-image"}}})
	path := fmt.Sprintf("/api/v1/admin/accounts/%d/test", id)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE accounts SET status='error',error_message='temporary',rate_limit_reset_at=now()+interval '1 hour',extra=extra || '{\"quota_used\":2.125}'::jsonb,updated_at=now() WHERE id=$1", id)
	body := map[string]any{"model_id": "alias", "mode": "image"}
	if w := request("POST", path, user, body); w.Code != 403 {
		t.Fatal("user could probe account", w.Code)
	}
	w := request("POST", path, admin, body)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"image"`) || !strings.Contains(w.Body.String(), `"test_complete"`) || strings.Contains(w.Body.String(), "health-secret") {
		t.Fatal("manual media SSE", w.Code, w.Body.String())
	}
	var status, quota string
	var limited bool
	check := func(want string, wantLimited bool) {
		t.Helper()
		if err := a.DB.QueryRow("SELECT status,extra->>'quota_used',rate_limit_reset_at IS NOT NULL FROM accounts WHERE id=$1", id).Scan(&status, &quota, &limited); err != nil || status != want || quota != "2.125" || limited != wantLimited {
			t.Fatal("health/counter state", status, quota, limited, err)
		}
	}
	check("active", false)
	if !a.takeSlot("account", id, 1) {
		t.Fatal("cannot reserve slot")
	}
	before := calls.Load()
	w = request("POST", path, admin, body)
	a.releaseSlot("account", id)
	if strings.Contains(w.Body.String(), `"test_complete"`) || calls.Load() != before {
		t.Fatal("probe bypassed account concurrency")
	}
	planID := create("/api/v1/admin/scheduled-test-plans", map[string]any{"account_id": id, "model_id": "alias", "cron_expression": "0 * * * *", "auto_recover": true, "max_results": 2})
	planPath := fmt.Sprintf("/api/v1/admin/scheduled-test-plans/%d", planID)
	defer a.DB.Exec("DELETE FROM scheduled_test_plans WHERE id=$1", planID)
	run := func() {
		t.Helper()
		a.planMu.Lock()
		defer a.planMu.Unlock()
		var raw []byte
		if err := a.DB.QueryRow("SELECT to_jsonb(p) FROM scheduled_test_plans p WHERE id=$1", planID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan testPlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			t.Fatal(err)
		}
		if err := a.runTestPlan(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE accounts SET status='error',error_message='temporary',rate_limit_reset_at=now()+interval '1 hour',updated_at=now() WHERE id=$1", id)
	run()
	check("active", false)
	exec("UPDATE accounts SET status='inactive',rate_limit_reset_at=now()+interval '1 hour',updated_at=now() WHERE id=$1", id)
	run()
	check("inactive", true)
	fail.Store(true)
	exec("UPDATE accounts SET status='error',updated_at=now() WHERE id=$1", id)
	run()
	check("error", true)
	w = request("GET", planPath+"/results", admin, nil)
	var results []accountTestResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &results) != nil || len(results) != 2 || results[0].Status != "failed" || results[1].Status != "success" || strings.Contains(w.Body.String(), "base64") || strings.Contains(w.Body.String(), "health-secret") {
		t.Fatal("scheduled media results", w.Body.String())
	}
	var usage int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE account_id=$1", id).Scan(&usage); err != nil || usage != 0 {
		t.Fatal("probe billed internal user", usage, err)
	}
}
