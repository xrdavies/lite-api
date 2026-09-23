package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAudioContracts(t *testing.T) {
	in, err := parseAudioRequest("tts", "application/json", []byte(`{"text":" 你好🌍 "}`))
	if err != nil || in.Characters != 3 {
		t.Fatal(in, err)
	}
	u, err := in.usage("tts", []byte{0, 255}, time.Second)
	cost, priceErr := (gatewayGroup{Rate: "2"}).audioCost("tts", u.AudioUnits)
	if err != nil || priceErr != nil || cost.Total != "0.0000450000" || cost.Actual != "0.0000900000" {
		t.Fatal(cost, err, priceErr)
	}
	for _, raw := range []string{`{}`, `null`, `{"text":3}`, `{"text":"x","Text":"long"}`, `{"text":"x","stream":true}`, `{"text":"x","input":"long text"}`, `{"text":"x","prompt":{}}`} {
		if _, err := parseAudioRequest("tts", "", []byte(raw)); err == nil {
			t.Fatal("invalid TTS accepted", raw)
		}
	}
	duplicate, err := parseAudioRequest("tts", "application/json", []byte(`{"text":"first long text","text":"x"}`))
	if err != nil || duplicate.Characters != 1 || string(duplicate.Body) != `{"text":"x"}` {
		t.Fatal("ambiguous JSON forwarded", duplicate, err)
	}
	for _, raw := range []string{`{}`, `{"url":"file:///etc/passwd"}`, `{"url":"https://u:p@example.test"}`, `{"url":"https://example.test/a","stream":true}`} {
		if _, err := parseAudioRequest("stt", "", []byte(raw)); err == nil {
			t.Fatal("invalid STT accepted", raw)
		}
	}
	in, err = parseAudioRequest("stt", "application/json", []byte(`{"url":"https://example.test/a?signature=abc","duration_seconds":0.001}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"text":"ok","duration":1800}`, `{"text":"ok","duration_seconds":1800}`, `{"text":"ok","audio_duration":1800}`, `{"text":"ok","usage":{"seconds":1800}}`} {
		u, err := in.usage("stt", []byte(raw), time.Second)
		cost, priceErr := (gatewayGroup{Rate: "2"}).audioCost("stt", u.AudioUnits)
		if err != nil || priceErr != nil || u.AudioUnits != "1/2" || cost.Actual != "0.1000000000" {
			t.Fatal(raw, u, cost, err, priceErr)
		}
	}
	u, err = in.usage("stt", []byte(`{"text":"ok"}`), 2*time.Second)
	if err != nil || u.AudioUnits != "1/1800" {
		t.Fatal("compat duration estimate", u, err)
	}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"error":{"message":"private"}}`} {
		if _, err := in.usage("stt", []byte(raw), time.Second); err == nil {
			t.Fatal("invalid transcription accepted")
		}
	}
	if _, err := audioResponseType("tts", "text/html"); err == nil {
		t.Fatal("TTS HTML accepted")
	}
	if _, err := audioResponseType("stt", "audio/mpeg"); err == nil {
		t.Fatal("STT audio accepted")
	}
	free, err := (gatewayGroup{audioPrices: audioPrices{TTS: number("0")}, Rate: "3"}).audioCost("tts", "1")
	if err != nil || free.Actual != "0.0000000000" {
		t.Fatal("explicit free price", free, err)
	}
	p := modelPrice{Platform: "grok", Models: []string{"tts"}, BillingMode: "per_request", PerRequest: number("10"), Intervals: []priceInterval{{Label: "tts", PerRequest: number("20")}}}
	cost, err = calculatePrice(p, priceUsage{AudioUnits: "1/2"}, "2", "", "", "tts", time.Now(), true)
	if err != nil || cost.Actual != "20.0000000000" {
		t.Fatal("audio fractional tier price", cost, err)
	}
	for _, fields := range [][]string{{"file", "model"}, {"file", "file"}, {"model"}, {"model", "model", "file"}, {"file", "url"}} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for i, field := range fields {
			if field == "file" {
				p, _ := form.CreateFormFile("file", "audio.mp3")
				_, _ = p.Write([]byte("audio"))
			} else {
				_ = form.WriteField(field, fmt.Sprintf("model-%d", i))
			}
		}
		_ = form.Close()
		if _, err := parseAudioRequest("stt", form.FormDataContentType(), body.Bytes()); err == nil {
			t.Fatal("invalid multipart accepted", fields)
		}
	}
}

func testGrokAudio(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token, ct, idem string, raw []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.190:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-session")
		r.Header.Set("X-Api-Key", token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := call(method, path, token, "application/json", "", raw)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "audio@example.test", "password": "audio-test-password", "balance": 10}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "audio@example.test", "password": "audio-test-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Audio", "platform": "grok", "rate_multiplier": 2, "audio_tts_price_per_million_chars": 20, "audio_stt_price_per_hour": 0.5}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	for _, field := range []string{"audio_tts_price_per_million_chars", "audio_stt_price_per_hour"} {
		for _, value := range []json.Number{"0.000000001", "1000000000000", "-0.000000001"} {
			raw, _ := json.Marshal(map[string]any{field: value})
			if w := call("PUT", gp, admin, "application/json", "", raw); w.Code != 400 {
				t.Fatal("invalid audio price accepted", field, value, w.Code)
			}
		}
	}
	keyData := must("POST", "/api/v1/keys", user, map[string]any{"name": "Audio", "group_id": gid, "quota": 10, "rate_limit_5h": 10})
	key, kid := keyData["key"].(string), id(keyData)
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "Other audio", "group_id": gid})["key"].(string)
	var calls, primaryStatus, mode atomic.Int32
	var priceChange atomic.Bool
	binary := []byte{'I', 'D', '3', 0, 255, 254, 0, 1}
	var form bytes.Buffer
	f := multipart.NewWriter(&form)
	_ = f.WriteField("model", "grok-voice-transcribe-2.0")
	_ = f.WriteField("duration_seconds", "0.001")
	_ = f.WriteField("keyterm", "first")
	_ = f.WriteField("keyterm", "second")
	part, _ := f.CreateFormFile("file", "sound.mp3")
	_, _ = part.Write(binary)
	_ = f.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("client credential leaked")
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer audio-primary" && auth != "Bearer audio-backup" {
			t.Error("upstream credential", auth)
		}
		if auth == "Bearer audio-primary" && primaryStatus.Load() != 0 {
			w.WriteHeader(int(primaryStatus.Load()))
			_, _ = w.Write([]byte("private upstream error"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		if priceChange.Swap(false) {
			if _, err := a.DB.Exec("UPDATE groups SET audio_tts_price_per_million_chars=100 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Xai-Request-Id", "reused-upstream-id")
		if mode.Load() == 1 {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("private upstream error"))
			return
		}
		switch r.URL.Path {
		case "/v1/tts":
			var input map[string]json.RawMessage
			if json.Unmarshal(body, &input) != nil || input["model"] != nil || credentialString(input, "text") == "" {
				t.Error("TTS body changed", string(body))
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			if mode.Load() == 2 {
				w.Header().Set("Content-Length", "9999")
			}
			_, _ = w.Write(binary)
		case "/v1/stt":
			if r.Header.Get("Content-Type") != f.FormDataContentType() || !bytes.Equal(body, form.Bytes()) {
				t.Error("multipart body or boundary changed")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"text":"hello","duration":1800,"words":[]}`))
		default:
			t.Error("audio path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	account := func(secret string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": secret, "platform": "grok", "type": "apikey", "group_ids": []int64{gid}, "priority": priority, "rate_multiplier": 0.5, "credentials": map[string]any{"api_key": secret, "base_url": upstream.URL, "model_mapping": map[string]string{"text-only": "grok-text"}}, "extra": map[string]any{"quota_limit": 10}}))
	}
	aid := account("audio-primary", 1)
	_ = account("audio-backup", 2)
	body := []byte(`{"text":"你好🌍","language":"auto","voice_id":"eve"}`)
	checkCost := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		var cost, model, billing, upstreamID string
		var tokens int
		err := a.DB.QueryRow("SELECT actual_cost::text,model,billing_mode,upstream_request_id,input_tokens+output_tokens FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost, &model, &billing, &upstreamID, &tokens)
		if err != nil || cost != want || !audioProtocol(model) || billing != "per_request" || upstreamID != "reused-upstream-id" || tokens != 0 {
			t.Fatal("audio receipt", cost, model, billing, upstreamID, tokens, err)
		}
	}
	first := call("POST", "/v1/tts", key, "application/json", "audio-once", body)
	second := call("POST", "/tts", key, "application/json", "audio-once", body)
	if first.Code != 200 || second.Code != 200 || !bytes.Equal(first.Body.Bytes(), binary) || !bytes.Equal(second.Body.Bytes(), binary) || first.Header().Get("Content-Type") != "audio/mpeg" || second.Header().Get("Content-Type") != "audio/mpeg" || second.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("audio replay: %d %s / %d %s calls=%d", first.Code, first.Body, second.Code, second.Body, calls.Load())
	}
	checkCost(first, "0.0001200000")
	if w := call("POST", "/tts", key, "application/json", "audio-once", []byte(`{"text":"changed"}`)); w.Code != 409 {
		t.Fatal("audio idempotency conflict", w.Code)
	}
	if w := call("POST", "/tts", user, "application/json", "", body); w.Code != 401 {
		t.Fatal("JWT used as API key", w.Code)
	}
	if w := call("POST", "/tts", other, "application/json", "audio-once", body); w.Code != 200 || calls.Load() != 2 || w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("cross-key audio replay", w.Code, calls.Load())
	}
	transcript := call("POST", "/stt", key, f.FormDataContentType(), "audio-stt", form.Bytes())
	if transcript.Code != 200 || !strings.Contains(transcript.Body.String(), `"text":"hello"`) {
		t.Fatal("transcription", transcript.Code, transcript.Body.String())
	}
	checkCost(transcript, "0.5000000000")
	beforePolicy := calls.Load()
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"tts"}}})
	if w := call("POST", "/stt", key, f.FormDataContentType(), "", form.Bytes()); w.Code != 403 || calls.Load() != beforePolicy {
		t.Fatal("multipart model bypassed allowlist", w.Code, calls.Load()-beforePolicy)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"grok-voice-transcribe-2.0"}}})
	if w := call("POST", "/tts", key, "application/json", "", body); w.Code != 403 || calls.Load() != beforePolicy {
		t.Fatal("TTS bypassed allowlist", w.Code, calls.Load()-beforePolicy)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	var quota, accountQuota string
	if err := a.DB.QueryRow("SELECT quota_used::text FROM api_keys WHERE id=$1", kid).Scan(&quota); err != nil || quota != "0.50012000" {
		t.Fatal("audio key quota", quota, err)
	}
	if err := a.DB.QueryRow("SELECT extra->>'quota_used' FROM accounts WHERE id=$1", aid).Scan(&accountQuota); err != nil || accountQuota != "0.12506000" {
		t.Fatal("audio account cost", accountQuota, err)
	}
	priceChange.Store(true)
	w := call("POST", "/tts", key, "application/json", "", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	checkCost(w, "0.0001200000")
	must("PUT", gp, admin, map[string]any{"audio_tts_price_per_million_chars": 0})
	w = call("POST", "/tts", key, "application/json", "", body)
	checkCost(w, "0.0000000000")
	must("PUT", gp, admin, map[string]any{"audio_tts_price_per_million_chars": -1})
	w = call("POST", "/tts", key, "application/json", "", body)
	checkCost(w, "0.0000900000")
	primaryStatus.Store(503)
	before := calls.Load()
	w = call("POST", "/tts", key, "application/json", "", body)
	if w.Code != 200 || calls.Load() != before+2 {
		t.Fatal("audio failover", w.Code, w.Body.String(), calls.Load()-before)
	}
	checkCost(w, "0.0000900000")
	primaryStatus.Store(0)
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	mode.Store(1)
	w = call("POST", "/tts", key, "application/json", "", body)
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 0 || w.Code != 502 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("invalid audio charged", w.Code, count, err)
	}
	mode.Store(2)
	w = call("POST", "/tts", key, "application/json", "", body)
	if w.Code != 502 {
		t.Fatal("partial TTS reported success", w.Code)
	}
	checkCost(w, "0.0000900000")
	mode.Store(0)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_audio_settlement CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w = call("POST", "/tts", key, "application/json", "audio-billing-failure", body)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_audio_settlement"); err != nil {
		t.Fatal(err)
	}
	if w.Code != 503 {
		t.Fatal("audio settlement failure", w.Code, w.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	checkCost(w, "0.0000900000")
	replay := call("POST", "/tts", key, "application/json", "audio-billing-failure", body)
	if replay.Code != 503 || calls.Load() != before || replay.Header().Get("Content-Type") != "application/json" {
		t.Fatal("failed audio replay resent", replay.Code, calls.Load()-before)
	}
	// Per-request channel tariffs price fractional audio units, not one whole call.
	must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Audio price", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "grok", "models": []string{"tts"}, "billing_mode": "per_request", "per_request_price": 30}}})
	w = call("POST", "/tts", key, "application/json", "", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	checkCost(w, "0.0001800000")
	before = calls.Load()
	quotaPath := fmt.Sprintf("/api/v1/admin/users/%d/platform-quotas", uid)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "grok", "daily_limit_usd": 0}}})
	if w := call("POST", "/tts", key, "application/json", "", body); w.Code != 429 || calls.Load() != before {
		t.Fatal("audio platform quota bypassed", w.Code, w.Body.String())
	}
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "grok", "daily_limit_usd": 100}}})
	must("PUT", gp, admin, map[string]any{"rpm_limit": 1})
	for minute := time.Now().Unix() / 60; minute <= time.Now().Unix()/60+1; minute++ {
		if err := a.Redis.Set(t.Context(), fmt.Sprintf("gateway:rpm:g:%d:%d:%d", uid, gid, minute), 1, 2*time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if w := call("POST", "/tts", key, "application/json", "", body); w.Code != 429 || calls.Load() != before {
		t.Fatal("audio RPM bypassed", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"rpm_limit": 0})
	otherGroup := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Audio unsupported", "platform": "openai"}))
	otherPlatform := must("POST", "/api/v1/keys", user, map[string]any{"name": "Not Grok", "group_id": otherGroup})["key"].(string)
	if w := call("POST", "/tts", otherPlatform, "application/json", "", body); w.Code != 404 || calls.Load() != before {
		t.Fatal("audio accepted other platform", w.Code)
	}
	must("POST", fmt.Sprintf("/api/v1/admin/users/%d/balance", uid), admin, map[string]any{"operation": "set", "balance": 0})
	before = calls.Load()
	if w := call("POST", "/tts", key, "application/json", "audio-once", body); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), binary) {
		t.Fatal("zero balance replay", w.Code, w.Body.String())
	}
	if w := call("POST", "/tts", key, "application/json", "", body); w.Code != 402 || calls.Load() != before {
		t.Fatal("zero balance spent", w.Code, calls.Load()-before)
	}
}
