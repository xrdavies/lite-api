package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestGroupPriceResolution(t *testing.T) {
	catalog, err := parsePriceCatalog(bundledPrices)
	if err != nil {
		t.Fatal(err)
	}
	group := []modelPrice{{Platform: "openai", Models: []string{"gpt-5.6-luna"}, BillingMode: "token", Input: number("0.001"), CacheWrite: number("0"), Intervals: []priceInterval{{Input: number("9")}}}}
	channel := []modelPrice{{Platform: "openai", Models: []string{"gpt-5.6-luna"}, BillingMode: "token", Input: number("0.1"), Output: number("0.2")}}
	p, err := effectiveModelPrice(catalog, group, channel, "openai", "gpt-5.6-luna", true)
	if err != nil || p.Input.String() != "0.001" || p.Output.String() != "0.0000012" || p.CacheWrite1h.String() != "0" {
		t.Fatal("group card must replace channel and inherit only reference components", p, err)
	}
	for _, tc := range []struct {
		input int64
		long  bool
		want  string
	}{{272000, true, "272.0000000000"}, {272001, true, "544.0020000000"}, {272001, false, "272.0010000000"}} {
		cost, err := calculatePrice(p, priceUsage{Input: tc.input}, "1", "", "", "", time.Time{}, tc.long)
		if err != nil || cost.Total != tc.want {
			t.Fatal("group flat card must use the reference context ladder", tc, cost, err)
		}
	}
	if group[0].Intervals[0].Input.String() != "9" || group[0].Output != nil {
		t.Fatal("mutated group price snapshot")
	}
	if _, err := effectiveModelPrice(catalog, group, nil, "openai", "gpt-5.6-luna", true); err == nil {
		t.Fatal("group card bypassed channel admission")
	}
	if p, err := effectiveModelPrice(catalog, group, channel, "openai", "gpt-5.6-luna-high", false); err != nil || p.Input.String() != "0.1" {
		t.Fatal("group literals must not absorb effort variants", p, err)
	}
	// Labels are stored metadata; group matching uses names across platforms.
	group = []modelPrice{{Platform: "anthropic", Models: []string{"claude-*"}, BillingMode: "token", Input: number("0.2")}, {Platform: "openai", Models: []string{"claude-opus-4.6"}, BillingMode: "token", Input: number("0.1")}}
	if p, err := effectiveModelPrice(catalog, group, nil, "openai", "CLAUDE-OPUS-4-6", false); err != nil || p.Input.String() != "0.1" || p.Platform != "openai" {
		t.Fatal("exact normalized group price must win over wildcard", p, err)
	}
	group = []modelPrice{{Models: []string{"m"}, BillingMode: "per_request", PerRequest: number("0.1"), Intervals: []priceInterval{{Min: 10, PerRequest: number("0.2")}}}}
	p, err = effectiveModelPrice(catalog, group, nil, "openai", "m", false)
	if err != nil || len(p.Intervals) != 1 {
		t.Fatal("per-request tiers must survive group resolution", p, err)
	}
	cost, err := calculatePrice(p, priceUsage{Input: 11}, "2", "", "", "", time.Time{}, true)
	if err != nil || cost.Actual != "0.4000000000" {
		t.Fatal("per-request group price", cost, err)
	}
}

func testGroupPricing(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.93:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var envelope struct{ Data map[string]json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	idOf := func(v map[string]json.RawMessage) int64 {
		var id int64
		if err := json.Unmarshal(v["id"], &id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	str := func(raw json.RawMessage) string {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "group-price@example.test", "password": "price-password", "balance": "10"}))
	user := str(must("POST", "/api/v1/auth/login", "", map[string]any{"email": "group-price@example.test", "password": "price-password"})["access_token"])
	card := func(input string) []modelPrice {
		return []modelPrice{{Models: []string{"gpt-5.6-luna"}, Input: number(input)}}
	}
	gid := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Group pricing", "platform": "openai", "rate_multiplier": "2", "model_pricing": card("0.001")}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	for _, invalid := range []any{
		[]any{map[string]any{"models": []string{"m"}, "input_price": -1}},
		[]any{map[string]any{"models": []string{"m"}, "platform": "bedrock"}},
		[]any{map[string]any{"models": []string{"m"}, "unexpected": 1}},
		[]any{map[string]any{"models": []string{"m"}, "time_pricing": map[string]any{"timezone": "UTC", "periods": []any{map[string]any{"start_time": "09:00", "end_time": "10:00", "multiplier": 2}}}}},
		append(card("0.1"), card("0.2")...),
	} {
		w := call("PUT", gp, admin, map[string]any{"model_pricing": invalid})
		if w.Code != 400 {
			t.Fatal("invalid group pricing accepted", w.Code, w.Body.String())
		}
	}
	if w := call("PUT", gp, user, map[string]any{"model_pricing": card("0")}); w.Code != 403 {
		t.Fatal("user changed group prices", w.Code)
	}
	// A patch to an unrelated field preserves the entire stored JSON, including unknown fields.
	if _, err := a.DB.Exec(`UPDATE groups SET model_pricing=jsonb_set(model_pricing,'{0,future_field}','true') WHERE id=$1`, gid); err != nil {
		t.Fatal(err)
	}
	view := must("PUT", gp, admin, map[string]any{"description": "Keep pricing"})
	if !bytes.Contains(view["model_pricing"], []byte(`"future_field": true`)) && !bytes.Contains(view["model_pricing"], []byte(`"future_field":true`)) {
		t.Fatal("unrelated patch removed unknown price metadata", string(view["model_pricing"]))
	}
	key := str(must("POST", "/api/v1/keys", user, map[string]any{"name": "Group pricing", "group_id": gid, "quota": "100"})["key"])
	var calls atomic.Int32
	var hold atomic.Bool
	started, release := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model  string
			Stream bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "gpt-5.6-luna" || r.Header.Get("Authorization") != "Bearer group-price-upstream" {
			t.Error("group pricing changed upstream routing or credentials")
		}
		if hold.Swap(false) {
			started <- struct{}{}
			<-release
		}
		const response = `{"id":"group-price","model":"gpt-5.6-luna","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", response)
		} else {
			fmt.Fprint(w, response)
		}
	}))
	defer func() { close(release); up.Close() }()
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Group pricing", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "group-price-upstream", "base_url": up.URL}})
	body := map[string]any{"model": "gpt-5.6-luna", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
	check := func(w *httptest.ResponseRecorder, want string) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var cost, balance, used string
		if err := a.DB.QueryRow(`SELECT actual_cost::text FROM usage_logs WHERE user_id=$1 AND request_id=$2`, uid, w.Header().Get("X-Request-ID")).Scan(&cost); err != nil || cost != want {
			t.Fatal("group billing", cost, want, err)
		}
		if err := a.DB.QueryRow(`SELECT (10-u.balance)::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE u.id=$1 AND k.key=$2`, uid, key).Scan(&balance, &used); err != nil || rat(json.Number(balance)).Cmp(rat(json.Number(used))) != 0 {
			t.Fatal("balance and key usage diverged", balance, used, err)
		}
	}
	check(call("POST", "/v1/chat/completions", key, body), "0.0160120800") // No channel required.
	channelPrice := []modelPrice{{Platform: "openai", Models: []string{"gpt-5.6-luna"}, Input: number("0.1"), Output: number("0.8")}}
	cid := idOf(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Group price channel", "group_ids": []int64{gid}, "model_pricing": channelPrice, "restrict_models": true}))
	cp := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	hold.Store(true)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "/v1/chat/completions", key, body) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	must("PUT", gp, admin, map[string]any{"model_pricing": card("0.002")})
	release <- struct{}{}
	check(<-done, "0.0160120800")
	body["stream"] = true
	check(call("POST", "/v1/chat/completions", key, body), "0.0320120800")
	body["stream"] = false
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": true, "model_plaza_require_auth": true})
	var plaza []struct {
		ID     int64
		Models []struct {
			Name    string
			Pricing modelPrice
		}
	}
	if err := json.Unmarshal(must("GET", "/api/v1/model-plaza", user, nil)["groups"], &plaza); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range plaza {
		if g.ID == gid {
			found = len(g.Models) == 1 && g.Models[0].Pricing.Input.String() == "0.002" && g.Models[0].Pricing.Output.String() == "0.0000012"
		}
	}
	if !found {
		t.Fatal("model plaza does not show effective group prices")
	}
	must("PUT", cp, admin, map[string]any{"model_pricing": []any{}})
	count := calls.Load()
	if w := call("POST", "/v1/chat/completions", key, body); w.Code != 403 || calls.Load() != count {
		t.Fatal("group price bypassed channel restriction", w.Code)
	}
	must("PUT", cp, admin, map[string]any{"restrict_models": false, "model_pricing": channelPrice})
	// null / omission keep the current card; [] explicitly clears it.
	must("PUT", gp, admin, map[string]any{"model_pricing": nil})
	check(call("POST", "/v1/chat/completions", key, body), "0.0320120800")
	must("PUT", gp, admin, map[string]any{"model_pricing": []modelPrice{{Models: []string{"gpt-5.6-luna"}, Input: number("0"), Output: number("0"), CacheRead: number("0")}}})
	check(call("POST", "/v1/chat/completions", key, body), "0.0000000000")
	must("PUT", gp, admin, map[string]any{"model_pricing": []any{}})
	check(call("POST", "/v1/chat/completions", key, body), "9.6000000800")
}
