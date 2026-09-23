package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForQueue(t *testing.T, a *App, kind string, id int64, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		a.gatewayMu.Lock()
		got := a.gatewayWaiting[fmt.Sprintf("%s:%d", kind, id)]
		a.gatewayMu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("queue count did not reach", kind, id, want)
}

func TestConcurrencyQueue(t *testing.T) {
	status := func(err error, want int) {
		t.Helper()
		var e *apiError
		if !errors.As(err, &e) || e.status != want {
			t.Fatal("queue error", err, want)
		}
	}
	for _, kind := range []string{"user", "account"} {
		a := &App{}
		if !a.takeSlot(kind, 1, 1) {
			t.Fatal("initial slot")
		}
		attempt := func(context.Context) (bool, error) { return a.takeSlot(kind, 1, 1), nil }
		_, err := a.waitAdmission(context.Background(), kind, 1, 10*time.Millisecond, attempt, nil)
		status(err, 429)
		waitForQueue(t, a, kind, 1, 0)
		limit := 20
		if kind == "account" {
			limit = 100
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, limit)
		for i := 0; i < limit; i++ {
			go func() {
				_, err := a.waitAdmission(ctx, kind, 1, 5*time.Second, attempt, nil)
				done <- err
			}()
		}
		waitForQueue(t, a, kind, 1, limit)
		_, err = a.waitAdmission(ctx, kind, 1, time.Second, attempt, nil)
		status(err, 429)
		cancel()
		for i := 0; i < limit; i++ {
			status(<-done, 499)
		}
		waitForQueue(t, a, kind, 1, 0)
		a.releaseSlot(kind, 1)
	}
	a := &App{gatewayQueued: 128}
	_, err := a.waitAdmission(context.Background(), "user", 2, time.Second, func(context.Context) (bool, error) { return false, nil }, nil)
	status(err, 429)
	// Concurrent release/cancel must neither oversubscribe nor strand slots.
	a = &App{}
	var active, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.waitAdmission(context.Background(), "user", 1, 5*time.Second, func(context.Context) (bool, error) { return a.takeSlot("user", 1, 2), nil }, nil)
			if err != nil {
				t.Error(err)
				return
			}
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			a.releaseSlot("user", 1)
		}()
	}
	wg.Wait()
	if peak.Load() > 2 || len(a.gatewayActive) != 0 || len(a.gatewayWaiting) != 0 || a.gatewayQueued != 0 {
		t.Fatal("queue oversubscribed or leaked")
	}
	a.takeSlot("user", 1, 1)
	done := make(chan error, 1)
	go func() {
		_, err := a.waitAdmission(context.Background(), "user", 1, time.Minute, func(context.Context) (bool, error) { return a.takeSlot("user", 1, 1), nil }, nil)
		done <- err
	}()
	waitForQueue(t, a, "user", 1, 1)
	a.StopAdmission()
	status(<-done, 503)
	a.releaseSlot("user", 1)
	if a.takeSlot("user", 1, 1) || len(a.gatewayActive) != 0 || a.gatewayQueued != 0 {
		t.Fatal("shutdown admitted work or leaked state")
	}
}

func testGatewayQueues(t *testing.T, a *App, admin string) {
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.99:1234"
		r.Header.Set("Authorization", "Bearer "+token)
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
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "queue@example.test", "password": "queue-password", "balance": 100, "concurrency": 1}))
	upath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "queue@example.test", "password": "queue-password"})["access_token"].(string)
	price := func(input string) []any {
		return []any{map[string]any{"platform": "openai", "models": []string{"queue-model"}, "input_price": input, "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}
	}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Queue", "platform": "openai", "model_pricing": price("0.001")}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Queue", "group_id": gid, "quota": 100})
	key, kid := k["key"].(string), id(k)
	kp := fmt.Sprintf("/api/v1/keys/%d", kid)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct{ Stream bool }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path == "/v1/responses" {
			fmt.Fprintf(w, `{"id":"resp_queue_%d","object":"response","status":"completed","model":"queue-model","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n)
		} else if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"model\":\"queue-model\",\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, `{"model":"queue-model","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		}
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Queue", "platform": "openai", "type": "apikey", "concurrency": 1, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "queue-upstream-secret", "base_url": up.URL}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "queue-model", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
	begin := func(kind string, slot int64) <-chan *httptest.ResponseRecorder {
		t.Helper()
		if !a.takeSlot(kind, slot, 1) {
			t.Fatal("occupy queue slot")
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- call("POST", "/v1/chat/completions", key, body) }()
		waitForQueue(t, a, kind, slot, 1)
		return done
	}
	finish := func(done <-chan *httptest.ResponseRecorder, kind string, slot int64, code int) *httptest.ResponseRecorder {
		t.Helper()
		a.releaseSlot(kind, slot)
		select {
		case w := <-done:
			if w.Code != code {
				t.Fatal("queued response", code, w.Code, w.Body.String())
			}
			waitForQueue(t, a, kind, slot, 0)
			return w
		case <-time.After(5 * time.Second):
			t.Fatal("queued request did not finish")
			return nil
		}
	}
	checkCost := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		var cost string
		if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost); err != nil || cost != want {
			t.Fatal("queued billing", cost, want, err)
		}
	}
	done := begin("user", uid)
	users := must("GET", "/api/v1/admin/ops/user-concurrency", admin, nil)["user"].(map[string]any)
	load := users[fmt.Sprint(uid)].(map[string]any)
	if load["current_in_use"] != float64(1) || load["waiting_in_queue"] != float64(1) || load["load_percentage"] != float64(100) || load["user_email"] != "queue@example.test" {
		t.Fatal("user concurrency diagnostics", load)
	}
	// Pricing is captured only once the user's concurrency is admitted.
	must("PUT", gp, admin, map[string]any{"model_pricing": price("0.003")})
	checkCost(finish(done, "user", uid, 200), "0.0400000000")
	must("PUT", gp, admin, map[string]any{"model_pricing": price("0.001")})
	done = begin("account", aid)
	stats := must("GET", "/api/v1/admin/ops/concurrency?group_id="+fmt.Sprint(gid), admin, nil)
	load = stats["account"].(map[string]any)[fmt.Sprint(aid)].(map[string]any)
	if load["current_in_use"] != float64(1) || load["waiting_in_queue"] != float64(1) || load["account_id"] != float64(aid) || load["group_id"] != float64(gid) {
		t.Fatal("account queue diagnostics", load)
	}
	for _, part := range []string{"group", "platform"} {
		lookup := fmt.Sprint(gid)
		if part == "platform" {
			lookup = "openai"
		}
		entry := stats[part].(map[string]any)[lookup].(map[string]any)
		if entry["current_in_use"] != float64(1) || entry["waiting_in_queue"] != float64(1) || entry["max_capacity"] != float64(1) {
			t.Fatal("aggregate queue diagnostics", part, entry)
		}
	}
	checkCost(finish(done, "account", aid, 200), "0.0200000000")
	for _, path := range []string{"/api/v1/admin/ops/concurrency", "/api/v1/admin/ops/user-concurrency"} {
		if w := call("GET", path, user, nil); w.Code != 403 {
			t.Fatal("user can access queue diagnostics", path, w.Code)
		}
	}
	for _, query := range []string{"?group_id=-1", "?group_id=abc", "?platform=unsupported"} {
		if w := call("GET", "/api/v1/admin/ops/concurrency"+query, admin, nil); w.Code != 400 {
			t.Fatal("invalid queue filter", w.Code)
		}
	}
	// Raising concurrency admits the queued user without releasing the old slot.
	done = begin("user", uid)
	must("PUT", upath, admin, map[string]any{"concurrency": 2})
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatal("increased concurrency", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		a.releaseSlot("user", uid)
		t.Fatal("increased concurrency did not wake user")
	}
	a.releaseSlot("user", uid)
	must("PUT", upath, admin, map[string]any{"concurrency": 1})
	// Every revocation is made while a real request is waiting; none dispatch.
	for _, tc := range []struct {
		name, kind, path string
		patch, reset     map[string]any
		code             int
	}{
		{"key disabled", "user", kp, map[string]any{"status": "inactive"}, map[string]any{"status": "active"}, 401},
		{"user disabled", "user", upath, map[string]any{"status": "disabled"}, map[string]any{"status": "active"}, 401},
		{"group disabled", "account", gp, map[string]any{"status": "inactive"}, map[string]any{"status": "active"}, 403},
		{"account disabled", "account", ap, map[string]any{"status": "inactive"}, map[string]any{"status": "active"}, 503},
		{"model policy changed", "account", gp, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other"}}}, map[string]any{"model_allowlist": map[string]any{"enabled": false}}, 409},
		{"key IP revoked", "account", kp, map[string]any{"ip_blacklist": []string{"192.0.2.99"}}, map[string]any{"ip_blacklist": []string{}}, 403},
	} {
		slot := uid
		if tc.kind == "account" {
			slot = aid
		}
		done = begin(tc.kind, slot)
		before := calls.Load()
		token := admin
		if tc.path == kp {
			token = user
		}
		must("PUT", tc.path, token, tc.patch)
		finish(done, tc.kind, slot, tc.code)
		if calls.Load() != before {
			t.Fatal(tc.name, "reached upstream")
		}
		if tc.name == "user disabled" {
			must("PUT", tc.path, admin, tc.reset)
			user = must("POST", "/api/v1/auth/login", "", map[string]any{"email": "queue@example.test", "password": "queue-password"})["access_token"].(string)
		} else {
			must("PUT", tc.path, token, tc.reset)
		}
	}
	done = begin("account", aid)
	must("POST", upath+"/balance", admin, map[string]any{"operation": "set", "balance": 0})
	finish(done, "account", aid, 402)
	must("POST", upath+"/balance", admin, map[string]any{"operation": "set", "balance": 100})
	done = begin("account", aid)
	must("PUT", upath, admin, map[string]any{"rpm_limit": 1})
	finish(done, "account", aid, 429)
	must("PUT", upath, admin, map[string]any{"rpm_limit": 0})
	done = begin("account", aid)
	must("PUT", upath+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": 0}}})
	finish(done, "account", aid, 429)
	must("PUT", upath+"/platform-quotas", admin, map[string]any{"quotas": []any{map[string]any{"platform": "openai", "daily_limit_usd": nil}}})
	// A live HTTP stream flushes waiting heartbeats and still settles once.
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	streamRaw, _ := json.Marshal(map[string]any{"model": "queue-model", "stream": true, "messages": body["messages"]})
	liveRequest := func() *http.Request {
		r, err := http.NewRequest("POST", server.URL+"/v1/chat/completions", bytes.NewReader(streamRaw))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", "queued-stream")
		return r
	}
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy stream account")
	}
	liveDone := make(chan *http.Response, 1)
	liveErr := make(chan error, 1)
	before := calls.Load()
	go func() {
		resp, err := server.Client().Do(liveRequest())
		if err != nil {
			liveErr <- err
		} else {
			liveDone <- resp
		}
	}()
	waitForQueue(t, a, "account", aid, 1)
	duplicate, err := server.Client().Do(liveRequest())
	if err != nil {
		t.Fatal(err)
	}
	duplicate.Body.Close()
	if duplicate.StatusCode != 409 {
		t.Fatal("queued idempotency duplicated", duplicate.StatusCode)
	}
	var streamResponse *http.Response
	select {
	case streamResponse = <-liveDone:
	case err := <-liveErr:
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		a.releaseSlot("account", aid)
		t.Fatal("waiting SSE heartbeat not flushed")
	}
	reader := bufio.NewReader(streamResponse.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, ": waiting") || calls.Load() != before {
		t.Fatal("queue heartbeat or premature dispatch", line, err)
	}
	a.releaseSlot("account", aid)
	remaining, err := io.ReadAll(reader)
	streamResponse.Body.Close()
	if err != nil || !strings.Contains(string(remaining), "[DONE]") || strings.Contains(string(remaining), "error") || calls.Load() != before+1 {
		t.Fatal("queued stream completion", string(remaining), err)
	}
	replay, err := server.Client().Do(liveRequest())
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := io.ReadAll(replay.Body)
	replay.Body.Close()
	if err != nil || replay.Header.Get("Idempotency-Replayed") != "true" || string(replayed) != line+string(remaining) || calls.Load() != before+1 {
		t.Fatal("queued stream replay changed", err)
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", streamResponse.Header.Get("X-Request-ID")).Scan(&count); err != nil || count != 1 {
		t.Fatal("queued stream billed incorrectly", count, err)
	}
	// A bound Responses account waits even when an unrelated account is free.
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	responseBody := map[string]any{"model": "queue-model", "input": "hello"}
	w := call("POST", "/v1/responses", key, responseBody)
	var response struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
		t.Fatal("initial response", w.Code, w.Body.String())
	}
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Queue second", "platform": "openai", "type": "apikey", "priority": 0, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "other-secret", "api_protocol": "responses", "base_url": up.URL}})
	responseBody["previous_response_id"] = response.ID
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy bound response")
	}
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	before = calls.Load()
	go func() { responseDone <- call("POST", "/v1/responses", key, responseBody) }()
	waitForQueue(t, a, "account", aid, 1)
	if calls.Load() != before {
		t.Fatal("response escaped bound account while queued")
	}
	w = finish(responseDone, "account", aid, 200)
	var gotAccount int64
	if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&gotAccount); err != nil || gotAccount != aid {
		t.Fatal("response moved account", gotAccount, err)
	}
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy rotated response")
	}
	go func() { responseDone <- call("POST", "/v1/responses", key, responseBody) }()
	waitForQueue(t, a, "account", aid, 1)
	before = calls.Load()
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-secret"}})
	finish(responseDone, "account", aid, 503)
	if calls.Load() != before {
		t.Fatal("queued response used rotated identity")
	}
	// Shared account membership must not double the platform total.
	shared := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Queue shared", "platform": "composite"}))
	must("PUT", ap, admin, map[string]any{"group_ids": []int64{gid, shared}})
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy shared account")
	}
	stats = must("GET", "/api/v1/admin/ops/concurrency?platform=openai", admin, nil)
	for _, group := range []int64{gid, shared} {
		load = stats["group"].(map[string]any)[fmt.Sprint(group)].(map[string]any)
		if load["current_in_use"] != float64(1) {
			t.Fatal("shared group capacity", load)
		}
	}
	load = stats["platform"].(map[string]any)["openai"].(map[string]any)
	if load["current_in_use"] != float64(1) {
		t.Fatal("platform double counted shared account", load)
	}
	a.releaseSlot("account", aid)
	// Reassigning a key while queued requires a new request in the new group.
	keyPatch := map[string]any{"group_id": shared}
	done = begin("user", uid)
	before = calls.Load()
	must("PUT", kp, user, keyPatch)
	finish(done, "user", uid, 409)
	if calls.Load() != before {
		t.Fatal("queued key followed a changed assignment")
	}
	must("PUT", kp, user, map[string]any{"group_id": gid})
	// Composite decisions are rechecked during an account wait, including an
	// administrator changing only the route table (not the group row).
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
	sharedPath := fmt.Sprintf("/api/v1/admin/groups/%d", shared)
	must("PUT", sharedPath, admin, map[string]any{"model_pricing": price("0.001")})
	route := must("POST", sharedPath+"/composite-routes", admin, map[string]any{"public_model": "queue-model", "target_platform": "openai", "upstream_model": "queue-model"})
	sharedKey := must("POST", "/api/v1/keys", user, map[string]any{"name": "Composite queue", "group_id": shared})["key"].(string)
	oldKey := key
	key = sharedKey
	done = begin("account", aid)
	before = calls.Load()
	must("PUT", fmt.Sprintf("%s/composite-routes/%d", sharedPath, id(route)), admin, map[string]any{"public_model": "queue-model", "target_platform": "openai", "upstream_model": "changed-model"})
	finish(done, "account", aid, 409)
	if calls.Load() != before {
		t.Fatal("queued request kept stale composite route")
	}
	key = oldKey
	a.gatewayMu.Lock()
	active, waiting, queued := len(a.gatewayActive), len(a.gatewayWaiting), a.gatewayQueued
	a.gatewayMu.Unlock()
	if active != 0 || waiting != 0 || queued != 0 {
		t.Fatal("queue cleanup", active, waiting, queued)
	}
}
