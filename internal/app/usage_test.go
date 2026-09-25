package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUsageEffortView(t *testing.T) {
	for _, row := range []struct {
		requested, forwarded, want string
		different                  bool
	}{
		{" max ", "high", "max", true}, {"extra-high", "xhigh", "extra-high", false},
		{" HIGH ", "high", "HIGH", false}, {"", "low", "low", false},
		{"   ", "medium", "medium", false}, {"max", "", "max", false},
	} {
		for _, admin := range []bool{false, true} {
			raw, _ := json.Marshal(map[string]any{"requested_reasoning_effort": row.requested, "reasoning_effort": row.forwarded, "actual_cost": json.Number("1234567890.1234567890")})
			out, err := usageEffortView(raw, admin)
			var fields map[string]json.RawMessage
			if err != nil || json.Unmarshal(out, &fields) != nil || credentialString(fields, "reasoning_effort") != row.want || string(fields["actual_cost"]) != "1234567890.1234567890" {
				t.Fatal("usage effort or decimal projection", string(out), err)
			}
			if _, exists := fields["upstream_reasoning_effort"]; exists != (admin && row.different) {
				t.Fatal("upstream effort visibility", string(out), admin)
			}
		}
	}
	if _, err := usageEffortView(json.RawMessage(`invalid`), true); err == nil {
		t.Fatal("invalid usage JSON accepted")
	}
}

func testUsageErrors(t *testing.T, a *App, admin, other string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.RemoteAddr = "192.0.2.242:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	if _, err := a.DB.Exec("DELETE FROM settings WHERE key='allow_user_view_error_requests'"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("DELETE FROM settings WHERE key='allow_user_view_error_requests'")
	uid := int64(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "error-query@example.test", "password": "error-query-password"})["id"].(float64))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "error-query@example.test", "password": "error-query-password"})["access_token"].(string)
	key := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Error query"})
	kid := int64(key["id"].(float64))
	const root = "/api/v1/usage/errors"
	for _, path := range []string{root, root + "/1"} {
		for _, token := range []string{admin, user} {
			if w := call("GET", path, token, nil); w.Code != 403 {
				t.Fatal("missing setting opened errors", path, w.Code)
			}
		}
	}
	if manage("GET", "/api/v1/settings/public", "", nil)["allow_user_view_error_requests"] != false {
		t.Fatal("public default")
	}
	if w := call("PUT", "/api/v1/admin/settings", user, map[string]any{"allow_user_view_error_requests": true}); w.Code != 403 {
		t.Fatal("user enabled errors", w.Code)
	}
	if w := call("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": "true"}); w.Code != 400 {
		t.Fatal("invalid setting accepted", w.Code)
	}
	manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": true})
	if manage("GET", "/api/v1/settings/public", "", nil)["allow_user_view_error_requests"] != true {
		t.Fatal("public setting not updated")
	}
	manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": nil})
	at := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	var ids []int64
	for i, row := range []struct {
		model, phase, kind string
		status             int
		at                 time.Time
		count              bool
	}{
		{" Alpha%_ ", "request", "rate_limit_error", 429, at, false},
		{"beta", "gateway", "request_failed", 402, at.Add(time.Hour), false},
		{"alpha", "gateway", "request_failed", 503, at.Add(time.Hour), false},
		{"gamma", "internal", "internal_error", 500, at.Add(22 * time.Hour), false},
		{"before", "auth", "auth_error", 401, at.Add(-time.Second), false},
		{"after", "network", "network_error", 502, at.Add(23 * time.Hour), false},
		{"hidden-attempt", "upstream", "upstream_rejected", 503, at, false},
		{"hidden-count", "request", "invalid_request_error", 400, at, true},
		{"hidden-success", "gateway", "request_failed", 200, at, false},
	} {
		var id int64
		err := a.DB.QueryRow(`INSERT INTO ops_error_logs(user_id,api_key_id,model,requested_model,error_phase,error_type,status_code,created_at,is_count_tokens,
 request_id,request_path,error_message,error_body,upstream_error_message,upstream_endpoint,api_key_prefix,client_ip,user_agent)
 VALUES($1,$2,'private-model',$3,$4,$5,$6,$7,$8,$9,'/v1/videos','safe summary','private-body','private-upstream','private-endpoint','private-prefix','192.0.2.242','test-client') RETURNING id`,
			uid, kid, row.model, row.phase, row.kind, row.status, row.at, row.count, fmt.Sprintf("user-error-%d", i)).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	base := "start_date=2026-03-08&end_date=2026-03-08&timezone=America%2FNew_York"
	check := func(query string, total int, want ...int) []map[string]any {
		t.Helper()
		data := manage("GET", root+"?"+query, user, nil)
		rows := data["items"].([]any)
		if data["total"] != float64(total) || len(rows) != len(want) {
			t.Fatalf("error filter %s: %v", query, data)
		}
		out := []map[string]any{}
		for i, v := range rows {
			item := v.(map[string]any)
			if item["id"] != float64(ids[want[i]]) {
				t.Fatal("error sort", query, item, want)
			}
			out = append(out, item)
		}
		return out
	}
	check(base, 4, 3, 2, 1, 0)
	check(base+"&sort_by=created_at&sort_order=asc&page_size=2&page=2", 4, 2, 3)
	rows := check(base+"&sort_by=model&sort_order=asc", 4, 0, 2, 1, 3)
	if rows[0]["model"] != "Alpha%_" || rows[0]["category"] != "rate_limit" || rows[1]["category"] != "service_unavailable" || rows[2]["category"] != "quota" {
		t.Fatal("model/category projection", rows)
	}
	check(base+"&sort_by=status_code&sort_order=asc", 4, 1, 0, 3, 2)
	check(base+"&model=%25_", 1, 0)
	check(base+"&model=ALPHA", 2, 2, 0)
	check(base+"&model=private-model", 0)
	check(base+"&api_key_id="+fmt.Sprint(kid), 4, 3, 2, 1, 0)
	check(base+"&api_key_id=0&category=other&sort_by=invalid", 4, 3, 2, 1, 0)
	check(base+"&category=unknown&user_id=1&error_phase=upstream&view=all", 4, 3, 2, 1, 0)
	check(base+"&category=quota&status_code=402", 1, 1)
	check(base+"&category=service_unavailable", 1, 2)
	check(base+"&category=upstream", 0)
	check("category=upstream", 1, 5)
	check("category=auth", 1, 4)
	check(base+"&api_key_id=9223372036854775807", 0)
	// Exercise the shared error writer, not only prebuilt database records.
	g := &gatewayIdentity{UserID: uid, Key: gatewayKey{ID: kid}, Group: gatewayGroup{Platform: "openai"}}
	for i, path := range []string{"/v1/messages/count_tokens", "/v1/responses/input_tokens", "/v1beta/models/test:countTokens", "/v1beta/models/test/countTokens", "/v1/videos"} {
		r := httptest.NewRequest("POST", path, nil)
		r.RemoteAddr = "192.0.2.242:1234"
		r.Header.Set("User-Agent", "test-client")
		requestID := fmt.Sprintf("error-query-writer-%d", i)
		a.recordGatewayError(requestID, g, nil, r, textRequest{}, &apiError{429, "rate limit exceeded"}, time.Now())
		var id int64
		if err := a.DB.QueryRow("SELECT id FROM ops_error_logs WHERE request_id=$1", requestID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		w := call("GET", root+"/"+fmt.Sprint(id), user, nil)
		if i < 4 {
			if w.Code != 404 {
				t.Fatal("count-only final error visible", path, w.Code)
			}
		} else {
			detail := manage("GET", root+"/"+fmt.Sprint(id), user, nil)
			if detail["category"] != "rate_limit" || detail["client_ip"] != "192.0.2.242" || detail["user_agent"] != "test-client" || detail["inbound_endpoint"] != path {
				t.Fatal("recorded user error metadata", detail)
			}
		}
	}
	for _, query := range []string{"api_key_id=-1", "api_key_id=oops", "status_code=600", "status_code=-1", "start_date=bad", "timezone=invalid", "start_date=2026-04-01&end_date=2026-03-01", "model=" + strings.Repeat("x", 101)} {
		if w := call("GET", root+"?"+query, user, nil); w.Code != 400 {
			t.Fatal("invalid error query", query, w.Code)
		}
	}
	if data := manage("GET", root+"?page_size=1000", user, nil); data["page_size"] != float64(100) {
		t.Fatal("error pagination cap", data)
	}
	for _, path := range []string{root, root + "/" + fmt.Sprint(ids[0])} {
		for _, credential := range []string{"", key["key"].(string)} {
			if w := call("GET", path, credential, nil); w.Code != 401 {
				t.Fatal("error authentication", path, w.Code)
			}
		}
		w := call("GET", path, user, nil)
		for _, secret := range []string{"private-", "upstream_error_message", "error_body", "upstream_endpoint", "api_key_prefix", "account_id"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("error diagnostic exposed", path, secret)
			}
		}
	}
	for _, token := range []string{other, admin} {
		if w := call("GET", root+"/"+fmt.Sprint(ids[0]), token, nil); w.Code != 404 {
			t.Fatal("foreign error detail exposed", w.Code)
		}
		data := manage("GET", root+"?api_key_id="+fmt.Sprint(kid), token, nil)
		if data["total"] != float64(0) {
			t.Fatal("foreign Key filter escaped ownership", data)
		}
	}
	for _, i := range []int{6, 7, 8} {
		if w := call("GET", root+"/"+fmt.Sprint(ids[i]), user, nil); w.Code != 404 {
			t.Fatal("hidden error detail exposed", i, w.Code)
		}
	}
	manage("DELETE", "/api/v1/keys/"+fmt.Sprint(kid), user, nil)
	detail := manage("GET", root+"/"+fmt.Sprint(ids[0]), user, nil)
	if detail["key_deleted"] != true || detail["key_name"] != "Error query" || detail["message"] != "safe summary" || detail["inbound_endpoint"] != "/v1/videos" {
		t.Fatal("deleted Key metadata", detail)
	}
	check(base+"&api_key_id="+fmt.Sprint(kid), 4, 3, 2, 1, 0)
	manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"allow_user_view_error_requests": false})
	for _, path := range []string{root, root + "/" + fmt.Sprint(ids[0])} {
		if w := call("GET", path, user, nil); w.Code != 403 {
			t.Fatal("disabled error view", w.Code)
		}
	}
	if _, err := a.DB.Exec("UPDATE settings SET value='invalid' WHERE key='allow_user_view_error_requests'"); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", root, user, nil); w.Code != 403 {
		t.Fatal("corrupt setting opened errors", w.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.usageErrors(httptest.NewRecorder(), httptest.NewRequest("GET", root, nil).WithContext(ctx)); err == nil || err.(*apiError).status != 403 {
		t.Fatal("unreadable setting opened errors", err)
	}
}

func TestUsageFilterRange(t *testing.T) {
	now := time.Date(2026, 3, 9, 18, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/api/v1/usage?start_date=2026-03-08&end_date=2026-03-08&timezone=America/New_York", nil)
	start, end, err := usageFilterRange(r, false, now)
	if err != nil || end.Sub(start) != 23*time.Hour || start.UTC().Hour() != 5 || end.UTC().Hour() != 4 {
		t.Fatal("DST calendar range", start, end, err)
	}
	r = httptest.NewRequest("GET", "/api/v1/usage?start_date=2026-03-08&end_date=2026-03-08", nil)
	start, end, err = usageFilterRange(r, false, now)
	if err != nil || start.UTC().Format(time.RFC3339) != "2026-03-07T16:00:00Z" || end.Sub(start) != 24*time.Hour {
		t.Fatal("default zone", start, end, err)
	}
	for _, admin := range []bool{false, true} {
		r = httptest.NewRequest("GET", "/api/v1/usage/stats?timezone=UTC", nil)
		start, end, err = usageFilterRange(r, admin, now)
		want := "2026-03-02"
		if admin {
			want = "2026-03-09"
		}
		if err != nil || start.Format("2006-01-02") != want || admin && !end.Equal(now) || !admin && end.Format("2006-01-02") != "2026-03-10" {
			t.Fatal("default stats range", admin, start, end, err)
		}
	}
	for _, query := range []string{"start_date=oops", "end_date=2026-02-30", "start_date=2026-04-01&end_date=2026-03-01", "timezone=Local", "timezone=not-a-zone", "period=year"} {
		if _, _, err := usageFilterRange(httptest.NewRequest("GET", "/api/v1/usage/stats?"+query, nil), false, now); err == nil {
			t.Fatal("invalid filter accepted", query)
		}
	}
}

func testUsageQueries(t *testing.T, a *App, admin, other string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.RemoteAddr = "192.0.2.241:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "usage-queries@example.test", "password": "usage-query-password"}))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "usage-queries@example.test", "password": "usage-query-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Usage query group", "platform": "openai"}))
	gid2 := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Usage query second group", "platform": "openai"}))
	key := manage("POST", "/api/v1/keys", user, map[string]any{"name": "usage queries", "group_id": gid})
	kid := id(key)
	kid2 := id(manage("POST", "/api/v1/keys", user, map[string]any{"name": "usage queries second", "group_id": gid2}))
	foreign := id(manage("POST", "/api/v1/keys", other, map[string]any{"name": "foreign usage queries", "group_id": gid}))
	aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Usage query source", "platform": "openai", "type": "apikey", "credentials": map[string]string{"api_key": "not-dispatched"}}))
	aid2 := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Usage query source two", "platform": "openai", "type": "apikey", "credentials": map[string]string{"api_key": "not-dispatched-either"}}))
	start := time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC)
	end := start.Add(23 * time.Hour)
	rows := []struct {
		model, requested       string
		kind                   int
		stream, ws, compaction bool
		mode                   any
		images                 int
		mismatch               any
		at                     time.Time
		key, group, account    int64
	}{
		{"billed-z", "alpha", 1, false, false, false, "token", 0, false, start, kid, gid, aid},
		{"billed-a", "beta", 2, true, false, false, "image", 1, true, start.Add(time.Hour), kid2, gid2, aid2},
		{"gamma", "   ", 0, true, true, false, "video", 0, nil, end.Add(-time.Microsecond), kid, gid, aid},
		{"billed-z", "alpha", 0, true, false, false, nil, 0, false, start.Add(-time.Second), kid, gid, aid},
		{"billed-z", "alpha", 0, false, false, false, nil, 1, false, end, kid, gid, aid},
		{"billed-z", " alpha ", 3, true, true, true, "per_request", 0, nil, start.Add(2 * time.Hour), kid, gid, aid},
		{"billed-z", "alpha", 0, false, false, false, nil, 0, false, start.Add(2 * time.Hour), kid2, gid2, aid2},
		{"recent", "recent", 1, false, false, false, "token", 0, false, time.Now().Add(-time.Second), kid, gid, aid},
		{"older", "older", 1, false, false, false, "token", 0, false, time.Now().AddDate(0, 0, -60), kid, gid, aid},
	}
	ids := []int64{}
	for i, row := range rows {
		var rid int64
		err := a.DB.QueryRow(`INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,request_id,model,requested_model,request_type,stream,openai_ws_mode,native_compaction_v2,billing_mode,image_count,image_size,upstream_model_mismatch,created_at,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,account_rate_multiplier,account_stats_cost,duration_ms,ip_address)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'1K',$14,$15,10,20,30,40,0.9876543219,0.1234567891,3,0.2,80,'192.0.2.241') RETURNING id`, uid, row.key, row.account, row.group, fmt.Sprintf("usage-query-%d", i), row.model, row.requested, row.kind, row.stream, row.ws, row.compaction, row.mode, row.images, row.mismatch, row.at).Scan(&rid)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, rid)
	}
	// The same public model and group cannot expand a user's scope to a sibling user.
	if _, err := a.DB.Exec(`INSERT INTO usage_logs(user_id,api_key_id,account_id,group_id,model,requested_model,created_at) SELECT user_id,id,$2,$3,'billed-z','alpha',$4 FROM api_keys WHERE id=$1`, foreign, aid, gid, start); err != nil {
		t.Fatal(err)
	}
	base := "start_date=2026-03-08&end_date=2026-03-08&timezone=America/New_York"
	type pageData struct {
		Items []map[string]json.RawMessage
		Total int
	}
	check := func(root, token, query string, want ...int) pageData {
		t.Helper()
		w := call("GET", root+"?"+query, token, nil)
		var out struct{ Data pageData }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("usage %s: %d %s", query, w.Code, w.Body.String())
		}
		if out.Data.Total != len(want) || len(out.Data.Items) != len(want) {
			t.Fatalf("usage %s: got %d/%d want %d", query, out.Data.Total, len(out.Data.Items), len(want))
		}
		for i, index := range want {
			if string(out.Data.Items[i]["id"]) != fmt.Sprint(ids[index]) {
				t.Fatalf("usage order %s: %s want %d", query, out.Data.Items[i]["id"], ids[index])
			}
			var model, kind string
			json.Unmarshal(out.Data.Items[i]["model"], &model)
			json.Unmarshal(out.Data.Items[i]["request_type"], &kind)
			requested := strings.TrimSpace(rows[index].requested)
			if requested == "" {
				requested = rows[index].model
			}
			wantKind := []string{"sync", "stream", "ws_v2", "stream", "sync", "ws_v2", "sync", "sync", "sync"}[index]
			if model != requested || kind != wantKind {
				t.Fatal("client-facing usage metadata", out.Data.Items[i])
			}
		}
		return out.Data
	}
	for _, who := range []struct{ root, token, prefix string }{{"/api/v1/usage", user, ""}, {"/api/v1/admin/usage", admin, fmt.Sprintf("user_id=%d&", uid)}} {
		query := who.prefix + base + "&sort_by=id&sort_order=asc"
		check(who.root, who.token, query, 0, 1, 2, 5, 6)
		check(who.root, who.token, query+"&model=alpha", 0, 5, 6)
		check(who.root, who.token, query+"&model=billed-z")
		check(who.root, who.token, query+"&model=gamma", 2)
		check(who.root, who.token, query+"&model="+url.QueryEscape("alpha' OR 1=1 --"))
		check(who.root, who.token, query+fmt.Sprintf("&group_id=%d", gid), 0, 2, 5)
		check(who.root, who.token, query+fmt.Sprintf("&api_key_id=%d", kid), 0, 2, 5)
		check(who.root, who.token, query+"&request_type=ws_v2&stream=bad", 2, 5)
		check(who.root, who.token, query+"&request_type=sync", 0, 6)
		check(who.root, who.token, query+"&request_type=stream", 1)
		check(who.root, who.token, query+"&request_type=unknown", 2, 6)
		check(who.root, who.token, query+"&stream=false", 0, 6)
		check(who.root, who.token, query+"&native_compaction_v2=true", 5)
		check(who.root, who.token, query+"&billing_mode=token", 0, 6)
		check(who.root, who.token, query+"&billing_mode=image", 1)
		check(who.root, who.token, query+"&billing_mode=video", 2)
		check(who.root, who.token, query+"&billing_mode=per_request", 5)
		check(who.root, who.token, query+"&billing_type=1")
		check(who.root, who.token, who.prefix+base+"&sort_by=model&sort_order=ASC", 0, 5, 6, 1, 2)
		check(who.root, who.token, who.prefix+base, 2, 6, 5, 1, 0)
		check(who.root, who.token, who.prefix+base+"&sort_by="+url.QueryEscape("id;DROP TABLE usage_logs")+"&sort_order=bad", 6, 5, 2, 1, 0)
		w := call("GET", who.root+"?"+who.prefix+base+"&sort_by=model&sort_order=asc&page_size=2&page=2", who.token, nil)
		var page struct{ Data pageData }
		json.Unmarshal(w.Body.Bytes(), &page)
		if w.Code != 200 || page.Data.Total != 5 || len(page.Data.Items) != 2 || string(page.Data.Items[0]["id"]) != fmt.Sprint(ids[6]) || string(page.Data.Items[1]["id"]) != fmt.Sprint(ids[1]) {
			t.Fatal("pagination before sorting", w.Code, w.Body.String())
		}
		stats := call("GET", who.root+"/stats?"+query, who.token, nil)
		var result struct{ Data map[string]json.RawMessage }
		json.Unmarshal(stats.Body.Bytes(), &result)
		if stats.Code != 200 || string(result.Data["total_requests"]) != "5" || string(result.Data["total_tokens"]) != "500" || string(result.Data["total_actual_cost"]) != "0.6172839455" || string(result.Data["actual_cost"]) != "0.6172839455" {
			t.Fatal("exact scoped stats", stats.Code, stats.Body.String())
		}
		if who.token == user && result.Data["total_account_cost"] != nil {
			t.Fatal("user stats exposes account cost")
		}
		if who.token == admin {
			var cost json.Number
			if json.Unmarshal(result.Data["total_account_cost"], &cost) != nil || rat(cost).Cmp(rat(json.Number("3"))) != 0 {
				t.Fatal("historical account cost", string(result.Data["total_account_cost"]))
			}
		}
		stats = call("GET", who.root+"/stats?"+query+"&model=alpha&request_type=sync&billing_mode=token", who.token, nil)
		json.Unmarshal(stats.Body.Bytes(), &result)
		if stats.Code != 200 || string(result.Data["total_requests"]) != "2" {
			t.Fatal("list and stats filters diverged", stats.Code, stats.Body.String())
		}
		stats = call("GET", who.root+"/stats?"+who.prefix, who.token, nil)
		json.Unmarshal(stats.Body.Bytes(), &result)
		if stats.Code != 200 || string(result.Data["total_requests"]) != "1" {
			t.Fatal("default stats period includes old usage", stats.Code, stats.Body.String())
		}
		for _, invalid := range []string{"request_type=bad", "stream=bad", "native_compaction_v2=bad", "billing_mode=bad", "billing_type=256", "group_id=-1", "api_key_id=bad", "timezone=bad", "start_date=bad", "start_date=2026-04-01&end_date=2026-03-01"} {
			for _, suffix := range []string{"", "/stats"} {
				if w := call("GET", who.root+suffix+"?"+invalid, who.token, nil); w.Code != 400 {
					t.Fatal("invalid filter accepted", invalid, w.Code)
				}
			}
		}
	}
	scoped := base + fmt.Sprintf("&user_id=%d&sort_by=id&sort_order=asc", uid)
	check("/api/v1/admin/usage", admin, scoped+fmt.Sprintf("&account_id=%d", aid2), 1, 6)
	check("/api/v1/admin/usage", admin, scoped+"&request_id=usage-query-1", 1)
	check("/api/v1/admin/usage", admin, scoped+"&upstream_model_mismatch=true", 1)
	check("/api/v1/admin/usage", admin, scoped+"&upstream_model_mismatch=false", 0, 6)
	check("/api/v1/admin/usage", admin, scoped+"&exact_total=false", 0, 1, 2, 5, 6)
	for _, q := range []string{"account_id=bad", "user_id=-1", "exact_total=bad", "upstream_model_mismatch=bad"} {
		if w := call("GET", "/api/v1/admin/usage?"+q, admin, nil); w.Code != 400 {
			t.Fatal("invalid admin query", q, w.Code)
		}
	}
	check("/api/v1/usage", user, base+"&user_id=1&account_id=bad&upstream_model_mismatch=bad&sort_by=id&sort_order=asc", 0, 1, 2, 5, 6)
	for _, suffix := range []string{"", "/stats"} {
		if w := call("GET", "/api/v1/usage"+suffix+fmt.Sprintf("?api_key_id=%d", foreign), user, nil); w.Code != 403 {
			t.Fatal("foreign Key filter", w.Code)
		}
		if w := call("GET", "/api/v1/admin/usage"+suffix, user, nil); w.Code != 403 {
			t.Fatal("admin usage authorization", w.Code)
		}
		if w := call("GET", "/api/v1/usage"+suffix, key["key"].(string), nil); w.Code != 401 {
			t.Fatal("API Key used as panel login", w.Code)
		}
	}
	// Both list and detail expose the user's IP and compaction flag without provider identities.
	for _, path := range []string{"/api/v1/usage?" + base, "/api/v1/usage/" + fmt.Sprint(ids[5])} {
		w := call("GET", path, user, nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"ip_address":"192.0.2.241"`) || !strings.Contains(w.Body.String(), `"native_compaction_v2":true`) {
			t.Fatal("user usage fields", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"model":"alpha"`) || !strings.Contains(w.Body.String(), `"request_type":"ws_v2"`) {
			t.Fatal("list/detail projection mismatch", w.Body.String())
		}
		for _, hidden := range []string{"account_id", "account_stats_cost", "account_rate_multiplier", "upstream_model", "channel_id", "billing_tier"} {
			if strings.Contains(w.Body.String(), `"`+hidden+`"`) {
				t.Fatal("private usage field", hidden)
			}
		}
	}
	if w := call("GET", "/api/v1/usage/"+fmt.Sprint(ids[0]), other, nil); w.Code != 404 {
		t.Fatal("foreign usage detail", w.Code)
	}
	// Relations are scoped from usage, and preserve client/effective effort separately.
	if _, err := a.DB.Exec("UPDATE usage_logs SET requested_reasoning_effort=' max ',reasoning_effort='high' WHERE id=$1", ids[0]); err != nil {
		t.Fatal(err)
	}
	manage("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"notes": "private usage note"})
	manage("PUT", fmt.Sprintf("/api/v1/keys/%d", kid), user, map[string]any{"group_id": gid2})
	for _, who := range []struct{ root, token, query string }{{"/api/v1/usage", user, base}, {"/api/v1/admin/usage", admin, scoped}} {
		page := check(who.root, who.token, who.query+"&sort_by=id&sort_order=asc", 0, 1, 2, 5, 6)
		row := page.Items[0]
		var profile, apiKey, group, account map[string]any
		json.Unmarshal(row["user"], &profile)
		json.Unmarshal(row["api_key"], &apiKey)
		json.Unmarshal(row["group"], &group)
		if profile["id"] != float64(uid) || profile["email"] != "usage-queries@example.test" || profile["notes"] != nil || profile["password_hash"] != nil ||
			apiKey["id"] != float64(kid) || apiKey["name"] != "usage queries" || apiKey["group_id"] != float64(gid2) || apiKey["key"] != nil || group["id"] != float64(gid) ||
			string(row["reasoning_effort"]) != `"max"` || group["model_routing"] != nil || group["account_count"] != nil {
			t.Fatal("usage relations or effort", row)
		}
		if who.root == "/api/v1/admin/usage" {
			json.Unmarshal(row["account"], &account)
			if len(account) != 2 || account["id"] != float64(aid) || account["name"] != "Usage query source" || string(row["upstream_reasoning_effort"]) != `"high"` {
				t.Fatal("administrator usage metadata", row)
			}
		} else {
			if row["account"] != nil || row["upstream_reasoning_effort"] != nil {
				t.Fatal("user saw upstream effort or account")
			}
			w := call("GET", fmt.Sprintf("/api/v1/usage/%d", ids[0]), user, nil)
			var detail struct{ Data map[string]json.RawMessage }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || !reflect.DeepEqual(row, detail.Data) {
				t.Fatal("usage list/detail relations diverged", w.Code)
			}
		}
	}
	var originalEffort bool
	if err := a.DB.QueryRow("SELECT requested_reasoning_effort=' max ' AND reasoning_effort='high' FROM usage_logs WHERE id=$1", ids[0]).Scan(&originalEffort); err != nil || !originalEffort {
		t.Fatal("usage projection rewrote effort", err)
	}
	var unchanged bool
	if err := a.DB.QueryRow("SELECT model='billed-z' AND request_type=3 AND requested_model=' alpha ' FROM usage_logs WHERE id=$1", ids[5]).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("query rewrote raw usage", err)
	}
	// Soft deletion leaves raw account/Key usage queryable by its owner and administrators.
	manage("DELETE", "/api/v1/keys/"+fmt.Sprint(kid), user, nil)
	manage("DELETE", "/api/v1/admin/accounts/"+fmt.Sprint(aid), admin, nil)
	manage("DELETE", "/api/v1/admin/groups/"+fmt.Sprint(gid), admin, nil)
	page := check("/api/v1/usage", user, base+"&sort_by=id&sort_order=asc", 0, 1, 2, 5, 6)
	if string(page.Items[0]["api_key"]) != "null" || string(page.Items[0]["group"]) != "null" {
		t.Fatal("deleted Key/group relationship retained", page.Items[0])
	}
	page = check("/api/v1/admin/usage", admin, scoped+fmt.Sprintf("&api_key_id=%d&account_id=%d", kid, aid), 0, 2, 5)
	if string(page.Items[0]["account"]) != "null" || string(page.Items[0]["group_id"]) != fmt.Sprint(gid) || string(page.Items[0]["api_key_id"]) != fmt.Sprint(kid) {
		t.Fatal("deleted account or historical identity", page.Items[0])
	}
	if w := call("GET", "/api/v1/usage?api_key_id="+fmt.Sprint(kid), user, nil); w.Code != 404 {
		t.Fatal("deleted Key explicit lookup", w.Code)
	}
	manage("DELETE", "/api/v1/admin/users/"+fmt.Sprint(uid), admin, nil)
	page = check("/api/v1/admin/usage", admin, scoped+fmt.Sprintf("&api_key_id=%d", kid), 0, 2, 5)
	var deletedUser map[string]any
	json.Unmarshal(page.Items[0]["user"], &deletedUser)
	if deletedUser["id"] != float64(uid) || deletedUser["deleted_at"] == nil || deletedUser["notes"] != nil {
		t.Fatal("deleted usage owner metadata", deletedUser)
	}
}
