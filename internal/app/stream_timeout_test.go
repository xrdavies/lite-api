package app

import (
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

	"github.com/coder/websocket"
	"github.com/redis/go-redis/v9"
)

func TestStreamIntervals(t *testing.T) {
	text, image, err := parseStreamIntervals(Config{})
	if err != nil || text != 180*time.Second || image != 900*time.Second {
		t.Fatal(text, image, err)
	}
	for _, cfg := range []Config{
		{StreamDataIntervalTimeout: "-1"}, {StreamDataIntervalTimeout: "29"}, {StreamDataIntervalTimeout: "301"}, {StreamDataIntervalTimeout: "NaN"},
		{ImageStreamDataIntervalTimeout: "59"}, {ImageStreamDataIntervalTimeout: "1801"},
	} {
		if _, _, err := parseStreamIntervals(cfg); err == nil {
			t.Fatal("invalid stream interval", cfg)
		}
	}
	text, image, err = parseStreamIntervals(Config{StreamDataIntervalTimeout: "0", ImageStreamDataIntervalTimeout: "0"})
	if err != nil || text != 0 || image != 0 {
		t.Fatal("disabled detection", err)
	}
	t.Setenv("GATEWAY_STREAM_DATA_INTERVAL_TIMEOUT", "45")
	t.Setenv("GATEWAY_IMAGE_STREAM_DATA_INTERVAL_TIMEOUT", "120")
	text, image, err = parseStreamIntervals(ConfigFromEnv())
	if err != nil || text != 45*time.Second || image != 120*time.Second {
		t.Fatal("environment intervals", err)
	}
}

func TestIdleStreamBody(t *testing.T) {
	for _, mode := range []string{"timeout", "canceled", "disabled", "slow-downstream"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			stop := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
			defer stop()
			b := &idleStreamBody{ReadCloser: reader, ctx: ctx, cancel: cancel, idle: 20 * time.Millisecond}
			switch mode {
			case "canceled":
				cancel()
			case "disabled":
				b.idle = 0
			case "slow-downstream":
				time.Sleep(60 * time.Millisecond)
			}
			if mode == "disabled" || mode == "slow-downstream" {
				go func() {
					if mode == "disabled" {
						time.Sleep(60 * time.Millisecond)
					}
					_, _ = writer.Write([]byte("data"))
				}()
			}
			data := make([]byte, 4)
			n, err := b.Read(data)
			switch mode {
			case "timeout":
				if !errors.Is(err, errStreamIdle) {
					t.Fatal(err)
				}
			case "canceled":
				if !errors.Is(err, context.Canceled) || errors.Is(err, errStreamIdle) {
					t.Fatal(err)
				}
			default:
				if err != nil || n != 4 || ctx.Err() != nil {
					t.Fatal(n, err, ctx.Err())
				}
			}
		})
	}
}

func testStreamTimeout(t *testing.T, original *App, admin, ordinary string) {
	t.Helper()
	// A separate handler uses short test intervals without mutating live workers.
	a := &App{DB: original.DB, Redis: original.Redis, secret: original.secret, instanceLock: original.instanceLock,
		privateUpstreams: original.privateUpstreams, mux: http.NewServeMux(), streamIdle: 150 * time.Millisecond, imageStreamIdle: 800 * time.Millisecond}
	a.prices.Store(original.prices.Load())
	a.routes()
	defer a.StopAdmission()
	ctx := context.Background()
	const path = "/api/v1/admin/settings/stream-timeout"
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", streamTimeoutSetting)
	call := func(ctx context.Context, method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(ctx)
		r.RemoteAddr = "127.0.0.1:4455"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(ctx, method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	load := func(id int64) *upstreamAccount {
		t.Helper()
		u, err := a.loadAccount(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	policy := func(s streamTimeoutSettings) { manage("PUT", path, admin, s) }
	defaults := streamTimeoutSettings{false, "temp_unsched", 5, 3, 10}
	if got := manage("GET", path, admin, nil); got["enabled"] != false || got["threshold_count"] != float64(3) {
		t.Fatal(got)
	}
	for _, method := range []string{"GET", "PUT"} {
		for token, want := range map[string]int{"": 401, ordinary: 403} {
			if w := call(ctx, method, path, token, defaults, ""); w.Code != want {
				t.Fatal("policy auth", w.Code)
			}
		}
	}
	for _, body := range []any{nil, map[string]any{}, map[string]any{"unsupported": true}, streamTimeoutSettings{true, "invalid", 5, 3, 10}, streamTimeoutSettings{false, "none", 0, 3, 10}, streamTimeoutSettings{true, "temp_unsched", 61, 3, 10}, streamTimeoutSettings{true, "error", 1, 11, 10}, streamTimeoutSettings{true, "error", 1, 1, 61}} {
		if w := call(ctx, "PUT", path, admin, body, ""); w.Code != 400 {
			t.Fatal("invalid policy", w.Code, w.Body.String())
		}
	}
	exec("INSERT INTO settings(key,value) VALUES($1,'broken') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value", streamTimeoutSetting)
	if s, err := a.loadStreamTimeoutSettings(ctx); err != nil || s != defaults {
		t.Fatal(s, err)
	}
	exec(`UPDATE settings SET value='{"enabled":true,"action":"unknown","temp_unsched_minutes":99,"threshold_count":0,"threshold_window_minutes":100}' WHERE key=$1`, streamTimeoutSetting)
	if s, err := a.loadStreamTimeoutSettings(ctx); err != nil || s != (streamTimeoutSettings{true, "temp_unsched", 60, 1, 60}) {
		t.Fatal(s, err)
	}
	policy(streamTimeoutSettings{true, "temp_unsched", 2, 2, 1})
	if strings.Contains(call(ctx, "GET", "/api/v1/settings/public", "", nil, "").Body.String(), "threshold_count") {
		t.Fatal("policy exposed publicly")
	}
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "stream-timeout@example.test", "password": "timeout-password", "balance": 100, "concurrency": 8})
	uid := id(user)
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "stream-timeout@example.test", "password": "timeout-password"})["access_token"].(string)
	var mode, calls atomic.Int32
	cancelReady := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		kind := r.URL.Path
		first, last := `data: {"model":"stream-model","choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`+"\n\n", "data: [DONE]\n\n"
		if strings.Contains(kind, "messages") {
			first = `data: {"type":"message_start","message":{"model":"stream-model","usage":{"input_tokens":2,"output_tokens":3}}}` + "\n\n"
			last = "data: {\"type\":\"message_stop\"}\n\n"
		}
		if strings.Contains(kind, "responses") {
			first = `data: {"type":"response.in_progress","response":{"object":"response","id":"resp_timeout","model":"stream-model","status":"in_progress","usage":{"input_tokens":2,"output_tokens":3}}}` + "\n\n"
			last = `data: {"type":"response.completed","response":{"object":"response","id":"resp_timeout","model":"stream-model","status":"completed","usage":{"input_tokens":2,"output_tokens":3}}}` + "\n\n"
		}
		if strings.Contains(kind, "v1beta") {
			first = `data: {"modelVersion":"stream-model","candidates":[{"index":0,"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}` + "\n\n"
			last = `data: {"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}` + "\n\n"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_ = http.NewResponseController(w).Flush()
		m := mode.Load()
		if m != 4 {
			_, _ = io.WriteString(w, first)
			_ = http.NewResponseController(w).Flush()
		}
		if m == 3 {
			cancelReady <- struct{}{}
		}
		if m == 2 {
			return
		}
		if m == 1 || m == 5 {
			if m == 5 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(300 * time.Millisecond):
				}
			}
			_, _ = io.WriteString(w, last)
			return
		}
		<-r.Context().Done()
	}))
	defer up.Close()
	var lastAccount int64
	for _, protocol := range []string{"chat_completions", "anthropic", "responses", "gemini"} {
		platform := "openai"
		if protocol == "anthropic" || protocol == "gemini" {
			platform = protocol
		}
		gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Idle " + protocol, "platform": platform, "allow_image_generation": true, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"stream-model"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}}))
		credentials := map[string]any{"api_key": "timeout-upstream-secret", "base_url": up.URL}
		if platform == "openai" {
			credentials["api_protocol"] = protocol
		}
		aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Idle " + protocol, "platform": platform, "type": "apikey", "concurrency": 8, "group_ids": []int64{gid}, "credentials": credentials, "extra": map[string]any{"quota_limit": 100}}))
		exec(`UPDATE accounts SET extra=extra || '{"unchanged":"preserve"}'::jsonb WHERE id=$1`, aid)
		k := manage("POST", "/api/v1/keys", token, map[string]any{"name": protocol, "group_id": gid, "quota": 100})
		key := k["key"].(string)
		lastAccount = aid
		endpoint := "/v1/chat/completions"
		body := map[string]any{"model": "stream-model", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": true, "max_tokens": 30}
		switch protocol {
		case "anthropic":
			endpoint = "/v1/messages"
		case "responses":
			endpoint = "/v1/responses"
			body = map[string]any{"model": "stream-model", "input": "hello", "stream": true, "store": false}
		case "gemini":
			endpoint = "/v1beta/models/stream-model:streamGenerateContent"
			body = map[string]any{"contents": []any{map[string]any{"parts": []any{map[string]string{"text": "hello"}}}}}
		}
		original := load(aid)
		mode.Store(0)
		for i := 0; i < 2; i++ {
			w := call(ctx, "POST", endpoint, key, body, fmt.Sprintf("idle-%d", i))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "upstream stream data interval timed out") || strings.Contains(w.Body.String(), lastTerminal(protocol)) {
				t.Fatal("idle failure", protocol, w.Code, w.Body.String())
			}
			var n int
			var cost string
			if err := a.DB.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n, &cost); err != nil || n != 1 || cost != "0.0080000000" {
				t.Fatal("partial consumption", protocol, n, cost, err)
			}
			var status int
			if err := a.DB.QueryRow("SELECT status_code FROM ops_error_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&status); err != nil || status != 504 {
				t.Fatal("timeout audit", status, err)
			}
			if i == 0 {
				if n, err := a.Redis.Get(ctx, streamTimeoutCounter(original)).Int(); err != nil || n != 1 {
					t.Fatal("first timeout count", n, err)
				}
			}
		}
		var until *time.Time
		var reason string
		if err := a.DB.QueryRow("SELECT temp_unschedulable_until,temp_unschedulable_reason FROM accounts WHERE id=$1", aid).Scan(&until, &reason); err != nil || until == nil || time.Until(*until) < time.Minute || !strings.Contains(reason, "stream_timeout") {
			t.Fatal("timeout scheduling", until, reason, err)
		}
		before := calls.Load()
		w := call(ctx, "POST", endpoint, key, body, "idle-1")
		if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
			t.Fatal("timeout replay", w.Code, w.Body.String())
		}
		if w = call(ctx, "POST", endpoint, key, body, "new"); w.Code != 503 || calls.Load() != before {
			t.Fatal("unschedulable selected", w.Code)
		}
		ap := fmt.Sprintf("/api/v1/admin/accounts/%d/recover-state", aid)
		manage("POST", ap, admin, nil)
		for _, m := range []int32{1, 2, 3, 4} {
			mode.Store(m)
			reqCtx, stop := context.WithCancel(ctx)
			if m == 3 {
				go func() {
					select {
					case <-cancelReady:
						stop()
					case <-reqCtx.Done():
					}
				}()
			}
			w = call(reqCtx, "POST", endpoint, key, body, fmt.Sprintf("case-%d", m))
			stop()
			if m == 1 && (w.Code != 200 || strings.Contains(w.Body.String(), `"error"`)) {
				t.Fatal("healthy stream", protocol, w.Code, w.Body.String())
			}
			if m == 4 && (w.Code != 504 || !strings.Contains(w.Body.String(), "interval timed out")) {
				t.Fatal("first byte timeout", protocol, w.Code, w.Body.String())
			}
		}
		if n, err := a.Redis.Get(ctx, streamTimeoutCounter(load(aid))).Int(); err != nil || n != 1 {
			t.Fatal("non-idle failures counted", n, err)
		}
		manage("POST", ap, admin, nil)
		if protocol == "gemini" {
			mode.Store(5)
			body["generationConfig"] = map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}
			if w := call(ctx, "POST", endpoint, key, body, "image-interval"); w.Code != 200 || strings.Contains(w.Body.String(), "timed out") {
				t.Fatal("image interval ignored", w.Code, w.Body.String())
			}
		}
	}
	// Concurrency, stale administrator edits, fixed window expiry, and fallback on
	// Redis failure exercise account policy independently of network timing.
	u := load(lastAccount)
	policy(streamTimeoutSettings{true, "error", 1, 3, 1})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.markStreamTimeout(ctx, u, "stream-model") }()
	}
	wg.Wait()
	if got := load(lastAccount); got.Status != "error" || credentialString(got.Extra, "unchanged") != "preserve" {
		t.Fatal("concurrent timeout state", got.Status)
	}
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d/recover-state", lastAccount)
	manage("POST", ap, admin, nil)
	a.markStreamTimeout(ctx, u, "stale")
	if load(lastAccount).Status != "active" {
		t.Fatal("old timeout undid recovery")
	}
	u = load(lastAccount)
	a.markStreamTimeout(ctx, u, "stream-model")
	if err := a.Redis.PExpire(ctx, streamTimeoutCounter(u), time.Millisecond).Err(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	a.markStreamTimeout(ctx, u, "stream-model")
	if n, _ := a.Redis.Get(ctx, streamTimeoutCounter(u)).Int(); n != 1 {
		t.Fatal("timeout window did not expire", n)
	}
	for _, s := range []streamTimeoutSettings{{false, "error", 1, 1, 1}, {true, "none", 1, 1, 1}} {
		policy(s)
		a.markStreamTimeout(ctx, u, "ignored")
		if load(lastAccount).Status != "active" {
			t.Fatal("disabled action")
		}
	}
	closed := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = closed.Close()
	noRedis := &App{DB: a.DB, Redis: closed}
	policy(streamTimeoutSettings{true, "error", 1, 2, 1})
	noRedis.markStreamTimeout(ctx, u, "stream-model")
	if load(lastAccount).Status != "active" {
		t.Fatal("Redis failure fabricated repeated timeouts")
	}
	policy(streamTimeoutSettings{true, "temp_unsched", 1, 1, 1})
	exec("UPDATE accounts SET temp_unschedulable_until=now()+interval '1 hour',temp_unschedulable_reason='longer' WHERE id=$1", lastAccount)
	a.markStreamTimeout(ctx, u, "stream-model")
	var reason string
	if err := a.DB.QueryRow("SELECT temp_unschedulable_reason FROM accounts WHERE id=$1", lastAccount).Scan(&reason); err != nil || reason != "longer" {
		t.Fatal("shortened prior cooldown", reason, err)
	}
	manage("POST", ap, admin, nil)
	// All known partial consumption, including canceled clients, remains billable.
	var total string
	if err := a.DB.QueryRow("SELECT COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE user_id=$1", uid).Scan(&total); err != nil {
		t.Fatal(err)
	}
	var balance string
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE id=$1", uid).Scan(&balance); err != nil || rat(json.Number(balance)).Add(rat(json.Number(balance)), rat(json.Number(total))).Cmp(rat("100")) != 0 {
		t.Fatal("timeout accounting", balance, total, err)
	}
	testSocketStreamIdle(t, a, admin, token, manage)
}

func lastTerminal(protocol string) string {
	switch protocol {
	case "anthropic":
		return "message_stop"
	case "responses":
		return "response.completed"
	case "gemini":
		return "STOP"
	default:
		return "[DONE]"
	}
}

func testSocketStreamIdle(t *testing.T, a *App, admin, token string, manage func(string, string, string, any) map[string]any) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		_, _, err = c.Read(r.Context())
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.in_progress","response":{"object":"response","id":"resp_gap","status":"in_progress","usage":{"input_tokens":2,"output_tokens":3}}}`))
		_, _, _ = c.Read(r.Context())
	}))
	defer up.Close()
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Socket idle", "platform": "openai", "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"stream-model"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}}))
	aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Socket idle", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"base_url": up.URL, "api_key": "socket-secret", "api_protocol": "responses"}, "extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}}))
	k := manage("POST", "/api/v1/keys", token, map[string]any{"name": "Socket idle", "group_id": gid, "quota": 100})
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, server.URL+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + k["key"].(string)}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"stream-model","input":"hello","store":false}`)); err != nil {
		t.Fatal(err)
	}
	_, first, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(first), "response.in_progress") {
		t.Fatal("WS partial response", string(first), err)
	}
	_, last, err := c.Read(ctx)
	if err != nil || !strings.Contains(string(last), "upstream stream data interval timed out") || strings.Contains(string(last), "response.completed") {
		t.Fatal("WS timeout error", string(last), err)
	}
	var n int
	var cost string
	if err = a.DB.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE api_key_id=$1", id(k)).Scan(&n, &cost); err != nil || n != 1 || cost != "0.0080000000" {
		t.Fatal("WS partial usage", n, cost, err)
	}
	var blocked bool
	if err = a.DB.QueryRow("SELECT temp_unschedulable_until>now() FROM accounts WHERE id=$1", aid).Scan(&blocked); err != nil || !blocked {
		t.Fatal("WS timeout scheduling", blocked, err)
	}
}
