package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestModelRouting(t *testing.T) {
	g := gatewayGroup{RoutingEnabled: true, ModelRouting: map[string][]int64{"*": {1}, "family-*": {2}, "family-long-*": {3}, "family-long-exact": {4}, "family-empty": {}}}
	for _, tc := range []struct {
		model, platform string
		want            []int64
	}{{"family-long-exact", "openai", []int64{4}}, {"family-long-other", "anthropic", []int64{3}}, {"family-short", "openai", []int64{2}}, {"family-empty", "openai", []int64{2}}, {"FAMILY-short", "openai", []int64{1}}, {"family-long-exact", "gemini", nil}, {"family-short", "deepseek", nil}} {
		for i := 0; i < 30; i++ {
			if got := g.routingAccounts(tc.model, tc.platform); !reflect.DeepEqual(got, tc.want) {
				t.Fatal("model routing precedence", tc, got)
			}
		}
	}
	g.RoutingEnabled = false
	if got := g.routingAccounts("family-long-exact", "openai"); len(got) != 0 {
		t.Fatal("disabled routing still applied", got)
	}
	for _, routes := range []map[string][]int64{{"": {1}}, {"a*b": {1}}, {"a": {0}}, {"a": {1, 1}}, {"a": {-1}}} {
		if validateModelRouting(routes) == nil {
			t.Fatal("invalid routing accepted", routes)
		}
	}
}

func testModelRouting(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.95:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		// Each scenario starts a new conversation; sticky pool behavior is tested separately.
		r.Header.Set("Session-Id", randomToken(12))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	idOf := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "routing@example.test", "password": "routing-password", "balance": "10"}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "routing@example.test", "password": "routing-password"})["access_token"].(string)
	gid := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Routing", "platform": "openai", "model_routing_enabled": true, "model_routing": map[string][]int64{}}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Routing", "group_id": gid})["key"].(string)
	var failB atomic.Bool
	var pausePost, pauseGet atomic.Bool
	var calls atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		label := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer route-")
		if r.Header.Get("X-Api-Key") != "" {
			label = strings.TrimPrefix(r.Header.Get("X-Api-Key"), "route-")
		}
		if r.Method == "GET" {
			if label == "b" && pauseGet.Swap(false) {
				started <- struct{}{}
				<-release
			}
			window := map[string]int{"a": 16000, "b": 128000, "c": 64000, "outside": 1000}[label]
			fmt.Fprintf(w, `{"data":[{"id":"up-%s","reasoning":true,"supported_reasoning_levels":["low","high"],"input_modalities":["text","image"],"context_window":%d}]}`, label, window)
			return
		}
		n := calls.Add(1)
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "up-"+label {
			t.Error("wrong upstream model", body, err)
		}
		if label == "b" && pausePost.Swap(false) {
			started <- struct{}{}
			<-release
		}
		if label == "b" && failB.Load() {
			w.WriteHeader(503)
			return
		}
		switch r.URL.Path {
		case "/v1/responses":
			fmt.Fprintf(w, `{"id":"resp_route_%d","object":"response","status":"completed","model":"up-%s","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n, label)
		case "/v1/messages":
			fmt.Fprintf(w, `{"id":"msg_%d","type":"message","role":"assistant","model":"up-%s","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, n, label)
		default:
			fmt.Fprintf(w, `{"id":"chat_%d","model":"up-%s","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, n, label)
		}
	}))
	defer func() { close(release); up.Close() }()
	account := func(label, platform string, group, priority int64) int64 {
		return idOf(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Routing " + label + platform, "platform": platform, "type": "apikey", "priority": priority, "concurrency": 1, "group_ids": []int64{group}, "credentials": map[string]any{"api_key": "route-" + label, "base_url": up.URL, "model_mapping": map[string]string{"pool-model": "up-" + label}}}))
	}
	first, preferred, second := account("a", "openai", gid, 1), account("b", "openai", gid, 50), account("c", "openai", gid, 25)
	outsideGroup := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Outside routing", "platform": "openai"}))
	outside := account("outside", "openai", outsideGroup, 0)
	channel := func(group int64, platform, public string) int64 {
		return idOf(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Routing channel " + fmt.Sprint(group), "group_ids": []int64{group}, "model_mapping": map[string]any{platform: map[string]string{public: "pool-model"}}, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"pool-model"}, "input_price": "0.001", "output_price": "0.002", "cache_write_price": "0", "cache_read_price": "0"}}}))
	}
	cid := channel(gid, "openai", "public-model")
	cp := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	route := func(ids ...int64) {
		must("PUT", gp, admin, map[string]any{"model_routing": map[string][]int64{"pool-*": ids}})
	}
	body := map[string]any{"model": "public-model", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
	check := func(w *httptest.ResponseRecorder, want int64) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var account, group int64
		var cost string
		if err := a.DB.QueryRow(`SELECT account_id,group_id,actual_cost::text FROM usage_logs WHERE user_id=$1 AND request_id=$2`, uid, w.Header().Get("X-Request-ID")).Scan(&account, &group, &cost); err != nil || account != want || group != gid || cost != "0.0200000000" {
			t.Fatal("routing or billing changed", account, group, cost, want, err)
		}
	}
	check(call("POST", "/v1/chat/completions", key, body), first)
	route(preferred)
	check(call("POST", "/v1/chat/completions", key, body), preferred)
	// Array order is not priority: candidate ordering remains authoritative.
	route(preferred, second)
	check(call("POST", "/v1/chat/completions", key, body), second)
	route(preferred)
	manifest := func(want any) {
		t.Helper()
		w := call("GET", "/backend-api/codex/models", key, nil)
		var catalog struct{ Models []map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil {
			t.Fatal("routing manifest", w.Code, w.Body.String())
		}
		for _, m := range catalog.Models {
			if m["slug"] == "public-model" {
				if m["context_window"] != want {
					t.Fatal("manifest ignored routed account capabilities", m["context_window"], want)
				}
				return
			}
		}
		t.Fatal("public model missing from manifest")
	}
	manifest(float64(128000))
	route(preferred, second)
	manifest(float64(64000))
	route(preferred)
	must("PUT", gp, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": true, "account_ids": []int64{first}, "fallback_to_scheduler": false}})
	manifest(float64(16000))
	must("PUT", gp, admin, map[string]any{"codex_models_manifest_config": map[string]any{"enabled": false}})
	if !a.takeSlot("account", preferred, 1) {
		t.Fatal("could not occupy preferred account")
	}
	busy := call("POST", "/v1/chat/completions", key, body)
	a.releaseSlot("account", preferred)
	check(busy, first)
	prefPath := fmt.Sprintf("/api/v1/admin/accounts/%d", preferred)
	for _, patch := range []map[string]any{{"status": "inactive"}, {"extra": map[string]any{"quota_limit": "1"}}, {"expires_at": 1, "auto_pause_on_expired": true}} {
		must("PUT", prefPath, admin, patch)
		if patch["extra"] != nil {
			if _, err := a.DB.Exec(`UPDATE accounts SET extra=extra||'{"quota_used":1}'::jsonb WHERE id=$1`, preferred); err != nil {
				t.Fatal(err)
			}
		}
		check(call("POST", "/v1/chat/completions", key, body), first)
		must("PUT", prefPath, admin, map[string]any{"status": "active", "expires_at": 0, "extra": map[string]any{"quota_limit": "0"}, "credentials": map[string]any{"api_protocol": "chat_completions"}})
	}
	route(outside, 99999999)
	check(call("POST", "/v1/chat/completions", key, body), first)
	manifest(nil)
	route(preferred)
	failB.Store(true)
	before := calls.Load()
	check(call("POST", "/v1/chat/completions", key, body), first)
	if calls.Load() != before+2 {
		t.Fatal("preferred failure did not perform bounded fallback")
	}
	failB.Store(false)
	must("POST", prefPath+"/clear-rate-limit", admin, nil)
	for _, invalid := range []any{map[string][]int64{"pool-*": {preferred, preferred}}, map[string][]int64{"a*b": {first}}, map[string][]int64{"m": {-1}}, []any{}, map[string]any{"m": []any{1.5}}} {
		if w := call("PUT", gp, admin, map[string]any{"model_routing": invalid}); w.Code != 400 {
			t.Fatal("invalid routing accepted", w.Code, w.Body.String())
		}
	}
	if w := call("PUT", gp, user, map[string]any{"model_routing": map[string][]int64{}}); w.Code != 403 {
		t.Fatal("user changed routing", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"description": "routing remains", "model_routing": nil})
	check(call("POST", "/v1/chat/completions", key, body), preferred)
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": false})
	check(call("POST", "/v1/chat/completions", key, body), first)
	manifest(float64(16000))
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": true})
	// Routing may prefer a restricted account, but cannot bypass upstream price admission.
	must("PUT", cp, admin, map[string]any{"billing_model_source": "upstream", "restrict_models": true, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"up-a"}, "input_price": "0.001", "output_price": "0.002", "cache_write_price": "0", "cache_read_price": "0"}}})
	check(call("POST", "/v1/chat/completions", key, body), first)
	must("PUT", cp, admin, map[string]any{"billing_model_source": "channel_mapped", "restrict_models": false, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"pool-model"}, "input_price": "0.001", "output_price": "0.002", "cache_write_price": "0", "cache_read_price": "0"}}})
	// A request keeps its routing snapshot even if an administrator edits it during retry.
	must("PUT", gp, admin, map[string]any{"model_routing": map[string][]int64{"pool-model": {preferred, second}}})
	// Make b the first of the preferred pool for this scenario.
	must("PUT", prefPath, admin, map[string]any{"priority": 10, "group_ids": []int64{gid}})
	pausePost.Store(true)
	failB.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "/v1/chat/completions", key, body) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	route(first)
	release <- struct{}{}
	check(<-done, second)
	failB.Store(false)
	must("POST", prefPath+"/clear-rate-limit", admin, nil)
	// Model discovery refuses a mixed configuration when routing changes mid-fetch.
	u, err := a.loadAccount(context.Background(), preferred)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Redis.Del(context.Background(), modelCacheKey(u)).Err(); err != nil {
		t.Fatal(err)
	}
	pauseGet.Store(true)
	go func() { done <- call("GET", "/backend-api/codex/models", key, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery did not start")
	}
	route(preferred)
	release <- struct{}{}
	if w := <-done; w.Code != 409 {
		t.Fatal("stale routing metadata returned", w.Code, w.Body.String())
	}
	// Responses affinity outranks route edits and never falls back to another account.
	for _, id := range []int64{first, preferred} {
		must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", id), admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	}
	responseBody := map[string]any{"model": "public-model", "input": "ok"}
	w := call("POST", "/v1/responses", key, responseBody)
	check(w, preferred)
	var response struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	route(first)
	responseBody["previous_response_id"] = response.ID
	check(call("POST", "/v1/responses", key, responseBody), preferred)
	failB.Store(true)
	before = calls.Load()
	if w := call("POST", "/v1/responses", key, responseBody); w.Code != 503 || calls.Load() != before+1 {
		t.Fatal("bound response fell back to another account", w.Code, calls.Load()-before)
	}
	failB.Store(false)
	// Clearing all rules restores ordinary account ordering without changing the switch.
	must("PUT", gp, admin, map[string]any{"model_routing": map[string][]int64{}})
	delete(responseBody, "previous_response_id")
	check(call("POST", "/v1/responses", key, responseBody), first)
	// The same preferred-pool behavior works for Anthropic and explicit composite routes.
	for _, platform := range []string{"anthropic", "composite"} {
		gid = idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Routed " + platform, "platform": platform, "model_routing_enabled": true}))
		gp = fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		key = must("POST", "/api/v1/keys", user, map[string]any{"name": platform, "group_id": gid})["key"].(string)
		upstreamPlatform := platform
		if platform == "composite" {
			upstreamPlatform = "openai"
		}
		account("a", upstreamPlatform, gid, 1)
		preferred = account("b", upstreamPlatform, gid, 50)
		public := "public-model"
		if platform == "composite" {
			must("POST", gp+"/composite-routes", admin, map[string]any{"public_model": public, "target_platform": "openai", "upstream_model": "route-alias"})
			public = "route-alias"
		}
		cid = channel(gid, upstreamPlatform, public)
		route(preferred)
		path := "/v1/chat/completions"
		if platform == "anthropic" {
			path = "/v1/messages"
			body["max_tokens"] = 16
		}
		check(call("POST", path, key, body), preferred)
		if platform == "composite" {
			account("a", "anthropic", gid, 1)
			must("POST", gp+"/composite-routes", admin, map[string]any{"public_model": "public-model", "target_platform": "anthropic", "upstream_model": "route-alias", "endpoint": "messages"})
			must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"model_mapping": map[string]any{"openai": map[string]string{"route-alias": "pool-model"}, "anthropic": map[string]string{"route-alias": "pool-model"}}})
			manifest(float64(16000))
		}
	}
}
