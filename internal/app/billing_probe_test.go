package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const billingDeclaration = `{"object":"lite-api.key_billing","schema_version":1,"billing_scope":"token","group_rate_multiplier":2,"user_rate_multiplier":0.123456,"resolved_rate_multiplier":0.123456,"peak_rate_enabled":false,"effective_rate_multiplier":0.123456,"observed_at":"2026-09-24T12:00:00Z"}`

func TestBillingDeclaration(t *testing.T) {
	b, err := parseBillingInfo([]byte(billingDeclaration))
	if err != nil || billingSyncRate(b).String() != "0.1235" {
		t.Fatal("exact decimal conversion", b, err)
	}
	for _, edit := range []struct{ old, new string }{
		{`"schema_version":1`, `"schema_version":2`},
		{`"billing_scope":"token"`, `"billing_scope":"image"`},
		{`"group_rate_multiplier":2`, `"group_rate_multiplier":null`},
		{`"resolved_rate_multiplier":0.123456`, `"resolved_rate_multiplier":0.12`},
		{`"effective_rate_multiplier":0.123456`, `"effective_rate_multiplier":0.12`},
		{`"peak_rate_enabled":false`, `"peak_rate_enabled":true`},
		{`"peak_rate_enabled":false`, `"peak_rate_enabled":null`},
		{`2026-09-24T12:00:00Z`, `0001-01-01T00:00:00Z`},
		{`0.123456`, `-0.123456`}, {`0.123456`, `1e100000`},
	} {
		if _, err := parseBillingInfo([]byte(strings.ReplaceAll(billingDeclaration, edit.old, edit.new))); err == nil {
			t.Fatal("invalid declaration accepted", edit)
		}
	}
	peak := strings.Replace(billingDeclaration, `"peak_rate_enabled":false`, `"peak_rate_enabled":true,"peak_start":"10:00","peak_end":"14:00","timezone":"UTC","peak_rate_multiplier":2,"applied_peak_multiplier":2`, 1)
	peak = strings.Replace(peak, `"effective_rate_multiplier":0.123456`, `"effective_rate_multiplier":0.246912`, 1)
	b, err = parseBillingInfo([]byte(peak))
	if err != nil || billingSyncRate(b).String() != "0.1235" {
		t.Fatal("sync must exclude peak", b, err)
	}
	for _, raw := range []string{strings.Replace(peak, "10:00", "15:00", 1), strings.Replace(peak, "UTC", "Local", 1), strings.Replace(peak, `"applied_peak_multiplier":2`, `"applied_peak_multiplier":1`, 1)} {
		if _, err := parseBillingInfo([]byte(raw)); err == nil {
			t.Fatal("invalid peak declaration accepted")
		}
	}
	for _, value := range []string{"0", "0.00001", "100.0001", "99999"} {
		n := json.Number(value)
		if billingSyncRate(&keyBillingInfo{Resolved: &n}) != nil {
			t.Fatal("unsafe automatic multiplier", value)
		}
	}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		if _, official, err := billingProbeBase(&upstreamAccount{Platform: platform}); err != nil || !official {
			t.Fatal("official endpoint should not be probed", platform, err)
		}
	}
	for _, item := range []struct {
		base string
		want bool
	}{{"https://API.OPENAI.COM./v1", true}, {"https://api.openai.com.evil.example", false}, {"https://relay.example/team/v1", false}} {
		raw, _ := json.Marshal(item.base)
		if _, got, err := billingProbeBase(&upstreamAccount{Platform: "openai", Credentials: map[string]json.RawMessage{"base_url": raw}}); err != nil || got != item.want {
			t.Fatal("domain boundary", item, got, err)
		}
	}
	for _, raw := range []string{`{"upstream_billing_probe_enabled":false,"upstream_billing_rate_sync_enabled":true}`, `{"extra":{"upstream_billing_rate_sync_enabled":null}}`, `{"upstream_billing_rate_sync_enabled":true,"extra":{"upstream_billing_rate_sync_enabled":false}}`} {
		var in accountInput
		if json.Unmarshal([]byte(raw), &in) != nil || in.validate(false) == nil {
			t.Fatal("invalid flags accepted", raw)
		}
	}
	now := time.Now()
	if got := billingProbeDelay(5, false, "7200", now); got < 2*time.Hour {
		t.Fatal("Retry-After shortened", got)
	}
	if got := billingProbeDelay(1440, true, "", now); got != 24*time.Hour {
		t.Fatal("unsupported delay not capped", got)
	}
}

func testBillingProbes(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	const root = "/api/v1/admin/accounts/"
	call := func(method, path, token string, body any, headers ...string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.212:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		for i := 0; i < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	settings := root + "upstream-billing-probe/settings"
	if got := manage("GET", settings, nil); got["enabled"] != true || got["interval_minutes"] != float64(30) {
		t.Fatal("defaults", got)
	}
	for _, body := range []any{nil, map[string]any{}, map[string]any{"enabled": true, "interval_minutes": 4}, map[string]any{"enabled": true, "interval_minutes": 1441}} {
		if w := call("PUT", settings, admin, body); w.Code != 400 {
			t.Fatal("invalid settings", w.Code)
		}
	}
	manage("PUT", settings, map[string]any{"enabled": false, "interval_minutes": 5})
	defer exec("DELETE FROM settings WHERE key=$1", billingProbeSetting)
	var mode atomic.Int32
	var calls atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/team"+keyBillingPath || r.Header.Get("Authorization") != "Bearer billing-test-secret" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("X-Goog-Api-Key") != "" {
			t.Error("billing protocol, path or credential isolation")
			w.WriteHeader(400)
			return
		}
		switch mode.Load() {
		case 1:
			w.Header().Set("Retry-After", "7200")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":"billing-test-secret"}`)
		case 2:
			w.WriteHeader(404)
		case 3:
			fmt.Fprint(w, strings.Repeat("x", (64<<10)+1))
		case 4:
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			fmt.Fprint(w, billingDeclaration)
		case 5:
			w.Header().Set("Location", "/must-not-follow")
			w.WriteHeader(307)
		case 6:
			fmt.Fprint(w, strings.ReplaceAll(billingDeclaration, "0.123456", "0"))
		case 7:
			fmt.Fprint(w, `{}`)
		default:
			fmt.Fprint(w, strings.TrimSuffix(billingDeclaration, "}")+`,"api_key":"must-not-persist"}`)
		}
	}))
	defer server.Close()
	var ids []int64
	defer func() {
		for _, id := range ids {
			exec("UPDATE accounts SET deleted_at=now() WHERE id=$1", id)
		}
	}()
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		suffix := "/v1"
		if cnPaygPlatform(platform) {
			suffix = "/anthropic/v1"
		}
		account := manage("POST", "/api/v1/admin/accounts", map[string]any{"name": "Billing probe " + platform, "platform": platform, "type": "apikey", "credentials": map[string]any{"base_url": server.URL + "/team" + suffix, "api_key": "billing-test-secret"}})
		id := int64(account["id"].(float64))
		ids = append(ids, id)
		out := manage("POST", root+fmt.Sprint(id)+"/upstream-billing-probe", nil)
		if out["snapshot"].(map[string]any)["status"] != "ok" {
			t.Fatal("platform probe failed", platform, out)
		}
	}
	id, path := ids[0], root+fmt.Sprint(ids[0])
	for _, entry := range []struct{ method, path string }{{"GET", settings}, {"PUT", settings}, {"POST", root + "upstream-billing-probe/batch"}, {"GET", root + "upstream-billing-rates"}, {"PUT", path + "/upstream-billing-probe"}, {"POST", path + "/upstream-billing-probe"}} {
		if w := call(entry.method, entry.path, ordinary, nil); w.Code != 403 {
			t.Fatal("non-admin allowed", entry, w.Code)
		}
	}
	manage("PUT", path, map[string]any{"upstream_billing_rate_sync_enabled": true})
	exec(`UPDATE accounts SET extra=extra || '{"quota_used":17,"untouched":"keep"}'::jsonb WHERE id=$1`, id)
	probe := func() *billingSnapshot {
		t.Helper()
		w := call("POST", path+"/upstream-billing-probe", admin, nil)
		var out struct{ Data billingProbeResult }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Data.Snapshot == nil {
			t.Fatalf("probe: %d %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "billing-test-secret") || strings.Contains(w.Body.String(), "must-not-persist") {
			t.Fatal("probe leaked secrets")
		}
		return out.Data.Snapshot
	}
	good := probe()
	if good.Synced == nil || good.Synced.String() != "0.1235" {
		t.Fatal("rate not synced", good)
	}
	if w := call("PUT", path, admin, map[string]any{"rate_multiplier": 2}); w.Code != 409 {
		t.Fatal("manual rate change bypassed sync", w.Code)
	}
	for _, scenario := range []struct {
		mode          int32
		status, error string
	}{{1, "failed", "http_error"}, {2, "unsupported", "unsupported"}, {3, "failed", "response_too_large"}, {5, "failed", "http_error"}, {7, "failed", "invalid_response"}} {
		mode.Store(scenario.mode)
		before := calls.Load()
		got := probe()
		if got.Status != scenario.status || got.Error != scenario.error || got.Synced != nil || got.Data == nil || !got.Received.Equal(*good.Received) || !got.Fresh.Equal(*good.Fresh) || calls.Load() != before+1 {
			t.Fatal("failed probe changed last good snapshot or retried", got)
		}
		if scenario.mode == 1 && got.Next.Sub(got.Attempt) < 2*time.Hour {
			t.Fatal("Retry-After lost")
		}
	}
	mode.Store(6)
	if got := probe(); got.Status != "ok" || got.Synced != nil {
		t.Fatal("zero declaration must not zero account cost", got)
	}
	var rate, extra string
	if err := a.DB.QueryRow("SELECT rate_multiplier::text,extra::text FROM accounts WHERE id=$1", id).Scan(&rate, &extra); err != nil || rate != "0.1235" || !strings.Contains(extra, `"quota_used": 17`) || !strings.Contains(extra, `"untouched": "keep"`) || strings.Contains(extra, "must-not-persist") {
		t.Fatal("probe mutated unrelated state", rate, extra, err)
	}
	mode.Store(4)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", path+"/upstream-billing-probe", admin, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	if w := call("POST", path+"/upstream-billing-probe", admin, nil); w.Code != 409 {
		t.Error("concurrent probe accepted", w.Code)
	}
	manage("PUT", path, map[string]any{"upstream_billing_rate_sync_enabled": false, "rate_multiplier": 3})
	release <- struct{}{}
	if w := <-done; w.Code != 409 {
		t.Fatal("stale probe overwrote administrator edit", w.Code, w.Body.String())
	}
	// Proxy fallback revisions also belong to the observed upstream identity.
	// Expired direct fallback avoids requiring a second proxy server here.
	proxy := manage("POST", "/api/v1/admin/proxies", map[string]any{"name": "Billing proxy", "protocol": "http", "host": "127.0.0.1", "port": 9, "expires_at": time.Now().Add(-time.Hour).Unix(), "fallback_mode": "direct"})
	proxyID := int64(proxy["id"].(float64))
	manage("PUT", path, map[string]any{"proxy_id": proxyID})
	go func() { done <- call("POST", path+"/upstream-billing-probe", admin, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("proxy probe did not start")
	}
	manage("PUT", "/api/v1/admin/proxies/"+fmt.Sprint(proxyID), map[string]any{"password": "rotated-proxy-secret"})
	release <- struct{}{}
	if w := <-done; w.Code != 409 {
		t.Fatal("stale proxy probe persisted", w.Code, w.Body.String())
	}
	manage("PUT", path, map[string]any{"proxy_id": 0})
	manage("DELETE", "/api/v1/admin/proxies/"+fmt.Sprint(proxyID), nil)
	mode.Store(0)
	manage("PUT", path+"/upstream-billing-probe", map[string]any{"enabled": false})
	if account, err := a.loadAccount(ctx, id); err != nil || billingFlag(account.Extra, billingSyncKey) || billingFlag(account.Extra, billingEnabledKey) {
		t.Fatal("disabling probes left sync enabled", err)
	}
	// Static snapshot route must beat the dynamic account-ID route; reads never probe.
	list := root + "upstream-billing-rates?search=Billing%20probe&page_size=100"
	before := calls.Load()
	w := call("GET", list, admin, nil)
	if w.Code != 200 || w.Header().Get("ETag") == "" || strings.Contains(w.Body.String(), "billing-test-secret") {
		t.Fatal("snapshot listing", w.Code, w.Body.String())
	}
	if cached := call("GET", list, admin, nil, "If-None-Match", "W/"+w.Header().Get("ETag")); cached.Code != 304 || cached.Body.Len() != 0 || calls.Load() != before {
		t.Fatal("snapshot conditional read performed work", cached.Code)
	}
	batch := manage("POST", root+"upstream-billing-probe/batch", map[string]any{"account_ids": []int64{id, id, ids[1], 999999999}})
	if results := batch["results"].([]any); len(results) != 3 || results[2].(map[string]any)["error"] != "probe_failed" {
		t.Fatal("batch deduplication or partial failure", batch)
	}
	for _, ids := range [][]int64{nil, {0}, make([]int64, 21)} {
		if w := call("POST", root+"upstream-billing-probe/batch", admin, map[string]any{"account_ids": ids}); w.Code != 400 {
			t.Fatal("invalid batch accepted", w.Code)
		}
	}
	// A changed credential invalidates previously observed billing/capability data.
	manage("PUT", path, map[string]any{"credentials": map[string]any{"api_key": "rotated-test-secret"}})
	if account, err := a.loadAccount(ctx, id); err != nil || account.Extra[billingSnapshotKey] != nil {
		t.Fatal("snapshot survived credential change", err)
	}
	// Cancelled requests must not replace a good snapshot or send a request.
	before = calls.Load()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := a.probeBilling(cancelled, ids[1], false); err == nil || calls.Load() != before {
		t.Fatal("cancelled probe ran", err)
	}
	// The persisted scheduler honors opt-in, global disable and due times.
	manage("PUT", root+fmt.Sprint(ids[1])+"/upstream-billing-probe", map[string]any{"enabled": true})
	exec("UPDATE accounts SET extra=extra - 'upstream_billing_probe' WHERE id=$1", ids[1])
	if err := a.runBillingProbes(ctx); err != nil || calls.Load() != before {
		t.Fatal("global disable ignored", err)
	}
	manage("PUT", settings, map[string]any{"enabled": true, "interval_minutes": 5})
	if err := a.runBillingProbes(ctx); err != nil || calls.Load() != before+1 {
		t.Fatal("due probe did not run", calls.Load()-before, err)
	}
	if err := a.runBillingProbes(ctx); err != nil || calls.Load() != before+1 {
		t.Fatal("future probe ran early", err)
	}
	manage("PUT", settings, map[string]any{"enabled": false, "interval_minutes": 5})
	// The public declaration uses client Key auth and precise per-user rates,
	// even at zero balance; it discloses no internal account identity or wallet.
	group := manage("POST", "/api/v1/admin/groups", map[string]any{"name": "Billing declaration", "platform": "openai", "rate_multiplier": 2})
	gid := int64(group["id"].(float64))
	w = call("POST", "/api/v1/keys", ordinary, map[string]any{"name": "Billing declaration", "group_id": gid})
	var key struct {
		Data struct {
			ID  int64
			Key string
		}
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &key) != nil || key.Data.Key == "" {
		t.Fatal("create client key", w.Code, w.Body.String())
	}
	exec(`INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) SELECT user_id,$2,0.1234 FROM api_keys WHERE id=$1`, key.Data.ID, gid)
	w = call("GET", keyBillingPath, key.Data.Key, nil)
	b, err := parseBillingInfo(w.Body.Bytes())
	if w.Code != 200 || err != nil || b.Resolved.String() != "0.1234" || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "account_id") {
		t.Fatal("client declaration", w.Code, w.Body.String(), err)
	}
	for _, token := range []string{"", ordinary, admin} {
		if w := call("GET", keyBillingPath, token, nil); w.Code != 401 {
			t.Fatal("management credential accepted as client key", w.Code)
		}
	}
	exec("UPDATE api_keys SET status='inactive' WHERE id=$1", key.Data.ID)
	if w := call("GET", keyBillingPath, key.Data.Key, nil); w.Code == 200 {
		t.Fatal("disabled client key accepted")
	}
}
