package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRealtimeEvents(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"type":1}`, `{"type":"session.update","session":null}`, `{"type":"session.update","Session":{"model":"other"}}`, `{"type":"session.update","session":{"model":"other"}}`, `{"type":"session.update","session":{"Model":"grok-voice-latest"}}`, `{"type":"session.update","session":{"resumption":{"enabled":true}}}`, `{"type":"session.update","resumption":{"enabled":false,"Enabled":true}}`, `{"type":"response.create","response":{"conversation_id":"foreign"}}`, `{"type":"input_audio_buffer.append","audio":"not base64"}`} {
		if _, _, err := realtimeEvent(websocket.MessageText, []byte(raw), "grok-voice-latest", true); err == nil {
			t.Fatal("unsafe realtime event accepted", raw)
		}
	}
	raw, audio, err := realtimeEvent(websocket.MessageText, []byte(`{"type":"session.update","session":{"model":"other","model":"grok-voice-latest","resumption":{"enabled":true,"enabled":false}}}`), "grok-voice-latest", true)
	if err != nil || audio || bytes.Contains(raw, []byte(`"other"`)) || bytes.Contains(raw, []byte(`true`)) {
		t.Fatal("ambiguous model/resumption", string(raw), err)
	}
	for _, kind := range []websocket.MessageType{websocket.MessageText, websocket.MessageBinary} {
		raw := []byte{0, 255, 128, 1}
		if kind == websocket.MessageText {
			raw = []byte(`{"type":"response.output_audio.delta","delta":"AP+AAQ=="}`)
		}
		out, audio, err := realtimeEvent(kind, raw, "grok-voice-latest", false)
		if err != nil || !audio || !bytes.Equal(out, raw) && kind == websocket.MessageBinary {
			t.Fatal("audio corrupted", audio, err)
		}
	}
	if _, audio, err := realtimeEvent(websocket.MessageText, []byte(`{"type":"response.audio_transcript.delta","delta":"plain text"}`), "grok-voice-latest", false); err != nil || audio {
		t.Fatal("transcript billed as audio", err)
	}
	if _, _, err := realtimeEvent(websocket.MessageText, []byte(`{"type":"error","error":{"message":"secret"}}`), "grok-voice-latest", false); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("upstream secret error", err)
	}
	for _, tc := range []struct {
		price *json.Number
		total string
	}{{nil, "0.0750000000"}, {number("0"), "0.0000000000"}, {number("2"), "3.0000000000"}} {
		cost, err := (gatewayGroup{audioPrices: audioPrices{Realtime: tc.price}, Rate: "2"}).audioCost("realtime", "3/2")
		if err != nil || cost.Total != tc.total || rat(json.Number(cost.Actual)).Cmp(new(big.Rat).Mul(rat(json.Number(tc.total)), big.NewRat(2, 1))) != 0 {
			t.Fatal("realtime price", cost, err)
		}
	}
}

func testGrokRealtime(t *testing.T, a *App, admin string) {
	t.Helper()
	ctx := context.Background()
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.191:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		var v struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "realtime@example.test", "password": "realtime-password", "balance": 100, "concurrency": 5}))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "realtime@example.test", "password": "realtime-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Realtime", "platform": "grok", "rate_multiplier": 2, "audio_realtime_price_per_min": 60}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	kd := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Realtime", "group_id": gid, "quota": 100, "rate_limit_5h": 100})
	key, kid := kd["key"].(string), id(kd)
	kp := fmt.Sprintf("/api/v1/keys/%d", kid)
	var calls, primaryStatus, mode atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/realtime" || r.URL.RawQuery != "model=grok-voice-latest" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("realtime path or credential isolation", r.URL.String())
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer voice-primary" && auth != "Bearer voice-backup" {
			t.Error("upstream key replacement")
		}
		if auth == "Bearer voice-primary" && primaryStatus.Load() != 0 {
			w.WriteHeader(int(primaryStatus.Load()))
			return
		}
		w.Header().Set("Xai-Request-Id", "same-provider-id")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			kind, raw, err := c.Read(r.Context())
			if err != nil {
				return
			}
			if mode.Load() == 1 {
				raw = []byte(`{"type":"error","error":{"message":"private-provider-error"}}`)
				kind = websocket.MessageText
			}
			if err = c.Write(r.Context(), kind, raw); err != nil {
				return
			}
		}
	}))
	defer up.Close()
	account := func(secret string, priority int) int64 {
		return id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": secret, "platform": "grok", "type": "apikey", "group_ids": []int64{gid}, "priority": priority, "concurrency": 5, "rate_multiplier": 0.5, "extra": map[string]any{"quota_limit": 100}, "credentials": map[string]any{"api_key": secret, "base_url": up.URL + "/v1", "model_mapping": map[string]string{"text-only": "text-model"}}}))
	}
	aid := account("voice-primary", 1)
	account("voice-backup", 2)
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	dial := func(path, token string, extra http.Header, want int) (*websocket.Conn, string) {
		t.Helper()
		if extra == nil {
			extra = http.Header{}
		}
		extra.Set("Authorization", "Bearer "+token)
		extra.Set("Cookie", "private-cookie")
		extra.Set("X-Api-Key", token)
		dialCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		c, resp, err := websocket.Dial(dialCtx, server.URL+path, &websocket.DialOptions{HTTPHeader: extra})
		if want != 101 {
			if err == nil {
				c.CloseNow()
				t.Fatal("invalid handshake accepted")
			}
			if resp == nil || resp.StatusCode != want {
				t.Fatalf("handshake want %d: %v %v", want, resp, err)
			}
			return nil, ""
		}
		if err != nil {
			t.Fatal("realtime dial", err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c, resp.Header.Get("X-Request-ID")
	}
	read := func(c *websocket.Conn) (websocket.MessageType, []byte) {
		t.Helper()
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		kind, raw, err := c.Read(readCtx)
		if err != nil {
			t.Fatal("realtime read", err)
		}
		return kind, raw
	}
	send := func(c *websocket.Conn, kind websocket.MessageType, raw []byte) {
		t.Helper()
		writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if err := c.Write(writeCtx, kind, raw); err != nil {
			t.Fatal(err)
		}
	}
	wait := func(condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !condition() {
			if time.Now().After(deadline) {
				t.Fatal("realtime condition did not complete")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	finished := func(rid string) {
		wait(func() bool {
			a.gatewayMu.Lock()
			defer a.gatewayMu.Unlock()
			for s := range a.websockets {
				if s.realtimeID == rid {
					return false
				}
			}
			return true
		})
	}
	count := func(rid string) int {
		t.Helper()
		var n int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", rid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	dial("/realtime", user, nil, 401)
	dial("/realtime?conversation_id=foreign", key, nil, 400)
	dial("/realtime?model=a&model=b", key, nil, 400)
	dial("/realtime", key, http.Header{"Idempotency-Key": []string{"no"}}, 400)
	dial("/realtime", key, http.Header{"Sec-WebSocket-Protocol": []string{"private-token"}}, 400)
	c, rid := dial("/v1/realtime", key, nil, 101)
	send(c, websocket.MessageText, []byte(`{"type":"session.update","session":{"model":"grok-voice-latest","voice":"eve"}}`))
	_, raw := read(c)
	if !bytes.Contains(raw, []byte(`"voice":"eve"`)) {
		t.Fatal("session update changed", string(raw))
	}
	c.Close(websocket.StatusNormalClosure, "")
	finished(rid)
	if count(rid) != 0 {
		t.Fatal("handshake/control-only session billed")
	}
	c, rid = dial("/realtime?model=grok-voice-latest", key, nil, 101)
	send(c, websocket.MessageText, []byte(`{"type":"input_audio_buffer.append","audio":"AP+AAQ=="}`))
	_, raw = read(c)
	if !bytes.Contains(raw, []byte(`"audio":"AP+AAQ=="`)) {
		t.Fatal("JSON audio changed")
	}
	binary := []byte{0, 255, 128, 1}
	send(c, websocket.MessageBinary, binary)
	kind, raw := read(c)
	if kind != websocket.MessageBinary || !bytes.Equal(raw, binary) {
		t.Fatal("binary audio changed")
	}
	if err := a.recoverRealtime(ctx); err != nil || count(rid) != 0 {
		t.Fatal("active checkpoint settled", err)
	}
	checkpoint, err := a.Redis.HGet(ctx, realtimeBilling, rid).Bytes()
	if err != nil || bytes.Contains(checkpoint, []byte("voice-primary")) || bytes.Contains(checkpoint, []byte("AP+AAQ==")) {
		t.Fatal("checkpoint absent or contains secret/content", err)
	}
	manage("PUT", gp, admin, map[string]any{"audio_realtime_price_per_min": 120})
	time.Sleep(120 * time.Millisecond)
	c.Close(websocket.StatusNormalClosure, "")
	finished(rid)
	var total, actual, model, providerID, accountCost string
	var duration int64
	var ws bool
	err = a.DB.QueryRow("SELECT total_cost::text,actual_cost::text,model,upstream_request_id,duration_ms,openai_ws_mode,account_rate_multiplier::text FROM usage_logs WHERE request_id=$1", rid).Scan(&total, &actual, &model, &providerID, &duration, &ws, &accountCost)
	// Original snapshot: 60/min = 1/sec, user multiplier 2; SQL duration has millisecond precision.
	delta := new(big.Rat).Sub(rat(json.Number(total)), big.NewRat(duration, 1000))
	if err != nil || !ws || model != "grok-voice-latest" || providerID != "same-provider-id" || delta.Sign() < 0 || delta.Cmp(big.NewRat(1, 1000)) >= 0 || rat(json.Number(actual)).Cmp(new(big.Rat).Mul(rat(json.Number(total)), big.NewRat(2, 1))) != 0 {
		t.Fatal("realtime snapshot accounting", total, actual, duration, ws, err)
	}
	var matched bool
	err = a.DB.QueryRow(`SELECT u.balance=100-round(l.actual_cost,8) AND k.quota_used=round(l.actual_cost,8) AND k.usage_5h=round(l.actual_cost,8) AND (ac.extra->>'quota_used')::numeric=round(l.total_cost*0.5,8) FROM users u JOIN api_keys k ON k.user_id=u.id JOIN usage_logs l ON l.api_key_id=k.id JOIN accounts ac ON ac.id=l.account_id WHERE l.request_id=$1`, rid).Scan(&matched)
	if err != nil || !matched {
		t.Fatal("realtime balance/Key/account counters", err)
	}
	if err = a.recoverRealtime(ctx); err != nil || count(rid) != 1 {
		t.Fatal("completed checkpoint replay", err)
	}
	manage("PUT", gp, admin, map[string]any{"audio_realtime_price_per_min": 60})
	// Revocation must terminate an established session before forwarding another event.
	c, rid = dial("/realtime", key, nil, 101)
	manage("PUT", kp, user, map[string]any{"status": "inactive"})
	send(c, websocket.MessageText, []byte(`{"type":"response.create"}`))
	_, raw = read(c)
	if !bytes.Contains(raw, []byte(`"type":"error"`)) {
		t.Fatal("revocation not reported", string(raw))
	}
	c.CloseNow()
	finished(rid)
	manage("PUT", kp, user, map[string]any{"status": "active"})
	// Mid-session model changes and native errors cannot bypass policy or leak secrets.
	for _, tc := range []struct {
		event  string
		native int32
	}{{`{"type":"session.update","session":{"model":"other"}}`, 0}, {`{"type":"response.create"}`, 1}} {
		mode.Store(tc.native)
		c, rid = dial("/realtime", key, nil, 101)
		send(c, websocket.MessageText, []byte(tc.event))
		_, raw = read(c)
		if !bytes.Contains(raw, []byte(`"type":"error"`)) || bytes.Contains(raw, []byte("private-provider-error")) {
			t.Fatal("invalid event/error escaped", string(raw))
		}
		c.CloseNow()
		finished(rid)
	}
	mode.Store(0)
	primaryStatus.Store(503)
	before := calls.Load()
	c, rid = dial("/realtime", key, nil, 101)
	c.CloseNow()
	finished(rid)
	if calls.Load()-before != 2 {
		t.Fatal("handshake failover", calls.Load()-before)
	}
	primaryStatus.Store(0)
	if _, err = a.DB.Exec("UPDATE accounts SET overload_until=NULL WHERE id=$1", aid); err != nil {
		t.Fatal(err)
	}
	// A frozen final receipt survives settlement failure; recovery must never reopen the provider.
	if _, err = a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_realtime_failure CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	c, rid = dial("/realtime", key, nil, 101)
	send(c, websocket.MessageBinary, binary)
	read(c)
	c.CloseNow()
	finished(rid)
	if count(rid) != 0 {
		t.Fatal("failed settlement did not roll back")
	}
	final, err := a.Redis.HGet(ctx, realtimeBilling, rid).Bytes()
	if err != nil {
		t.Fatal("lost final checkpoint", err)
	}
	if _, err = a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_realtime_failure"); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	restarted := &App{DB: a.DB, Redis: a.Redis}
	for range 2 {
		if err = restarted.recoverRealtime(ctx); err != nil {
			t.Fatal(err)
		}
		if err = restarted.recoverReceipts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var receipt usageReceipt
	if err = json.Unmarshal(final, &receipt); err != nil {
		t.Fatal(err)
	}
	if count(rid) != 1 || calls.Load() != before {
		t.Fatal("recovery repeated consumption")
	}
	if err = a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", rid).Scan(&actual); err != nil || actual != receipt.Cost.Actual {
		t.Fatal("recovery recomputed snapshot", actual, receipt.Cost.Actual, err)
	}
	// Orphan checkpoint models a process exit; time elapsed after exit must not be billed.
	receipt.RequestID = randomToken(24)
	receipt.At = time.Now().Add(-time.Hour)
	orphan, _ := json.Marshal(receipt)
	if err = a.Redis.HSet(ctx, realtimeBilling, receipt.RequestID, orphan).Err(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = restarted.recoverRealtime(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if count(receipt.RequestID) != 1 || calls.Load() != before {
		t.Fatal("orphan recovery failed")
	}
	manage("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"other"}}})
	dial("/realtime", key, nil, 403)
	manage("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	manage("PUT", gp, admin, map[string]any{"claude_code_only": true})
	dial("/realtime", key, nil, 403)
	manage("PUT", gp, admin, map[string]any{"claude_code_only": false})
	for _, platform := range []string{"openai", "composite"} {
		group := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "No realtime " + platform, "platform": platform}))
		k := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Other realtime", "group_id": group})["key"].(string)
		dial("/realtime", k, nil, 404)
	}
}
