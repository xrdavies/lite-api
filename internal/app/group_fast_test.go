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
	"time"
)

func TestOpenAIFastBilling(t *testing.T) {
	for _, tc := range []struct{ requested, observed, want string }{
		{"priority", "default", "default"}, {"priority", "flex", "flex"}, {"priority", "", "priority"},
		{"priority", "future", "priority"}, {"", "priority", ""}, {"flex", "priority", "flex"},
		{"fast", "standard", "standard"}, {"auto", "default", "auto"}, {"ultrafast", "default", "ultrafast"},
	} {
		if got := openAIBillingTier(tc.requested, tc.observed); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	a := &App{}
	g := &gatewayIdentity{Key: gatewayKey{Quota: "0", Limit5h: "0", Limit1d: "0", Limit7d: "0"}, Group: gatewayGroup{Platform: "openai", Rate: "0.5", FreeFast: true}}
	p := modelPrice{BillingMode: "token", Platform: "openai", Models: []string{"fast-test"}, Input: number("0.001"), Output: number("0.002"), Fast: number("3")}
	s := &gatewaySelection{Account: &upstreamAccount{Platform: "openai"}, Rate: "2", ChannelModel: "fast-test", GroupPricing: []modelPrice{p}}
	for _, tc := range []struct{ tier, total, actual, debit string }{
		{"priority", "0.6000000000", "0.1000000000", "1.20000000"},
		{"fast", "0.6000000000", "0.1000000000", "1.20000000"},
		{"default", "0.2000000000", "0.1000000000", "0.40000000"},
		{"flex", "0.1000000000", "0.0500000000", "0.20000000"},
	} {
		r, err := a.makeReceipt("fast", g, s, "fast-test", "", tc.tier, "", priceUsage{Input: 100, Output: 50}, false, 0, 0, time.Now(), "", "", "", "", "")
		if err != nil || r.Cost.Total != tc.total || r.Cost.Actual != tc.actual || r.AccountDebit != tc.debit || r.ServiceTier != tc.tier {
			t.Fatal("tier and account costs must survive the concession", tc, r, err)
		}
	}
	for _, tc := range []struct{ source, target string }{{"anthropic", "openai"}, {"composite", "grok"}} {
		g.SourcePlatform, s.Account.Platform = tc.source, tc.target
		r, err := a.makeReceipt("other-platform", g, s, "fast-test", "", "priority", "", priceUsage{Input: 100, Output: 50}, false, 0, 0, time.Now(), "", "", "", "", "")
		if err != nil || r.Cost.Actual != "0.3000000000" {
			t.Fatal("concession applied outside OpenAI source/target", tc, r, err)
		}
	}
	g.SourcePlatform, s.Account.Platform = "openai", "openai"
	// Search retains its own tariff. Image token pricing retains the group
	// multiplier, even if an independent per-image multiplier is configured.
	g.Group.SearchPrice = number("10")
	for _, images := range []bool{false, true} {
		u := priceUsage{Input: 100, Output: 50, SearchCalls: 2}
		actual := "0.1100000000"
		if images {
			u.ImageCount, u.ImageSize = 1, "2K"
			s.ResponseImage = &responseImageConfig{Model: "fast-test"}
			independent := true
			g.Group.IndependentImage, g.Group.ImageRate = &independent, number("0.25")
		}
		r, err := a.makeReceipt("fast", g, s, "fast-test", "", "priority", "", u, false, 0, 0, time.Now(), "", "", "", "", "")
		if err != nil || r.Cost.Total != "0.6200000000" || r.Cost.Actual != actual || r.AccountDebit != "1.24000000" {
			t.Fatal("tool cost or image concession", images, r, err)
		}
	}
	for _, tc := range []struct {
		source, target, protocol string
		count                    bool
	}{
		{"anthropic", "openai", "chat_completions", false}, {"openai", "grok", "responses", false},
		{"composite", "openai", "anthropic", false}, {"openai", "openai", "responses", true},
	} {
		g.SourcePlatform, g.Group.ForceFast = tc.source, true
		account := &upstreamAccount{Platform: tc.target, Credentials: map[string]json.RawMessage{"api_protocol": json.RawMessage(fmt.Sprintf("%q", tc.protocol))}}
		body := map[string]json.RawMessage{}
		if tier, err := g.applyFast(body, account, textRequest{Protocol: "responses", CountOnly: tc.count}); err != nil || tier != "" || body["service_tier"] != nil {
			t.Fatal("force applied outside eligible source/protocol", tc, tier, err)
		}
	}
}

func testGroupFast(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.208:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var e struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &e) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return e.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "fast@example.test", "password": "fast-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "fast@example.test", "password": "fast-password"})["access_token"].(string)
	var calls, mutate atomic.Int64
	var want, observed atomic.Value
	want.Store("priority")
	observed.Store("")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got := credentialString(body, "service_tier"); got != want.Load().(string) {
			t.Error("wrong outgoing service tier", got, want.Load())
		}
		if gid := mutate.Swap(0); gid != 0 {
			if _, err := a.DB.Exec("UPDATE groups SET free_openai_fast=false,force_openai_fast=false WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		tier := observed.Load().(string)
		var response, terminal string
		if r.URL.Path == "/v1/responses" {
			response = fmt.Sprintf(`{"id":"resp_fast_%d","object":"response","status":"completed","model":"fast-test","service_tier":%q,"output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n, tier)
			terminal = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
		} else {
			response = fmt.Sprintf(`{"id":"chat_fast_%d","model":"fast-test","service_tier":%q,"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, n, tier)
			terminal = "data: " + response + "\n\ndata: [DONE]\n\n"
		}
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, terminal)
		} else {
			fmt.Fprint(w, response)
		}
	}))
	defer up.Close()
	for _, groupPlatform := range []string{"openai", "composite"} {
		for _, wire := range []string{"chat_completions", "responses"} {
			prices := []modelPrice{{Platform: "openai", Models: []string{"fast-test"}, Input: number("0.001"), Output: number("0.002"), CacheRead: number("0"), CacheWrite: number("0"), Fast: number("3")}}
			group := must("POST", "/api/v1/admin/groups", admin, map[string]any{"allow_messages_dispatch": true, "name": "Fast " + groupPlatform + wire, "platform": groupPlatform, "rate_multiplier": "0.5", "force_openai_fast": true, "free_openai_fast": true, "model_pricing": prices})
			gid := id(group)
			gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
			if group["force_openai_fast"] != true || group["free_openai_fast"] != true {
				t.Fatal("create fast flags not stored", group)
			}
			if groupPlatform == "composite" {
				w := call("POST", gp+"/composite-routes", admin, map[string]any{"public_model": "fast-test", "target_platform": "openai"}, "")
				if w.Code != 201 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			account := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Fast upstream", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "rate_multiplier": "2", "credentials": map[string]any{"base_url": up.URL, "api_key": "fast-secret", "api_protocol": wire}, "extra": map[string]any{"quota_limit": 100}})
			key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Fast", "group_id": gid, "quota": 100, "rate_limit_5h": 100})["key"].(string)
			for _, patch := range []any{map[string]any{"description": "keep"}, map[string]any{"force_openai_fast": nil, "free_openai_fast": nil}} {
				v := must("PUT", gp, admin, patch)
				if v["force_openai_fast"] != true || v["free_openai_fast"] != true {
					t.Fatal("omission/null cleared policy")
				}
			}
			check := func(w *httptest.ResponseRecorder, tier, total, actual string) {
				t.Helper()
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				var gotTier, gotTotal, gotActual, accountRate string
				if err := a.DB.QueryRow("SELECT COALESCE(service_tier,''),total_cost::text,actual_cost::text,account_rate_multiplier::text FROM usage_logs WHERE request_id=$1 AND user_id=$2", w.Header().Get("X-Request-ID"), uid).Scan(&gotTier, &gotTotal, &gotActual, &accountRate); err != nil || gotTier != tier || gotTotal != total || gotActual != actual || accountRate != "2.0000" {
					t.Fatal("fast accounting", gotTier, gotTotal, gotActual, accountRate, err)
				}
			}
			want.Store("priority")
			observed.Store("")
			for _, entry := range []string{"chat/completions", "responses", "messages"} {
				body := map[string]any{"model": "fast-test", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "max_tokens": 20}
				if entry == "responses" {
					delete(body, "messages")
					delete(body, "max_tokens")
					body["input"] = "hello"
				}
				for _, stream := range []bool{false, true} {
					body["stream"] = stream
					check(call("POST", "/v1/"+entry, key, body, ""), "priority", "0.0600000000", "0.0100000000")
				}
			}
			var accountUsed string
			if err := a.DB.QueryRow("SELECT extra->>'quota_used' FROM accounts WHERE id=$1", id(account)).Scan(&accountUsed); err != nil || accountUsed != "0.72000000" {
				t.Fatal("account concession leaked into upstream quota", accountUsed, err)
			}
			var keyUsed, windowUsed string
			if err := a.DB.QueryRow("SELECT quota_used::text,usage_5h::text FROM api_keys WHERE key=$1", key).Scan(&keyUsed, &windowUsed); err != nil || keyUsed != "0.06000000" || windowUsed != "0.06000000" {
				t.Fatal("customer counters did not use concession", keyUsed, windowUsed, err)
			}
			body := map[string]any{"model": "fast-test", "input": "hello", "service_tier": "flex"}
			mutate.Store(gid)
			check(call("POST", "/v1/responses", key, body, "fast-replay"), "priority", "0.0600000000", "0.0100000000")
			before := calls.Load()
			if w := call("POST", "/responses", key, body, "fast-replay"); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || before != calls.Load() {
				t.Fatal("policy edit broke completed replay", w.Code)
			}
			want.Store("flex")
			check(call("POST", "/responses", key, body, ""), "flex", "0.0100000000", "0.0050000000")
			body["service_tier"] = " FAST "
			want.Store("priority")
			check(call("POST", "/responses", key, body, ""), "priority", "0.0600000000", "0.0300000000")
			flags := must("PUT", gp, admin, map[string]any{"free_openai_fast": true})
			if flags["force_openai_fast"] != false || flags["free_openai_fast"] != true {
				t.Fatal("partial fast edit")
			}
			check(call("POST", "/responses", key, body, ""), "priority", "0.0600000000", "0.0100000000")
			observed.Store("default")
			check(call("POST", "/responses", key, body, ""), "default", "0.0200000000", "0.0100000000")
			delete(body, "service_tier")
			want.Store("")
			observed.Store("priority")
			check(call("POST", "/responses", key, body, ""), "", "0.0200000000", "0.0100000000")
			body["service_tier"] = "invalid"
			before = calls.Load()
			if w := call("POST", "/responses", key, body, ""); w.Code != 400 || before != calls.Load() {
				t.Fatal("invalid tier reached upstream", w.Code)
			}
			if w := call("PUT", gp, user, map[string]any{"free_openai_fast": true}, ""); w.Code != 403 {
				t.Fatal("user modified charge policy", w.Code)
			}
			if w := call("GET", "/api/v1/groups/available", user, nil, ""); strings.Contains(w.Body.String(), "free_openai_fast") || strings.Contains(w.Body.String(), "force_openai_fast") {
				t.Fatal("administrator policy exposed")
			}
		}
	}
	for _, platform := range []string{"anthropic", "gemini", "grok", "kimi", "deepseek", "zhipu", "minimax"} {
		v := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "No Fast " + platform, "platform": platform, "force_openai_fast": true, "free_openai_fast": true})
		v = must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", id(v)), admin, map[string]any{"force_openai_fast": true, "free_openai_fast": true})
		if v["force_openai_fast"] != false || v["free_openai_fast"] != false {
			t.Fatal("unsupported platform flags not normalized", platform)
		}
	}
}
