package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testTLSProfiles(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const root = "/api/v1/admin/tls-fingerprint-profiles"
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	defaults := manage("POST", root, map[string]any{"name": "TLS default"})
	dpath := root + "/" + string(defaults["id"])
	for _, name := range strings.Split(tlsProfileLists, ",") {
		if string(defaults[name]) != "[]" {
			t.Fatal("default list", name, string(defaults[name]))
		}
	}
	if string(defaults["enable_grease"]) != "false" || string(defaults["description"]) != "null" {
		t.Fatal("defaults", defaults)
	}
	input := map[string]any{"name": "TLS custom", "description": "retained", "enable_grease": true, "cipher_suites": []int{4867, 4865, 65535}, "curves": []int{29, 23}, "point_formats": []int{0}, "signature_algorithms": []int{1027, 2052}, "alpn_protocols": []string{"h2", "http/1.1"}, "supported_versions": []int{772, 771}, "key_share_groups": []int{29}, "psk_modes": []int{1}, "extensions": []int{0, 43, 10, 16}}
	custom := manage("POST", root, input)
	path := root + "/" + string(custom["id"])
	defer a.DB.Exec("DELETE FROM tls_fingerprint_profiles WHERE name LIKE 'TLS %'")
	for name, want := range input {
		raw, _ := json.Marshal(want)
		if !bytes.Equal(custom[name], raw) {
			t.Fatal("profile field changed", name, string(custom[name]), string(raw))
		}
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		for token, status := range map[string]int{"": 401, ordinary: 403, "invalid-client-key": 401} {
			if w := call(method, path, token, input); w.Code != status {
				t.Fatal("profile permissions", method, w.Code)
			}
		}
	}
	for _, method := range []string{"GET", "POST"} {
		if w := call(method, root, ordinary, input); w.Code != 403 {
			t.Fatal("list/create permission", method, w.Code)
		}
	}
	for _, patch := range []any{nil, []int{}, map[string]any{"name": " "}, map[string]any{"name": strings.Repeat("字", 101)}, map[string]any{"name": "bad\x00"}, map[string]any{"id": 2}, map[string]any{"cipher_suites": []int{-1}}, map[string]any{"curves": []int{65536}}, map[string]any{"psk_modes": []float64{1.5}}, map[string]any{"enable_grease": "true"}, map[string]any{"extensions": make([]int, 257)}, map[string]any{"alpn_protocols": []string{""}}, map[string]any{"alpn_protocols": []string{"http\r\n"}}} {
		if w := call("PUT", path, admin, patch); w.Code != 400 {
			t.Fatal("invalid profile accepted", patch, w.Code)
		}
	}
	if w := call("POST", root, admin, input); w.Code != 409 {
		t.Fatal("duplicate name", w.Code)
	}
	changed := manage("PUT", path, map[string]any{"curves": []int{}, "description": nil, "enable_grease": false})
	if string(changed["curves"]) != "[]" || string(changed["description"]) != `"retained"` || string(changed["cipher_suites"]) != string(custom["cipher_suites"]) || string(changed["enable_grease"]) != "false" {
		t.Fatal("partial update", changed)
	}
	var storedNull bool
	if err := a.DB.QueryRow("SELECT curves IS NULL FROM tls_fingerprint_profiles WHERE id=$1", string(custom["id"])).Scan(&storedNull); err != nil || !storedNull {
		t.Fatal("empty list storage semantics", storedNull, err)
	}
	var wg sync.WaitGroup
	for _, patch := range []any{map[string]any{"name": "TLS renamed"}, map[string]any{"description": "concurrent update"}} {
		wg.Add(1)
		go func(v any) {
			defer wg.Done()
			if w := call("PUT", path, admin, v); w.Code != 200 {
				t.Error("concurrent update", w.Code)
			}
		}(patch)
	}
	wg.Wait()
	changed = manage("GET", path, nil)
	if string(changed["name"]) != `"TLS renamed"` || string(changed["description"]) != `"concurrent update"` || string(changed["created_at"]) != string(custom["created_at"]) {
		t.Fatal("concurrent fields lost", changed)
	}
	w := call("GET", root, admin, nil)
	var listed struct{ Data []map[string]json.RawMessage }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Data) != 2 || string(listed.Data[0]["id"]) != string(defaults["id"]) {
		t.Fatal("profile list order", w.Code, w.Body.String())
	}
	// A preserved template and legacy JSON flags do not enable a fingerprint
	// on API-key accounts. Observe real ClientHello and reject untrusted TLS.
	hellos := make(chan []uint16, 8)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS accepted") }))
	up.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos <- slices.Clone(hello.CipherSuites)
		return nil, nil
	}}
	up.StartTLS()
	defer up.Close()
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		u := &upstreamAccount{Platform: platform, Type: "apikey", Credentials: map[string]json.RawMessage{"api_key": json.RawMessage(`"tls-secret"`), "base_url": json.RawMessage(fmt.Sprintf("%q", up.URL))}, Extra: map[string]json.RawMessage{"enable_tls_fingerprint": json.RawMessage(`true`), "tls_fingerprint_profile_id": custom["id"]}}
		if resp, err := a.upstreamRequest(context.Background(), u, "GET", "/v1/models", nil); err == nil {
			resp.Body.Close()
			t.Fatal("untrusted TLS succeeded", platform)
		}
		select {
		case ciphers := <-hellos:
			if slices.Contains(ciphers, 65535) || len(ciphers) <= 3 {
				t.Fatal("API key used stored template", ciphers)
			}
		default:
			t.Fatal("TLS handshake was not attempted", platform)
		}
		in := accountInput{Extra: u.Extra}
		if in.validate(false) == nil {
			t.Fatal("unsupported fingerprint account configuration accepted")
		}
	}
	manage("DELETE", path, nil)
	manage("DELETE", dpath, nil)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if w := call(method, path, admin, input); w.Code != 404 {
			t.Fatal("deleted profile", method, w.Code)
		}
	}
	if w := call("GET", root, admin, nil); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"data":[]`)) {
		t.Fatal("empty profiles", w.Code, w.Body.String())
	}
	var audit bool
	if err := a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM audit_logs WHERE path=$1 AND method='PUT' AND status_code=200)", path).Scan(&audit); err != nil || !audit {
		t.Fatal("profile audit", audit, err)
	}
}

type unreadTelemetry struct{ t *testing.T }

func (b unreadTelemetry) Read([]byte) (int, error) { b.t.Fatal("telemetry body read"); return 0, nil }
func (b unreadTelemetry) Close() error             { return nil }

func TestDiscardClientTelemetry(t *testing.T) {
	// No database, cache, authentication, ingestion, or forwarding dependency.
	a := &App{mux: http.NewServeMux()}
	a.routes()
	r := httptest.NewRequest("POST", "/api/event_logging/batch", nil)
	r.Body = unreadTelemetry{t}
	r.Header.Set("Authorization", "Bearer never-store-this")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("telemetry acknowledgement", w.Code, w.Body.String())
	}
	r = httptest.NewRequest("GET", "/api/event_logging/batch", nil)
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatal("unexpected telemetry method", w.Code)
	}
}
