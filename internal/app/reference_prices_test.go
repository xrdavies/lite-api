package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReferenceCatalog(t *testing.T) {
	c, err := parsePriceCatalog(bundledPrices)
	if err != nil {
		t.Fatal(err)
	}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		found := false
		for _, p := range c.Prices {
			found = found || p.Platform == platform
		}
		if !found {
			t.Fatal("missing platform", platform)
		}
	}
	luna, ok := c.lookup("openai", "gpt-5.6-luna")
	if !ok || luna.Input.String() != "0.0000002" || luna.CacheWrite.String() != "0.00000025" {
		t.Fatal("fixed reference price", luna)
	}
	for _, test := range []struct {
		context int64
		want    string
	}{{272000, "0.0544000000"}, {272001, "0.1088004000"}} {
		cost, err := calculatePrice(luna, priceUsage{Input: test.context}, "1", "", "", "", time.Time{}, true)
		if err != nil || cost.Total != test.want {
			t.Fatal("context threshold", cost, err)
		}
	}
	grok, ok := c.lookup("grok", "grok-4.7")
	if !ok {
		t.Fatal("missing grok")
	}
	cost, err := calculatePrice(grok, priceUsage{Input: 200000}, "1", "", "", "", time.Time{}, true)
	if err != nil || cost.Total != "0.8000000000" {
		t.Fatal("inclusive threshold", cost, err)
	}
	mini, _ := c.lookup("openai", "gpt-4o-mini")
	cost, err = calculatePrice(mini, priceUsage{Input: 3, Output: 3}, "1", "priority", "", "", time.Time{}, true)
	if err != nil || cost.Total != "0.0000037500" {
		t.Fatal("priority fractional ratio rounded", cost, err)
	}
	deepseek, _ := c.lookup("deepseek", "deepseek-v4-pro")
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC), "0.0000300000"},
		{time.Date(2026, 9, 23, 4, 0, 0, 0, time.UTC), "0.0000150000"},
		{time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC), "0.0000150000"},
	} {
		cost, err := calculatePrice(deepseek, priceUsage{Input: 100}, "1", "", "", "", tc.at, true)
		if err != nil || cost.Total != tc.want {
			t.Fatal("fixed DeepSeek time policy", cost, err)
		}
	}
	custom := modelPrice{Platform: "openai", Models: []string{"gpt-5.6-luna"}, BillingMode: "token", Input: number("0"), CacheWrite: number("0.000001")}
	merged, err := resolvedModelPrice(c, []modelPrice{custom}, "openai", "gpt-5.6-luna", true)
	if err != nil || merged.Input.String() != "0" || merged.Output.String() != luna.Output.String() || merged.CacheWrite1h.String() != "0.000001" || len(merged.Intervals) != 1 {
		t.Fatal("reference overrides", merged, err)
	}
	if _, err := resolvedModelPrice(c, nil, "openai", "gpt-5.6-luna", true); err == nil {
		t.Fatal("reference bypassed channel restriction")
	}
	if _, err := resolvedModelPrice(c, nil, "openai", "unknown-gpt-model", false); err == nil {
		t.Fatal("unknown model guessed")
	}
	for _, name := range []string{"gpt-5.6-luna-high", "gpt-5.6-luna-20260923", "models/gpt-5.6-luna"} {
		p, ok := c.lookup("openai", name)
		if !ok || p.Input.String() != luna.Input.String() {
			t.Fatal("known model variant", name, p)
		}
	}
	if p, err := resolvedModelPrice(c, nil, "openai", "claude-sonnet-4-6", false); err != nil || p.Platform != "openai" || p.Input.String() != "0.000003" {
		t.Fatal("relay model reference", p, err)
	}
	variant := modelPrice{Platform: "openai", Models: []string{"gpt-5.6-luna-high"}, BillingMode: "token", Input: number("0.1")}
	if p, err := resolvedModelPrice(c, []modelPrice{custom, variant}, "openai", "gpt-5.6-luna-high", true); err != nil || p.Input.String() != "0.1" {
		t.Fatal("literal variant lost precedence", p, err)
	}
	if p, err := resolvedModelPrice(c, []modelPrice{custom}, "openai", "gpt-5.6-luna-high", true); err != nil || p.Input.String() != "0" {
		t.Fatal("variant bypassed custom price", p, err)
	}
	if custom.Output != nil || luna.Input.String() != "0.0000002" {
		t.Fatal("mutated catalog or configuration")
	}
	// A configured time policy does not inherit the default provider peak policy.
	dsCustom := modelPrice{Platform: "deepseek", Models: []string{"deepseek-v4-pro"}, BillingMode: "token", Input: number("0.000001")}
	dsMerged, err := resolvedModelPrice(c, []modelPrice{dsCustom}, "deepseek", "deepseek-v4-pro", false)
	if err != nil || dsMerged.TimePricing != nil {
		t.Fatal("provider peak applied to channel tariff", err)
	}
	for _, raw := range []string{`null`, `{}`, `{"as_of":"2026-09-23","source":"test","prices":[]}`, `{"as_of":"2026-09-23","source":"test","prices":[{"platform":"openai","models":["m*"],"input_price":1}]}`, `{"as_of":"2026-09-23","source":"test","prices":[{"platform":"openai","models":["m"],"input_price":-1}]}`, `{"as_of":"2026-09-23","source":"test","prices":[{"platform":"openai","models":["m"],"input_price":1,"unsupported_cost":2}]}`} {
		if _, err := parsePriceCatalog([]byte(raw)); err == nil {
			t.Fatal("invalid catalog accepted", raw)
		}
	}
	file := filepath.Join(t.TempDir(), "prices.json")
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(bundledPrices)
	a := &App{priceFile: file}
	if err := a.reloadPrices(); err != nil {
		t.Fatal(err)
	}
	first := a.prices.Load()
	selection := gatewaySelection{Account: &upstreamAccount{Platform: "openai"}, Catalog: first}
	write([]byte(`{"prices":`))
	if err := a.reloadPrices(); err == nil || a.prices.Load() != first {
		t.Fatal("failed reload replaced catalog")
	}
	luna.Input = number("0.05")
	replacement, _ := json.Marshal(priceCatalog{AsOf: "2026-09-23", Source: "test", Prices: []referencePrice{{modelPrice: luna}}})
	write(replacement)
	if err := a.reloadPrices(); err != nil {
		t.Fatal(err)
	}
	old, err := selection.price("gpt-5.6-luna")
	if err != nil || old.Input.String() != "0.0000002" {
		t.Fatal("in-flight price changed", old, err)
	}
	current, ok := a.prices.Load().lookup("openai", "gpt-5.6-luna")
	if !ok || current.Input.String() != "0.05" {
		t.Fatal("new price not published")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadPrices(); err == nil || a.prices.Load().hash == first.hash {
		t.Fatal("missing file silently changed prices")
	}
}

func testReferencePricing(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.90:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != 200 {
			t.Fatalf("%s %s %d %s", method, path, w.Code, w.Body.String())
		}
		var e struct{ Data map[string]json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		return e.Data
	}
	idOf := func(v map[string]json.RawMessage) int64 {
		var id int64
		if err := json.Unmarshal(v["id"], &id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	str := func(v json.RawMessage) string {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	expect := func(want int, method, path, token string, body any) {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != want {
			t.Fatalf("%s %s: %d want %d %s", method, path, w.Code, want, w.Body.String())
		}
	}
	uid := idOf(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "reference@example.test", "password": "reference-password", "balance": "10"}))
	user := str(must("POST", "/api/v1/auth/login", "", map[string]any{"email": "reference@example.test", "password": "reference-password"})["access_token"])
	const pricingPath = "/api/v1/admin/channels/model-pricing"
	for _, path := range []string{pricingPath + "?model=gpt-5.6-luna", "/api/v1/admin/channels/pricing/sync-models?platform=openai"} {
		expect(401, "GET", path, "", nil)
		expect(403, "GET", path, user, nil)
	}
	expect(400, "GET", pricingPath, admin, nil)
	expect(400, "GET", "/api/v1/admin/channels/pricing/sync-models?platform=bedrock", admin, nil)
	if got := must("GET", pricingPath+"?model=not-a-priced-model", admin, nil); string(got["found"]) != "false" {
		t.Fatal("unknown reference", got)
	}
	if got := must("GET", pricingPath+"?model=gpt-5.6-luna", admin, nil); string(got["found"]) != "true" || string(got["input_price"]) != "0.0000002" || str(got["as_of"]) != "2026-09-23" {
		t.Fatal("reference response", got)
	}
	list := must("GET", "/api/v1/admin/channels/pricing/sync-models?platform=kimi", admin, nil)
	if !bytes.Contains(list["models"], []byte("kimi-k3")) || bytes.Contains(list["models"], []byte("gpt-")) {
		t.Fatal("platform reference list", list)
	}
	var calls atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "gpt-5.6-luna" || r.Header.Get("Authorization") != "Bearer reference-upstream" {
			t.Error("reference routing/authentication")
		}
		if n == 2 {
			started <- struct{}{}
			<-release
		}
		fmt.Fprint(w, `{"id":"ref","model":"gpt-5.6-luna","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2}}}`)
	}))
	defer func() { close(release); up.Close() }()
	gid := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Reference", "platform": "openai"}))
	key := str(must("POST", "/api/v1/keys", user, map[string]any{"name": "Reference", "group_id": gid})["key"])
	must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Reference", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "reference-upstream", "base_url": up.URL}})
	body := map[string]any{"model": "gpt-5.6-luna", "messages": []any{map[string]any{"role": "user", "content": "ok"}}}
	queryCost := func(request string, want string) {
		t.Helper()
		var cost string
		if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE user_id=$1 AND request_id=$2", uid, request).Scan(&cost); err != nil || cost != want {
			t.Fatal("reference settlement", cost, want, err)
		}
	}
	w := call("POST", "/v1/chat/completions", key, body)
	if w.Code != 200 {
		t.Fatal("reference-only gateway", w.Code, w.Body.String())
	}
	queryCost(w.Header().Get("X-Request-ID"), "0.0000076400")
	original := a.prices.Load()
	defer a.prices.Store(original)
	file := filepath.Join(t.TempDir(), "prices.json")
	oldFile := a.priceFile
	// Set the test path under the reload lock; the worker reads it under that lock.
	a.priceMu.Lock()
	a.priceFile = file
	a.priceMu.Unlock()
	defer func() { a.priceMu.Lock(); a.priceFile = oldFile; a.priceMu.Unlock() }()
	changed := []byte(`{"as_of":"2026-09-24","source":"integration test","prices":[{"platform":"openai","models":["gpt-5.6-luna"],"input_price":0.01,"output_price":0.02,"cache_read_price":0.001}]}`)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "/v1/chat/completions", key, body) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	if err := os.WriteFile(file, changed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadPrices(); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	w = <-done
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	queryCost(w.Header().Get("X-Request-ID"), "0.0000076400")
	w = call("POST", "/v1/chat/completions", key, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	queryCost(w.Header().Get("X-Request-ID"), "0.1820000000")
	if err := os.WriteFile(file, []byte(`{"prices":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.reloadPrices(); err == nil {
		t.Fatal("invalid reload accepted")
	}
	expect(200, "POST", "/v1/chat/completions", key, body)
	a.prices.Store(original)
	// Even token-count requests cannot use reference prices to bypass restrictions.
	cid := idOf(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Restricted reference", "group_ids": []int64{gid}, "restrict_models": true}))
	chPath := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	count := calls.Load()
	expect(403, "POST", "/v1/chat/completions", key, body)
	if calls.Load() != count {
		t.Fatal("restricted reference reached upstream")
	}
	must("PUT", chPath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"gpt-5.6-luna"}, "input_price": 0}}})
	w = call("POST", "/v1/chat/completions", key, body)
	if w.Code != 200 {
		t.Fatal("partial channel overrides", w.Code, w.Body.String())
	}
	queryCost(w.Header().Get("X-Request-ID"), "0.0000060400")
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": true, "model_plaza_require_auth": true})
	plaza := must("GET", "/api/v1/model-plaza", user, nil)
	if !bytes.Contains(plaza["groups"], []byte(`"official_pricing":{`)) || str(plaza["pricing_checksum"]) != original.hash {
		t.Fatal("reference directory missing catalog metadata")
	}
	// Other endpoint aliases still use ordinary permissions and no upstream secrets.
	if strings.Contains(string(plaza["groups"]), "reference-upstream") {
		t.Fatal("credential in price directory")
	}
}
