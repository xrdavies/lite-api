package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
)

func TestAccountCompatibilityHelpers(t *testing.T) {
	u := &upstreamAccount{Platform: "openai", Type: "apikey", Credentials: map[string]json.RawMessage{
		"model_mapping":           json.RawMessage(`{"public":"normal"}`),
		"compact_model_mapping":   json.RawMessage(`{"public":"compact"}`),
		"header_override_enabled": json.RawMessage(`true`),
		"header_overrides":        json.RawMessage(`{"X-Relay-Route":"team-a"}`),
	}}
	if got, _, err := u.resolveModelMappingFor("public", true); err != nil || got != "compact" {
		t.Fatalf("compact mapping = %q, %v", got, err)
	}
	for _, tc := range []struct {
		normal, compact, model, want string
		explicit                     bool
	}{
		{`{"public":"normal"}`, `{"public":"compact"}`, "public", "normal", false},
		{`{"public":"normal"}`, `{"normal":"compact"}`, "public", "compact", true},
		{`{"public":"normal"}`, `{"other":"compact"}`, "public", "normal", true},
		{`{}`, `{"p*":"broad","pub*":"narrow","public":"exact"}`, "public", "exact", true},
		{`{}`, `{"p*":"broad","pub*":"narrow"}`, "public", "narrow", true},
		{`{"other":"normal"}`, `{"public":"compact"}`, "public", "compact", true},
	} {
		u.Credentials["model_mapping"], u.Credentials["compact_model_mapping"] = json.RawMessage(tc.normal), json.RawMessage(tc.compact)
		if got, _, err := u.resolveModelMappingFor(tc.model, tc.explicit); err != nil || got != tc.want {
			t.Fatal("mapping precedence", tc, got, err)
		}
	}
	h := http.Header{"Authorization": []string{"Bearer secret"}, "X-Relay-Route": []string{"old"}, "x-relay-route": []string{"duplicate"}}
	applyAccountHeaderOverrides(h, u)
	if h.Get("X-Relay-Route") != "team-a" || h.Get("Authorization") != "Bearer secret" || len(h) != 2 {
		t.Fatalf("header overrides changed incorrectly: %#v", h)
	}
	if _, err := normalizeHeaderOverrides(json.RawMessage(`{"Authorization":"bad"}`)); err == nil {
		t.Fatal("sensitive header override accepted")
	}
}

func TestHeaderOverrideBoundaries(t *testing.T) {
	for name := range blockedHeaderOverrides {
		raw, _ := json.Marshal(map[string]string{strings.ToUpper(name): "override"})
		if _, err := normalizeHeaderOverrides(raw); err == nil {
			t.Fatalf("blocked header accepted: %s", name)
		}
	}
	for _, raw := range []string{`[]`, `{"x":null}`, `{"x":1}`, `{"X-Test":"a","x-test":"b"}`, `{"bad name":"x"}`, `{"x":"a\r\nb"}`, `{"x":"a\u0000b"}`} {
		if _, err := normalizeHeaderOverrides(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid override accepted: %s", raw)
		}
	}
	for _, entries := range []map[string]string{{strings.Repeat("x", 201): "a"}, {"x": strings.Repeat("a", 8193)}} {
		raw, _ := json.Marshal(entries)
		if _, err := normalizeHeaderOverrides(raw); err == nil {
			t.Fatal("oversized header override accepted")
		}
	}
	entries := map[string]string{}
	for i := 0; i <= maxHeaderOverrides; i++ {
		entries[fmt.Sprintf("x-%d", i)] = "value"
	}
	raw, _ := json.Marshal(entries)
	if _, err := normalizeHeaderOverrides(raw); err == nil {
		t.Fatal("too many header overrides accepted")
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Relay-Route") != "team-a" || r.Header.Get("Cookie") != "" {
			t.Error("missing route or injected cookie")
		}
		if r.URL.Path == "/socket" {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			_, _, _ = conn.Read(r.Context())
			return
		}
		if r.Header.Get("Authorization") != "Bearer original" && r.Header.Get("X-Api-Key") != "original" {
			t.Error("API key replaced")
		}
		w.WriteHeader(200)
	}))
	defer up.Close()
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	base, _ := json.Marshal(up.URL)
	u := &upstreamAccount{Type: "apikey", Credentials: map[string]json.RawMessage{
		"api_key": json.RawMessage(`"original"`), "base_url": base,
		"header_override_enabled": json.RawMessage(`true`),
		// Defensive read of older rows must also protect authentication.
		"header_overrides": json.RawMessage(`{"X-Relay-Route":"team-a","Authorization":"evil","Cookie":"evil"}`),
	}}
	for _, platform := range []string{"openai", "anthropic", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		u.Platform = platform
		resp, err := a.upstreamRequest(t.Context(), u, "GET", "/v1/models", nil)
		if err != nil {
			t.Fatal(platform, err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(platform, resp.StatusCode)
		}
	}
	u.Platform = "openai"
	before := responseTarget(u)
	conn, _, err := a.dialUpstreamSocket(t.Context(), u, "/socket", http.Header{"Authorization": {"Bearer original"}})
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
	u.Credentials["header_override_enabled"] = json.RawMessage(`false`)
	h := http.Header{}
	applyAccountHeaderOverrides(h, u)
	if len(h) != 0 || before == responseTarget(u) {
		t.Fatal("disabled override or source identity")
	}
}

// These checks are part of the standard disposable-database integration run.
func testAccountCompatibility(t *testing.T, a *App, admin string) {
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.219:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "compatibility@example.test", "password": "compatibility-password", "balance": 10})
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "compatibility@example.test", "password": "compatibility-password"})["access_token"].(string)
	var calls atomic.Int64
	seen := make(chan map[string]json.RawMessage, 32)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		body["observed_path"], _ = json.Marshal(r.URL.Path)
		seen <- body
		if r.Header.Get("X-Relay-Route") != "team-a" || r.Header.Get("Authorization") != "Bearer compatibility-upstream" {
			t.Error("relay headers")
		}
		object := "response"
		if strings.Contains(r.URL.Path, "/compact") {
			object = "response.compaction"
		}
		result := fmt.Sprintf(`{"id":"resp_compat_%d","object":%q,"status":"completed","model":%s,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`, n, object, body["model"])
		w.Header().Set("Content-Type", "application/json")
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"+result+"}\n\n")
		} else {
			fmt.Fprint(w, result)
		}
	}))
	defer up.Close()
	group := func(platform string) (int64, string) {
		gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Compatibility " + platform, "platform": platform, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"*"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0", "cache_write_1h_price": "0"}}}))
		return gid, must("POST", "/api/v1/keys", user, map[string]any{"name": "Compatibility", "group_id": gid})["key"].(string)
	}
	account := func(platform string, gid int64, factor int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Compatibility", "platform": platform, "type": "apikey", "concurrency": 3, "load_factor": factor, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "compatibility-upstream", "base_url": up.URL, "api_protocol": "responses", "header_override_enabled": true, "header_overrides": map[string]string{"X-Relay-Route": "team-a"}, "model_mapping": map[string]string{"public": "normal"}, "compact_model_mapping": map[string]string{"public": "compact"}}}))
	}
	gid, key := group("openai")
	granted := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Header resource grant", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "compatibility-upstream", "base_url": up.URL, "api_protocol": "responses", "header_override_enabled": true, "header_overrides": map[string]string{"X-Relay-Route": "team-a"}}, "extra": map[string]any{responseFilesKey: map[string][]string{fmt.Sprint(gid): {"file_team"}}}}))
	grantAccount, err := a.loadAccount(t.Context(), granted)
	if err != nil || !grantAccount.allowsResponseResources(responseFilesKey, gid, []string{"file_team"}) {
		t.Fatal("create hashed grant without headers", err)
	}
	first, second := account("openai", gid, 1), account("openai", gid, 10)
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", first)
	t.Run("LoadFactorSelectionAndReset", func(t *testing.T) {
		for _, aid := range []int64{first, second} {
			if !a.takeSlot("account", aid, 3) {
				t.Fatal("slot")
			}
			defer a.releaseSlot("account", aid)
		}
		g := &gatewayIdentity{Key: gatewayKey{GroupID: gid}, Group: gatewayGroup{ID: gid, Platform: "openai", Rate: "1"}}
		pick := func(want int64, sticky *responseBinding) {
			t.Helper()
			s, err := a.chooseAccount(t.Context(), g, "public", textRequest{Protocol: "responses"}, nil, nil, sticky, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Release()
			if s.Account.ID != want {
				t.Fatalf("selected %d, want %d", s.Account.ID, want)
			}
		}
		pick(second, nil) // same physical load, different normalized load
		must("PUT", ap, admin, map[string]any{"priority": 1})
		pick(first, nil) // explicit priority still wins over load
		must("PUT", ap, admin, map[string]any{"priority": 50})
		if !a.takeSlot("account", second, 3) || !a.takeSlot("account", second, 3) {
			t.Fatal("fill account")
		}
		pick(first, nil) // factor 10 must not raise physical capacity 3
		a.releaseSlot("account", second)
		a.releaseSlot("account", second)
		must("PUT", ap, admin, map[string]any{"load_factor": 100})
		pick(first, nil)
		must("PUT", ap, admin, map[string]any{"notes": "preserve", "load_factor": nil})
		pick(first, nil)
		for _, reset := range []int{0, -1} {
			must("PUT", ap, admin, map[string]any{"load_factor": reset})
			var cleared bool
			if err := a.DB.QueryRow("SELECT load_factor IS NULL FROM accounts WHERE id=$1", first).Scan(&cleared); err != nil || !cleared {
				t.Fatal("factor not cleared", err)
			}
			pick(second, nil)
		}
		u, err := a.loadAccount(t.Context(), first)
		if err != nil {
			t.Fatal(err)
		}
		pick(first, &responseBinding{AccountID: first, Target: responseTarget(u)})
		monitor := must("GET", "/api/v1/admin/ops/concurrency", admin, nil)
		load := monitor["account"].(map[string]any)[fmt.Sprint(first)].(map[string]any)
		if load["max_capacity"] != float64(3) || load["load_percentage"].(float64) < 33 {
			t.Fatal("physical capacity changed", load)
		}
		if w := call("PUT", ap, admin, map[string]any{"load_factor": 10001}); w.Code != 400 {
			t.Fatal("invalid factor", w.Code)
		}
	})
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", second), admin, map[string]any{"status": "inactive"})
	t.Run("CompactMappingAndHeaderPersistence", func(t *testing.T) {
		for _, tc := range []struct {
			path, model string
			trigger     bool
		}{{"/v1/responses/compact", "compact", false}, {"/responses/compact", "compact", false}, {"/v1/responses", "normal", false}, {"/v1/responses", "normal", true}} {
			body := map[string]any{"model": "public", "input": "hello"}
			if tc.trigger {
				body["stream"] = true
				body["input"] = []any{map[string]string{"role": "user", "content": "hello"}, map[string]string{"type": "compaction_trigger"}}
			}
			w := call("POST", tc.path, key, body)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if got := <-seen; credentialString(got, "model") != tc.model {
				t.Fatal("wire model", got)
			}
			var model, cost string
			if err := a.DB.QueryRow("SELECT upstream_model,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&model, &cost); err != nil || model != tc.model || cost != "0.0140000000" {
				t.Fatal("mapping settlement", model, cost, err)
			}
		}
		for _, credentials := range []any{map[string]any{"header_overrides": map[string]string{"Cookie": "bad"}}, map[string]any{"compact_model_mapping": []string{"bad"}}} {
			if w := call("PUT", ap, admin, map[string]any{"credentials": credentials}); w.Code != 400 {
				t.Fatal("invalid credentials", w.Code)
			}
		}
		must("PUT", ap, admin, map[string]any{"notes": "keep credentials"})
		u, err := a.loadAccount(t.Context(), first)
		if err != nil {
			t.Fatal(err)
		}
		before := responseTarget(u)
		must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"header_overrides": map[string]string{"X-Relay-Route": "team-b"}}})
		u, err = a.loadAccount(t.Context(), first)
		if err != nil {
			t.Fatal(err)
		}
		if before == responseTarget(u) || credentialString(u.Credentials, "api_key") != "compatibility-upstream" {
			t.Fatal("route change did not isolate source or lost key")
		}
	})
	for _, platform := range []string{"deepseek", "kimi", "minimax"} {
		t.Run("StatelessResponses/"+platform, func(t *testing.T) {
			gid, key := group(platform)
			aid := account(platform, gid, 0)
			for _, stream := range []bool{false, true} {
				w := call("POST", "/v1/responses", key, map[string]any{"model": "public", "input": "full history", "store": true, "stream": stream})
				if w.Code != 200 || strings.Contains(w.Body.String(), `"type":"error"`) {
					t.Fatal(w.Code, w.Body.String())
				}
				got := <-seen
				path := "/v1/responses"
				if platform == "deepseek" {
					path = "/responses"
				}
				if string(got["store"]) != "false" || got["previous_response_id"] != nil || credentialString(got, "observed_path") != path {
					t.Fatal("native body/path", got)
				}
			}
			u, err := a.loadAccount(t.Context(), aid)
			if err != nil {
				t.Fatal(err)
			}
			result := a.runAccountTest(t.Context(), u, accountTestInput{Model: "public"})
			if result.Status != "success" {
				t.Fatal(result.Error)
			}
			if got := <-seen; string(got["store"]) != "false" {
				t.Fatal("health bypassed normalization")
			}
			r := httptest.NewRequest("POST", "/v1/responses", nil)
			r.Header.Set("Authorization", "Bearer "+key)
			g, err := a.gatewayAuth(r, false)
			if err != nil {
				t.Fatal(err)
			}
			// Even an old binding must not silently drop a requested conversation.
			if err = a.bindResponse(t.Context(), g, u, "resp_old_stateless"); err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			for _, fields := range []map[string]any{{"previous_response_id": "resp_old_stateless"}, {"background": true}} {
				body := map[string]any{"model": "public", "input": "continue"}
				for k, v := range fields {
					body[k] = v
				}
				if w := call("POST", "/v1/responses", key, body); w.Code != 400 {
					t.Fatal("stateful request accepted", w.Code, w.Body.String())
				}
			}
			if calls.Load() != before {
				t.Fatal("stateful request reached stateless upstream")
			}
		})
	}
}

func TestNativeCNResponsesNormalization(t *testing.T) {
	account := &upstreamAccount{Platform: "deepseek", Type: "apikey", Credentials: map[string]json.RawMessage{"api_protocol": json.RawMessage(`"responses"`)}}
	body := []byte(`{"store":true,"previous_response_id":"resp_1","input":[{"type":"message","content":[{"type":"input_image","image_url":{"url":"https://example.test/image.png"}}]}]}`)
	normalized := normalizeNativeCNResponsesBody(account, body)
	var got map[string]any
	if err := json.Unmarshal(normalized, &got); err != nil {
		t.Fatal(err)
	}
	if got["store"] != false || got["previous_response_id"] != nil {
		t.Fatalf("stateless fields not normalized: %s", normalized)
	}
	part := got["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["url"] != "https://example.test/image.png" || part["image_url"] != "https://example.test/image.png" {
		t.Fatalf("DeepSeek image fields not normalized: %#v", part)
	}
	openAI := &upstreamAccount{Platform: "openai", Type: "apikey", Credentials: map[string]json.RawMessage{"api_protocol": json.RawMessage(`"responses"`)}}
	if got := normalizeNativeCNResponsesBody(openAI, body); string(got) != string(body) {
		t.Fatalf("non-CN Responses body changed: %s", got)
	}
	openAI.Credentials["base_url"] = json.RawMessage(`"https://api.deepseek.com"`)
	normalized = normalizeNativeCNResponsesBody(openAI, body)
	if !bytes.Contains(normalized, []byte(`"url":"https://example.test/image.png"`)) || !bytes.Contains(normalized, []byte(`"store":true`)) || !bytes.Contains(normalized, []byte(`"previous_response_id":"resp_1"`)) {
		t.Fatalf("DeepSeek host alias changed storage policy or missed image: %s", normalized)
	}
	for _, platform := range []string{"openai", "deepseek", "kimi", "minimax", "zhipu"} {
		account.Platform = platform
		for _, base := range []string{"https://example.test", "https://example.test/v1", "https://example.test/relay"} {
			account.Credentials["base_url"], _ = json.Marshal(base)
			path := nativeCNResponsesPath(account, "/v1/responses/compact")
			got, err := upstreamURL(base, path)
			want := strings.TrimSuffix(base, "/v1") + "/v1/responses/compact"
			if platform == "deepseek" {
				want = base + "/responses/compact"
			}
			if err != nil || got != want {
				t.Fatal("Responses URL", platform, base, got, err)
			}
		}
	}
	account.Platform = "deepseek"
	body = []byte(`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_image\"}","call_id":"9007199254740993"},{"type":"message","content":[{"type":"input_image","image_url":"https://example.test/i"}]}],"metadata":{"integer":9007199254740993}}`)
	normalized = normalizeNativeCNResponsesBody(account, body)
	if !bytes.Contains(normalized, []byte(`9007199254740993`)) || !bytes.Contains(normalized, []byte(`"arguments":"{\"type\":\"input_image\"}"`)) {
		t.Fatal("normalization changed opaque data", string(normalized))
	}
}
