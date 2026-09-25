package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testGroupModelCandidates(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	path := func(gid int64, platform string) string {
		return fmt.Sprintf("/api/v1/admin/groups/%d/model-allowlist-candidates?platform=%s", gid, platform)
	}
	read := func(gid int64, platform string) []string {
		t.Helper()
		w := call("GET", path(gid, platform), admin, nil)
		var out struct{ Data struct{ Models []string } }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Data.Models == nil || !slices.IsSorted(out.Data.Models) {
			t.Fatalf("invalid candidates: %d %s", w.Code, w.Body.String())
		}
		if len(slices.Compact(slices.Clone(out.Data.Models))) != len(out.Data.Models) {
			t.Fatal("duplicate model candidates")
		}
		return out.Data.Models
	}
	for _, token := range []string{"", ordinary} {
		w := call("GET", path(0, ""), token, nil)
		want := 401
		if token != "" {
			want = 403
		}
		if w.Code != want {
			t.Fatal("candidate permission", w.Code, want)
		}
	}
	for _, badPath := range []string{path(-1, ""), path(0, "unknown"), "/api/v1/admin/groups/bad/model-allowlist-candidates"} {
		if w := call("GET", badPath, admin, nil); w.Code != 400 {
			t.Fatal("invalid candidates input", w.Code)
		}
	}
	if w := call("GET", path(9223372036854775807, ""), admin, nil); w.Code != 404 {
		t.Fatal("missing group candidates", w.Code)
	}
	if !slices.Equal(read(0, ""), read(0, "anthropic")) {
		t.Fatal("default platform changed")
	}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		models := read(0, platform)
		for _, p := range a.prices.Load().Prices {
			if p.Platform == platform {
				for _, m := range p.Models {
					if concreteModel(m) && !slices.Contains(models, m) {
						t.Fatal("catalog candidate missing", platform, m)
					}
				}
			}
		}
	}
	compositeDefaults := read(0, "composite")
	for _, m := range read(0, "gemini") {
		if !slices.Contains(compositeDefaults, m) {
			t.Fatal("composite default missing", m)
		}
	}
	gid := id(manage("POST", "/api/v1/admin/groups", map[string]any{"name": "candidate-group", "platform": "openai", "model_allowlist": map[string]any{"enabled": true, "models": []string{"only-one"}}}))
	other := id(manage("POST", "/api/v1/admin/groups", map[string]any{"name": "candidate-other", "platform": "openai"}))
	cgid := id(manage("POST", "/api/v1/admin/groups", map[string]any{"name": "candidate-composite", "platform": "composite"}))
	var probes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1); w.WriteHeader(500) }))
	defer provider.Close()
	account := func(name, platform string, group int64) int64 {
		return id(manage("POST", "/api/v1/admin/accounts", map[string]any{"name": name, "platform": platform, "type": "apikey", "group_ids": []int64{group, cgid}, "credentials": map[string]any{"api_key": "candidate-secret", "base_url": provider.URL, "model_mapping": map[string]string{name: "private-target", name + "-*": "private-target", "shared-alias": "private-target"}}}))
	}
	aid := account("active-alias", "openai", gid)
	account("foreign-alias", "openai", other)
	manage("POST", "/api/v1/admin/accounts", map[string]any{"name": "candidate-gemini", "platform": "gemini", "type": "apikey", "group_ids": []int64{cgid}, "credentials": map[string]any{"api_key": "candidate-secret", "base_url": provider.URL, "model_mapping": map[string]string{"gemini-alias": "private-gemini-target"}}})
	for _, state := range []string{"inactive", "paused", "expired", "limited", "overloaded", "temporary", "deleted"} {
		id := account(state+"-alias", "openai", gid)
		clause := map[string]string{"inactive": "status='inactive'", "paused": "schedulable=false", "expired": "expires_at=now()-interval '1 minute',auto_pause_on_expired=true", "limited": "rate_limit_reset_at=now()+interval '1 minute'", "overloaded": "overload_until=now()+interval '1 minute'", "temporary": "temp_unschedulable_until=now()+interval '1 minute'", "deleted": "deleted_at=now()"}[state]
		if _, err := a.DB.Exec("UPDATE accounts SET "+clause+" WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
	}
	models := read(gid, "")
	for _, m := range []string{"active-alias", "active-alias-*", "shared-alias"} {
		if !slices.Contains(models, m) {
			t.Fatal("request alias omitted", m)
		}
	}
	for _, m := range models {
		if strings.Contains(m, "private-target") || strings.Contains(m, "candidate-secret") || strings.Contains(m, "foreign-alias") || strings.Contains(m, "gemini-alias") {
			t.Fatal("candidate leaked foreign/target/secret", m)
		}
	}
	for _, state := range []string{"inactive", "paused", "expired", "limited", "overloaded", "temporary", "deleted"} {
		if slices.Contains(models, state+"-alias") {
			t.Fatal("unschedulable alias visible", state)
		}
	}
	if slices.Contains(read(gid, "gemini"), "active-alias") {
		t.Fatal("platform override ignored")
	}
	models = read(cgid, "")
	if !slices.Contains(models, "active-alias") || !slices.Contains(models, "gemini-alias") {
		t.Fatal("composite aliases omitted")
	}
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), map[string]any{"credentials": map[string]any{"model_mapping": map[string]string{"updated-alias": "private-target"}}})
	if models = read(gid, ""); !slices.Contains(models, "updated-alias") || slices.Contains(models, "active-alias") {
		t.Fatal("stale candidate mapping")
	}
	if probes.Load() != 0 {
		t.Fatal("allowlist suggestions called upstream", probes.Load())
	}
}

func TestModelMetadata(t *testing.T) {
	m, err := decodeModel(json.RawMessage(`{"slug":"reasoner","reasoning":true,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"}],"input_modalities":["text","image"],"context_window":32000,"api_key":"must-drop"}`))
	if err != nil || !m.complete() || len(m.SupportedReasoningLevels) != 2 {
		t.Fatal(m, err)
	}
	other := m
	other.ContextWindow = 16000
	other.InputModalities = []string{"text"}
	other.SupportedReasoningLevels = []string{"low"}
	common := commonModelCapabilities(m, other)
	if common.ContextWindow != 16000 || len(common.InputModalities) != 1 || len(common.SupportedReasoningLevels) != 1 || common.DefaultReasoningLevel != "low" {
		t.Fatal("common model capabilities", common)
	}
	raw, _ := json.Marshal(m)
	if bytes.Contains(raw, []byte("must-drop")) {
		t.Fatal("unknown upstream fields escaped")
	}
	gemini, err := decodeModel(json.RawMessage(`{"name":"models/gemini-test","displayName":"Gemini","inputTokenLimit":128000,"outputTokenLimit":4000,"supportedGenerationMethods":["generateContent","countTokens"]}`))
	if err != nil || gemini.ID != "gemini-test" || gemini.ContextWindow != 128000 || len(gemini.Methods) != 2 {
		t.Fatal(gemini, err)
	}
	for _, raw := range []string{`null`, `{}`, `{"id":"*"}`, `{"id":"m","context_window":-1}`, `{"id":"m","reasoning":"true"}`, `{"id":"m","supported_reasoning_levels":[null]}`, `{"id":"m","context_window":9999999999}`} {
		if _, err := decodeModel(json.RawMessage(raw)); err == nil {
			t.Fatal("invalid model accepted", raw)
		}
	}
}

func testModelDiscovery(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Cookie", "client-private-cookie")
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, nil)
		if w.Code != 200 {
			t.Fatalf("%s %s %d %s", method, path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	var calls, mode atomic.Int32
	start, release := make(chan struct{}, 1), make(chan struct{})
	const secret = "model-discovery-secret"
	const full = `{"id":"upstream-a","display_name":"Upstream A","reasoning":true,"default_reasoning_level":"high","supported_reasoning_levels":["low","high"],"input_modalities":["text","image"],"context_window":64000,"owned_by":"provider","private_config":"must-not-leak"}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Cookie") != "" || r.URL.Query().Get("key") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("model discovery leaked client credentials or used wrong method")
		}
		if r.URL.Path == "/v1beta/models" {
			if r.Header.Get("X-Goog-Api-Key") != secret || r.Header.Get("Authorization") != "" {
				t.Error("Gemini model credentials")
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"models":[{"name":"models/gemini-a","displayName":"A","inputTokenLimit":64000,"outputTokenLimit":4096,"version":"v1","supportedGenerationMethods":["generateContent","countTokens"]}],"nextPageToken":"next opaque/&?"}`)
			} else if r.URL.Query().Get("pageToken") == "next opaque/&?" {
				fmt.Fprint(w, `{"models":[{"name":"models/gemini-b","displayName":"B","inputTokenLimit":128000}]}`)
			} else {
				t.Error("upstream pagination cursor changed")
			}
			return
		}
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("model discovery upstream endpoint/credential")
		}
		if mode.Load() == 1 {
			w.WriteHeader(403)
			fmt.Fprint(w, secret)
			return
		}
		if mode.Load() == 2 {
			fmt.Fprint(w, `{"data":[{"id":"ids-only"}]}`)
			return
		}
		if mode.Load() == 3 {
			fmt.Fprint(w, `{"data":[`+full+`],"has_more":true,"last_id":"repeated"}`)
			return
		}
		if mode.Load() == 4 {
			select {
			case start <- struct{}{}:
			default:
			}
			<-release
		}
		fmt.Fprint(w, `{"data":[`+full+`,{"id":"extra-model"}]}`)
	}))
	defer up.Close()
	uid := int64(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "models@example.test", "password": "models-password"})["id"].(float64))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "models@example.test", "password": "models-password"})["access_token"].(string)
	group := func(name, platform string) int64 {
		return int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": name, "platform": platform})["id"].(float64))
	}
	account := func(name, platform string, gid int64, mapping map[string]string) int64 {
		credentials := map[string]any{"api_key": secret, "base_url": up.URL}
		if mapping != nil {
			credentials["model_mapping"] = mapping
		}
		return int64(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": credentials})["id"].(float64))
	}
	gid := group("Discovery", "openai")
	gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	key := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Discovery", "group_id": gid})["key"].(string)
	aid := account("Mapped discovery", "openai", gid, map[string]string{"account-alias": "upstream-a", "wild-*": "upstream-a"})
	apath := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Discovery", "group_ids": []int64{gid}, "model_mapping": map[string]any{"openai": map[string]string{"client-alias": "account-alias"}}})
	manage("PUT", gpath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"client-alias", "wild-concrete"}}})
	first := call("GET", "/v1/models", key, nil, nil)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "client-alias") || !strings.Contains(first.Body.String(), "wild-concrete") || strings.Contains(first.Body.String(), "upstream-a") || strings.Contains(first.Body.String(), "extra-model") || strings.Contains(first.Body.String(), "must-not-leak") {
		t.Fatal("scoped model discovery", first.Code, first.Body.String())
	}
	for _, path := range []string{"/models", "/v1/models"} {
		w := call("GET", path, key, nil, map[string]string{"If-None-Match": "W/" + first.Header().Get("ETag")})
		if w.Code != 304 || w.Body.Len() != 0 || calls.Load() != 1 {
			t.Fatal("model ETag or cache", w.Code, w.Body.String(), calls.Load())
		}
	}
	for _, prefix := range []string{"", "/v1"} {
		detail := call("GET", prefix+"/models/client-alias?client_version=test", key, nil, map[string]string{"If-None-Match": first.Header().Get("ETag")})
		if detail.Code != 200 || strings.Contains(detail.Body.String(), "slug") || !strings.Contains(detail.Body.String(), `"id":"client-alias"`) {
			t.Fatal("model retrieve representation", detail.Code, detail.Body.String())
		}
	}
	if w := call("GET", "/models/upstream-a", key, nil, nil); w.Code != 404 {
		t.Fatal("model retrieval bypassed allowlist", w.Code)
	}
	for _, path := range []string{"/backend-api/codex/models", "/v1/models?client_version=1"} {
		w := call("GET", path, key, nil, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"slug":"client-alias"`) || !strings.Contains(w.Body.String(), `"context_window":64000`) || !strings.Contains(w.Body.String(), `"effort":"high"`) {
			t.Fatal("Codex manifest", w.Code, w.Body.String())
		}
	}
	// Model queries work at zero balance, but remain subject to user/key status.
	if _, err := a.DB.Exec("UPDATE users SET status='inactive' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/models", key, nil, map[string]string{"If-None-Match": "*"}); w.Code != 401 {
		t.Fatal("ETag bypassed authentication", w.Code)
	}
	if _, err := a.DB.Exec("UPDATE users SET status='active' WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec(`UPDATE accounts SET extra=extra||'{"quota_used":3.125,"unknown_state":{"keep":true}}'::jsonb WHERE id=$1`, aid); err != nil {
		t.Fatal(err)
	}
	sync := manage("POST", apath+"/models/sync-upstream", admin, nil)
	if len(sync["models"].([]any)) != 2 || len(sync["warnings"].([]any)) != 1 {
		t.Fatal("sync catalog partial metadata", sync)
	}
	var snapshot, credentials []byte
	if err := a.DB.QueryRow("SELECT extra,credentials FROM accounts WHERE id=$1", aid).Scan(&snapshot, &credentials); err != nil || !bytes.Contains(snapshot, []byte("upstream_model_metadata")) || !bytes.Contains(snapshot, []byte("3.125")) || !bytes.Contains(snapshot, []byte("unknown_state")) || !bytes.Contains(credentials, []byte("account-alias")) {
		t.Fatal("sync overwrote unrelated config", err)
	}
	mode.Store(2)
	manage("POST", apath+"/models/sync-upstream", admin, nil)
	var unchanged []byte
	if err := a.DB.QueryRow("SELECT extra FROM accounts WHERE id=$1", aid).Scan(&unchanged); err != nil || !bytes.Equal(snapshot, unchanged) {
		t.Fatal("incomplete sync destroyed capability snapshot", err)
	}
	for _, state := range []int32{1, 3} {
		mode.Store(state)
		w := call("POST", apath+"/models/sync-upstream", admin, nil, nil)
		if w.Code != 502 || strings.Contains(w.Body.String(), secret) {
			t.Fatal("model sync error handling", state, w.Code, w.Body.String())
		}
	}
	mode.Store(4)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", apath+"/models/sync-upstream", admin, nil, nil) }()
	select {
	case <-start:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("sync did not reach upstream")
	}
	manage("PUT", apath, admin, map[string]any{"notes": "edited during sync"})
	close(release)
	if w := <-done; w.Code != 409 {
		t.Fatal("stale sync overwrote concurrent config", w.Code, w.Body.String())
	}
	mode.Store(0)
	manage("POST", apath+"/models/sync-upstream", admin, nil)
	// Configuration changes while discovery is blocked cannot return the old list.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-unblock
		fmt.Fprint(w, `{"data":[`+full+`]}`)
	}))
	defer slow.Close()
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"base_url": slow.URL}})
	discoveryDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { discoveryDone <- call("GET", "/models", key, nil, nil) }()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		close(unblock)
		t.Fatal("discovery did not reach upstream")
	}
	manage("PUT", gpath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"new-allowlist-only"}}})
	close(unblock)
	if w := <-discoveryDone; w.Code != 409 || strings.Contains(w.Body.String(), "client-alias") {
		t.Fatal("discovery returned stale authorization", w.Code, w.Body.String())
	}
	manage("PUT", gpath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"client-alias", "wild-concrete"}}})
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"base_url": up.URL}})
	manage("POST", apath+"/models/sync-upstream", admin, nil)
	// Account source changes clear only the old source's capability snapshot.
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-key"}, "extra": map[string]any{"quota_limit": 10}})
	if err := a.DB.QueryRow("SELECT extra FROM accounts WHERE id=$1", aid).Scan(&unchanged); err != nil || bytes.Contains(unchanged, []byte("upstream_model_metadata")) || !bytes.Contains(unchanged, []byte("3.125")) {
		t.Fatal("rotated credentials retained stale metadata or lost quota", err)
	}
	manage("PUT", apath, admin, map[string]any{"credentials": map[string]any{"api_key": secret}})
	manage("PUT", gpath, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": true, "account_ids": []int64{aid, aid}, "fallback_to_scheduler": false}})
	if w := call("PUT", gpath, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": true, "account_ids": []int64{999999}}}, nil); w.Code != 400 {
		t.Fatal("invalid pinned account accepted", w.Code)
	}
	mode.Store(1)
	if w := call("GET", "/models", key, nil, nil); w.Code != 503 {
		t.Fatal("pinned fetch failure silently used mapping", w.Code, w.Body.String())
	}
	otherID := account("Fallback mapping", "openai", gid, map[string]string{"account-alias": "upstream-a"})
	manage("PUT", gpath, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": true, "account_ids": []int64{aid}, "fallback_to_scheduler": true}})
	if w := call("GET", "/models", key, nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "client-alias") {
		t.Fatal("pinned fallback policy", w.Code, w.Body.String())
	}
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", otherID), admin, map[string]any{"status": "inactive"})
	manage("PUT", gpath, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": false}})
	if w := call("GET", "/models", key, nil, nil); w.Code != 200 {
		t.Fatal("relay explicit mapping fallback", w.Code, w.Body.String())
	}
	mode.Store(0)
	geminiGroup := group("Gemini discovery", "gemini")
	account("Gemini discovery", "gemini", geminiGroup, nil)
	geminiKey := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Gemini discovery", "group_id": geminiGroup})["key"].(string)
	native := call("GET", "/v1beta/models?pageSize=1&key="+geminiKey, "", nil, nil)
	if native.Code != 200 || !strings.Contains(native.Body.String(), `"nextPageToken":"1"`) || !strings.Contains(native.Body.String(), `"name":"models/gemini-a"`) || !strings.Contains(native.Body.String(), `"inputTokenLimit":64000`) {
		t.Fatal("Gemini models pagination", native.Code, native.Body.String())
	}
	if w := call("GET", "/v1beta/models?pageSize=1&pageToken=1", geminiKey, nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "models/gemini-b") || strings.Contains(w.Body.String(), "nextPageToken") {
		t.Fatal("Gemini second page", w.Code, w.Body.String())
	}
	if w := call("GET", "/v1beta/models/gemini-a", geminiKey, nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "supportedGenerationMethods") {
		t.Fatal("Gemini model metadata", w.Code, w.Body.String())
	}
	if w := call("GET", "/v1beta/models", key, nil, nil); w.Code != 400 {
		t.Fatal("Gemini platform guard", w.Code)
	}
	if w := call("GET", "/v1beta/models?pageSize=1001", geminiKey, nil, nil); w.Code != 400 {
		t.Fatal("Gemini pagination bounds", w.Code)
	}
	if w := call("POST", apath+"/models/sync-upstream", user, nil, nil); w.Code != 403 {
		t.Fatal("ordinary user performed admin model sync", w.Code)
	}
	var logs int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&logs); err != nil || logs != 0 {
		t.Fatal("model discovery generated consumption", logs, err)
	}
}
