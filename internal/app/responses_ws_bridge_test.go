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

func TestSocketHTTPBridgeHistory(t *testing.T) {
	s := &responseSocket{mode: "http_bridge"}
	var request map[string]json.RawMessage
	decode := func(raw string) map[string]json.RawMessage {
		t.Helper()
		var out map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	turn := &responseSocketTurn{socket: s}
	raw, err := turn.bridgeBody(decode(`{"input":"secret prompt","generate":false,"stream_id":"lane","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	request = decode(string(raw))
	if request["generate"] != nil || request["stream_id"] != nil || string(request["store"]) != "false" || !bytes.Contains(raw, []byte("reasoning.encrypted_content")) {
		t.Fatal("bridge wire fields", string(raw))
	}
	turn.observeBridge([]byte(`{"type":"response.completed","response":{"output":[{"id":"reason_1","type":"reasoning","encrypted_content":"opaque"},{"id":"call_1","type":"function_call","call_id":"f1","name":"lookup","arguments":"{}"}]}}`))
	s.rememberBridge("resp_1", append(turn.bridgeInput, turn.bridgeOutput...))
	next := &responseSocketTurn{socket: s}
	raw, err = next.bridgeBody(decode(`{"previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"f1","output":"42"}]}`))
	if err != nil || bytes.Contains(raw, []byte("previous_response_id")) || !bytes.Contains(raw, []byte("opaque")) || len(next.bridgeInput) != 4 {
		t.Fatal("lost continuation", string(raw), err)
	}
	if _, err = (&responseSocketTurn{socket: &responseSocket{}}).bridgeBody(decode(`{"previous_response_id":"resp_1"}`)); err == nil {
		t.Fatal("history crossed connections")
	}
	raw, err = next.bridgeBody(decode(`{"input":[{"type":"item_reference","id":"call_1"}]}`))
	if err != nil || !bytes.Contains(raw, []byte("function_call")) || bytes.Contains(raw, []byte("item_reference")) {
		t.Fatal(string(raw), err)
	}
	for _, body := range []string{`{"input":[{"type":"item_reference","id":"foreign"}]}`, `{"input":[null]}`, `{"input":{},"include":[]}`, `{"input":[],"include":42}`} {
		if _, err = next.bridgeBody(decode(body)); err == nil {
			t.Fatal("invalid bridge input", body)
		}
	}
	for i := 0; i < 40; i++ {
		s.rememberBridge(fmt.Sprint(i), []json.RawMessage{json.RawMessage(`{"role":"user","content":"` + strings.Repeat("a", 300000) + `"}`)})
	}
	if s.bridgeBytes > socketBridgeHistoryLimit || len(s.bridgeOrder) > 32 || s.bridgeHistory["resp_1"] != nil {
		t.Fatal("unbounded bridge history")
	}
	_, err = next.bridgeBody(decode(`{"input":"` + strings.Repeat("a", 4<<20) + `"}`))
	if err == nil {
		t.Fatal("oversized bridge input")
	}
}

func testSocketHTTPBridge(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.175:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var v struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "bridge@example.test", "password": "bridge-password", "balance": 100, "concurrency": 4}))
	user := must("POST", "/api/v1/auth/login", "", map[string]string{"email": "bridge@example.test", "password": "bridge-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "HTTP bridge", "platform": "openai", "model_pricing": []any{map[string]any{"models": []string{"bridge", "native-bridge"}, "platform": "openai", "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}}))
	keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "bridge-client", "group_id": gid, "quota": 100})
	key, kid := keyData["key"].(string), id(keyData)
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "other-bridge-client", "group_id": gid})["key"].(string)
	var calls, mode atomic.Int32
	requests := make(chan map[string]json.RawMessage, 32)
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer bridge-secret" || r.Header.Get("Upgrade") != "" || r.Header.Get("Cookie") != "" {
			t.Error("invalid bridge transport")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid bridge JSON")
		}
		requests <- body
		n := calls.Add(1)
		if body["generate"] != nil || body["previous_response_id"] != nil || body["type"] != nil || body["stream_id"] != nil || string(body["stream"]) != "true" || credentialString(body, "model") != "native-bridge" {
			t.Error("invalid bridge fields")
		}
		current := mode.Load()
		if current == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":"private upstream failure"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-ID", fmt.Sprintf("bridge-%d", n))
		response := map[string]any{"id": fmt.Sprintf("resp_bridge_%d", n), "object": "response", "model": "native-bridge", "status": "in_progress"}
		send := func(kind string) {
			raw, _ := json.Marshal(map[string]any{"type": kind, "response": response})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			w.(http.Flusher).Flush()
		}
		send("response.created")
		if current == 2 {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		response["status"] = "completed"
		response["output"] = []any{map[string]any{"id": fmt.Sprintf("fc_bridge_%d", n), "type": "function_call", "call_id": fmt.Sprintf("call_%d", n), "name": "lookup", "arguments": "{}"}}
		if current != 3 {
			response["usage"] = map[string]int{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}
		}
		send("response.completed")
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "HTTP-only upstream", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "bridge-secret", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"bridge": "native-bridge"}}, "extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "http_bridge", upstreamRequestIDHeaderKey: "X-Request-ID"}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	dial := func(path, key string) *websocket.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(ctx, server.URL+path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}, "Cookie": []string{"client-secret"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	write := func(c *websocket.Conn, body map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageText, raw); err != nil {
			t.Fatal(err)
		}
	}
	terminal := func(c *websocket.Conn, want string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		for i := 0; i < 10; i++ {
			_, raw, err := c.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Type     string
				Response struct{ ID string }
			}
			if json.Unmarshal(raw, &event) != nil {
				t.Fatal(string(raw))
			}
			if event.Type == "response.completed" || event.Type == "error" {
				if event.Type != want {
					t.Fatal("unexpected terminal", string(raw))
				}
				return event.Response.ID
			}
		}
		t.Fatal("no terminal")
		return ""
	}
	body := func() map[string]any {
		return map[string]any{"type": "response.create", "model": "bridge", "input": "private input", "store": false}
	}
	count := func() int {
		var n int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	c := dial("/v1/responses", key)
	b := body()
	b["generate"] = false
	write(c, b)
	warm := terminal(c, "response.completed")
	if calls.Load() != 0 || count() != 0 {
		t.Fatal("warmup dispatched or billed")
	}
	b = body()
	b["previous_response_id"] = warm
	b["input"] = "new input"
	write(c, b)
	first := terminal(c, "response.completed")
	request := <-requests
	if !bytes.Contains(request["input"], []byte("private input")) || !bytes.Contains(request["input"], []byte("new input")) || count() != 1 {
		t.Fatal("warmup continuation")
	}
	b = body()
	b["previous_response_id"] = first
	b["input"] = []any{map[string]string{"type": "function_call_output", "call_id": "call_1", "output": "42"}}
	write(c, b)
	terminal(c, "response.completed")
	request = <-requests
	if !bytes.Contains(request["input"], []byte("function_call")) || !bytes.Contains(request["input"], []byte("42")) || count() != 2 {
		t.Fatal("tool continuation")
	}
	b = body()
	b["previous_response_id"] = warm
	b["input"] = "branch"
	write(c, b)
	terminal(c, "response.completed")
	request = <-requests
	if bytes.Contains(request["input"], []byte("42")) || bytes.Contains(request["input"], []byte("function_call")) {
		t.Fatal("fork contaminated parent")
	}
	// Neither another connection nor another Key can borrow store=false history.
	for _, k := range []string{key, other} {
		foreign := dial("/responses", k)
		write(foreign, b)
		terminal(foreign, "error")
		foreign.CloseNow()
	}
	if calls.Load() != 3 {
		t.Fatal("foreign history dispatched")
	}
	var upstreamID *string
	var ws bool
	var requestType int
	if err := a.DB.QueryRow("SELECT upstream_request_id,openai_ws_mode,request_type FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&upstreamID, &ws, &requestType); err != nil || upstreamID != nil || !ws || requestType != 3 {
		t.Fatal("bridge usage metadata", upstreamID, ws, requestType, err)
	}
	// Account mode and client revocation apply to existing connections.
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "off"}})
	write(c, body())
	terminal(c, "error")
	c.CloseNow()
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "http_bridge"}})
	c = dial("/backend-api/codex/responses", key)
	must("PUT", fmt.Sprintf("/api/v1/keys/%d", kid), user, map[string]any{"status": "inactive"})
	write(c, body())
	terminal(c, "error")
	c.CloseNow()
	must("PUT", fmt.Sprintf("/api/v1/keys/%d", kid), user, map[string]any{"status": "active"})
	if calls.Load() != 3 {
		t.Fatal("revoked session dispatched")
	}
	// Missing usage suppresses success, and HTTP failure never resubmits a turn.
	for _, m := range []int32{3, 1} {
		mode.Store(m)
		c = dial("/responses", key)
		write(c, body())
		terminal(c, "error")
		c.CloseNow()
		<-requests
	}
	if calls.Load() != 5 || count() != 3 {
		t.Fatal("failed bridge retried or billed")
	}
	// A 503 marks the account rate limited; clear both independent health states
	// before checking that a disconnected bridge still drains and settles.
	must("POST", ap+"/clear-rate-limit", admin, nil)
	must("POST", ap+"/clear-error", admin, nil)
	mode.Store(2)
	c = dial("/backend-api/codex/responses", key)
	write(c, body())
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("bridge upstream did not start")
	}
	c.CloseNow()
	release <- struct{}{}
	select {
	case <-requests:
	case <-time.After(10 * time.Second):
		t.Fatal("bridge request was not observed")
	}
	deadline := time.Now().Add(8 * time.Second)
	for count() != 4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if count() != 4 {
		t.Fatal("disconnected bridge lost usage")
	}
	mode.Store(0)
	// A SQL failure suppresses completion; existing receipt recovery bills once.
	if _, err := a.DB.Exec(fmt.Sprintf("ALTER TABLE usage_logs ADD CONSTRAINT test_bridge_settlement CHECK(api_key_id<>%d) NOT VALID", kid)); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_bridge_settlement")
	c = dial("/responses", key)
	write(c, body())
	terminal(c, "error")
	c.CloseNow()
	<-requests
	if count() != 4 {
		t.Fatal("failed settlement persisted usage")
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_bridge_settlement"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 5 {
		t.Fatal("bridge receipt missing or duplicated")
	}
	var balance, quota string
	if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1", kid).Scan(&balance, &quota); err != nil || balance != "99.93000000" || quota != "0.07000000" {
		t.Fatal("bridge reconciliation", balance, quota, err)
	}
}
