package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFastPolicy(t *testing.T) {
	s := fastPolicySettings{Rules: []fastPolicyRule{
		{Tier: " ALL ", Scope: "all", Action: "filter"},
		{Tier: "priority", Scope: "apikey", Action: "block", Users: []int64{42}, Models: []string{" native-* "}, Message: "blocked", Fallback: "force_priority"},
		{Tier: "missing", Scope: "apikey", Action: "force_priority"},
	}}
	if err := s.validate(); err != nil || s.Rules[0].Tier != "all" || s.Rules[1].Models[0] != "native-*" {
		t.Fatal(s, err)
	}
	for _, tc := range []struct {
		user                         int64
		model, tier, action, message string
	}{
		{42, "native-one", "priority", "block", "blocked"},
		{42, "NATIVE-one", "priority", "force_priority", ""},
		{43, "native-one", "priority", "filter", ""},
		{42, "native-one", "flex", "filter", ""},
		{42, "native-one", "", "force_priority", ""},
	} {
		action, message := s.action(tc.user, tc.model, tc.tier)
		if action != tc.action || message != tc.message {
			t.Fatal(tc, action, message)
		}
	}
	s.Rules[1].Fallback = ""
	if action, _ := s.action(42, "other", "priority"); action != "pass" {
		t.Fatal("fallback pass fell through to global rule")
	}
	for _, action := range []string{"pass", "filter", "block", "force_priority"} {
		for _, platform := range []string{"openai", "grok", "kimi"} {
			p := fastPolicySettings{Rules: []fastPolicyRule{{Tier: "missing", Scope: "apikey", Action: action}}}
			body := map[string]json.RawMessage{}
			tier, err := p.apply(body, 42, "native", platform, "")
			want := ""
			if platform == "openai" && action == "force_priority" {
				want = "priority"
			}
			if err != nil || tier != want || credentialString(body, "service_tier") != want {
				t.Fatal("missing contract", platform, action, tier, err)
			}
		}
	}
	for _, r := range []fastPolicyRule{
		{Tier: "unknown", Scope: "all", Action: "pass"}, {Tier: "all", Scope: "oauth", Action: "pass"},
		{Tier: "all", Scope: "bedrock", Action: "pass"}, {Tier: "all", Scope: "apikey", Action: "unknown"},
		{Scope: "all", Action: "pass", Users: []int64{0}}, {Scope: "all", Action: "pass", Users: []int64{1, 1}},
		{Scope: "all", Action: "pass", Models: []string{" "}}, {Scope: "all", Action: "pass", Models: []string{"a*b"}},
		{Scope: "all", Action: "pass", Fallback: "unknown"},
	} {
		p := fastPolicySettings{Rules: []fastPolicyRule{r}}
		if err := p.validate(); err == nil {
			t.Fatal("invalid policy accepted", r)
		}
	}
	// Invalid explicit tiers cannot become valid merely because the group forces
	// priority. Missing/null remain eligible for an explicit upgrade rule.
	g := gatewayIdentity{Group: gatewayGroup{Platform: "openai", ForceFast: true}}
	u := &upstreamAccount{Platform: "openai", Credentials: map[string]json.RawMessage{}}
	for _, tier := range []string{"", " ", "future"} {
		body := map[string]json.RawMessage{"service_tier": mustJSON(tier)}
		if _, err := g.applyFast(body, u, textRequest{Protocol: "chat_completions", Tier: tier}); err == nil {
			t.Fatal("invalid tier was hidden by force", tier)
		}
	}
	for _, tc := range []struct {
		platform, wire, protocol string
		count, want              bool
	}{
		{"openai", "responses", "responses", false, true},
		{"grok", "responses", "responses", false, true},
		{"kimi", "chat_completions", "anthropic", false, true},
		{"openai", "anthropic", "responses", false, false},
		{"anthropic", "anthropic", "anthropic", false, false},
		{"gemini", "gemini", "chat_completions", false, false},
		{"openai", "responses", "anthropic", true, false},
		{"openai", "responses", "images", false, false},
	} {
		u := &upstreamAccount{Platform: tc.platform, Credentials: map[string]json.RawMessage{"api_protocol": mustJSON(tc.wire)}}
		if fastPolicyProtocol(u, textRequest{Protocol: tc.protocol, CountOnly: tc.count}) != tc.want {
			t.Fatal("policy protocol boundary", tc)
		}
	}
}

func testFastPolicy(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", fastPolicySetting)
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.210:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "fast-policy@example.test", "password": "fast-policy-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "fast-policy@example.test", "password": "fast-policy-password"})["access_token"].(string)
	const settings = "/api/v1/admin/settings"
	set := func(rules ...fastPolicyRule) {
		t.Helper()
		must("PUT", settings, admin, map[string]any{fastPolicySetting: fastPolicySettings{Rules: rules}})
	}
	rule := func(tier, action string) fastPolicyRule {
		return fastPolicyRule{Tier: tier, Action: action, Scope: "apikey"}
	}
	set()
	if w := call("GET", "/api/v1/settings/public", "", nil, ""); w.Code != 200 || strings.Contains(w.Body.String(), fastPolicySetting) {
		t.Fatal("public fast policy leak", w.Code)
	}
	if w := call("PUT", settings, user, map[string]any{fastPolicySetting: fastPolicySettings{}}, ""); w.Code != 403 {
		t.Fatal("ordinary user changed policy", w.Code)
	}
	set(rule("all", "filter"))
	saved := string(mustJSON(must("GET", settings, admin, nil)[fastPolicySetting]))
	for _, patch := range []any{map[string]any{}, map[string]any{fastPolicySetting: nil}} {
		if got := string(mustJSON(must("PUT", settings, admin, patch)[fastPolicySetting])); got != saved {
			t.Fatal("omitted/null changed fast settings", got)
		}
	}
	name := must("GET", settings, admin, nil)["site_name"]
	for _, raw := range []string{`{"rules":[{"service_tier":"all","action":"block","scope":"oauth"}]}`, `{"unknown":true}`, `{"rules":[{"service_tier":"priority","action":"filter","scope":"all","user_ids":[1,1]}]}`} {
		w := call("PUT", settings, admin, map[string]any{"site_name": "invalid-change", fastPolicySetting: json.RawMessage(raw)}, "")
		got := must("GET", settings, admin, nil)
		if w.Code != 400 || got["site_name"] != name || string(mustJSON(got[fastPolicySetting])) != saved {
			t.Fatal("invalid setting was partially committed", w.Code, got)
		}
	}
	if _, err := a.DB.Exec("ALTER TABLE settings ADD CONSTRAINT test_fast_policy_write CHECK(key<>'openai_fast_policy_settings') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE settings DROP CONSTRAINT IF EXISTS test_fast_policy_write")
	w := call("PUT", settings, admin, map[string]any{"site_name": "failed-policy-write", fastPolicySetting: fastPolicySettings{}}, "")
	if _, err := a.DB.Exec("ALTER TABLE settings DROP CONSTRAINT test_fast_policy_write"); err != nil {
		t.Fatal(err)
	}
	got := must("GET", settings, admin, nil)
	if w.Code != 500 || got["site_name"] != name || string(mustJSON(got[fastPolicySetting])) != saved {
		t.Fatal("failed policy write did not roll back settings", w.Code, got)
	}
	var calls, mutate atomic.Int64
	var seen atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen.Store(credentialString(body, "service_tier"))
		if credentialString(body, "model") != "native-policy" || r.Header.Get("Authorization") != "Bearer policy-upstream" {
			t.Error("mapped model or credentials changed")
		}
		if mutate.Swap(0) != 0 {
			if err := a.writeRuntimeSetting(r.Context(), fastPolicySetting, fastPolicySettings{Rules: []fastPolicyRule{rule("all", "block")}}); err != nil {
				t.Error(err)
			}
		}
		var result, stream string
		if r.URL.Path == "/v1/responses" {
			result = fmt.Sprintf(`{"id":"resp_policy_%d","object":"response","status":"completed","model":"native-policy","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n)
			stream = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + result + "}\n\n"
		} else {
			result = fmt.Sprintf(`{"id":"chat_policy_%d","model":"native-policy","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, n)
			stream = "data: " + result + "\n\ndata: [DONE]\n\n"
		}
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, stream)
		} else {
			fmt.Fprint(w, result)
		}
	}))
	defer up.Close()
	for _, wire := range []string{"responses", "chat_completions"} {
		price := []modelPrice{{Platform: "openai", Models: []string{"public-policy"}, Input: number("0.001"), Output: number("0.002"), CacheRead: number("0"), CacheWrite: number("0"), Fast: number("3")}}
		gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Policy " + wire, "platform": "openai", "allow_messages_dispatch": true, "force_openai_fast": true, "model_pricing": price}))
		gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		account := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Policy " + wire, "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "policy-upstream", "base_url": up.URL, "api_protocol": wire, "model_mapping": map[string]string{"public-policy": "native-policy"}}})
		key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Policy", "group_id": gid})["key"].(string)
		body := map[string]any{"model": "public-policy", "input": "hi", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "max_tokens": 16}
		check := func(path string, tier, cost, idem string) *httptest.ResponseRecorder {
			t.Helper()
			request := map[string]any{}
			for k, v := range body {
				request[k] = v
			}
			if strings.Contains(path, "responses") {
				delete(request, "messages")
				delete(request, "max_tokens")
			} else {
				delete(request, "input")
			}
			w := call("POST", path, key, request, idem)
			if w.Code != 200 || seen.Load() != tier || strings.Contains(w.Body.String(), `"type":"error"`) {
				t.Fatal("policy dispatch", wire, path, tier, seen.Load(), w.Code, w.Body.String())
			}
			var logged, actual string
			if err := a.DB.QueryRow("SELECT COALESCE(service_tier,''),actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&logged, &actual); err != nil || logged != tier || actual != cost {
				t.Fatal("policy billing", tier, logged, actual, cost, err)
			}
			return w
		}
		set(rule("priority", "filter"))
		for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				body["stream"] = stream
				check(path, "", "0.0200000000", "")
			}
		}
		body["stream"] = false
		// User IDs come from authentication; body metadata cannot select an exception.
		exception := rule("priority", "pass")
		exception.Users = []int64{uid}
		set(rule("all", "block"), exception)
		check("/v1/responses", "priority", "0.0600000000", "")
		exception.Users = []int64{uid + 10000}
		set(rule("all", "block"), exception)
		before := calls.Load()
		if w := call("POST", "/v1/responses", key, map[string]any{"model": "public-policy", "input": "hi", "user": fmt.Sprint(uid + 10000)}, ""); w.Code != 403 || calls.Load() != before {
			t.Fatal("untrusted user policy bypass", w.Code)
		}
		match := rule("priority", "pass")
		match.Models, match.Fallback, match.FallbackMessage = []string{"native-*"}, "block", "model-policy-denied"
		set(match, rule("all", "block"))
		check("/v1/responses", "priority", "0.0600000000", "")
		match.Models = []string{"public-*"}
		set(match)
		if w := call("POST", "/v1/responses", key, map[string]any{"model": "public-policy", "input": "hi"}, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "model-policy-denied") {
			t.Fatal("policy matched public instead of upstream model", w.Code, w.Body.String())
		}
		must("PUT", gp, admin, map[string]any{"force_openai_fast": false})
		set(rule("all", "block"))
		check("/v1/responses", "", "0.0200000000", "")
		set(rule("all", "filter"), rule("missing", "force_priority"))
		check("/v1/responses", "priority", "0.0600000000", "")
		body["service_tier"] = nil
		check("/v1/responses", "priority", "0.0600000000", "")
		body["service_tier"] = "ultrafast"
		set(rule("priority", "filter"))
		check("/v1/responses", "ultrafast", "0.0400000000", "")
		set(rule("ultrafast", "filter"))
		check("/v1/responses", "", "0.0200000000", "")
		body["service_tier"] = "flex"
		set(rule("flex", "force_priority"))
		check("/v1/responses", "priority", "0.0600000000", "")
		set()
		check("/v1/responses", "flex", "0.0100000000", "")
		mutate.Store(1)
		first := check("/v1/responses", "flex", "0.0100000000", "policy-replay")
		before = calls.Load()
		replayBody := map[string]any{"model": "public-policy", "input": "hi", "stream": false, "service_tier": "flex"}
		w := call("POST", "/responses", key, replayBody, "policy-replay")
		if w.Code != 200 || w.Body.String() != first.Body.String() || calls.Load() != before || w.Header().Get("Idempotency-Replayed") != "true" {
			t.Fatal("changed policy resent completed request", w.Code, w.Body.String())
		}
		if w := call("POST", "/responses", key, replayBody, ""); w.Code != 403 || calls.Load() != before {
			t.Fatal("new HTTP request ignored changed policy", w.Code)
		}
		// A malformed persisted policy fails closed without revealing its content.
		if _, err := a.DB.Exec("UPDATE settings SET value='broken-private-policy' WHERE key=$1", fastPolicySetting); err != nil {
			t.Fatal(err)
		}
		if w := call("POST", "/responses", key, replayBody, ""); w.Code != 503 || calls.Load() != before || strings.Contains(w.Body.String(), "private-policy") {
			t.Fatal("corrupt policy", w.Code)
		}
		set()
		for _, value := range []any{"", " ", "unknown", 1, false} {
			replayBody["service_tier"] = value
			if w := call("POST", "/responses", key, replayBody, ""); w.Code != 400 || calls.Load() != before {
				t.Fatal("invalid tier", value, w.Code)
			}
		}
		// Confirmed rejection retries retain the original policy snapshot.
		var rejected atomic.Int64
		badUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rejected.Add(1)
			var request map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["service_tier"] != nil {
				t.Error("first retry attempt did not apply filter")
			}
			if err := a.writeRuntimeSetting(r.Context(), fastPolicySetting, fastPolicySettings{Rules: []fastPolicyRule{rule("all", "block")}}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(503)
		}))
		badAccount := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Policy retry", "platform": "openai", "type": "apikey", "priority": 0, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "policy-upstream", "base_url": badUp.URL, "api_protocol": wire, "model_mapping": map[string]string{"public-policy": "native-policy"}}})
		defer badUp.Close()
		key = must("POST", "/api/v1/keys", user, map[string]any{"name": "Policy retry", "group_id": gid})["key"].(string)
		body["service_tier"] = "priority"
		set(rule("priority", "filter"))
		check("/v1/responses", "", "0.0200000000", "")
		if rejected.Load() != 1 {
			t.Fatal("retry policy case not exercised")
		}
		must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", id(badAccount)), admin, map[string]any{"status": "inactive"})
		must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", id(account)), admin, map[string]any{"status": "inactive"})
	}
	// Explicit tiers apply to API Key accounts using either OpenAI text wire.
	// Only an OpenAI target may be upgraded when the client omits the tier.
	for _, tc := range []struct{ group, target string }{
		{"composite", "openai"}, {"composite", "grok"}, {"grok", "grok"},
		{"kimi", "kimi"}, {"deepseek", "deepseek"}, {"zhipu", "zhipu"}, {"minimax", "minimax"},
	} {
		wire := "chat_completions"
		if tc.target == "grok" {
			wire = "responses"
		}
		price := []modelPrice{{Platform: tc.target, Models: []string{"public-policy"}, Input: number("0.001"), Output: number("0.002"), CacheRead: number("0"), CacheWrite: number("0"), Fast: number("3")}}
		gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Policy target " + tc.group + " " + tc.target, "platform": tc.group, "model_pricing": price}))
		if tc.group == "composite" {
			must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", gid), admin, map[string]any{"public_model": "public-policy", "target_platform": tc.target})
		}
		must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Policy target", "platform": tc.target, "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "policy-upstream", "base_url": up.URL, "api_protocol": wire, "model_mapping": map[string]string{"public-policy": "native-policy"}}})
		key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Policy target", "group_id": gid})["key"].(string)
		body := map[string]any{"model": "public-policy", "input": "hi", "service_tier": "priority"}
		set(rule("all", "block"))
		before := calls.Load()
		if w := call("POST", "/responses", key, body, ""); w.Code != 403 || calls.Load() != before {
			t.Fatal("target bypassed block", tc, w.Code, w.Body.String())
		}
		for _, missing := range []bool{false, true} {
			set(rule("priority", "filter"))
			if missing {
				delete(body, "service_tier")
				set(rule("missing", "force_priority"))
			}
			wantTier, wantCost := "", "0.0200000000"
			if missing && tc.target == "openai" {
				wantTier, wantCost = "priority", "0.0600000000"
			}
			w := call("POST", "/responses", key, body, "")
			var tier, actual string
			if w.Code != 200 || seen.Load() != wantTier {
				t.Fatal("target policy dispatch", tc, missing, w.Code, w.Body.String(), seen.Load())
			}
			if err := a.DB.QueryRow("SELECT COALESCE(service_tier,''),actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&tier, &actual); err != nil || tier != wantTier || actual != wantCost {
				t.Fatal("target policy billing", tc, tier, actual, err)
			}
		}
	}
}
