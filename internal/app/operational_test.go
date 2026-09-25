package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testErrorRequestMetadata(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.244:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("User-Agent", "error-metadata-client")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return out.Data
	}
	manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "error-metadata@example.test", "password": "error-metadata-password", "balance": 100})
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "error-metadata@example.test", "password": "error-metadata-password"})["access_token"].(string)
	keys := map[string]string{}
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok"} {
		group := manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Error metadata " + platform, "platform": platform})
		keys[platform] = manage("POST", "/api/v1/keys", user, map[string]any{"name": "Error metadata", "group_id": group["id"]})["key"].(string)
	}
	manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": true})
	defer manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": false})
	for _, tc := range []struct {
		platform, path, raw, model string
		status, kind               int
		count                      bool
	}{
		{"openai", "/v1/chat/completions", `{"model":"team-chat","messages":[{"role":"user","content":"private-client-prompt"}]}`, "team-chat", 503, 1, false},
		{"openai", "/chat/completions", `{"model":"team-chat","stream":true}`, "team-chat", 400, 2, false},
		{"openai", "/v1/responses", `{"model":"team-response","stream":true}`, "team-response", 400, 2, false},
		{"anthropic", "/v1/messages", `{"model":"team-claude","stream":true}`, "team-claude", 400, 2, false},
		{"anthropic", "/v1/messages/count_tokens", `{"model":"team-claude"}`, "team-claude", 400, 1, true},
		{"openai", "/v1/responses/input_tokens", `{"model":"team-count"}`, "team-count", 503, 1, true},
		{"gemini", "/v1beta/models/team-gemini:streamGenerateContent", `{"model":"ignored-body-model"}`, "team-gemini", 400, 2, false},
		{"gemini", "/v1beta/models/team-gemini/streamGenerateContent", `{}`, "team-gemini", 400, 2, false},
		{"gemini", "/v1beta/models/team-gemini:countTokens", `{}`, "team-gemini", 400, 1, true},
		{"openai", "/v1/images/generations", `{"model":"team-image","stream":true}`, "team-image", 400, 2, false},
		{"openai", "/api/v3/contents/generations/tasks", `{"model":"team-seedance"}`, "team-seedance", 400, 1, false},
		{"grok", "/v1/videos/edits", `{"model":"team-video","prompt":"private-client-prompt"}`, "team-video", 400, 1, false},
		{"grok", "/v1/videos/extensions", `{"model":"team-video","prompt":"private-client-prompt"}`, "team-video", 400, 1, false},
		{"grok", "/v1/tts", `{"model":"team-audio"}`, "team-audio", 400, 1, false},
		{"openai", "/v1/chat/completions", `{"model":42,"stream":true}`, "", 400, 2, false},
		{"openai", "/v1/chat/completions", `{"model":"team-invalid-stream","stream":"true"}`, "team-invalid-stream", 400, 1, false},
		{"openai", "/v1/chat/completions", `{"model":"` + strings.Repeat("x", 101) + `","stream":true}`, "", 400, 2, false},
		{"openai", "/v1/chat/completions", `null`, "", 400, 1, false},
	} {
		w := call("POST", tc.path, keys[tc.platform], json.RawMessage(tc.raw))
		if w.Code != tc.status {
			t.Fatalf("metadata producer %s: %d %s", tc.path, w.Code, w.Body.String())
		}
		var id int64
		var raw []byte
		if err := a.DB.QueryRow("SELECT id,to_jsonb(e) FROM ops_error_logs e WHERE request_id=$1 AND error_phase='gateway'", w.Header().Get("X-Request-ID")).Scan(&id, &raw); err != nil {
			t.Fatal(tc.path, err)
		}
		var row map[string]any
		if json.Unmarshal(raw, &row) != nil {
			t.Fatal("invalid error row")
		}
		if fmt.Sprint(row["model"]) != tc.model && !(tc.model == "" && row["model"] == nil) ||
			row["requested_model"] != row["model"] || row["upstream_model"] != nil || row["stream"] != (tc.kind == 2) ||
			row["request_type"] != float64(tc.kind) || row["is_count_tokens"] != tc.count || row["client_ip"] != "192.0.2.244" || row["user_agent"] != "error-metadata-client" || strings.Contains(string(raw), "private-client-prompt") {
			t.Fatalf("metadata %s: %s", tc.path, raw)
		}
		path := fmt.Sprintf("/api/v1/usage/errors/%d", id)
		if tc.count {
			if w := call("GET", path, user, nil); w.Code != 404 {
				t.Fatal("count error exposed", tc.path, w.Code)
			}
			continue
		}
		detail := manage("GET", path, user, nil)
		if detail["model"] != row["model"] || detail["request_type"] != float64(tc.kind) || detail["stream"] != row["stream"] || detail["upstream_model"] != nil {
			t.Fatal("user error metadata", detail)
		}
		if tc.model != "" {
			page := manage("GET", "/api/v1/usage/errors?model="+tc.model, user, nil)
			if page["total"].(float64) == 0 {
				t.Fatal("real error model filter empty", tc.model)
			}
		}
	}
}

func TestOperationalAvailability(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	good := operationalAccount{Status: "active", Schedulable: true, Concurrency: 2, AutoPause: true}
	if !good.available(now) {
		t.Fatal("active account unavailable")
	}
	for _, change := range []func(*operationalAccount){
		func(a *operationalAccount) { a.Status = "inactive" }, func(a *operationalAccount) { a.Schedulable = false },
		func(a *operationalAccount) { a.Expires = &past }, func(a *operationalAccount) { a.RateReset = &future },
		func(a *operationalAccount) { a.Overload = &future }, func(a *operationalAccount) { a.Temporary = &future },
		func(a *operationalAccount) {
			a.Extra = map[string]json.RawMessage{"quota_limit": json.RawMessage(`1`), "quota_used": json.RawMessage(`1`)}
		},
	} {
		u := good
		change(&u)
		if u.available(now) {
			t.Fatalf("unavailable account accepted: %+v", u)
		}
	}
	good.Expires = &past
	good.AutoPause = false
	good.RateReset = &past
	if !good.available(now) {
		t.Fatal("expired cooldown or disabled expiry gate blocked account")
	}
	for _, path := range []string{"/?platform=invalid", "/?group_id=0", "/?group_id=oops"} {
		if _, _, err := operationalScope(httptest.NewRequest("GET", path, nil)); err == nil {
			t.Fatal("invalid scope accepted", path)
		}
	}
}

func TestUsagePeriodStart(t *testing.T) {
	now := time.Date(2026, 8, 31, 16, 30, 0, 0, time.UTC)
	for period, want := range map[string]string{
		"day": "2026-09-01T00:00:00+08:00", "week": "2026-08-31T00:00:00+08:00", "month": "2026-09-01T00:00:00+08:00",
	} {
		got, err := usagePeriodStart(period, now)
		if err != nil || got.Format(time.RFC3339) != want {
			t.Fatal(period, got, err)
		}
	}
	if _, err := usagePeriodStart("year", now); err == nil {
		t.Fatal("unknown reporting period accepted")
	}
}

func testUsageSummaries(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(method, path string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, admin, body)
		var response struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return response.Data
	}
	uid := string(data("POST", "/api/v1/admin/users", map[string]any{"email": "usage-summary@example.test", "password": "usage-summary-password"})["id"])
	aid := string(data("POST", "/api/v1/admin/accounts", map[string]any{"name": "usage-summary", "platform": "openai", "type": "apikey", "credentials": map[string]any{"api_key": "summary-secret", "base_url": "http://127.0.0.1:1"}})["id"])
	var kid int64
	if err := a.DB.QueryRow("INSERT INTO api_keys(user_id,key,name) VALUES($1,'summary-client-key','summary') RETURNING id", uid).Scan(&kid); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	day, _ := quotaStarts(time.Now())
	for _, row := range []struct {
		Total, Actual            string
		Override, Rate, Duration any
	}{
		{"0.5", "0.3333333333", "0.1234567890", "2", 100},
		{"0.1000000001", "0.2222222222", nil, nil, 200},
		{"9", "0.1111111111", "0", "5", nil},
	} {
		exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,account_stats_cost,account_rate_multiplier,duration_ms,created_at)
 VALUES($1,$2,$3,'summary',1000000000,1000000000,1000000000,1000000000,$4,$5,$6,$7,$8,$9)`, uid, kid, aid, row.Total, row.Actual, row.Override, row.Rate, row.Duration, day)
	}
	for _, at := range []time.Time{day.Add(-time.Nanosecond * 1000), day.AddDate(0, 0, 1)} {
		exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,model,total_cost,actual_cost,created_at) VALUES($1,$2,$3,'outside-day',99,1,$4)`, uid, kid, aid, at)
	}
	// Historical account/key state must not rewrite the recorded costs.
	exec("UPDATE accounts SET status='inactive',rate_multiplier=99 WHERE id=$1", aid)
	exec("UPDATE api_keys SET deleted_at=now() WHERE id=$1", kid)
	uPath, aPath := "/api/v1/admin/users/"+uid+"/usage", "/api/v1/admin/accounts/"+aid+"/today-stats"
	check := func(got map[string]json.RawMessage, want map[string]string) {
		t.Helper()
		for k, v := range want {
			if !json.Valid(got[k]) || rat(json.Number(got[k])).Cmp(rat(json.Number(v))) != 0 {
				t.Fatalf("%s: got %s want %s", k, got[k], v)
			}
		}
	}
	check(data("GET", aPath, nil), map[string]string{"requests": "3", "tokens": "12000000000", "cost": "0.3469135781", "standard_cost": "9.6000000001", "user_cost": "0.6666666666"})
	check(data("GET", uPath+"?period=day", nil), map[string]string{"total_requests": "3", "total_tokens": "12000000000", "total_cost": "0.6666666666", "avg_duration_ms": "150"})
	for _, period := range []string{"week", "month", ""} {
		path := uPath
		if period != "" {
			path += "?period=" + period
		} else {
			period = "month"
		}
		got := data("GET", path, nil)
		start, _ := usagePeriodStart(period, time.Now())
		count, cost := "3", "0.6666666666"
		if start.Before(day) {
			count, cost = "4", "1.6666666666"
		}
		check(got, map[string]string{"total_requests": count, "total_cost": cost})
		if string(got["period"]) != `"`+period+`"` {
			t.Fatal("wrong period", got)
		}
	}
	for _, path := range []string{uPath, aPath} {
		for _, v := range []struct {
			Token  string
			Status int
		}{{"", 401}, {ordinary, 403}, {"summary-client-key", 401}} {
			if w := call("GET", path, v.Token, nil); w.Code != v.Status {
				t.Fatal("summary permissions", path, w.Code)
			}
		}
	}
	if w := call("GET", uPath+"?period=year", admin, nil); w.Code != 400 {
		t.Fatal("unknown period", w.Code)
	}
	for _, path := range []string{"/api/v1/admin/users/9223372036854775807/usage", "/api/v1/admin/accounts/9223372036854775807/today-stats"} {
		if w := call("GET", path, admin, nil); w.Code != 404 {
			t.Fatal("missing resource", w.Code)
		}
	}
	other := string(data("POST", "/api/v1/admin/users", map[string]any{"email": "empty-summary@example.test", "password": "usage-summary-password"})["id"])
	check(data("GET", "/api/v1/admin/users/"+other+"/usage", nil), map[string]string{"total_requests": "0", "total_tokens": "0", "total_cost": "0", "avg_duration_ms": "0"})
	exec("UPDATE users SET deleted_at=now() WHERE id=$1", uid)
	exec("UPDATE accounts SET deleted_at=now() WHERE id=$1", aid)
	for _, path := range []string{uPath, aPath} {
		if w := call("GET", path, admin, nil); w.Code != 404 {
			t.Fatal("deleted resource", w.Code)
		}
	}
}

func testOperational(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.155:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	// Removed convenience endpoints may match a resource wildcard. Even an
	// authenticated administrator must reach ID rejection, never those products.
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/admin/groups/live-capability"},
		{"PUT", "/api/v1/admin/groups/sort-order"},
		{"GET", "/api/v1/admin/accounts/data"},
		{"GET", "/api/v1/admin/proxies/data"},
	} {
		for _, actor := range []struct {
			token string
			code  int
		}{{admin, 400}, {ordinary, 403}, {"", 401}} {
			w := call(route.method, route.path, actor.token, map[string]any{})
			if w.Code != actor.code || actor.token == admin && !bytes.Contains(w.Body.Bytes(), []byte("invalid id")) {
				t.Fatalf("excluded route %s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
		}
	}
	data := func(method, path, token string, body any) json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var result struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Data) == 0 {
			t.Fatalf("invalid response: %s", w.Body.String())
		}
		return result.Data
	}
	manage := func(method, path, token string, body any) map[string]any {
		var result map[string]any
		if err := json.Unmarshal(data(method, path, token, body), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	group := manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Operational", "platform": "openai"})
	gid := int64(group["id"].(float64))
	second := int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Shared operational", "platform": "openai"})["id"].(float64))
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "ops-check@example.test", "password": "password-for-ops", "balance": 2})
	uid := int64(user["id"].(float64))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "ops-check@example.test", "password": "password-for-ops"})["access_token"].(string)
	key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "Ops", "group_id": gid})
	kid := int64(key["id"].(float64))
	secret := key["key"].(string)
	account := func(name string, groups []int64) int64 {
		return int64(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{
			"name": name, "platform": "openai", "type": "apikey", "concurrency": 3, "group_ids": groups,
			"credentials": map[string]any{"api_key": name + "-secret", "base_url": "https://buyonce.xyz"},
		})["id"].(float64))
	}
	firstAccount := account("ops-first", []int64{gid})
	sharedAccount := account("ops-shared", []int64{gid, second})
	now := time.Now().UTC()
	today, _ := quotaStarts(now)
	for _, fixture := range []struct {
		Cost string
		At   time.Time
	}{
		{"0.1234567890", today},
		{"0.2222222222", today.Add(-time.Second)},
		{"0.3333333333", today.AddDate(0, 0, -2)},
	} {
		exec("INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,model,actual_cost,created_at) VALUES($1,$2,$3,$4,'ops-fixture',$5,$6)", uid, kid, sharedAccount, gid, fixture.Cost, fixture.At)
	}
	for i := 0; i < 60; i++ {
		exec("INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,model,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,created_at) VALUES($1,$2,$3,$4,'rates',30,10,10,10,now())", uid, kid, sharedAccount, second)
	}
	// Billable interrupted requests have both usage and a final error. Neither
	// that error nor its upstream attempts may count as another client request.
	exec("UPDATE usage_logs SET request_id='ops-rate-'||id WHERE group_id=$1", second)
	for _, phase := range []string{"gateway", "upstream"} {
		exec(`INSERT INTO ops_error_logs(request_id,user_id,api_key_id,account_id,group_id,platform,error_phase,error_type,status_code)
 SELECT request_id,user_id,api_key_id,account_id,group_id,'openai',$2,'fixture',503 FROM usage_logs WHERE group_id=$1`, second, phase)
	}
	requestID, finalID := "recovered-operational-request", "failed-operational-request"
	exec(`INSERT INTO ops_error_logs(request_id,user_id,api_key_id,account_id,group_id,platform,model,request_path,error_phase,error_type,error_owner,error_source,status_code,upstream_status_code,error_message,created_at)
		VALUES($1,$2,$3,$4,$5,'openai','ops-model','/v1/chat/completions','upstream','upstream_rejected','provider','upstream',503,503,'upstream unavailable',now()),
		($6,$2,$3,$4,$5,'openai','ops-model','/v1/chat/completions','upstream','upstream_rejected','provider','upstream',503,503,'upstream unavailable',now()),
		($6,$2,$3,$4,$5,'openai','ops-model','/v1/chat/completions','gateway','request_failed','gateway','gateway',503,NULL,'all upstream attempts exhausted',now())`, requestID, uid, kid, firstAccount, gid, finalID)
	for _, path := range []string{
		"/api/v1/admin/ops/account-availability", "/api/v1/admin/ops/realtime-traffic", "/api/v1/admin/ops/errors",
		"/api/v1/admin/ops/request-errors", "/api/v1/admin/ops/upstream-errors", "/api/v1/admin/ops/ingress-rejections",
		"/api/v1/admin/ops/ingress-rejections/health", "/api/v1/admin/ops/auth-cache-invalidation/health",
		"/api/v1/admin/groups/usage-summary", "/api/v1/admin/groups/capacity-summary",
	} {
		data("GET", path, admin, nil)
		for _, denied := range []struct {
			Token  string
			Status int
		}{{"", http.StatusUnauthorized}, {ordinary, http.StatusForbidden}, {secret, http.StatusUnauthorized}} {
			if w := call("GET", path, denied.Token, nil); w.Code != denied.Status {
				t.Fatalf("permission %s: %d %s", path, w.Code, w.Body.String())
			}
		}
	}
	listErrors := func(path string) []map[string]any {
		var result map[string]any
		if err := json.Unmarshal(data("GET", "/api/v1/admin/ops/"+path, admin, nil), &result); err != nil {
			t.Fatal(err)
		}
		items := []map[string]any{}
		for _, raw := range result["items"].([]any) {
			items = append(items, raw.(map[string]any))
		}
		return items
	}
	upstream := listErrors("upstream-errors?request_id=" + requestID)
	if len(upstream) != 1 || upstream[0]["account_id"] != float64(firstAccount) || upstream[0]["upstream_status_code"] != float64(503) {
		t.Fatalf("wrong upstream attempt: %+v", upstream)
	}
	if got := listErrors("request-errors?request_id=" + requestID); len(got) != 0 {
		t.Fatalf("recovered request exposed as final error: %+v", got)
	}
	final := listErrors("request-errors?request_id=" + finalID)
	if len(final) != 1 {
		t.Fatalf("final request error missing: %+v", final)
	}
	linked := listErrors(fmt.Sprintf("request-errors/%d/upstream-errors", int64(final[0]["id"].(float64))))
	if len(linked) != 1 || linked[0]["request_id"] != finalID {
		t.Fatalf("wrong upstream correlation: %+v", linked)
	}
	for _, path := range []string{"errors/1", "request-errors/1", "upstream-errors/1"} {
		raw := data("GET", "/api/v1/admin/ops/"+path, admin, nil)
		for _, private := range []string{secret, "ops-first-secret", "credentials", "error_body"} {
			if bytes.Contains(raw, []byte(private)) {
				t.Fatal("error query leaked private data", private)
			}
		}
	}
	for _, path := range []string{"realtime-traffic?window=bad", "realtime-traffic?group_id=-1", "realtime-traffic?platform=unknown", "account-availability?group_id=no", "errors?account_id=0", "errors?resolved=unknown", "errors?status_code=0", "errors?start_time=no", "ingress-rejections?client_ip=invalid", "ingress-rejections?user_id=0"} {
		if w := call("GET", "/api/v1/admin/ops/"+path, admin, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %s returned %d", path, w.Code)
		}
	}
	availability := manage("GET", fmt.Sprintf("/api/v1/admin/ops/account-availability?group_id=%d", gid), admin, nil)
	if availability["platform"].(map[string]any)["openai"].(map[string]any)["total_accounts"] != float64(2) {
		t.Fatal("account availability count is wrong")
	}
	if !a.takeSlot("account", sharedAccount, 3) {
		t.Fatal("slot unavailable")
	}
	var capacities []struct {
		ID   int64 `json:"group_id"`
		Used int   `json:"concurrency_used"`
		Max  int   `json:"concurrency_max"`
	}
	if err := json.Unmarshal(data("GET", "/api/v1/admin/groups/capacity-summary", admin, nil), &capacities); err != nil {
		t.Fatal(err)
	}
	for _, capacity := range capacities {
		if capacity.ID == gid && (capacity.Used != 1 || capacity.Max != 6) {
			t.Fatalf("group capacity is wrong: %+v", capacity)
		}
		if capacity.ID == second && (capacity.Used != 1 || capacity.Max != 3) {
			t.Fatalf("shared account was duplicated within group: %+v", capacity)
		}
	}
	a.releaseSlot("account", sharedAccount)
	traffic := manage("GET", fmt.Sprintf("/api/v1/admin/ops/realtime-traffic?window=1m&group_id=%d&platform=openai", second), admin, nil)["summary"].(map[string]any)
	if traffic["qps"].(map[string]any)["current"] != float64(1) || traffic["tps"].(map[string]any)["avg"] != float64(60) {
		t.Fatalf("wrong traffic rates: %+v", traffic)
	}
	var wg sync.WaitGroup
	before := time.Now().UTC().Truncate(time.Minute)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := call("GET", "/v1/billing", "never-store-this-invalid-key", nil); w.Code != http.StatusUnauthorized {
				t.Errorf("invalid key returned %d", w.Code)
			}
		}()
	}
	wg.Wait()
	var rejected int
	if err := a.DB.QueryRow("SELECT COALESCE(sum(request_count),0) FROM ops_ingress_reject_aggregates WHERE client_ip='192.0.2.155' AND bucket_start >= $1 AND reject_reason='invalid_api_key'", before).Scan(&rejected); err != nil || rejected != 8 {
		t.Fatal("rejection aggregation", rejected, err)
	}
	rejections := data("GET", "/api/v1/admin/ops/ingress-rejections?reason=invalid_api_key&client_ip=192.0.2.155", admin, nil)
	if bytes.Contains(rejections, []byte("never-store")) {
		t.Fatal("rejection leaked key")
	}
	cache := manage("GET", "/api/v1/admin/ops/auth-cache-invalidation/health", admin, nil)
	if cache["enabled"] != false || cache["mode"] != "database" || cache["outbox_required"] != false {
		t.Fatal("fabricated subscriber health", cache)
	}

	// Exercise the real producer as well as queries: a recovered HTTP failure
	// stays in upstream diagnostics, while only the final failed request is
	// exposed to the user. Upstream bodies are never persisted.
	var primaryCalls, backupCalls atomic.Int32
	var primaryStatus atomic.Int32
	primaryStatus.Store(503)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer ops-first-secret" {
			primaryCalls.Add(1)
			w.WriteHeader(int(primaryStatus.Load()))
			_, _ = io.WriteString(w, `{"error":"private-upstream-body"}`)
			return
		}
		backupCalls.Add(1)
		_, _ = io.WriteString(w, `{"model":"ops-http","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	for i, aid := range []int64{firstAccount, sharedAccount} {
		manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"priority": i + 1, "credentials": map[string]any{"base_url": up.URL}})
	}
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Ops HTTP", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"ops-http"}, "billing_mode": "per_request", "per_request_price": 0.01}}})
	body := map[string]any{"model": "ops-http", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	success := call("POST", "/v1/chat/completions", secret, body)
	if success.Code != 200 || primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatal("HTTP retry", success.Code, success.Body.String(), primaryCalls.Load(), backupCalls.Load())
	}
	successID := success.Header().Get("X-Request-ID")
	attempts := listErrors("upstream-errors?status_code=503&request_id=" + successID)
	if len(attempts) != 1 || attempts[0]["status_code"] != nil || attempts[0]["upstream_status_code"] != float64(503) || attempts[0]["request_type"] != float64(1) || attempts[0]["upstream_model"] != nil {
		t.Fatal("upstream HTTP status filter", attempts)
	}
	for _, path := range []string{"errors", "request-errors"} {
		if got := listErrors(path + "?request_id=" + successID); len(got) != 0 {
			t.Fatal("recovered attempt became final error", path, got)
		}
	}
	manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": true})
	defer manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": false})
	if w := call("GET", fmt.Sprintf("/api/v1/usage/errors/%d", int64(attempts[0]["id"].(float64))), token, nil); w.Code != 404 {
		t.Fatal("upstream diagnostic exposed as user error", w.Code, w.Body.String())
	}
	manage("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", firstAccount), admin, map[string]any{})
	manage("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/schedulable", sharedAccount), admin, map[string]any{"schedulable": false})
	failure := call("POST", "/v1/chat/completions", secret, body)
	if failure.Code != 503 || primaryCalls.Load() != 2 || backupCalls.Load() != 1 {
		t.Fatal("HTTP exhaustion", failure.Code, failure.Body.String())
	}
	failureID := failure.Header().Get("X-Request-ID")
	errors := listErrors("request-errors?request_id=" + failureID)
	if len(errors) != 1 {
		t.Fatal("HTTP final error missing", errors)
	}
	linked = listErrors(fmt.Sprintf("request-errors/%d/upstream-errors?status_code=503", int64(errors[0]["id"].(float64))))
	if len(linked) != 1 || linked[0]["request_id"] != failureID {
		t.Fatal("HTTP correlation", linked)
	}
	var leaked int
	if err := a.DB.QueryRow("SELECT count(*) FROM ops_error_logs e WHERE e.request_id IN ($1,$2) AND (to_jsonb(e)::text LIKE '%private-upstream-body%' OR to_jsonb(e)::text LIKE '%ops-first-secret%')", successID, failureID).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatal("upstream secrets persisted", leaked, err)
	}
	// A nonretryable rejection keeps the public model and the actual mapped
	// model separate in both the attempt and the final request record.
	primaryStatus.Store(400)
	manage("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/clear-rate-limit", firstAccount), admin, map[string]any{})
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", firstAccount), admin, map[string]any{"credentials": map[string]any{"model_mapping": map[string]string{"ops-http": "ops-private-model"}}})
	body["stream"] = true
	failure = call("POST", "/v1/chat/completions", secret, body)
	if failure.Code != 502 {
		t.Fatal("mapped rejection", failure.Code, failure.Body.String())
	}
	failureID = failure.Header().Get("X-Request-ID")
	for _, path := range []string{"request-errors", "upstream-errors"} {
		rows := listErrors(path + "?request_id=" + failureID)
		if len(rows) != 1 || rows[0]["model"] != "ops-http" || rows[0]["requested_model"] != "ops-http" || rows[0]["upstream_model"] != "ops-private-model" || rows[0]["stream"] != true || rows[0]["request_type"] != float64(2) {
			t.Fatal("mapped rejection metadata", path, rows)
		}
		if path == "request-errors" {
			detail := manage("GET", fmt.Sprintf("/api/v1/usage/errors/%d", int64(rows[0]["id"].(float64))), token, nil)
			if detail["model"] != "ops-http" || detail["stream"] != true || detail["request_type"] != float64(2) || detail["upstream_model"] != nil {
				t.Fatal("upstream mapping exposed to user", detail)
			}
		}
	}
	var userErrors struct {
		Items []struct {
			Phase string `json:"error_phase"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data("GET", "/api/v1/usage/errors", token, nil), &userErrors); err != nil {
		t.Fatal(err)
	}
	for _, item := range userErrors.Items {
		if item.Phase != "gateway" {
			t.Fatal("upstream attempt in user errors", item)
		}
	}
}
