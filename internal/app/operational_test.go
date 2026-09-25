package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testIngressRejections(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	const client = "2001:db8:244::"
	const secret = "ingress-credential-canary"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var uid, gid, kid int64
	if err := a.DB.QueryRow("INSERT INTO users(email,password_hash,balance) SELECT 'ingress@example.test',password_hash,10 FROM users WHERE role='admin' LIMIT 1 RETURNING id").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow("INSERT INTO groups(name,platform) VALUES('Ingress checks','openai') RETURNING id").Scan(&gid); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.QueryRow("INSERT INTO api_keys(user_id,group_id,name,key) VALUES($1,$2,'Ingress',$3) RETURNING id", uid, gid, secret).Scan(&kid); err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"private":"request-body-canary"}`))
		r.RemoteAddr = "[" + client + "1234]:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	// Each denial is produced by the shared authentication path, not a synthetic log.
	for _, tc := range []struct {
		name, change, reset, token, reason string
		status                             int
		known                              bool
	}{
		{"missing", "", "", "", "api_key_required", 401, false},
		{"invalid", "", "", "not-a-valid-credential", "invalid_api_key", 401, false},
		{"long", "", "", strings.Repeat("k", 129), "invalid_api_key", 401, false},
		{"key-disabled", "UPDATE api_keys SET status='inactive' WHERE id=$1", "UPDATE api_keys SET status='active' WHERE id=$1", secret, "api_key_disabled", 401, true},
		{"expired", "UPDATE api_keys SET expires_at=now()-interval '1 second' WHERE id=$1", "UPDATE api_keys SET expires_at=NULL WHERE id=$1", secret, "api_key_disabled", 401, true},
		{"user-disabled", "UPDATE users SET status='disabled' WHERE id=$2", "UPDATE users SET status='active' WHERE id=$2", secret, "user_inactive", 401, true},
		{"unassigned", "UPDATE api_keys SET group_id=NULL WHERE id=$1", "UPDATE api_keys SET group_id=$3 WHERE id=$1", secret, "group_unassigned", 403, true},
		{"group-disabled", "UPDATE groups SET status='inactive' WHERE id=$3", "UPDATE groups SET status='active' WHERE id=$3", secret, "group_disabled", 403, true},
		{"group-deleted", "UPDATE groups SET deleted_at=now() WHERE id=$3", "UPDATE groups SET deleted_at=NULL WHERE id=$3", secret, "group_deleted", 403, true},
		{"private-group", "UPDATE groups SET is_exclusive=true WHERE id=$3", "UPDATE groups SET is_exclusive=false WHERE id=$3", secret, "group_not_allowed", 403, true},
		{"ip-denied", `UPDATE api_keys SET ip_blacklist='["2001:db8:244::/64"]'::jsonb WHERE id=$1`, "UPDATE api_keys SET ip_blacklist='[]'::jsonb WHERE id=$1", secret, "ip_restricted", 403, true},
		{"deleted-key", "UPDATE api_keys SET deleted_at=now() WHERE id=$1", "UPDATE api_keys SET deleted_at=NULL WHERE id=$1", secret, "invalid_api_key", 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Cast all three parameters even when only one is used in the mutation.
			change := func(query string) {
				if query != "" {
					exec(query+" AND $1::bigint>0 AND $2::bigint>0 AND $3::bigint>0", kid, uid, gid)
				}
			}
			change(tc.change)
			defer change(tc.reset)
			exec("DELETE FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client)
			w := call("GET", "/v1/usage", tc.token)
			if w.Code != tc.status || strings.Contains(w.Body.String(), tc.reason) || strings.Contains(w.Body.String(), secret) {
				t.Fatalf("denial response changed: %d %s", w.Code, w.Body.String())
			}
			var reason string
			var user, key, count int64
			if err := a.DB.QueryRow("SELECT reject_reason,user_id,api_key_id,request_count FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client).Scan(&reason, &user, &key, &count); err != nil || reason != tc.reason || count != 1 {
				t.Fatal("denial classification", reason, count, err)
			}
			if tc.known && (user != uid || key != kid) || !tc.known && (user != 0 || key != 0) {
				t.Fatal("unverified identity or missing known identity", user, key)
			}
		})
	}
	// Concurrent denials of a known key aggregate once per request and cannot
	// move either time boundary inward. No credential/body is stored.
	exec("DELETE FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client)
	exec("UPDATE api_keys SET status='inactive' WHERE id=$1", kid)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := call("GET", "/v1/usage", secret); w.Code != 401 {
				t.Errorf("concurrent denial returned %d", w.Code)
			}
		}()
	}
	wg.Wait()
	var count int
	var safe bool
	if err := a.DB.QueryRow(`SELECT sum(request_count),bool_and(user_id=$2 AND api_key_id=$3 AND first_seen<=last_seen AND to_jsonb(x)::text NOT LIKE '%canary%')
 FROM ops_ingress_reject_aggregates x WHERE client_ip=$1`, client, uid, kid).Scan(&count, &safe); err != nil || count != 8 || !safe {
		t.Fatal("concurrent attribution or sensitive data persisted", count, safe, err)
	}
	exec("UPDATE ops_ingress_reject_aggregates SET first_seen=bucket_start-interval '1 hour',last_seen=bucket_start+interval '1 hour' WHERE client_ip=$1", client)
	call("GET", "/v1/usage", secret)
	if err := a.DB.QueryRow("SELECT bool_and(first_seen=bucket_start-interval '1 hour' AND last_seen=bucket_start+interval '1 hour') FROM ops_ingress_reject_aggregates WHERE client_ip=$1 AND request_count>1", client).Scan(&safe); err != nil || !safe {
		t.Fatal("rejection time boundaries narrowed", err)
	}
	failures := a.ingressFailures.Load()
	exec("ALTER TABLE ops_ingress_reject_aggregates ADD CONSTRAINT test_ingress_write_failure CHECK(client_ip<>'2001:db8:244::'::inet) NOT VALID")
	defer a.DB.Exec("ALTER TABLE ops_ingress_reject_aggregates DROP CONSTRAINT IF EXISTS test_ingress_write_failure")
	if w := call("GET", "/v1/usage", secret); w.Code != 401 || a.ingressFailures.Load() != failures+1 {
		t.Fatal("recording failure changed denial or health", w.Code)
	}
	exec("ALTER TABLE ops_ingress_reject_aggregates DROP CONSTRAINT test_ingress_write_failure")
	exec("UPDATE api_keys SET status='active' WHERE id=$1", kid)
	call("GET", "/v1/usage", secret)
	if err := a.DB.QueryRow("SELECT sum(request_count) FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client).Scan(&count); err != nil || count != 9 {
		t.Fatal("successful authentication or failed recording added rejections", count, err)
	}
	// The query is an authenticated projection: normalized IP, optional known IDs,
	// strict filters, stable ordering and an exact [start,end) bucket range.
	exec("DELETE FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client)
	for _, tc := range []struct{ method, path, family, protocol, reason string }{
		{"POST", "/v1/messages", "messages", "anthropic", "invalid_api_key"},
		{"GET", "/backend-api/codex/models", "codex", "openai", "invalid_api_key"},
		{"GET", "/v1beta/models?key=query-credential-canary", "gemini", "google", "invalid_api_key"},
		{"GET", "/models?api_key=query-credential-canary", "models", "openai", "query_api_key_deprecated"},
	} {
		w := call(tc.method, tc.path, "invalid-credential")
		if w.Code != 401 && w.Code != 400 {
			t.Fatal("protocol denial", tc.path, w.Code)
		}
		var count int
		if err := a.DB.QueryRow("SELECT count(*) FROM ops_ingress_reject_aggregates WHERE client_ip=$1 AND route_family=$2 AND protocol=$3 AND reject_reason=$4 AND user_id=0 AND api_key_id=0", client, tc.family, tc.protocol, tc.reason).Scan(&count); err != nil || count != 1 {
			t.Fatal("protocol classification", tc.path, count, err)
		}
	}
	exec("DELETE FROM ops_ingress_reject_aggregates WHERE client_ip=$1", client)
	at := time.Now().UTC().Truncate(time.Minute).Add(-10 * time.Minute)
	ids := []int64{}
	for i := range 3 {
		var id int64
		err := a.DB.QueryRow(`INSERT INTO ops_ingress_reject_aggregates(bucket_start,reject_reason,route_family,protocol,client_ip,user_id,api_key_id,request_count,first_seen,last_seen)
 VALUES($1,'ip_restricted','messages','anthropic',$2,$3,$4,2,$1,$1) RETURNING id`, at.Add(time.Duration(i/2)*time.Minute), client, int64(i%2)*uid, int64(i%2)*kid).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	list := func(query string) (int, []map[string]any) {
		t.Helper()
		w := call("GET", "/api/v1/admin/ops/ingress-rejections?client_ip="+url.QueryEscape(" "+client+"9876 ")+"&"+query, admin)
		var out struct {
			Data struct {
				Total int
				Items []map[string]any
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("ingress query", w.Code, w.Body.String())
		}
		for _, item := range out.Data.Items {
			if item["client_ip"] != client || item["created_at"] != nil || item["updated_at"] != nil || item["user_id"] == float64(0) || item["api_key_id"] == float64(0) {
				t.Fatal("ingress public projection", item)
			}
		}
		return out.Data.Total, out.Data.Items
	}
	check := func(query string, total int, want ...int64) {
		t.Helper()
		count, rows := list(query)
		if count != total || len(rows) != len(want) {
			t.Fatal("ingress range or count", query, count, rows)
		}
		for i, id := range want {
			if rows[i]["id"] != float64(id) {
				t.Fatal("ingress ordering", query, rows)
			}
		}
	}
	check("", 3, ids[2], ids[1], ids[0])
	check("page_size=1&page=2", 3, ids[1])
	check(fmt.Sprintf("user_id=%%20%d%%20&api_key_id=%d&reason=%%20ip_restricted%%20&route_family=messages&protocol=anthropic", uid, kid), 1, ids[1])
	check("start_time="+url.QueryEscape(at.Add(time.Second).Format(time.RFC3339)), 1, ids[2])
	check("end_time="+url.QueryEscape(at.Add(time.Minute).Format(time.RFC3339)), 2, ids[1], ids[0])
	for _, query := range []string{"reason=unknown", "reason=ip_restricted%00", "reason=invalid_api_key%20ip_restricted", "route_family=unknown", "protocol=%FF", "user_id=0", "api_key_id=-1", "client_ip=not-an-ip"} {
		if w := call("GET", "/api/v1/admin/ops/ingress-rejections?"+query, admin); w.Code != 400 {
			t.Fatal("invalid ingress filter", query, w.Code)
		}
	}
	for _, tc := range []struct {
		token  string
		status int
	}{{"", 401}, {ordinary, 403}, {secret, 401}} {
		if w := call("GET", "/api/v1/admin/ops/ingress-rejections", tc.token); w.Code != tc.status {
			t.Fatal("ingress permissions", w.Code)
		}
	}
	// Snapshot pagination must remain coherent while independent writes arrive.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			var id int64
			err := a.DB.QueryRow(`INSERT INTO ops_ingress_reject_aggregates(bucket_start,reject_reason,route_family,protocol,client_ip,request_count,first_seen,last_seen)
 VALUES($1,'other','other','gateway',$2,1,$1,$1) RETURNING id`, at.Add(2*time.Minute), client).Scan(&id)
			if err == nil {
				_, err = a.DB.Exec("DELETE FROM ops_ingress_reject_aggregates WHERE id=$1", id)
			}
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 24 {
		total, rows := list("page_size=200")
		if total != len(rows) || total < 3 || total > 4 {
			t.Fatal("ingress count and page used different snapshots", total, rows)
		}
	}
}

func testOperationalQueries(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	var uid int64
	if err := a.DB.QueryRow("INSERT INTO users(email,password_hash) SELECT 'ops-query_%@example.test',password_hash FROM users WHERE role='admin' LIMIT 1 RETURNING id").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ids := map[string]int64{}
	for _, row := range []struct {
		name, phase, owner, requested string
		status, upstream              any
		limited, resolved             bool
		age                           time.Duration
	}{
		{"z", "gateway", "gateway", "zeta", 502, 504, false, false, time.Minute},
		{"a", "gateway", "gateway", "alpha", 500, nil, false, true, time.Minute},
		{"rate", "request", "gateway", "rate-model", 429, nil, true, false, time.Minute},
		{"quota", "gateway", "gateway", "quota-model", 402, nil, true, false, time.Minute},
		{"old", "gateway", "gateway", "old-model", 500, nil, false, false, 2 * time.Hour},
		{"z", "upstream", "provider", "zeta", nil, 503, true, false, 2 * time.Hour},
		{"auth", "account_auth", "provider", "auth-model", 200, 401, false, true, time.Minute},
		{"client", "gateway", "gateway", "client-model", 500, nil, false, false, time.Minute},
		{"client", "upstream", "provider", "client-model", nil, 502, false, false, 10 * 24 * time.Hour},
	} {
		var request any = "ops-query-" + row.name
		if row.name == "client" {
			request = nil
		}
		kind := "request_failed"
		if row.name == "rate" {
			kind = "rate_limit_error"
		}
		var id int64
		err := a.DB.QueryRow(`INSERT INTO ops_error_logs(request_id,client_request_id,user_id,platform,model,requested_model,error_phase,error_type,error_owner,error_source,status_code,upstream_status_code,is_business_limited,resolved,created_at,error_message,error_body)
VALUES($1,$2,$3,'openai','private-upstream-model',$4,$5,$6,$7,$7,$8,$9,$10,$11,$12,'safe summary','do-not-expose-body') RETURNING id`, request, "ops-query-client-"+row.name, uid, row.requested, row.phase, kind, row.owner, row.status, row.upstream, row.limited, row.resolved, now.Add(-row.age)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids[row.name+":"+row.phase] = id
	}
	call := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/v1/admin/ops/"+path, nil)
		r.RemoteAddr = "192.0.2.207:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	check := func(path, query string, total, size int, want ...int64) {
		t.Helper()
		w := call(path+"?q=ops-query&"+query, admin)
		var result struct {
			Data struct {
				Items []struct{ ID int64 }
				Total int
				Size  int `json:"page_size"`
			}
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Total != total || result.Data.Size != size || len(result.Data.Items) != len(want) || strings.Contains(w.Body.String(), "do-not-expose-body") {
			t.Fatalf("ops query %s %s: %d %s", path, query, w.Code, w.Body.String())
		}
		for i, id := range want {
			if result.Data.Items[i].ID != id {
				t.Fatalf("ops ordering %s %s: %s", path, query, w.Body.String())
			}
		}
	}
	z, alpha, client := ids["z:gateway"], ids["a:gateway"], ids["client:gateway"]
	rate, quota, old := ids["rate:request"], ids["quota:gateway"], ids["old:gateway"]
	for _, path := range []string{"errors", "request-errors"} {
		check(path, "sort_by=model&sort_order=asc", 3, 20, alpha, client, z)
		check(path, "sort_by=status_code&sort_order=asc&page_size=1&page=2", 3, 1, client)
		check(path, "sort_by=status_code&sort_order=desc&page_size=1", 3, 1, z)
		check(path, "sort_by=invalid&sort_order=asc", 3, 20, z, alpha, client)
		check(path, "view=excluded&sort_order=asc", 2, 20, rate, quota)
		check(path, "view=all&category=quota", 1, 20, quota)
		check(path, "view=excluded&category=rate_limit&phase=REQUEST", 1, 20, rate)
		check(path, "view=all&category=quota&phase=request", 0, 20)
		check(path, "view=unknown&category=unknown&sort_order=asc", 3, 20, z, alpha, client)
		check(path, "status_codes=504,,429,&view=all&sort_by=status_code&sort_order=asc", 2, 20, rate, z)
		check(path, "status_codes=0", 0, 20)
		check(path, "model=zeta&error_owner=GATEWAY&error_source=GATEWAY&resolved=no", 1, 20, z)
		check(path, "model=private-upstream-model", 0, 20)
		check(path, "resolved=YES&user_query="+url.QueryEscape("ops-query_%"), 1, 20, alpha)
		check(path, "resolved=YES&user_query="+url.QueryEscape("ops-query_Z%"), 0, 20)
		check(path, "client_request_id=ops-query-client-a", 1, 20, alpha)
		check(path, "time_range=24h&sort_order=asc", 4, 20, old, z, alpha, client)
		check(path, "time_range=unknown&sort_order=asc", 3, 20, z, alpha, client)
		check(path, "end_time="+url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)), 1, 20, old)
		check(path, "start_time="+url.QueryEscape(now.Format(time.RFC3339))+"&end_time="+url.QueryEscape(now.Format(time.RFC3339)), 0, 20)
		check(path, "page_size=700&sort_order=asc", 3, 500, z, alpha, client)
	}
	check("upstream-errors", "status_codes=401&resolved=yes", 1, 20, ids["auth:account_auth"])
	check("upstream-errors", "time_range=24h&view=all&sort_by=status_code&sort_order=asc", 2, 20, ids["auth:account_auth"], ids["z:upstream"])
	check("upstream-errors", "time_range=24h&view=excluded", 1, 20, ids["z:upstream"])
	check(fmt.Sprintf("request-errors/%d/upstream-errors", z), "", 1, 20, ids["z:upstream"])
	check(fmt.Sprintf("request-errors/%d/upstream-errors", client), "", 1, 20, ids["client:upstream"])
	check(fmt.Sprintf("request-errors/%d/upstream-errors", z), "time_range=1h", 0, 20)
	// The ingress list shares the same time parser and must honor a historical
	// end-only range rather than deriving its start from today's clock.
	if _, err := a.DB.Exec(`INSERT INTO ops_ingress_reject_aggregates(bucket_start,client_ip,route_family,protocol,reject_reason,request_count,first_seen,last_seen) VALUES($1,'192.0.2.207','chat','openai','invalid_api_key',1,$1,$1)`, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"time_range=24h", "end_time=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339))} {
		w := call("ingress-rejections?client_ip=192.0.2.207&"+query, admin)
		var result struct{ Data struct{ Total int } }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Total != 1 {
			t.Fatal("ingress time range", w.Code, w.Body.String())
		}
	}
	for _, query := range []string{"status_codes=-1", "status_codes=99999999999999999999", "status_codes=bad", "status_codes=600", "model=%00", "q=%00", "user_query=%00", "request_id=%00", "client_request_id=%00", "end_time=bad", "start_time=" + url.QueryEscape(now.Add(-31*24*time.Hour).Format(time.RFC3339))} {
		if w := call("errors?"+query, admin); w.Code != 400 {
			t.Fatal("invalid operational query accepted", query, w.Code)
		}
	}
	for _, path := range []string{"errors", "request-errors", "upstream-errors", fmt.Sprintf("request-errors/%d/upstream-errors", z)} {
		for _, tc := range []struct {
			token string
			code  int
		}{{ordinary, 403}, {"", 401}} {
			if w := call(path+"?view=all", tc.token); w.Code != tc.code {
				t.Fatal("operational query authorization", path, w.Code)
			}
		}
	}
}

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
	// Cooldown projections use one response clock, never rewrite persisted state,
	// and keep all memberships when filtering accounts by just one group.
	checkAvailability := func(limited, failed, temporary bool, availableCount int) map[string]any {
		t.Helper()
		v := manage("GET", fmt.Sprintf("/api/v1/admin/ops/account-availability?group_id=%d&platform=openai", second), admin, nil)
		rows := v["account"].(map[string]any)
		if len(rows) != 1 {
			t.Fatal("availability group filter", rows)
		}
		row := rows[fmt.Sprint(sharedAccount)].(map[string]any)
		if row["is_rate_limited"] != limited || row["is_overloaded"] != limited || row["has_error"] != failed || row["is_available"] != (availableCount == 1) || row["group_id"] != float64(gid) {
			t.Fatal("availability status or first group", row)
		}
		clock, err := time.Parse(time.RFC3339Nano, v["timestamp"].(string))
		if err != nil {
			t.Fatal(err)
		}
		for _, names := range [][2]string{{"rate_limit_reset_at", "rate_limit_remaining_sec"}, {"overload_until", "overload_remaining_sec"}} {
			until, present := row[names[0]]
			seconds, remainingPresent := row[names[1]]
			if !present || !remainingPresent {
				t.Fatal("missing nullable cooldown fields", row)
			}
			if limited {
				at, err := time.Parse(time.RFC3339Nano, until.(string))
				if err != nil || seconds != float64(int64(at.Sub(clock)/time.Second)) || seconds.(float64) <= 0 {
					t.Fatal("cooldown clock or seconds", row, err)
				}
			} else if until != nil || seconds != nil {
				t.Fatal("inactive cooldown exposed", row)
			}
		}
		if _, ok := row["temp_unschedulable_until"]; ok != temporary {
			t.Fatal("temporary deadline presence", row)
		}
		count := map[string]any{"available_count": float64(availableCount), "rate_limit_count": float64(0), "error_count": float64(0), "total_accounts": float64(1)}
		if limited {
			count["rate_limit_count"] = float64(1)
		}
		if failed {
			count["error_count"] = float64(1)
		}
		for _, summary := range []any{v["platform"].(map[string]any)["openai"], v["group"].(map[string]any)[fmt.Sprint(gid)], v["group"].(map[string]any)[fmt.Sprint(second)]} {
			for field, want := range count {
				if summary.(map[string]any)[field] != want {
					t.Fatal("availability aggregate", field, summary)
				}
			}
		}
		return row
	}
	if row := checkAvailability(false, false, false, 1); row["error_message"] != "" {
		t.Fatal("NULL error not exposed as empty string", row)
	}
	exec("UPDATE accounts SET rate_limit_reset_at=now()+interval '1 hour',overload_until=now()+interval '2 hours',temp_unschedulable_until=now()+interval '3 hours' WHERE id=$1", sharedAccount)
	checkAvailability(true, false, true, 0)
	message := "authentication rejected ops-shared-secret bearer hidden-secret password=private\n"
	exec("UPDATE accounts SET status='error',error_message=$2 WHERE id=$1", sharedAccount, message)
	if row := checkAvailability(false, true, true, 0); row["error_message"] != "authentication rejected [redacted] [redacted] [redacted] " {
		t.Fatal("availability error not safely projected", row)
	}
	var retained bool
	if err := a.DB.QueryRow("SELECT rate_limit_reset_at>now() AND overload_until>now() AND temp_unschedulable_until>now() AND error_message=$2 FROM accounts WHERE id=$1", sharedAccount, message).Scan(&retained); err != nil || !retained {
		t.Fatal("availability read changed stored cooldowns or error", err)
	}
	exec("UPDATE accounts SET status='active',error_message=NULL,rate_limit_reset_at=now()-interval '1 second',overload_until=now()-interval '1 second',temp_unschedulable_until=now()-interval '1 second' WHERE id=$1", sharedAccount)
	checkAvailability(false, false, false, 1)
	if err := a.DB.QueryRow("SELECT rate_limit_reset_at IS NOT NULL AND overload_until IS NOT NULL AND temp_unschedulable_until IS NOT NULL FROM accounts WHERE id=$1", sharedAccount).Scan(&retained); err != nil || !retained {
		t.Fatal("availability read cleared expired state", err)
	}
	exec(`UPDATE accounts SET extra=extra||'{"quota_limit":1,"quota_used":1}'::jsonb WHERE id=$1`, sharedAccount)
	if row := checkAvailability(false, false, false, 0); row["quota_available"] != false {
		t.Fatal("availability lost quota restriction", row)
	}
	exec("UPDATE accounts SET extra=extra-'quota_limit'-'quota_used',rate_limit_reset_at=NULL,overload_until=NULL,temp_unschedulable_until=NULL WHERE id=$1", sharedAccount)
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
