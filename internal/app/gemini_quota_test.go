package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeminiQuotaPolicy(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{`, `{} {}`, `{"unknown":true}`, `{"quota_rules":{"google_ai_pro":{}}}`, `{"tiers":{"aistudio_free":{"pro_rpd":-2}}}`, `{"quota_rules":{"aistudio_paid":{"rpm":-1}}}`, `{"quota_rules":{"aistudio_free":{"gemini_pro":{"rpd":-2}}}}`, `{"quota_rules":{"aistudio_paid":{},"AISTUDIO_PAID":{}}}`, `{"tiers":{"aistudio_paid":{"cooldown_minutes":-1}}}`, `{"quota_rules":{"aistudio_paid":{"shared_rpd":9223372036854775808}}}`} {
		if _, err := parseGeminiQuotaPolicy([]byte(raw), true); err == nil {
			t.Fatal("invalid quota policy accepted", raw)
		}
	}
	p, err := parseGeminiQuotaPolicy([]byte(`{"tiers":{"aistudio_free":{"pro_rpd":50,"flash_rpd":100,"cooldown_minutes":7}},"quota_rules":{" AISTUDIO_FREE ":{"shared_rpd":9007199254740993,"rpm":3,"gemini_pro":{"rpd":-1,"rpm":0},"gemini_flash":{"rpd":12}}}}`), true)
	q := geminiQuota{ProDay: 1, FlashDay: 2}
	if err != nil || !p.apply(&q, "aistudio_free") || q.SharedDay != 9007199254740993 || q.SharedMinute != 3 || q.ProDay != -1 || q.FlashDay != 12 || q.ProMinute != 0 {
		t.Fatal("V2 precedence or integer precision", q, err)
	}
	p, err = parseGeminiQuotaPolicy([]byte(`{"tiers":{"aistudio_free":{"pro_rpd":5,"flash_rpd":-1,"cooldown_minutes":0}},"future_field":{"kept":true}}`), false)
	q = geminiQuota{SharedDay: 10, FlashDay: 99}
	if err != nil || !p.apply(&q, "aistudio_free") || q.SharedDay != 5 || q.FlashDay != 99 {
		t.Fatal("legacy shared-pool override", q, err)
	}
	p, _ = parseGeminiQuotaPolicy([]byte(`{"tiers":{"aistudio_free":{"pro_rpd":0,"flash_rpd":13}}}`), true)
	if !p.apply(&q, "aistudio_free") || q.SharedDay != 0 || q.FlashDay != 13 {
		t.Fatal("legacy shared-pool removal", q)
	}
	for _, raw := range []string{strings.Repeat(" ", 32<<10) + `{}`, `{"quota_rules":{"aistudio_free":{"desc":"` + strings.Repeat("a", 2049) + `"}}}`, `{"quota_rules":{"aistudio_free":{"desc":"\u0000"}}}`} {
		if _, err := parseGeminiQuotaPolicy([]byte(raw), true); err == nil {
			t.Fatal("oversized or invalid description accepted")
		}
	}
	p, _ = parseGeminiQuotaPolicy([]byte(`{"quota_rules":{"aistudio_paid":{"shared_rpd":-9,"rpm":-2,"gemini_pro":{"rpd":-2,"rpm":-5}}}}`), false)
	q = geminiQuota{SharedDay: 3, ProDay: 4, ProMinute: 5}
	if !p.apply(&q, "aistudio_paid") || q.SharedDay != 0 || q.SharedMinute != 0 || q.ProDay != 0 || q.ProMinute != 0 {
		t.Fatal("stored-value normalization", q)
	}
	t.Setenv("GEMINI_QUOTA_POLICY", `{"quota_rules":{"aistudio_paid":{"rpm":7}}}`)
	if ConfigFromEnv().GeminiQuotaPolicy != `{"quota_rules":{"aistudio_paid":{"rpm":7}}}` {
		t.Fatal("environment policy not read")
	}
	if _, err := New(context.Background(), Config{GeminiQuotaPolicy: "invalid"}); err == nil || err.Error() != "invalid GEMINI_QUOTA_POLICY" {
		t.Fatal("invalid deployment policy did not fail before startup", err)
	}
}

func TestGeminiRateLimitSeconds(t *testing.T) {
	now := time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC) // DST transition: 23-hour day.
	for _, tc := range []struct {
		body, retry string
		want        int64
	}{
		{`{"error":{"details":[{"metadata":{"quotaResetDelay":"12.345s"}}]}}`, "1", 13},
		{`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"1.5s"}]}}`, "", 2},
		{`{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3600s"}]}}`, "", 900},
		{`{"error":{"details":[{"metadata":{"quotaResetDelay":"900h"}}]}}`, "", 86400},
		{`{"error":{"message":"Please retry in 3.001s"}}`, "", 4},
		{`{"error":{"message":"Quota per day exceeded","details":[{"metadata":{"quotaResetDelay":"1s"}}]}}`, "2", 23 * 3600},
		{`{"error":{"details":[{"@type":"not.RetryInfo","retryDelay":"1s"}]}}`, "9", 9},
		{`{"error":{"details":[{"metadata":{"quotaResetDelay":"-1s"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2.1s"}]}}`, "", 3},
		{`{"error":{"message":"Please retry in -1s"}}`, "", 23 * 3600},
		{`{`, "7", 7}, {`{`, "invalid", 23 * 3600},
		{`{"unrelated":"Please retry in 1s"}`, "", 23 * 3600},
		{`{}`, now.Add(time.Minute).Format(http.TimeFormat), 60},
		{strings.Repeat(" ", maxBalanceBody+1), "5", 5},
	} {
		if got := geminiRateLimitSeconds([]byte(tc.body), tc.retry, now); got != tc.want {
			t.Fatal("Gemini reset", tc, got)
		}
	}
	now = time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
	if got := geminiRateLimitSeconds(nil, "", now); got != 25*3600 {
		t.Fatal("DST fallback day", got)
	}
}

func testGeminiQuotaPolicy(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	const settings = "/api/v1/admin/settings"
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.195:1000"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(method, path, token string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	set := func(policy string) {
		t.Helper()
		data("PUT", settings, admin, map[string]any{geminiQuotaSetting: json.RawMessage(policy)})
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", geminiQuotaSetting)
	for _, token := range []string{"", ordinary, "not-an-admin-key"} {
		w := call("PUT", settings, token, map[string]any{geminiQuotaSetting: map[string]any{}})
		if w.Code != 401 && w.Code != 403 {
			t.Fatal("policy admin boundary", w.Code)
		}
	}
	var calls atomic.Int64
	var reject atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Goog-Api-Key") != "quota-policy-secret" || r.Header.Get("Authorization") != "" {
			t.Error("Gemini credential isolation")
		}
		if reject.Load() {
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"12.1s"}]}}`)
			return
		}
		fmt.Fprint(w, `{"modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"OK"}],"role":"model"},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)
	}))
	defer provider.Close()
	gid := string(data("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Gemini quota policy", "platform": "gemini"})["id"])
	uid := string(data("POST", "/api/v1/admin/users", admin, map[string]any{"email": "gemini-quota-policy@example.test", "password": "quota-policy-password", "balance": 100})["id"])
	login := data("POST", "/api/v1/auth/login", "", map[string]any{"email": "gemini-quota-policy@example.test", "password": "quota-policy-password"})
	user := credentialString(login, "access_token")
	keyObject := data("POST", "/api/v1/keys", user, map[string]any{"name": "quota-policy-key", "group_id": json.Number(gid)})
	kid, key := string(keyObject["id"]), credentialString(keyObject, "key")
	aid := string(data("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "quota-policy", "platform": "gemini", "type": "apikey", "group_ids": []json.Number{json.Number(gid)}, "credentials": map[string]any{"api_key": "quota-policy-secret", "base_url": provider.URL}})["id"])
	apath := "/api/v1/admin/accounts/" + aid
	var accountID int64
	_ = json.Unmarshal([]byte(aid), &accountID)
	u, err := a.loadAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	before := u.UpdatedAt
	// Exact money and counts across model buckets, current day and minute.
	for i, model := range []string{"gemini-2.5-pro", "gemini-2.5-flash", "gemini-lite"} {
		exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,input_tokens,output_tokens,actual_cost,created_at)
 VALUES($1,$2,$3,$4,$5,2000000000,2000000000,0.1000000001,clock_timestamp()-interval '1 millisecond')`, uid, kid, aid, fmt.Sprintf("quota-policy-seed-%d", i), model)
	}
	set(`{"quota_rules":{"AISTUDIO_FREE":{"shared_rpd":9007199254740993,"rpm":11,"gemini_pro":{"rpd":2},"gemini_flash":{"rpd":3},"desc":"local estimate"}}}`)
	got := data("GET", apath+"/usage", admin, nil)
	var shared map[string]json.RawMessage
	_ = json.Unmarshal(got["gemini_shared_daily"], &shared)
	var stats map[string]json.RawMessage
	_ = json.Unmarshal(shared["window_stats"], &stats)
	if string(shared["limit_requests"]) != "9007199254740993" || string(shared["used_requests"]) != "3" || string(stats["tokens"]) != "12000000000" || rat(json.Number(stats["cost"])).Cmp(rat("0.3000000003")) != 0 || got["gemini_pro_daily"] != nil || got["gemini_flash_minute"] != nil || string(got["quota_basis"]) != `"configured_policy"` {
		t.Fatal("shared quota precision or precedence", got, shared, stats)
	}
	if calls.Load() != 0 {
		t.Fatal("quota query contacted provider")
	}
	u, _ = a.loadAccount(ctx, accountID)
	if !u.UpdatedAt.Equal(before) || u.Status != "active" || !u.Schedulable {
		t.Fatal("quota display changed account state")
	}
	if got = data("GET", "/api/v1/settings/public", "", nil); got[geminiQuotaSetting] != nil {
		t.Fatal("public settings exposed internal quota policy")
	}
	stored := string(data("GET", settings, admin, nil)[geminiQuotaSetting])
	data("PUT", settings, admin, map[string]any{"site_name": "Team gateway", geminiQuotaSetting: nil})
	if string(data("GET", settings, admin, nil)[geminiQuotaSetting]) != stored {
		t.Fatal("omitted/null policy changed setting")
	}
	for _, raw := range []string{`[]`, `"x"`, `{"tiers":{"google_one_free":{}}}`, `{"quota_rules":{"aistudio_free":{"gemini_flash":{"rpm":-1}}}}`, `{"tiers":{"aistudio_free":{}," AISTUDIO_FREE ":{}}}`} {
		w := call("PUT", settings, admin, map[string]any{"site_name": "must-not-save", geminiQuotaSetting: json.RawMessage(raw)})
		if w.Code != 400 || string(data("GET", settings, admin, nil)[geminiQuotaSetting]) != stored {
			t.Fatal("invalid policy saved partially", raw, w.Code)
		}
	}
	set(`{"quota_rules":{"aistudio_free":{"shared_rpd":0,"rpm":0,"gemini_pro":{"rpd":-1,"rpm":0},"gemini_flash":{"rpd":7,"rpm":6}}}}`)
	got = data("GET", apath+"/usage", admin, nil)
	var flash map[string]json.RawMessage
	_ = json.Unmarshal(got["gemini_flash_daily"], &flash)
	if got["gemini_shared_daily"] != nil || got["gemini_pro_daily"] != nil || got["gemini_pro_minute"] != nil || string(flash["used_requests"]) != "2" || string(flash["limit_requests"]) != "7" {
		t.Fatal("zero/unlimited quota windows", got)
	}
	// Database override layers over deployment config; no process-global cache.
	deployment, _ := parseGeminiQuotaPolicy([]byte(`{"quota_rules":{"aistudio_free":{"gemini_pro":{"rpm":77,"rpd":90}}}}`), true)
	fresh := &App{DB: a.DB, geminiQuotaPolicy: deployment}
	set(`{"tiers":{"aistudio_free":{"pro_rpd":15,"flash_rpd":21,"cooldown_minutes":1}}}`)
	q, basis, err := fresh.accountGeminiQuota(ctx, u)
	if err != nil || q.ProDay != 15 || q.ProMinute != 77 || q.FlashDay != 21 || basis != "configured_policy" {
		t.Fatal("policy layers", q, basis, err)
	}
	set(`{}`)
	q, basis, err = fresh.accountGeminiQuota(ctx, u)
	if err != nil || q.ProDay != 90 || q.ProMinute != 77 || basis != "deployment_policy" {
		t.Fatal("clear database override", q, basis, err)
	}
	data("PUT", apath, admin, map[string]any{"credentials": map[string]string{"tier_id": "aistudio_paid"}})
	got = data("GET", apath+"/usage", admin, nil)
	if got["gemini_pro_daily"] != nil || got["gemini_flash_daily"] != nil || string(got["quota_tier"]) != `"aistudio_paid"` {
		t.Fatal("paid tier lost", got)
	}
	// Stored forward-compatible JSON is read without rewriting it; broken JSON
	// is reported instead of silently inventing a quota and is repairable by PUT.
	exec("UPDATE settings SET value=$2 WHERE key=$1", geminiQuotaSetting, `{"quota_rules":{"aistudio_paid":{"gemini_pro":{"rpm":5},"future":true},"google_ai_pro":{"rpm":2}},"unknown":"kept"}`)
	if w := call("GET", apath+"/usage", admin, nil); w.Code != 200 {
		t.Fatal("unknown stored fields rejected", w.Code, w.Body.String())
	}
	exec("UPDATE settings SET value=$2 WHERE key=$1", geminiQuotaSetting, `broken-private-data`)
	if w := call("GET", apath+"/usage", admin, nil); w.Code != 503 || strings.Contains(w.Body.String(), "private-data") {
		t.Fatal("malformed stored policy", w.Code, w.Body.String())
	}
	set(`{"quota_rules":{"aistudio_paid":{"rpm":1,"shared_rpd":1}}}`)
	// Reading diagnostic quotas above their limit must not create a new
	// scheduling gate: current native/converted handlers never called that helper.
	exec("DELETE FROM usage_logs WHERE account_id=$1", aid)
	native := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hello"}}}}}
	chat := map[string]any{"model": "gemini-2.5-pro", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}
	for i := range 3 {
		if w := call("POST", "/v1beta/models/gemini-2.5-pro:generateContent", key, native); w.Code != 200 {
			t.Fatal("diagnostic quota blocked dispatch", i, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 3 {
		t.Fatal("unexpected generation count", calls.Load())
	}
	// Real 429s across all actual Gemini wire paths update state using RetryInfo.
	reject.Store(true)
	for _, tc := range []struct {
		path string
		body any
	}{{"/v1beta/models/gemini-2.5-pro:generateContent", native}, {"/v1/chat/completions", chat}, {"/v1/messages", map[string]any{"model": "gemini-2.5-pro", "max_tokens": 10, "messages": chat["messages"]}}, {"/v1/responses", map[string]any{"model": "gemini-2.5-pro", "input": "hello", "store": false}}} {
		data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
		prior := calls.Load()
		w := call("POST", tc.path, key, tc.body)
		var remaining float64
		if err = a.DB.QueryRow("SELECT extract(epoch FROM rate_limit_reset_at-now()) FROM accounts WHERE id=$1", aid).Scan(&remaining); err != nil || remaining < 10 || remaining > 14 || calls.Load() != prior+1 || w.Code < 400 {
			t.Fatal("Gemini gateway rejection", tc.path, w.Code, remaining, calls.Load()-prior, err, w.Body.String())
		}
	}
	data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
	u, _ = a.loadAccount(ctx, accountID)
	// The generic cooldown switch does not disable provider-defined reset rules.
	if err = a.writeRuntimeSetting(ctx, rate429Setting, rate429Settings{false, 5}); err != nil {
		t.Fatal(err)
	}
	defer a.writeRuntimeSetting(ctx, rate429Setting, rate429Settings{true, 5})
	a.markGatewayFailure(ctx, &gatewaySelection{Account: u}, 429, "", nil)
	var reset time.Time
	if err = a.DB.QueryRow("SELECT rate_limit_reset_at FROM accounts WHERE id=$1", aid).Scan(&reset); err != nil {
		t.Fatal(err)
	}
	_, expected := geminiUsageDay(time.Now())
	if reset.Sub(expected) < 0 || reset.Sub(expected) > 2*time.Second {
		t.Fatal("Gemini fallback reset", reset, expected)
	}
	data("POST", apath+"/clear-rate-limit", admin, map[string]any{})
	u, _ = a.loadAccount(ctx, accountID)
	data("PUT", apath, admin, map[string]any{"notes": "new revision"})
	a.markGatewayFailure(ctx, &gatewaySelection{Account: u}, 429, "", []byte(`{"error":{"message":"Please retry in 1s"}}`))
	var marked bool
	if err = a.DB.QueryRow("SELECT rate_limit_reset_at IS NOT NULL FROM accounts WHERE id=$1", aid).Scan(&marked); err != nil || marked {
		t.Fatal("stale Gemini failure overwrote admin edit", err)
	}
	// Concurrent reads/saves remain valid whole policies, never partially merged.
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := call("PUT", settings, admin, map[string]any{geminiQuotaSetting: map[string]any{"quota_rules": map[string]any{"aistudio_paid": map[string]int{"rpm": i + 1, "shared_rpd": (i + 1) * 100}}}})
			if w.Code != 200 {
				t.Error("concurrent policy save", w.Code)
			}
			q, basis, err := fresh.accountGeminiQuota(ctx, u)
			if err != nil || basis != "configured_policy" || q.SharedMinute < 1 || q.SharedMinute > 6 || q.SharedDay != q.SharedMinute*100 {
				t.Error("concurrent policy read", q, basis, err)
			}
		}(i)
	}
	wg.Wait()
	var audit int
	if err = a.DB.QueryRow("SELECT count(*) FROM audit_logs WHERE path=$1 AND method='PUT'", settings).Scan(&audit); err != nil || audit == 0 {
		t.Fatal("policy audit missing", err)
	}
}
