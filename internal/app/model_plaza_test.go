package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlazaPricingMatchesSettlement(t *testing.T) {
	end := int64(100)
	p := modelPrice{Platform: "openai", Models: []string{"m"}, BillingMode: "token", Input: number("0.000001000001"), Output: number("0.000002"), CacheWrite: number("0.000003"), CacheWrite1h: number("0.000006"), CacheRead: number("0"), Intervals: []priceInterval{{Min: 10, Max: &end, InputMultiplier: number("1.000001"), CacheWrite: number("0.000004")}, {Min: 200, Input: number("0.000005"), WriteMultiplier: number("2")}}}
	for _, enabled := range []bool{false, true} {
		view := plazaPrice(p, enabled)
		if enabled && (view.Intervals[1].Input.String() != "0.000001000002000001" || view.Intervals[1].CacheWrite1h.String() != "0.000004" || view.Intervals[2].CacheWrite1h.String() != "0.000006") {
			t.Fatal("tier multiplier, 1h override, or base fallback changed", view)
		}
		if enabled && len(view.Intervals) != 4 || !enabled && len(view.Intervals) != 0 {
			t.Fatal("directory lost tier gaps or ignored group setting", view)
		}
		for _, context := range []int64{1, 10, 11, 100, 101, 200, 201, 1000} {
			original, shown := tokenPrices(p, p.interval(context)), tokenPrices(view, view.interval(context))
			if !enabled {
				original = tokenPrices(p, p.interval(1))
			}
			for i := range original {
				if original[i].Cmp(shown[i]) != 0 {
					t.Fatal("display price differs from billing", enabled, context, i, original[i], shown[i])
				}
			}
		}
	}
	if p.Input.String() != "0.000001000001" || p.Intervals[0].InputMultiplier.String() != "1.000001" {
		t.Fatal("view mutated shared pricing")
	}
	for _, mode := range []string{"image", "per_request"} {
		card := modelPrice{BillingMode: mode, PerRequest: number("0.02"), Intervals: []priceInterval{{Label: "HD", PerRequest: number("0.03")}}}
		view := plazaPrice(card, false)
		if view.PerRequest.String() != "0.02" || len(view.Intervals) != 1 || view.Intervals[0].PerRequest.String() != "0.03" {
			t.Fatal("media/request price changed", view)
		}
	}
}

func testModelPlaza(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) (int, json.RawMessage) {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.77:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("personal directory cached publicly")
		}
		var envelope struct{ Data json.RawMessage }
		if w.Code != 200 {
			return w.Code, w.Body.Bytes()
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return w.Code, envelope.Data
	}
	must := func(method, path, token string, body any) json.RawMessage {
		t.Helper()
		code, data := call(method, path, token, body)
		if code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, code, data)
		}
		return data
	}
	expect := func(want int, method, path, token string, body any) {
		t.Helper()
		code, data := call(method, path, token, body)
		if code != want {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, code, want, data)
		}
	}
	idOf := func(raw json.RawMessage) int64 {
		var v struct{ ID int64 }
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	const path = "/api/v1/model-plaza"
	expect(404, "GET", path, "", nil)
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "plaza@example.test", "password": "plaza-password"}))
	var session struct {
		Access string `json:"access_token"`
	}
	if err := json.Unmarshal(must("POST", "/api/v1/auth/login", "", map[string]any{"email": "plaza@example.test", "password": "plaza-password"}), &session); err != nil {
		t.Fatal(err)
	}
	user := session.Access
	group := func(name string, private bool) int64 {
		return idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": name, "platform": "openai", "is_exclusive": private, "rate_multiplier": "2"}))
	}
	public, private := group("Plaza Public", false), group("Plaza Private", true)
	price := map[string]any{"platform": "openai", "models": []string{"plaza-model"}, "input_price": json.Number("0.000001000001"), "output_price": json.Number("0.000002"), "cache_write_price": json.Number("0.000003"), "cache_read_price": 0, "intervals": []any{map[string]any{"min_tokens": 100, "input_multiplier": 2}}, "time_pricing": map[string]any{"timezone": "UTC", "periods": []any{map[string]any{"start_time": "09:00", "end_time": "10:00", "multiplier": 0.5}}}}
	cid := idOf(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Plaza channel", "group_ids": []int64{public, private}, "model_pricing": []any{price, map[string]any{"platform": "anthropic", "models": []string{"plaza-other-platform"}, "input_price": 1}}, "model_mapping": map[string]any{"openai": map[string]string{"plaza-alias": "plaza-model", "plaza-unpriced": "missing-upstream"}}}))
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": true, "model_plaza_require_auth": true, "model_plaza_description": "Team prices"})
	expect(401, "GET", path, "", nil)
	expect(401, "GET", path, "invalid-session", nil)
	expect(403, "PUT", "/api/v1/admin/settings", user, map[string]any{"model_plaza_require_auth": false})
	var key struct{ Key string }
	if err := json.Unmarshal(must("POST", "/api/v1/keys", user, map[string]any{"name": "Plaza Key", "group_id": public}), &key); err != nil {
		t.Fatal(err)
	}
	expect(401, "GET", path, key.Key, nil)
	type viewModel struct {
		Name     string
		Pricing  *modelPrice
		Official json.RawMessage `json:"official_pricing"`
		Basis    string          `json:"long_context_basis"`
		Time     *timePrice      `json:"time_pricing"`
	}
	type viewGroup struct {
		ID       int64
		Rate     json.Number  `json:"rate_multiplier"`
		UserRate *json.Number `json:"user_rate_multiplier"`
		Models   []viewModel
	}
	read := func(token string) []viewGroup {
		t.Helper()
		raw := must("GET", path, token, nil)
		for _, secret := range []string{"account_stats", "model_mapping", "missing-upstream", "sort_order", "plaza-other-platform"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatal("directory leaked internal or other platform data", secret, string(raw))
			}
		}
		var view struct {
			Description string
			Groups      []viewGroup
		}
		if err := json.Unmarshal(raw, &view); err != nil {
			t.Fatal(err)
		}
		if view.Description != "Team prices" {
			t.Fatal("description missing")
		}
		return view.Groups
	}
	find := func(groups []viewGroup, id int64) *viewGroup {
		for i := range groups {
			if groups[i].ID == id {
				return &groups[i]
			}
		}
		return nil
	}
	groups := read(user)
	g := find(groups, public)
	if g == nil || find(groups, private) != nil || g.Rate != "2.0000" || g.UserRate != nil {
		t.Fatal("initial visibility/rates", groups)
	}
	var billed modelPrice
	raw, _ := json.Marshal(price)
	if err := json.Unmarshal(raw, &billed); err != nil {
		t.Fatal(err)
	}
	if err := validateModelPrices([]modelPrice{billed}, false); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range g.Models {
		if string(m.Official) != "null" {
			t.Fatal("invented reference price")
		}
		if m.Name == "plaza-unpriced" && m.Pricing != nil {
			t.Fatal("missing price shown as free")
		}
		if m.Name != "plaza-alias" {
			continue
		}
		found = true
		if m.Pricing == nil || m.Basis != "whole_request" || len(m.Pricing.Intervals) != 2 || m.Time == nil {
			t.Fatal("directory lost price schedule", m)
		}
		m.Pricing.Platform, m.Pricing.Models = "openai", []string{"plaza-model"}
		at := time.Date(2026, 9, 23, 9, 30, 0, 0, time.UTC)
		u := priceUsage{Input: 101, Output: 7, CacheWrite: 10, CacheWrite1h: 10}
		original, err := calculatePrice(billed, u, "2", "priority", "", "", at, true)
		if err != nil {
			t.Fatal(err)
		}
		shown, err := calculatePrice(*m.Pricing, u, "2", "priority", "", "", at, true)
		if err != nil || original.Actual != shown.Actual {
			t.Fatal("display/settlement mismatch", original, shown, err)
		}
	}
	if !found {
		t.Fatal("mapped model missing")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"allowed_groups": []int64{private}, "group_rates": map[string]any{fmt.Sprint(public): 0.75}})
	groups = read(user)
	g = find(groups, public)
	if find(groups, private) == nil || g == nil || g.UserRate == nil || *g.UserRate != "0.7500" || g.Rate != "2.0000" {
		t.Fatal("personal rate or grant missing", groups)
	}
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_require_auth": false})
	groups = read("")
	if find(groups, private) != nil || find(groups, public).UserRate != nil {
		t.Fatal("anonymous inherited personal scope")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"restrict_public_groups": true})
	groups = read(user)
	if find(groups, public) != nil || find(groups, private) == nil {
		t.Fatal("public group restriction ignored")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", private), admin, map[string]any{"long_context_pricing_enabled": false, "model_allowlist": map[string]any{"enabled": true, "models": []string{"plaza-alias"}}})
	g = find(read(user), private)
	if len(g.Models) != 1 || g.Models[0].Name != "plaza-alias" || len(g.Models[0].Pricing.Intervals) != 0 {
		t.Fatal("group price or model policy ignored", g)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"billing_model_source": "requested"})
	if g = find(read(user), private); g.Models[0].Pricing != nil {
		t.Fatal("alias showed mapped price under requested policy")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"status": "disabled"})
	if find(read(user), private) != nil {
		t.Fatal("disabled channel visible")
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"status": "disabled"})
	expect(401, "GET", path, user, nil)
	raw = must("GET", "/api/v1/settings/public", "", nil)
	if bytes.Contains(raw, []byte("Team prices")) {
		t.Fatal("public settings leaked restricted description")
	}
	expect(400, "PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_description": strings.Repeat("x", 10001)})
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": false})
	expect(404, "GET", path, "", nil)
	if _, err := a.Redis.Set(t.Context(), "lite-api:panel:public:ip:192.0.2.77", 300, time.Minute).Result(); err != nil {
		t.Fatal(err)
	}
	expect(429, "GET", path, "", nil)
}
