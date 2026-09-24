package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestGrokAccountProbes(t *testing.T) {
	var scenario atomic.Int64
	var calls atomic.Int64
	audio, _, _ := accountTestAudio("")
	if len(audio) != 8044 || string(audio[:4]) != "RIFF" || binary.LittleEndian.Uint32(audio[40:]) != 8000 {
		t.Fatal("invalid silent WAV")
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer probe-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			t.Error("probe credential isolation")
		}
		if !strings.HasPrefix(r.URL.Path, "/relay/") {
			t.Error("relay root lost", r.URL.Path)
		}
		if scenario.Load() == 1 {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":"probe-secret"}`)
			return
		}
		if scenario.Load() == 2 {
			http.Redirect(w, r, "/wrong", 307)
			return
		}
		switch r.URL.Path {
		case "/relay/v1/responses":
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("invalid search JSON")
			}
			if credentialString(body, "model") != "search-model" || string(body["store"]) != "false" || string(body["stream"]) != "false" || !bytes.Contains(body["tools"], []byte("web_search")) {
				t.Error("search body drift", body)
			}
			switch scenario.Load() {
			case 3:
				fmt.Fprint(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"answer without search"}]}]}`)
			case 4:
				fmt.Fprint(w, `{"status":"completed","output":[{"type":"web_search_call","status":"failed"}]}`)
			case 5:
				fmt.Fprint(w, `{"status":"in_progress","output":[]}`)
			default:
				fmt.Fprint(w, `{"status":"completed","error":null,"output":[{"type":"web_search_call","status":"completed","action":{"sources":[{"url":"https://example.test"}]}}]}`)
			}
		case "/relay/v1/tts":
			if !strings.Contains(r.Header.Get("Accept"), "audio/*") {
				t.Error("TTS Accept header")
			}
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["language"] != "en" || body["voice_id"] != "eve" || body["text"] == "" || body["model"] != "" {
				t.Error("incorrect TTS body")
			}
			w.Header().Set("Content-Type", "audio/wav")
			switch scenario.Load() {
			case 3:
				fmt.Fprint(w, `{"error":"secret"}`)
			case 4:
			case 5:
				w.Write(make([]byte, (4<<20)+1))
			case 7:
				w.Write([]byte{0, 1, 2, 3})
			case 6:
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			default:
				w.Write(audio)
			}
		case "/relay/v1/stt":
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			option, err := reader.NextPart()
			if err != nil || option.FormName() != "language" {
				t.Error("STT options after file")
				return
			}
			value, _ := io.ReadAll(option)
			if string(value) != "en" {
				t.Error("STT language")
			}
			file, err := reader.NextPart()
			if err != nil || file.FormName() != "file" || file.FileName() != "probe.wav" {
				t.Error("missing STT file")
				return
			}
			content, _ := io.ReadAll(file)
			if !bytes.Equal(content, audio) {
				t.Error("STT upload changed")
			}
			if _, err = reader.NextPart(); err != io.EOF {
				t.Error("unexpected STT fields")
			}
			w.Header().Set("Content-Type", "application/json")
			switch scenario.Load() {
			case 3:
				fmt.Fprint(w, `{}`)
			case 4:
				fmt.Fprint(w, `{"text":3}`)
			case 5:
				fmt.Fprint(w, `{"text":null,"error":{"message":"probe-secret"}}`)
			default:
				fmt.Fprint(w, `{"text":"","duration":0.25}`)
			}
		case "/relay/v1/realtime":
			if r.URL.Query().Get("model") != "voice-model" {
				t.Error("Realtime model mapping lost", r.URL.RawQuery)
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			event := `{"type":"session.created","session":{"id":"secret-session","secret":"probe-secret"}}`
			switch scenario.Load() {
			case 3:
				event = `{"type":"error","error":{"message":"probe-secret"}}`
			case 4:
				event = `not json`
			case 5:
				event = `{"type":"response.created"}`
			case 6:
				conn.Read(r.Context())
				return
			case 7:
				conn.Write(r.Context(), websocket.MessageBinary, []byte{1, 2, 3})
				conn.Read(r.Context())
				return
			}
			conn.Write(r.Context(), websocket.MessageText, []byte(event))
			conn.Read(r.Context())
		default:
			t.Error("unexpected path", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	var credentials map[string]json.RawMessage
	raw, _ := json.Marshal(map[string]any{"api_key": "probe-secret", "base_url": upstream.URL + "/relay", "api_protocol": "anthropic", "model_mapping": map[string]string{"grok-4.6": "search-model", "grok-voice-latest": "voice-model"}})
	json.Unmarshal(raw, &credentials)
	u := &upstreamAccount{ID: 1, Platform: "grok", Type: "apikey", Credentials: credentials}
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	for _, mode := range []string{"search", "tts", "stt", "realtime"} {
		in := accountTestInput{Mode: mode}
		result := a.runAccountTest(context.Background(), u, in)
		if result.Status != "success" || strings.Contains(result.Text, "probe-secret") || strings.Contains(result.Text, "secret-session") {
			t.Fatal(mode, result)
		}
		if mode == "tts" {
			if len(result.Media) != 1 || result.Media[0]["audio_url"] != "data:audio/wav;base64,"+base64.StdEncoding.EncodeToString(audio) {
				t.Fatal("audio preview mismatch")
			}
			saved, _ := json.Marshal(result)
			if bytes.Contains(saved, []byte("base64")) {
				t.Fatal("audio preview persisted")
			}
		}
		for _, bad := range []int64{1, 2, 3, 4, 5} {
			scenario.Store(bad)
			before := calls.Load()
			result = a.runAccountTest(context.Background(), u, in)
			if result.Status != "failed" || strings.Contains(result.Error, "probe-secret") || calls.Load() != before+1 {
				t.Fatal("bad probe/retry", mode, bad, result, calls.Load()-before)
			}
		}
		scenario.Store(0)
	}
	in := accountTestInput{Mode: "stt", Audio: "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(audio)}
	if result := a.runAccountTest(context.Background(), u, in); result.Status != "success" || strings.Contains(result.Text, "Synthetic") {
		t.Fatal("uploaded audio probe", result)
	}
	for _, mode := range []string{"tts", "realtime"} {
		scenario.Store(6)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		result := a.runAccountTest(ctx, u, accountTestInput{Mode: mode})
		cancel()
		if result.Status != "failed" || !strings.Contains(result.Error, "timed out") {
			t.Fatal("probe cancellation", mode, result)
		}
	}
	scenario.Store(7)
	if result := a.runAccountTest(context.Background(), u, accountTestInput{Mode: "tts"}); result.Status != "failed" {
		t.Fatal("random bytes accepted as audio")
	}
	if result := a.runAccountTest(context.Background(), u, accountTestInput{Mode: "realtime"}); result.Status != "failed" {
		t.Fatal("binary initial event accepted")
	}
	scenario.Store(6)
	if result := a.runAccountTest(context.Background(), u, accountTestInput{Mode: "realtime"}); result.Status != "success" || !strings.Contains(result.Text, "no initial event") || !strings.Contains(result.Text, "Audio was not exercised") {
		t.Fatal("silent handshake classification", result)
	}
	before := calls.Load()
	for _, in := range []accountTestInput{{Mode: "stt", Audio: "https://example.test/file.wav"}, {Mode: "stt", Audio: "data:audio/wav;base64,!!!"}, {Mode: "stt", Audio: "data:text/plain;base64,aGk="}, {Mode: "stt", Audio: "data:audio/wav;base64,"}, {Mode: "tts", Audio: "bad"}, {Mode: "search", Image: "bad"}, {Mode: "realtime", Model: "bad?key=secret"}} {
		if result := a.runAccountTest(context.Background(), u, in); result.Status != "failed" {
			t.Fatal("unsafe input allowed", in)
		}
	}
	if calls.Load() != before {
		t.Fatal("unsafe probe reached upstream")
	}
	for _, mode := range []string{"search", "tts", "stt", "realtime"} {
		other := *u
		other.Platform = "openai"
		if _, _, err := prepareAccountTest(&other, accountTestInput{Mode: mode}); err == nil {
			t.Fatal("non Grok probe allowed", mode)
		}
	}
}

func testGrokProbeHealth(t *testing.T, a *App, admin, user string) {
	t.Helper()
	var bad atomic.Bool
	audio, _, _ := accountTestAudio("")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer grok-health-key" {
			t.Error("wrong probe credential")
		}
		switch r.URL.Path {
		case "/v1/tts":
			w.Header().Set("Content-Type", "audio/wav")
			w.Write(audio)
		case "/v1/stt":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"text":"hello"}`)
		case "/v1/responses":
			fmt.Fprint(w, `{"status":"completed","output":[{"type":"web_search_call","status":"completed"}]}`)
		case "/v1/realtime":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			event := `{"type":"session.created","private":"do-not-echo"}`
			if bad.Load() {
				event = `{"type":"error","error":{"message":"grok-health-key"}}`
			}
			conn.Write(r.Context(), websocket.MessageText, []byte(event))
			conn.Read(r.Context())
		default:
			t.Error("unexpected health path", r.URL.Path)
		}
	}))
	defer provider.Close()
	call := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.203:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	created := call("/api/v1/admin/accounts", admin, map[string]any{"name": "Grok probes", "platform": "grok", "type": "apikey", "concurrency": 1, "credentials": map[string]any{"api_key": "grok-health-key", "base_url": provider.URL}})
	var envelope struct{ Data struct{ ID int64 } }
	if created.Code != 200 || json.Unmarshal(created.Body.Bytes(), &envelope) != nil || envelope.Data.ID == 0 {
		t.Fatal("create probe account", created.Body.String())
	}
	id := envelope.Data.ID
	path := fmt.Sprintf("/api/v1/admin/accounts/%d/test", id)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		var status string
		var quota string
		if err := a.DB.QueryRow("SELECT status,extra->>'quota_used' FROM accounts WHERE id=$1", id).Scan(&status, &quota); err != nil || status != want || quota != "3.25" {
			t.Fatal("probe recovery", status, quota, err)
		}
	}
	for _, mode := range []string{"search", "tts", "stt", "realtime"} {
		exec("UPDATE accounts SET status='error',error_message='temporary',extra=extra || '{\"quota_used\":3.25}'::jsonb,updated_at=now() WHERE id=$1", id)
		body := map[string]string{"mode": mode}
		if w := call(path, user, body); w.Code != 403 {
			t.Fatal("ordinary user probe", w.Code)
		}
		w := call(path, admin, body)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"test_complete"`) || strings.Contains(w.Body.String(), "grok-health-key") || strings.Contains(w.Body.String(), "do-not-echo") {
			t.Fatal(mode, w.Code, w.Body.String())
		}
		if mode == "tts" && !strings.Contains(w.Body.String(), `"type":"audio"`) {
			t.Fatal("missing audio SSE")
		}
		check("active")
	}
	exec("UPDATE accounts SET status='error',error_message='temporary',updated_at=now() WHERE id=$1", id)
	bad.Store(true)
	w := call(path, admin, map[string]string{"mode": "realtime"})
	if strings.Contains(w.Body.String(), `"test_complete"`) || strings.Contains(w.Body.String(), "grok-health-key") {
		t.Fatal("Realtime error treated as healthy", w.Body.String())
	}
	check("error")
	bad.Store(false)
	exec("UPDATE accounts SET status='inactive',updated_at=now() WHERE id=$1", id)
	call(path, admin, map[string]string{"mode": "realtime"})
	check("inactive")
	if w := call(path, admin, map[string]string{"mode": "tts", "audio_data_url": "bad"}); w.Code != 400 {
		t.Fatal("bad media field accepted", w.Code)
	}
	var n int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE account_id=$1", id).Scan(&n); err != nil || n != 0 {
		t.Fatal("health tests billed a user", n, err)
	}
}
