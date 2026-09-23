package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestGatewaySessionKey(t *testing.T) {
	g := &gatewayIdentity{Key: gatewayKey{ID: 1, GroupID: 2}, Group: gatewayGroup{Platform: "openai"}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	in := textRequest{Protocol: "chat_completions", Model: "model"}
	key := func(raw string) string {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			t.Fatal(err)
		}
		result, err := gatewaySessionKey(r, g, in, body)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := key(`{"messages":[{"role":"system","content":"system"},{"role":"user","content":[{"type":"text","text":"secret prompt"}]}]}`)
	second := key(`{"stream":true,"messages":[{"role":"system","content":"system"},{"role":"user","content":[{"text":"secret prompt","type":"text"}]},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`)
	if first != second || strings.Contains(first, "secret") {
		t.Fatal("unstable or exposed content seed")
	}
	if first == key(`{"messages":[{"role":"user","content":"different"}]}`) {
		t.Fatal("independent prompts share affinity")
	}
	r.Header.Set("Session-Id", "session-private")
	explicit := key(`{"prompt_cache_key":"ignored","messages":[]}`)
	if explicit != key(`{"messages":[{"role":"user","content":"anything"}]}`) || strings.Contains(explicit, "session-private") {
		t.Fatal("explicit session precedence or redaction")
	}
	g.Key.ID++
	if explicit == key(`{}`) {
		t.Fatal("cross-key session collision")
	}
	g.Key.ID--
	g.Key.GroupID++
	if explicit == key(`{}`) {
		t.Fatal("cross-group session collision")
	}
	g.Key.GroupID--
	in.Protocol = "responses"
	if explicit == key(`{}`) {
		t.Fatal("cross-protocol session collision")
	}
	r.Header.Del("Session-Id")
	r.Header.Set("X-Grok-Conv-Id", "grok-session")
	if key(`{"input":"a"}`) == key(`{"input":"b"}`) {
		t.Fatal("Grok header applies to other platforms")
	}
	g.Group.Platform = "grok"
	grok := key(`{"input":"a"}`)
	if grok != key(`{"input":"b"}`) {
		t.Fatal("Grok header affinity ignored")
	}
	in.Model = "other-model"
	if grok == key(`{"input":"a"}`) {
		t.Fatal("Grok model isolation missing")
	}
	r.Header.Set("Session-Id", strings.Repeat("x", 1025))
	if _, err := gatewaySessionKey(r, g, in, nil); err == nil {
		t.Fatal("oversized session header accepted")
	}
	r.Header = http.Header{}
	in.Protocol, g.Group.Platform = "anthropic", "anthropic"
	session := "12345678-1234-1234-1234-123456789abc"
	legacy := key(fmt.Sprintf(`{"metadata":{"user_id":"user_%s_account__session_%s"}}`, strings.Repeat("a", 64), session))
	modern := key(`{"metadata":{"user_id":"{\"device_id\":\"device\",\"session_id\":\"` + session + `\"}"}}`)
	if legacy != modern {
		t.Fatal("metadata session formats differ")
	}
	cached := key(`{"system":[{"type":"text","text":"cached system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"one"}]}`)
	if cached != key(`{"system":[{"type":"text","text":"cached system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"two"}]}`) {
		t.Fatal("cacheable Anthropic prefix not reused")
	}
	if cached == key(`{"system":[{"type":"text","text":"cached system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"cached message","cache_control":{"type":"ephemeral"}}]}]}`) {
		t.Fatal("cached message did not override cached system")
	}
	in.CountOnly = true
	if key(`{}`) != "" {
		t.Fatal("token count changed affinity")
	}
	in.CountOnly, in.Protocol = false, "embeddings"
	if key(`{"input":"embedding"}`) != "" {
		t.Fatal("embedding created a conversation")
	}
	// Redis failure is surfaced before upstream dispatch, not silently treated as
	// a new conversation. No network connection is needed for the closed client.
	cache := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = cache.Close()
	a := &App{Redis: cache}
	if _, err := a.stickySession(context.Background(), "key"); err == nil {
		t.Fatal("Redis read failure ignored")
	}
	if err := a.bindSession(context.Background(), "key", &upstreamAccount{}); err == nil {
		t.Fatal("Redis write failure ignored")
	}
}

func testStickyGateway(t *testing.T, a *App, admin string) {
	ctx := context.Background()
	call := func(method, path, token string, body any, session, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.98:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Session-Id", session)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "", "")
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	idOf := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "sticky@example.test", "password": "sticky-password", "balance": "100"}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "sticky@example.test", "password": "sticky-password"})["access_token"].(string)
	group := func(name, platform string) int64 {
		return idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": name, "platform": platform, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"sticky-model"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}}))
	}
	gid := group("Sticky", "openai")
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	newKey := func(group int64) map[string]any {
		return must("POST", "/api/v1/keys", user, map[string]any{"name": "Sticky", "group_id": group, "quota": 100})
	}
	k := newKey(gid)
	key, kid := k["key"].(string), idOf(k)
	otherKey := newKey(gid)["key"].(string)
	var calls atomic.Int32
	var failA atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Session-Id") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("local affinity or idempotency header leaked")
		}
		label := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer sticky-secret-")
		if label == "" {
			label = strings.TrimPrefix(r.Header.Get("X-Api-Key"), "sticky-secret-")
		}
		if label == "a" && failA.Load() != 0 {
			w.WriteHeader(int(failA.Load()))
			return
		}
		var body struct {
			Model  string
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "sticky-model" {
			t.Error("invalid upstream body", err)
		}
		switch r.URL.Path {
		case "/v1/responses":
			fmt.Fprintf(w, `{"id":"resp_sticky_%d","object":"response","status":"completed","model":"sticky-model","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n)
		case "/v1/messages":
			fmt.Fprint(w, `{"id":"msg_sticky","type":"message","role":"assistant","model":"sticky-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
		default:
			if body.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"model\":\"sticky-model\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, `{"model":"sticky-model","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
			}
		}
	}))
	defer up.Close()
	account := func(label, platform, protocol string, group int64, priority int) int64 {
		return idOf(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Sticky " + label, "platform": platform, "type": "apikey", "group_ids": []int64{group}, "priority": priority, "concurrency": 1, "credentials": map[string]any{"api_key": "sticky-secret-" + label, "base_url": up.URL, "api_protocol": protocol}}))
	}
	first, second := account("a", "openai", "chat_completions", gid, 1), account("b", "openai", "chat_completions", gid, 20)
	ap, bp := fmt.Sprintf("/api/v1/admin/accounts/%d", first), fmt.Sprintf("/api/v1/admin/accounts/%d", second)
	body := map[string]any{"model": "sticky-model", "messages": []any{map[string]any{"role": "user", "content": "private prompt"}}}
	check := func(w *httptest.ResponseRecorder, want int64) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal("sticky request", w.Code, w.Body.String())
		}
		var got, group int64
		var cost string
		if err := a.DB.QueryRow(`SELECT account_id,group_id,actual_cost::text FROM usage_logs WHERE user_id=$1 AND request_id=$2`, uid, w.Header().Get("X-Request-ID")).Scan(&got, &group, &cost); err != nil || got != want || group != gid || cost != "0.0200000000" {
			t.Fatal("sticky routing or billing", got, want, group, gid, cost, err)
		}
	}
	request := func(session string) *httptest.ResponseRecorder {
		return call("POST", "/v1/chat/completions", key, body, session, "")
	}
	check(request("private-session"), first)
	must("PUT", bp, admin, map[string]any{"priority": 0, "group_ids": []int64{gid}})
	check(request("private-session"), first)
	check(call("POST", "/v1/chat/completions", otherKey, body, "private-session", ""), second)
	check(request("other-session"), second)
	// Implicit content affinity stays stable when later conversation turns append.
	check(request(""), second)
	must("PUT", bp, admin, map[string]any{"priority": 20, "group_ids": []int64{gid}})
	body["messages"] = []any{map[string]any{"role": "user", "content": "private prompt"}, map[string]any{"role": "assistant", "content": "ok"}, map[string]any{"role": "user", "content": "next"}}
	check(request(""), second)
	cacheKey := fmt.Sprintf("gateway:session:%d:%d:openai:chat_completions:%s", kid, gid, digest("explicit:private-session"))
	raw, err := a.Redis.Get(ctx, cacheKey).Result()
	if err != nil || strings.Contains(raw, "private") || strings.Contains(raw, "sticky-secret") {
		t.Fatal("affinity missing or leaking", err)
	}
	if ttl := a.Redis.TTL(ctx, cacheKey).Val(); ttl < 59*time.Minute || ttl > time.Hour {
		t.Fatal("affinity TTL", ttl)
	}
	if !a.takeSlot("account", first, 1) {
		t.Fatal("occupy account")
	}
	w := request("private-session")
	a.releaseSlot("account", first)
	check(w, second)
	check(request("private-session"), second)
	// Expiry and corrupt soft metadata resume normal scheduling.
	if err = a.Redis.PExpire(ctx, cacheKey, -time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	check(request("private-session"), first)
	if err = a.Redis.Set(ctx, cacheKey, `{"AccountID":-1}`, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	check(request("private-session"), first)
	// Administrative routing takes precedence over a different sticky pool.
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"sticky-model": {second}}})
	check(request("private-session"), second)
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": false})
	must("PUT", bp, admin, map[string]any{"status": "inactive"})
	check(request("private-session"), first)
	must("PUT", bp, admin, map[string]any{"status": "active"})
	// Affinity cannot bypass an account's model, expiration or spending gates.
	for _, patch := range []map[string]any{
		{"credentials": map[string]any{"model_mapping": map[string]string{"unrelated": "sticky-model"}}},
		{"expires_at": 1577836800},
		{"extra": map[string]any{"quota_limit": "0.001"}},
	} {
		if err = a.Redis.Del(ctx, cacheKey).Err(); err != nil {
			t.Fatal(err)
		}
		check(request("private-session"), first)
		must("PUT", ap, admin, patch)
		if patch["extra"] != nil {
			if _, err = a.DB.Exec(`UPDATE accounts SET extra=extra||'{"quota_used":1}'::jsonb WHERE id=$1`, first); err != nil {
				t.Fatal(err)
			}
		}
		check(request("private-session"), second)
		must("PUT", ap, admin, map[string]any{"expires_at": 0, "credentials": map[string]any{"model_mapping": map[string]string{}}, "extra": map[string]any{"quota_limit": "0"}})
	}
	if err = a.Redis.Del(ctx, cacheKey).Err(); err != nil {
		t.Fatal(err)
	}
	check(request("private-session"), first)
	// Within a model priority pool, a bound eligible account precedes account rank.
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string][]int64{"sticky-model": {first, second}}})
	must("PUT", bp, admin, map[string]any{"priority": 0, "group_ids": []int64{gid}})
	check(request("private-session"), first)
	must("PUT", bp, admin, map[string]any{"priority": 20, "group_ids": []int64{gid}})
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": false, "model_routing": map[string][]int64{"sticky-model": {second}}})
	// Retry only once per failed account and replace the sticky preference.
	failA.Store(503)
	before := calls.Load()
	check(request("private-session"), second)
	if calls.Load() != before+2 {
		t.Fatal("sticky retry loop")
	}
	failA.Store(0)
	must("POST", ap+"/clear-rate-limit", admin, nil)
	check(request("private-session"), second)
	// A rotated credential invalidates the old preference even at the same ID.
	check(request("rotate-session"), first)
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "sticky-secret-rotated"}})
	must("PUT", bp, admin, map[string]any{"priority": 0, "group_ids": []int64{gid}})
	check(request("rotate-session"), second)
	must("PUT", bp, admin, map[string]any{"priority": 20, "group_ids": []int64{gid}})
	// A malicious/corrupt cross-group binding never grants access to that account.
	outside := group("Sticky outside", "openai")
	outsider := account("outside", "openai", "chat_completions", outside, 0)
	u, err := a.loadAccount(ctx, outsider)
	if err != nil || a.bindSession(ctx, cacheKey, u) != nil {
		t.Fatal("prepare outside binding", err)
	}
	check(request("private-session"), first)
	body["stream"] = true
	w = call("POST", "/v1/chat/completions", key, body, "stream-session", "sticky-replay")
	check(w, first)
	if !strings.Contains(w.Body.String(), "[DONE]") {
		t.Fatal("sticky SSE incomplete")
	}
	before = calls.Load()
	replay := call("POST", "/v1/chat/completions", key, body, "changed-session", "sticky-replay")
	if replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != w.Body.String() || calls.Load() != before {
		t.Fatal("replay redispatched or altered")
	}
	delete(body, "stream")
	// Responses continuation outranks both a conflicting session and model pool.
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	must("PUT", bp, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	responseBody := map[string]any{"model": "sticky-model", "input": "hello"}
	w = call("POST", "/v1/responses", key, responseBody, "responses-session", "")
	check(w, first)
	var response struct{ ID string }
	if err = json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": true})
	check(call("POST", "/v1/responses", key, responseBody, "responses-session", ""), second)
	responseBody["previous_response_id"] = response.ID
	check(call("POST", "/v1/responses", key, responseBody, "responses-session", ""), first)
	must("PUT", ap, admin, map[string]any{"status": "inactive"})
	before = calls.Load()
	w = call("POST", "/v1/responses", key, responseBody, "responses-session", "")
	if w.Code != 503 || calls.Load() != before {
		t.Fatal("soft affinity overrode mandatory Responses binding", w.Code)
	}
	// Anthropic metadata affinity uses the same admission and billing path.
	gid = group("Sticky Anthropic", "anthropic")
	key = newKey(gid)["key"].(string)
	first = account("claude-a", "anthropic", "anthropic", gid, 1)
	second = account("claude-b", "anthropic", "anthropic", gid, 20)
	msg := map[string]any{"model": "sticky-model", "max_tokens": 20, "metadata": map[string]any{"user_id": `{"device_id":"device","session_id":"claude-session"}`}, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	check(call("POST", "/v1/messages", key, msg, "", ""), first)
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", second), admin, map[string]any{"priority": 0, "group_ids": []int64{gid}})
	msg["messages"] = []any{map[string]any{"role": "user", "content": "next turn"}}
	check(call("POST", "/v1/messages", key, msg, "", ""), first)
}
