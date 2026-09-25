package app

import (
	"bytes"
	"context"
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

	"github.com/coder/websocket"
)

func TestCustomVoiceValidation(t *testing.T) {
	for _, id := range []string{"", "..", "a/b", "x?key=bad", "x%2fy", "x\\y", strings.Repeat("a", 161)} {
		if validVoiceID(id) {
			t.Fatal("unsafe voice path", id)
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"name":""}`, `{"name":1}`, `{"voice_id":"foreign"}`, `{"name":true}`} {
		if _, err := customVoiceBody("PATCH", "application/json", []byte(raw)); err == nil {
			t.Fatal("invalid voice patch", raw)
		}
	}
	out, err := customVoiceBody("PATCH", "application/json", []byte(`{"name":"before","name":null,"tone":"calm"}`))
	if err != nil || string(out) != `{"name":null,"tone":"calm"}` {
		t.Fatal(string(out), err)
	}
	metadata, native, err := voiceMetadata([]byte(`{"voice_id":"native123","name":"Demo","api_key":"secret","account_id":1}`), "voice_local")
	if err != nil || native != "native123" || string(metadata) != `{"name":"Demo","voice_id":"voice_local"}` {
		t.Fatal(string(metadata), native, err)
	}
	for _, raw := range []string{`{"text":"x","voice_id":"a/b"}`, `{"text":"x","voice":"eve","voice_id":"ara"}`, `{"text":"x","Voice_id":"eve"}`, `{"text":"x","voice_id":null}`} {
		if _, err := parseAudioRequest("tts", "application/json", []byte(raw)); err == nil {
			t.Fatal("ambiguous voice", raw)
		}
	}
	for _, names := range [][]string{{"name"}, {"file", "file"}, {"other"}, {"file", "name", "name"}} {
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for _, name := range names {
			if name == "file" {
				part, _ := form.CreateFormFile(name, "ref.wav")
				_, _ = part.Write([]byte("audio"))
			} else {
				_ = form.WriteField(name, "demo")
			}
		}
		_ = form.Close()
		if _, err := customVoiceBody("POST", form.FormDataContentType(), body.Bytes()); err == nil {
			t.Fatal("invalid voice upload", names)
		}
	}
}

func testCustomVoices(t *testing.T, a *App, admin string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token, ct, idem string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.RemoteAddr = "192.0.2.192:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		if !strings.HasPrefix(path, "/api/v1/") {
			r.Header.Set("X-Api-Key", token)
		}
		r.Header.Set("Cookie", "private-cookie")
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := call(method, path, token, "application/json", "", raw)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "custom-voice@example.test", "password": "voice-test-password", "balance": 20, "concurrency": 5}))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "custom-voice@example.test", "password": "voice-test-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Custom voices", "platform": "grok"}))
	kd := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Voice library", "group_id": gid})
	key, kid := kd["key"].(string), id(kd)
	other := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Other voice library", "group_id": gid})["key"].(string)
	var creates, calls, createStatus atomic.Int32
	var ttsVoice, patchValue atomic.Value
	var form bytes.Buffer
	upload := multipart.NewWriter(&form)
	_ = upload.WriteField("name", "Test voice")
	p, _ := upload.CreateFormFile("file", "reference.wav")
	_, _ = p.Write([]byte("RIFF-test-reference"))
	_ = upload.Close()
	ct := upload.FormDataContentType()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Idempotency-Key") != "" {
			t.Error("client secrets forwarded")
		}
		if r.Header.Get("Authorization") != "Bearer voice-owner" {
			t.Error("wrong voice upstream", r.Header.Get("Authorization"))
			w.WriteHeader(500)
			return
		}
		if r.URL.Path == "/v1/realtime" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			for {
				kind, raw, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if bytes.Contains(raw, []byte("voice_")) {
					t.Error("local voice not translated")
				}
				if err = conn.Write(r.Context(), kind, raw); err != nil {
					return
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/custom-voices":
			n := creates.Add(1)
			raw, _ := io.ReadAll(r.Body)
			if !bytes.Equal(raw, form.Bytes()) || r.Header.Get("Content-Type") != ct {
				t.Error("reference multipart changed")
			}
			if status := createStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				_, _ = w.Write([]byte(`{"error":"private upstream secret"}`))
				return
			}
			w.WriteHeader(201)
			_, _ = fmt.Fprintf(w, `{"voice_id":"native%02d","name":"Test voice","account_id":987,"api_key":"private"}`, n)
		case r.Method == "POST" && r.URL.Path == "/v1/tts":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			ttsVoice.Store(credentialString(body, "voice_id"))
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFF-TTS"))
		case strings.HasSuffix(r.URL.Path, "/audio"):
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFF-test-reference"))
		case r.Method == "DELETE":
			_, _ = w.Write([]byte(`{"deleted":true}`))
		case r.Method == "GET" || r.Method == "PATCH":
			native := strings.TrimPrefix(r.URL.Path, "/v1/custom-voices/")
			if !strings.HasPrefix(native, "native") {
				t.Error("invalid native voice path", r.URL.Path)
			}
			if r.Method == "PATCH" {
				body, _ := io.ReadAll(r.Body)
				patchValue.Store(string(body))
			}
			_, _ = fmt.Fprintf(w, `{"voice_id":%q,"name":"Updated voice"}`, native)
		default:
			t.Error("unexpected voice upstream", r.Method, r.URL.String())
			w.WriteHeader(500)
		}
	}))
	defer up.Close()
	account := func(secret string, priority int) int64 {
		return id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": secret, "platform": "grok", "type": "apikey", "group_ids": []int64{gid}, "priority": priority, "concurrency": 5, "credentials": map[string]any{"api_key": secret, "base_url": up.URL, "model_mapping": map[string]string{"text-only": "text-model"}}}))
	}
	aid := account("voice-owner", 1)
	backup := account("voice-other", 2)
	created := call("POST", "/v1/custom-voices", key, ct, "voice-create-once", form.Bytes())
	if created.Code != 201 || bytes.Contains(created.Body.Bytes(), []byte("private")) || bytes.Contains(created.Body.Bytes(), []byte("account_id")) {
		t.Fatal("voice create", created.Code, created.Body)
	}
	var voice map[string]any
	_ = json.Unmarshal(created.Body.Bytes(), &voice)
	vid, _ := voice["voice_id"].(string)
	if !strings.HasPrefix(vid, "voice_") {
		t.Fatal("public voice ID", vid)
	}
	replay := call("POST", "/custom-voices", key, ct, "voice-create-once", form.Bytes())
	if replay.Code != 201 || replay.Body.String() != created.Body.String() || creates.Load() != 1 || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("voice create replay", replay.Code, replay.Body, creates.Load())
	}
	if w := call("POST", "/custom-voices", key, ct, "voice-create-once", bytes.ReplaceAll(form.Bytes(), []byte("Test voice"), []byte("Other voice"))); w.Code != 409 {
		t.Fatal("voice create conflict", w.Code)
	}
	var usage int
	var balance string
	if err := a.DB.QueryRow("SELECT balance::text,(SELECT count(*) FROM usage_logs WHERE api_key_id=$2) FROM users WHERE id=$1", uid, kid).Scan(&balance, &usage); err != nil || balance != "20.00000000" || usage != 0 {
		t.Fatal("voice management billed", balance, usage, err)
	}
	g := &gatewayIdentity{UserID: uid, Key: gatewayKey{ID: kid, GroupID: gid}}
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret}
	persisted, err := fresh.loadCustomVoice(ctx, g, vid)
	if err != nil || persisted.Binding.AccountID != aid || persisted.UpstreamID != "native01" {
		t.Fatal("voice registry lost across App instances", persisted, err)
	}
	raw, err := a.Redis.HGet(ctx, customVoiceKey(g), vid).Bytes()
	if err != nil || bytes.Contains(raw, []byte("voice-owner")) || bytes.Contains(raw, []byte("RIFF")) {
		t.Fatal("voice registry secret or audio", err)
	}
	for _, prefix := range []string{"/custom-voices", "/v1/custom-voices"} {
		for _, method := range []string{"GET", "PATCH", "DELETE"} {
			w := call(method, prefix+"/"+vid, other, "application/json", "", []byte(`{"name":"steal"}`))
			if w.Code != 404 {
				t.Fatal("cross-key voice access", method, w.Code)
			}
		}
		w := call("GET", prefix+"/"+vid+"/audio", other, "", "", nil)
		if w.Code != 404 {
			t.Fatal("cross-key audio", w.Code)
		}
		w = call("GET", prefix+"/"+vid, user, "", "", nil)
		if w.Code != 401 {
			t.Fatal("JWT voice access", w.Code)
		}
		w = call("GET", prefix+"/native01", key, "", "", nil)
		if w.Code != 404 {
			t.Fatal("native voice access", w.Code)
		}
	}
	before := calls.Load()
	list := call("GET", "/v1/custom-voices", key, "", "", nil)
	foreignList := call("GET", "/custom-voices", other, "", "", nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), vid) || foreignList.Code != 200 || strings.Contains(foreignList.Body.String(), vid) || calls.Load() != before {
		t.Fatal("voice list isolation", list.Body, foreignList.Body)
	}
	// The same user with a moved Key must neither discover nor replay the old library.
	newGroup := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Other voice group", "platform": "grok"}))
	keyPath := fmt.Sprintf("/api/v1/keys/%d", kid)
	manage("PUT", keyPath, user, map[string]any{"group_id": newGroup})
	if w := call("GET", "/custom-voices/"+vid, key, "", "", nil); w.Code != 404 {
		t.Fatal("moved key read old voice", w.Code)
	}
	if w := call("POST", "/custom-voices", key, ct, "voice-create-once", form.Bytes()); w.Code != 503 || w.Header().Get("Idempotency-Replayed") != "" {
		t.Fatal("moved key replay", w.Code, w.Body)
	}
	manage("PUT", keyPath, user, map[string]any{"group_id": gid})
	groupPath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	manage("PUT", groupPath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"tts"}}})
	before = calls.Load()
	if w := call("GET", "/custom-voices", key, "", "", nil); w.Code != 403 || calls.Load() != before {
		t.Fatal("voice model policy", w.Code)
	}
	manage("PUT", groupPath, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/custom-voices", key, "", "", nil); w.Code != 402 {
		t.Fatal("voice balance eligibility", w.Code)
	}
	if w := call("POST", "/custom-voices", key, ct, "voice-create-once", form.Bytes()); w.Code != 201 {
		t.Fatal("zero balance replay", w.Code)
	}
	if _, err := a.DB.Exec("UPDATE users SET balance=20 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec("UPDATE users SET rpm_limit=1 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix() / 60
	for _, minute := range []int64{now, now + 1} {
		if err := a.Redis.Set(ctx, fmt.Sprintf("gateway:rpm:u:%d:%d", uid, minute), 1, 2*time.Minute).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if w := call("GET", "/custom-voices", key, "", "", nil); w.Code != 429 {
		t.Fatal("voice RPM", w.Code, w.Body)
	}
	if _, err := a.DB.Exec("UPDATE users SET rpm_limit=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if !a.takeSlot("voice-management", kid, 1) {
		t.Fatal("voice test slot")
	}
	if w := call("PATCH", "/custom-voices/"+vid, key, "application/json", "", []byte(`{"name":"busy"}`)); w.Code != 409 {
		t.Fatal("concurrent voice mutation", w.Code)
	}
	a.releaseSlot("voice-management", kid)
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", backup), admin, map[string]any{"priority": 0})
	for _, prefix := range []string{"/custom-voices", "/v1/custom-voices"} {
		w := call("GET", prefix+"/"+vid, key, "", "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), vid) {
			t.Fatal("pinned get", w.Code, w.Body)
		}
		w = call("GET", prefix+"/"+vid+"/audio", key, "", "", nil)
		if w.Code != 200 || w.Header().Get("Content-Type") != "audio/wav" || w.Body.String() != "RIFF-test-reference" {
			t.Fatal("reference audio", w.Code, w.Body)
		}
	}
	patch := call("PATCH", "/custom-voices/"+vid, key, "application/json", "voice-patch", []byte(`{"name":null,"tone":"calm"}`))
	if patch.Code != 200 || patchValue.Load() != `{"name":null,"tone":"calm"}` {
		t.Fatal("voice patch", patch.Code, patch.Body, patchValue.Load())
	}
	before = calls.Load()
	if w := call("PATCH", "/v1/custom-voices/"+vid, key, "application/json", "voice-patch", []byte(`{"name":null,"tone":"calm"}`)); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || !bytes.Equal(w.Body.Bytes(), patch.Body.Bytes()) || calls.Load() != before {
		t.Fatal("voice patch alias repeated mutation", w.Code, w.Body.String())
	}
	body, _ := json.Marshal(map[string]string{"text": "test", "voice_id": vid})
	if w := call("POST", "/tts", key, "application/json", "voice-tts", body); w.Code != 200 || ttsVoice.Load() != "native01" {
		t.Fatal("pinned TTS", w.Code, w.Body, ttsVoice.Load())
	}
	for _, v := range []string{vid, "native01", "unknownvoice"} {
		raw, _ := json.Marshal(map[string]string{"text": "test", "voice_id": v})
		if w := call("POST", "/tts", other, "application/json", "", raw); w.Code != 404 {
			t.Fatal("cross-key TTS", v, w.Code)
		}
	}
	second := call("POST", "/custom-voices", key, ct, "voice-create-two", form.Bytes())
	if second.Code != 201 {
		t.Fatal("second voice binding", second.Code, second.Body)
	}
	firstPage := call("GET", "/custom-voices?limit=1", key, "", "", nil)
	var page struct {
		Voices []json.RawMessage `json:"voices"`
		Token  string            `json:"pagination_token"`
	}
	_ = json.Unmarshal(firstPage.Body.Bytes(), &page)
	if firstPage.Code != 200 || len(page.Voices) != 1 || page.Token == "" {
		t.Fatal("voice page", firstPage.Code, firstPage.Body)
	}
	secondPage := call("GET", "/v1/custom-voices?limit=1&pagination_token="+page.Token, key, "", "", nil)
	var next struct {
		Voices []json.RawMessage `json:"voices"`
		Token  any               `json:"pagination_token"`
	}
	_ = json.Unmarshal(secondPage.Body.Bytes(), &next)
	if secondPage.Code != 200 || len(next.Voices) != 1 || next.Token != nil || bytes.Equal(page.Voices[0], next.Voices[0]) {
		t.Fatal("voice next page", secondPage.Code, secondPage.Body)
	}
	for _, q := range []string{"?limit=0", "?limit=1001", "?limit=1&limit=2", "?pagination_token=foreign", "?key=private", "?limit=%ZZ"} {
		if w := call("GET", "/custom-voices"+q, key, "", "", nil); w.Code < 400 {
			t.Fatal("invalid query", q, w.Code)
		}
	}
	// A socket selects the library account before it receives the requested voice.
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, server.URL+"/realtime", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	event, _ := json.Marshal(map[string]any{"type": "session.update", "session": map[string]string{"voice": vid}})
	if err = conn.Write(dialCtx, websocket.MessageText, event); err != nil {
		t.Fatal(err)
	}
	_, event, err = conn.Read(dialCtx)
	if err != nil || !bytes.Contains(event, []byte(vid)) || bytes.Contains(event, []byte("native01")) {
		t.Fatal("realtime voice mapping", string(event), err)
	}
	if err = conn.Write(dialCtx, websocket.MessageText, []byte(`{"type":"session.update","session":{"audio":{"output":{"voice":"native01"}}}}`)); err != nil {
		t.Fatal(err)
	}
	_, event, err = conn.Read(dialCtx)
	if err != nil || !bytes.Contains(event, []byte(`"type":"error"`)) {
		t.Fatal("realtime foreign voice", string(event), err)
	}
	_ = conn.CloseNow()
	upstreamAccount, err := a.loadAccount(ctx, aid)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{`{"type":"session.update","session":{"Voice":"eve"}}`, `{"type":"session.update","session":{"Audio":{"output":{"voice":"eve"}}}}`, `{"type":"session.update","session":{"audio":{"output":{"voice":"native01"}}}}`} {
		if _, err := a.realtimeVoices(ctx, g, upstreamAccount, []byte(event), true); err == nil {
			t.Fatal("unsafe nested realtime voice", event)
		}
	}
	nativeEvent, _ := json.Marshal(map[string]any{"type": "session.update", "session": map[string]any{"audio": map[string]any{"output": map[string]string{"voice": vid}}}})
	if mapped, err := a.realtimeVoices(ctx, g, upstreamAccount, nativeEvent, true); err != nil || !bytes.Contains(mapped, []byte("native01")) {
		t.Fatal("nested realtime voice mapping", string(mapped), err)
	}
	// Provider permission errors must not disable the entire account.
	createStatus.Store(403)
	rejected := call("POST", "/custom-voices", key, ct, "voice-no-permission", form.Bytes())
	if rejected.Code != 403 || strings.Contains(rejected.Body.String(), "private") {
		t.Fatal("voice permission response", rejected.Code, rejected.Body)
	}
	var status string
	_ = a.DB.QueryRow("SELECT status FROM accounts WHERE id=$1", aid).Scan(&status)
	if status != "active" {
		t.Fatal("voice feature permission disabled key", status)
	}
	createStatus.Store(503)
	uncertain := call("POST", "/custom-voices", key, ct, "voice-uncertain", form.Bytes())
	before = creates.Load()
	uncertainReplay := call("POST", "/custom-voices", key, ct, "voice-uncertain", form.Bytes())
	if uncertain.Code != 503 || uncertainReplay.Code != 409 || creates.Load() != before {
		t.Fatal("indeterminate create retried", uncertain.Code, uncertainReplay.Code, creates.Load())
	}
	createStatus.Store(0)
	manage("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	// Credentials and account membership are part of the immutable source binding.
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "rotated"}})
	before = calls.Load()
	if w := call("GET", "/custom-voices/"+vid, key, "", "", nil); w.Code != 503 || calls.Load() != before {
		t.Fatal("voice moved to another source", w.Code, w.Body)
	}
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"credentials": map[string]any{"api_key": "voice-owner"}})
	removed := call("DELETE", "/v1/custom-voices/"+vid, key, "", "voice-delete", nil)
	removalReplay := call("DELETE", "/custom-voices/"+vid, key, "", "voice-delete", nil)
	if removed.Code != 200 || removalReplay.Code != 200 || removalReplay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("voice deletion replay", removed.Code, removed.Body, removalReplay.Code, removalReplay.Body)
	}
	if w := call("GET", "/custom-voices/"+vid, key, "", "", nil); w.Code != 404 {
		t.Fatal("deleted voice accessible", w.Code)
	}
	if w := call("POST", "/tts", key, "application/json", "", body); w.Code != 404 {
		t.Fatal("deleted voice synthesis", w.Code)
	}
	// Disabling the Key is enforced even for locally served listings.
	manage("PUT", fmt.Sprintf("/api/v1/keys/%d", kid), user, map[string]any{"status": "inactive"})
	if w := call("GET", "/custom-voices", key, "", "", nil); w.Code != 401 {
		t.Fatal("disabled key voice access", w.Code)
	}
}
