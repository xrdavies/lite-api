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

	"github.com/coder/websocket"
)

func TestResponseSocketValidation(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"type":"response.cancel"}`, `{"type":"response.create","stream_id":""}`, `{"type":"response.create","stream_id":"a/b"}`, `{"type":"response.create","generate":null}`, `{"type":"response.create","generate":"false"}`, `{"type":"response.create","background":true}`} {
		if _, _, err := parseSocketTurn([]byte(raw), nil); err == nil {
			t.Fatal("invalid turn accepted", raw)
		}
	}
	b, turn, err := parseSocketTurn([]byte(`{"type":"response.create","generate":false,"stream_id":"agent.a-1","input":"hi"}`), nil)
	if err != nil || !turn.warmup || turn.streamID != "agent.a-1" || bytes.Contains(b, []byte("stream_id")) || !bytes.Contains(b, []byte(`"stream":true`)) {
		t.Fatal(string(b), turn, err)
	}
	u := &upstreamAccount{Platform: "openai", Credentials: map[string]json.RawMessage{"api_protocol": json.RawMessage(`"responses"`)}, Extra: map[string]json.RawMessage{}}
	if u.supportsResponseSocket() {
		t.Fatal("socket opt in missing")
	}
	u.Extra["responses_websockets_v2_enabled"] = json.RawMessage("true")
	if !u.supportsResponseSocket() {
		t.Fatal("legacy enabled flag")
	}
	u.Extra["openai_apikey_responses_websockets_v2_enabled"] = json.RawMessage("false")
	if u.supportsResponseSocket() {
		t.Fatal("typed flag must override legacy")
	}
	u.Extra["openai_apikey_responses_websockets_v2_mode"] = json.RawMessage(`"passthrough"`)
	if !u.supportsResponseSocket() {
		t.Fatal("explicit mode precedence")
	}
	u.Extra["openai_ws_force_http"] = json.RawMessage("true")
	if u.supportsResponseSocket() {
		t.Fatal("force HTTP ignored")
	}
}

func testResponsesWebSocket(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "127.0.0.1:4321"
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
		var v struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	_ = id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "sockets@example.test", "password": "sockets-password", "balance": 100, "concurrency": 2}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "sockets@example.test", "password": "sockets-password"})["access_token"].(string)
	price := []any{map[string]any{"platform": "openai", "models": []string{"public-ws"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0.0005", "cache_write_price": "0"}}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Sockets", "platform": "openai", "model_pricing": price, "max_reasoning_effort": "low"}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "Sockets", "group_id": gid, "quota": 100})
	key, kid := keyData["key"].(string), id(keyData)
	kp := fmt.Sprintf("/api/v1/keys/%d", kid)
	var handshakes, calls atomic.Int32
	var mode atomic.Int32
	var release = make(chan struct{}, 1)
	started := make(chan struct{}, 1)
	upstreamDone := make(chan struct{}, 32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer native-secret" || r.Header.Get("OpenAI-Beta") == "" || r.Header.Get("Cookie") != "" {
			t.Error("wrong upstream handshake", r.URL.Path)
		}
		if mode.Load() == 6 {
			w.Header().Set("Location", "http://127.0.0.1:1/forbidden")
			w.WriteHeader(302)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		defer func() { upstreamDone <- struct{}{} }()
		handshakes.Add(1)
		c.SetReadLimit(4 << 20)
		for {
			_, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req map[string]json.RawMessage
			if json.Unmarshal(raw, &req) != nil {
				t.Error("invalid forwarded JSON")
				return
			}
			if credentialString(req, "type") != "response.create" || req["stream"] != nil || req["stream_id"] != nil || credentialString(req, "model") != "up-ws" {
				t.Error("wrong forwarded request", string(raw))
			}
			var reasoning struct{ Effort string }
			_ = json.Unmarshal(req["reasoning"], &reasoning)
			if reasoning.Effort != "low" {
				t.Error("effort not enforced", reasoning)
			}
			n := calls.Add(1)
			rid := fmt.Sprintf("resp_ws_%d", n)
			send := func(v any) bool {
				raw, _ := json.Marshal(v)
				return c.Write(r.Context(), websocket.MessageText, raw) == nil
			}
			response := map[string]any{"id": rid, "object": "response", "model": "up-ws", "status": "in_progress"}
			if !send(map[string]any{"type": "response.created", "response": response}) {
				return
			}
			currentMode := mode.Load()
			if currentMode == 1 || currentMode == 2 || currentMode == 7 {
				started <- struct{}{}
				<-release
			}
			if currentMode == 3 {
				return
			}
			if currentMode == 4 {
				send(map[string]any{"type": "error", "error": map[string]string{"message": "secret-upstream-error"}})
				continue
			}
			warm := string(req["generate"]) == "false"
			if !warm {
				send(map[string]any{"type": "response.output_text.delta", "delta": "hello"})
				response["output"] = []any{map[string]any{"type": "message", "id": "msg_" + rid, "role": "assistant", "content": []any{}}}
				response["usage"] = map[string]any{"input_tokens": 10, "output_tokens": 5, "input_tokens_details": map[string]int{"cached_tokens": 2}}
			}
			response["status"] = "completed"
			if currentMode == 5 {
				response["status"] = "failed"
				response["error"] = map[string]string{"message": "failed"}
			}
			if currentMode == 8 {
				delete(response, "usage")
			}
			if currentMode == 9 {
				response["status"] = "incomplete"
			}
			if currentMode == 10 {
				if !strings.Contains(string(req["tools"]), `"execution":"client"`) {
					t.Error("WS tool search declaration lost")
				}
				response["output"] = []any{map[string]any{"type": "tool_search_call", "id": "tsc_ws", "execution": "client", "call_id": "search_ws", "arguments": map[string]string{"goal": "files"}, "status": "completed"}}
			}
			if currentMode == 11 {
				if !strings.Contains(string(req["input"]), `"type":"tool_search_output"`) || !strings.Contains(string(req["input"]), `"name":"files"`) {
					t.Error("WS tool discovery output lost")
				}
				response["output"] = []any{map[string]any{"type": "function_call", "id": "fc_ws", "namespace": "files", "name": "read", "call_id": "read_ws", "arguments": "{}", "status": "completed"}}
			}
			if !send(map[string]any{"type": "response." + response["status"].(string), "response": response}) {
				return
			}
		}
	}))
	defer func() { close(release); up.Close() }()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Socket upstream", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "concurrency": 2, "credentials": map[string]any{"api_key": "native-secret", "base_url": up.URL + "/v1", "api_protocol": "responses", "model_mapping": map[string]string{"public-ws": "up-ws"}}, "extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	dial := func(path, token string, extra http.Header, want int) *websocket.Conn {
		t.Helper()
		if extra == nil {
			extra = http.Header{}
		}
		extra.Set("Authorization", "Bearer "+token)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, resp, err := websocket.Dial(ctx, server.URL+path, &websocket.DialOptions{HTTPHeader: extra})
		if want != 101 {
			if err == nil {
				c.CloseNow()
				t.Fatal("invalid handshake accepted")
			}
			if resp == nil || resp.StatusCode != want {
				t.Fatal("handshake status", resp, err, want)
			}
			return nil
		}
		if err != nil {
			t.Fatal("dial", err)
		}
		t.Cleanup(func() { c.CloseNow() })
		c.SetReadLimit(4 << 20)
		return c
	}
	write := func(c *websocket.Conn, b map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(b)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	read := func(c *websocket.Conn) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, raw, err := c.Read(ctx)
		if err != nil {
			t.Fatal("read", err)
		}
		var v map[string]any
		if json.Unmarshal(raw, &v) != nil {
			t.Fatal(string(raw))
		}
		return v
	}
	terminal := func(c *websocket.Conn) map[string]any {
		t.Helper()
		for i := 0; i < 12; i++ {
			v := read(c)
			switch v["type"] {
			case "response.completed", "response.incomplete", "error":
				return v
			}
		}
		t.Fatal("no terminal")
		return nil
	}
	body := func() map[string]any {
		return map[string]any{"type": "response.create", "model": "public-ws", "input": "hello", "store": false, "reasoning": map[string]string{"effort": "high"}}
	}
	assertResult := func(v map[string]any, typ string) string {
		t.Helper()
		if v["type"] != typ {
			t.Fatal("wrong response", v)
		}
		if r, ok := v["response"].(map[string]any); ok {
			return r["id"].(string)
		}
		return ""
	}
	usageCount := func() int {
		t.Helper()
		var n int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitUsage := func(n int) {
		t.Helper()
		end := time.Now().Add(8 * time.Second)
		for time.Now().Before(end) {
			if usageCount() == n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("usage count", usageCount(), n)
	}
	if w := call("GET", "/v1/responses", key, nil); w.Code != 426 {
		t.Fatal("upgrade required", w.Code)
	}
	dial("/v1/responses", "bad", nil, 401)
	dial("/v1/responses", key, http.Header{"Origin": []string{"https://untrusted.test"}}, 403)
	dial("/v1/responses", key, http.Header{"Idempotency-Key": []string{"connect"}}, 400)
	c := dial("/v1/responses", key, nil, 101)
	b := body()
	b["stream_id"] = "main"
	write(c, b)
	first := assertResult(terminal(c), "response.completed")
	if usageCount() != 1 {
		t.Fatal("completion before settlement")
	}
	var cost, effort, requested string
	var ws bool
	var kind int
	if err := a.DB.QueryRow("SELECT actual_cost::text,reasoning_effort,requested_reasoning_effort,openai_ws_mode,request_type FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&cost, &effort, &requested, &ws, &kind); err != nil || cost != "0.0190000000" || effort != "low" || requested != "high" || !ws || kind != 3 {
		t.Fatal(cost, effort, requested, ws, kind, err)
	}
	b = body()
	b["previous_response_id"] = first
	b["input"] = []any{map[string]string{"type": "item_reference", "id": "msg_" + first}, map[string]string{"type": "function_call_output", "call_id": "call_a", "output": "ok"}}
	write(c, b)
	assertResult(terminal(c), "response.completed")
	if handshakes.Load() != 1 {
		t.Fatal("connection not reused")
	}
	warm := body()
	warm["generate"] = false
	write(c, warm)
	warmID := assertResult(terminal(c), "response.completed")
	if usageCount() != 2 {
		t.Fatal("warmup billed")
	}
	b = body()
	b["previous_response_id"] = warmID
	b["input"] = []any{map[string]string{"id": "msg_" + first}}
	write(c, b)
	assertResult(terminal(c), "response.completed")
	c.CloseNow()
	other := dial("/responses", key, nil, 101)
	b = body()
	b["input"] = []any{map[string]string{"id": "msg_" + first}}
	write(other, b)
	assertResult(terminal(other), "error")
	other.CloseNow()
	// Stored results can reconnect through any alias but cannot cross keys.
	c = dial("/backend-api/codex/responses", key, nil, 101)
	b = body()
	b["store"] = true
	write(c, b)
	stored := assertResult(terminal(c), "response.completed")
	c.CloseNow()
	c = dial("/responses", key, nil, 101)
	b = body()
	b["previous_response_id"] = stored
	b["input"] = []any{map[string]string{"id": "msg_" + stored}}
	write(c, b)
	assertResult(terminal(c), "response.completed")
	c.CloseNow()
	key2 := must("POST", "/api/v1/keys", user, map[string]any{"name": "Other socket", "group_id": gid})["key"].(string)
	other = dial("/v1/responses", key2, nil, 101)
	write(other, b)
	assertResult(terminal(other), "error")
	other.CloseNow()
	// Health, credentials and permission changes are checked on every turn.
	c = dial("/v1/responses", key, nil, 101)
	write(c, body())
	terminal(c)
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	before := calls.Load()
	write(c, body())
	assertResult(terminal(c), "error")
	if calls.Load() != before {
		t.Fatal("stale upstream credential reused")
	}
	c.CloseNow()
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "native-secret"}})
	c = dial("/v1/responses", key, nil, 101)
	must("PUT", kp, user, map[string]any{"status": "inactive"})
	write(c, body())
	assertResult(terminal(c), "error")
	c.CloseNow()
	must("PUT", kp, user, map[string]any{"status": "active"})
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other"}}})
	c = dial("/v1/responses", key, nil, 101)
	before = calls.Load()
	write(c, body())
	assertResult(terminal(c), "error")
	if before != calls.Load() {
		t.Fatal("model deny dispatched")
	}
	c.CloseNow()
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "off"}})
	c = dial("/v1/responses", key, nil, 101)
	write(c, body())
	assertResult(terminal(c), "error")
	c.CloseNow()
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}})
	// Missing usage, failed usage, incomplete status and redirect handling.
	for _, m := range []int32{3, 4, 5, 6, 8, 9} {
		mode.Store(m)
		before = calls.Load()
		count := usageCount()
		c = dial("/v1/responses", key, nil, 101)
		write(c, body())
		v := terminal(c)
		want := "error"
		if m == 9 {
			want = "response.incomplete"
		}
		assertResult(v, want)
		if bytes.Contains(mustJSON(v), []byte("secret-upstream-error")) {
			t.Fatal("upstream secret leaked")
		}
		if calls.Load() > before+1 {
			t.Fatal("failed turn retried")
		}
		if m == 5 || m == 9 {
			waitUsage(count + 1)
		} else if usageCount() != count {
			t.Fatal("invalid response billed")
		}
		c.CloseNow()
	}
	mode.Store(0)
	// Client disconnect after dispatch drains a terminal usage frame and bills once.
	mode.Store(1)
	c = dial("/v1/responses", key, nil, 101)
	count := usageCount()
	write(c, body())
	<-started
	c.CloseNow()
	release <- struct{}{}
	waitUsage(count + 1)
	mode.Store(0)
	// Settlement failures suppress completion and retain a recoverable WS receipt.
	if _, err := a.DB.Exec(fmt.Sprintf("ALTER TABLE usage_logs ADD CONSTRAINT test_ws_settlement CHECK(api_key_id<>%d) NOT VALID", kid)); err != nil {
		t.Fatal(err)
	}
	c = dial("/v1/responses", key, nil, 101)
	count = usageCount()
	write(c, body())
	assertResult(terminal(c), "error")
	c.CloseNow()
	if usageCount() != count {
		t.Fatal("settlement failure wrote usage")
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_ws_settlement"); err != nil {
		t.Fatal(err)
	}
	if err := a.recoverReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitUsage(count + 1)
	if err := a.recoverReceipts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if usageCount() != count+1 {
		t.Fatal("receipt charged twice")
	}
	// Client discovery remains native on a reused socket and bills only text usage.
	mode.Store(10)
	c = dial("/responses", key, nil, 101)
	count = usageCount()
	search := body()
	search["tools"] = []any{json.RawMessage(clientSearchDeclaration)}
	write(c, search)
	event := terminal(c)
	searchID := assertResult(event, "response.completed")
	output := event["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if output["type"] != "tool_search_call" || output["execution"] != "client" {
		t.Fatal("WS client search result", output)
	}
	mode.Store(11)
	search = body()
	search["previous_response_id"] = searchID
	search["input"] = []any{map[string]any{"type": "tool_search_output", "execution": "client", "call_id": "search_ws", "status": "completed", "tools": json.RawMessage(discoveredClientTools)}}
	write(c, search)
	event = terminal(c)
	assertResult(event, "response.completed")
	output = event["response"].(map[string]any)["output"].([]any)[0].(map[string]any)
	if output["namespace"] != "files" || usageCount() != count+2 {
		t.Fatal("WS discovery result/billing", output, usageCount())
	}
	before = calls.Load()
	search = body()
	search["tools"] = []any{map[string]string{"type": "tool_search", "execution": "invalid"}}
	write(c, search)
	assertResult(terminal(c), "error")
	if calls.Load() != before {
		t.Fatal("WS invalid tool search admitted")
	}
	c.CloseNow()
	mode.Store(0)
	// Queued disconnects release user/account wait state without dispatch.
	must("PUT", ap, admin, map[string]any{"concurrency": 1})
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy account")
	}
	c = dial("/v1/responses", key, nil, 101)
	before = calls.Load()
	write(c, body())
	waitForQueue(t, a, "account", aid, 1)
	c.CloseNow()
	waitForQueue(t, a, "account", aid, 0)
	a.releaseSlot("account", aid)
	if calls.Load() != before {
		t.Fatal("canceled queue dispatched")
	}
	// The source key cannot move to another group inside a live connection.
	c = dial("/v1/responses", key, nil, 101)
	gid2 := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Other WS group"}))
	must("PUT", kp, user, map[string]any{"group_id": gid2})
	write(c, body())
	assertResult(terminal(c), "error")
	c.CloseNow()
	must("PUT", kp, user, map[string]any{"group_id": gid})
	// Outbound address validation is also applied at the WS handshake.
	if _, err := a.DB.Exec("UPDATE accounts SET credentials=jsonb_set(credentials,'{base_url}','\"http://169.254.169.254\"'::jsonb) WHERE id=$1", aid); err != nil {
		t.Fatal(err)
	}
	c = dial("/v1/responses", key, nil, 101)
	before = calls.Load()
	write(c, body())
	assertResult(terminal(c), "error")
	c.CloseNow()
	if calls.Load() != before {
		t.Fatal("private target reached")
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"base_url": up.URL + "/v1"}})
	// Capacity includes idle sockets but they do not hold user/account slots.
	var held []*websocket.Conn
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		a.gatewayMu.Lock()
		n := a.gatewayActive[fmt.Sprintf("websocket:%d", kid)]
		a.gatewayMu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 4; i++ {
		held = append(held, dial("/v1/responses", key, nil, 101))
	}
	dial("/v1/responses", key, nil, 429)
	for _, c := range held {
		c.CloseNow()
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
