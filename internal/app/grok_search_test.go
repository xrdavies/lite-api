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

func TestGrokSearchContract(t *testing.T) {
	for _, tc := range []struct {
		price                      *json.Number
		rate, total, actual, debit string
	}{
		{nil, "2", "0.0050000000", "0.0100000000", "0.01000000"},
		{number("0"), "2", "0.0000000000", "0.0000000000", "0.00000000"},
		{number("123.45678901"), "0.3333", "0.1234567890", "0.0411481478", "0.04114815"},
	} {
		cost, err := (gatewayGroup{SearchPrice: tc.price, Rate: json.Number(tc.rate)}).searchCost("x_search")
		if err != nil || cost.Total != tc.total || cost.Actual != tc.actual || cost.Debit != tc.debit {
			t.Fatal("search decimal billing", cost, err)
		}
	}
	for _, value := range []string{"1000000000000", "0.000000001", "-1000000000000"} {
		if err := (&groupInput{SearchPrice: number(value)}).validate(false); err == nil {
			t.Fatal("out of schema precision accepted", value)
		}
	}
	for _, raw := range []string{`{}`, `{"query":4}`, `{"query":"x","max_results":1.2}`, `{"query":"x","stream":true}`, `{"query":"x","stream":null}`, `{"query":"x","allowed_x_handles":"user"}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if _, err := parseGrokSearch(body); err == nil {
			t.Fatal("bad search accepted", raw)
		}
	}
	for _, n := range []int{-1, 0, 3, 21} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"query":"  ","input":" team query ","max_results":%d}`, n)), &body)
		in, err := parseTextRequest(httptest.NewRequest("POST", "/web_search", nil), "web_search", body)
		want := n
		if want <= 0 {
			want = 5
		}
		if want > 20 {
			want = 20
		}
		if err != nil || in.Model != "grok-4.6" || in.Search.Query != "team query" || in.Search.MaxResults != want {
			t.Fatal("search request normalization", in, err)
		}
	}
	in := &grokSearchRequest{Query: "query", MaxResults: 5}
	for _, raw := range []string{`null`, `[]`, `{}`, `{"output":null}`, `{"output":[],"error":{"message":"bad"}}`, `{"output":[],"status":"failed"}`, `{"output":[],"status":"in_progress"}`} {
		if _, _, err := in.response([]byte(raw)); err == nil {
			t.Fatal("invalid upstream response", raw)
		}
	}
	enrichment, _ := json.Marshal(map[string]any{"results": []searchResult{
		{URL: "https://example.test/a#model", Title: "Article", Snippet: "Summary"},
		{URL: "https://fiction.test/invented", Title: "Invented", Snippet: "Uncited"},
	}})
	body := map[string]any{"status": "completed", "model": "grok-4.6", "output": []any{
		map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "```json\n" + string(enrichment) + "\n```", "annotations": []any{map[string]any{"type": "url_citation", "url": "https://example.test/b", "title": "Citation"}}}}},
		map[string]any{"type": "web_search_call", "action": map[string]any{"sources": []searchResult{
			{URL: "https://EXAMPLE.test/a#source", Title: "1"},
			{URL: "https://example.test/a#duplicate", Title: "Alternate"},
			{URL: "https://www.example.test/c", Title: "123"},
			{URL: "javascript:alert(1)"}, {URL: "https://user:password@example.test/"},
		}}},
	}}
	raw, _ := json.Marshal(body)
	result, model, err := in.response(raw)
	var decoded struct{ Results []searchResult }
	_ = json.Unmarshal(result, &decoded)
	if err != nil || model != "grok-4.6" || len(decoded.Results) != 3 || decoded.Results[0].Title != "Article" || decoded.Results[0].Snippet != "Summary" || decoded.Results[0].URL != "https://EXAMPLE.test/a#source" || decoded.Results[1].Title != "Citation" || decoded.Results[2].Title != "example.test" || strings.Contains(string(result), "Invented") {
		t.Fatal("search citation extraction", string(result), err)
	}
	in.MaxResults = 1
	result, _, err = in.response(raw)
	_ = json.Unmarshal(result, &decoded)
	if err != nil || len(decoded.Results) != 1 {
		t.Fatal("result cap", string(result), err)
	}
	result, _, err = in.response([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"https://invented.test"}]}]}`))
	if err != nil || !strings.Contains(string(result), `"results":[]`) {
		t.Fatal("model text treated as citation", string(result), err)
	}
}

func testGrokSearch(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, key, idem string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-cookie")
		r.Header.Set("X-Api-Key", key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var envelope struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "grok-search@example.test", "password": "grok-search-password", "balance": 10}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "grok-search@example.test", "password": "grok-search-password"})["access_token"].(string)
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Grok search", "platform": "grok", "rate_multiplier": 2}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "Grok search", "group_id": gid, "quota": 10, "rate_limit_5h": 10})
	key, kid := k["key"].(string), id(k)
	quotaPath := fmt.Sprintf("/api/v1/admin/users/%d/platform-quotas", uid)
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "grok", "daily_limit_usd": 10}}})
	var calls, primaryStatus, changePrice atomic.Int32
	var malformed atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("search header/path isolation", r.URL.Path)
		}
		var body struct {
			Model, Input  string
			Store, Stream bool
			ToolChoice    string `json:"tool_choice"`
			Include       []string
			Tools         []map[string]any
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "native-grok-search" || body.Store || body.Stream || len(body.Tools) != 1 || !strings.Contains(body.Input, "team query") {
			t.Error("wrong search upstream body", body)
		}
		kind := body.Tools[0]["type"].(string)
		if len(body.Include) != 1 || body.Include[0] != kind+"_call.action.sources" {
			t.Error("missing sources request", body)
		}
		if kind == "x_search" && (body.ToolChoice != "required" || body.Tools[0]["from_date"] != "2026-09-01" || body.Tools[0]["enable_image_understanding"] != false || body.Tools[0]["enable_video_understanding"] != true || len(body.Tools[0]["allowed_x_handles"].([]any)) != 1) {
			t.Error("X filters lost", body)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer grok-primary" && auth != "Bearer grok-backup" {
			t.Error("wrong upstream credential")
		}
		if auth == "Bearer grok-primary" && primaryStatus.Load() != 0 {
			w.WriteHeader(int(primaryStatus.Load()))
			_, _ = fmt.Fprint(w, `{"error":"private upstream secret"}`)
			return
		}
		if changePrice.Swap(0) == 1 {
			if _, err := a.DB.Exec("UPDATE groups SET search_price_per_1k=20 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "grok-search-upstream-id")
		if malformed.Load() {
			_, _ = fmt.Fprint(w, `{"status":"failed","error":{"message":"private"},"output":[]}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "model": "grok-response-model", "usage": map[string]int{"input_tokens": 100, "output_tokens": 50}, "output": []any{map[string]any{"type": kind + "_call", "action": map[string]any{"sources": []searchResult{{URL: "https://example.test/result", Title: "Source", Snippet: "Summary"}}}}}})
	}))
	defer upstream.Close()
	account := func(name, protocol string, priority int) int64 {
		return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": name, "platform": "grok", "type": "apikey", "priority": priority, "concurrency": 1, "group_ids": []int64{gid}, "rate_multiplier": 3, "extra": map[string]any{"upstream_request_id_header": "X-Request-ID", "quota_limit": 100}, "credentials": map[string]any{"api_key": name, "base_url": upstream.URL + "/v1", "api_protocol": protocol, "model_mapping": map[string]string{"grok-4.6": "native-grok-search"}}}))
	}
	aid := account("grok-primary", "chat_completions", 1)
	body := map[string]any{"query": " team query ", "max_results": 100, "model": "ignored-model", "tools": "untrusted", "store": true}
	xbody := map[string]any{"input": "team query", "allowed_x_handles": []string{"example"}, "from_date": " 2026-09-01 ", "enable_image_understanding": false, "enable_video_understanding": true}
	check := func(w *httptest.ResponseRecorder, label, total, actual string) {
		t.Helper()
		if w.Code != 200 {
			t.Fatal("search request failed", w.Code, w.Body.String())
		}
		var result struct {
			Query, Provider string
			Results         []searchResult
			Max             int `json:"max_results"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &result)
		if result.Query != "team query" || result.Provider != "grok-native" || len(result.Results) != 1 || strings.Contains(w.Body.String(), "upstream") || (label == "web-search" && result.Max != 20) {
			t.Fatal("search output", w.Body.String())
		}
		var gotTotal, gotActual, mode, model, requested, upstreamModel, responseModel, endpoint, requestID string
		var tokens int64
		err := a.DB.QueryRow(`SELECT total_cost::text,actual_cost::text,billing_mode,model,requested_model,upstream_model,upstream_response_model,upstream_endpoint,upstream_request_id,input_tokens+output_tokens+cache_read_tokens+cache_creation_tokens FROM usage_logs WHERE request_id=$1`, w.Header().Get("X-Request-ID")).Scan(&gotTotal, &gotActual, &mode, &model, &requested, &upstreamModel, &responseModel, &endpoint, &requestID, &tokens)
		if err != nil || gotTotal != total || gotActual != actual || mode != "per_request" || model != "grok-"+label || requested != "grok-4.6" || upstreamModel != "native-grok-search" || responseModel != "grok-response-model" || endpoint != "/v1/responses" || requestID != "grok-search-upstream-id" || tokens != 0 {
			t.Fatal("search accounting", gotTotal, gotActual, model, requested, mode, tokens, err)
		}
	}
	first := call("/v1/web_search", key, "same-search", body)
	check(first, "web-search", "0.0050000000", "0.0100000000")
	if w := call("/web_search", key, "same-search", body); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || w.Body.String() != first.Body.String() || calls.Load() != 1 {
		t.Fatal("web alias replay", w.Code, w.Body.String(), calls.Load())
	}
	var balance, used, window, accountUsed, platformUsed string
	if err := a.DB.QueryRow(`SELECT u.balance::text,k.quota_used::text,k.usage_5h::text,a.extra->>'quota_used',q.daily_usage_usd::text FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts a ON a.id=$3 JOIN user_platform_quotas q ON q.user_id=u.id AND q.platform='grok' WHERE u.id=$1 AND k.id=$2`, uid, kid, aid).Scan(&balance, &used, &window, &accountUsed, &platformUsed); err != nil || balance != "9.99000000" || used != "0.01000000" || window != "0.01000000" || rat(json.Number(accountUsed)).Cmp(rat("0.015")) != 0 || rat(json.Number(platformUsed)).Cmp(rat("0.01")) != 0 {
		t.Fatal("search counters", balance, used, window, accountUsed, platformUsed, err)
	}
	check(call("/v1/x_search", key, "same-search", xbody), "x-search", "0.0050000000", "0.0100000000")
	if w := call("/x_search", key, "same-search", xbody); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 2 {
		t.Fatal("X alias replay", w.Code, calls.Load())
	}
	check(call("/web_search", key, "", body), "web-search", "0.0050000000", "0.0100000000")
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 123.45678901, "web_search_price_per_call": 9})
	must("PUT", gp+"/rate-multipliers", admin, map[string]any{"entries": []any{map[string]any{"user_id": uid, "rate_multiplier": 0.3333}}})
	changePrice.Store(1)
	check(call("/web_search", key, "", body), "web-search", "0.1234567890", "0.0411481478")
	check(call("/web_search", key, "", body), "web-search", "0.0200000000", "0.0066660000")
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 0})
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": nil})
	check(call("/x_search", key, "", xbody), "x-search", "0.0000000000", "0.0000000000")
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": -1})
	check(call("/web_search", key, "", body), "web-search", "0.0050000000", "0.0016665000")
	before := calls.Load()
	for _, token := range []string{"", user} {
		if w := call("/web_search", token, "", body); w.Code != 401 || calls.Load() != before {
			t.Fatal("invalid search authentication", w.Code)
		}
	}
	for _, platform := range []string{"openai", "composite"} {
		group := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "No Grok search " + platform, "platform": platform, "search_price_per_1k": 1}))
		other := must("POST", "/api/v1/keys", user, map[string]any{"name": "No search", "group_id": group})["key"].(string)
		if w := call("/web_search", other, "", body); w.Code != 404 || calls.Load() != before {
			t.Fatal("search bypassed platform restriction", w.Code)
		}
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"ignored-model"}}})
	if w := call("/web_search", key, "", body); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed actual model allowlist", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}, "claude_code_only": true})
	if w := call("/x_search", key, "", xbody); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed client restriction", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"claude_code_only": false})
	cid := id(must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Grok search channel", "group_ids": []int64{gid}, "restrict_models": true, "model_pricing": []any{map[string]any{"platform": "grok", "models": []string{"other"}, "input_price": 0}}}))
	if w := call("/web_search", key, "", body); w.Code != 403 || calls.Load() != before {
		t.Fatal("search bypassed channel restriction", w.Code)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", cid), admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "grok", "models": []string{"grok-4.6"}, "billing_mode": "per_request", "per_request_price": 9}}})
	check(call("/web_search", key, "", body), "web-search", "0.0050000000", "0.0016665000")
	backup := account("grok-backup", "responses", 2)
	for _, status := range []int{401, 402, 403, 429, 500, 503} {
		primaryStatus.Store(int32(status))
		check(call("/web_search", key, "", body), "web-search", "0.0050000000", "0.0016665000")
		var logged int64
		if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE user_id=$1 ORDER BY id DESC LIMIT 1", uid).Scan(&logged); err != nil || logged != backup {
			t.Fatal("search failover", logged, err)
		}
		must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-error", aid), admin, nil)
		must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", aid), admin, nil)
	}
	primaryStatus.Store(0)
	// All eligible accounts may fail, but each is tried at most once.
	primaryStatus.Store(503)
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", backup), admin, map[string]any{"credentials": map[string]any{"api_key": "grok-primary"}})
	third := account("grok-primary", "responses", 3)
	fourth := account("grok-backup", "chat_completions", 4)
	before = calls.Load()
	check(call("/web_search", key, "", body), "web-search", "0.0050000000", "0.0016665000")
	if calls.Load() != before+4 {
		t.Fatal("Grok search did not reach fourth account", calls.Load()-before)
	}
	for _, accountID := range []int64{aid, backup} {
		must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", accountID), admin, nil)
	}
	for _, accountID := range []int64{third, fourth} {
		must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", accountID), admin, map[string]any{"schedulable": false})
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", backup), admin, map[string]any{"credentials": map[string]any{"api_key": "grok-backup"}})
	primaryStatus.Store(0)
	malformed.Store(true)
	failed := call("/web_search", key, "", body)
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || failed.Code != 502 || count != 0 || strings.Contains(failed.Body.String(), "private") {
		t.Fatal("failed search charged or leaked error", failed.Code, count, err)
	}
	malformed.Store(false)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_grok_search_receipt CHECK(user_id<>" + fmt.Sprint(uid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	failed = call("/web_search", key, "", body)
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_grok_search_receipt"); err != nil {
		t.Fatal(err)
	}
	if failed.Code != 503 {
		t.Fatal("search settlement failure not surfaced", failed.Code, failed.Body.String())
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1 AND model='grok-web-search' AND actual_cost=0.0016665", failed.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 1 || calls.Load() != before {
		t.Fatal("search recovery duplicated", count, err)
	}
	must("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", backup), admin, map[string]any{"schedulable": false})
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("occupy search account")
	}
	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() { queued <- call("/web_search", key, "", body) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, waiting := a.concurrencySnapshot()
		if waiting[fmt.Sprintf("account:%d", aid)] > 0 {
			break
		}
		if time.Now().After(deadline) {
			a.releaseSlot("account", aid)
			t.Fatal("search did not queue")
		}
		time.Sleep(10 * time.Millisecond)
	}
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 10})
	a.releaseSlot("account", aid)
	select {
	case w := <-queued:
		if w.Code != 409 || calls.Load() != before {
			t.Fatal("search queued through price change", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("search queue did not wake")
	}
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "grok", "daily_limit_usd": 0}}})
	if w := call("/web_search", key, "", body); w.Code != 429 || calls.Load() != before {
		t.Fatal("search bypassed platform quota", w.Code)
	}
	must("PUT", quotaPath, admin, map[string]any{"quotas": []any{map[string]any{"platform": "grok", "daily_limit_usd": 10}}})
	if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w := call("/web_search", key, "", body); w.Code != 402 || calls.Load() != before {
		t.Fatal("search bypassed balance", w.Code)
	}
	if w := call("/web_search", key, "same-search", body); w.Code != 200 || w.Body.String() != first.Body.String() || calls.Load() != before {
		t.Fatal("search replay recharged exhausted user", w.Code)
	}
}
