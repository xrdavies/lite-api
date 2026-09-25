package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

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
	var unchanged bool
	if err := a.DB.QueryRow("SELECT model='billed-z' AND request_type=3 AND requested_model=' alpha ' FROM usage_logs WHERE id=$1", ids[5]).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("query rewrote raw usage", err)
	}
	// Soft deletion leaves raw account/Key usage queryable by its owner and administrators.
	manage("DELETE", "/api/v1/keys/"+fmt.Sprint(kid), user, nil)
	manage("DELETE", "/api/v1/admin/accounts/"+fmt.Sprint(aid), admin, nil)
	check("/api/v1/usage", user, base+"&sort_by=id&sort_order=asc", 0, 1, 2, 5, 6)
	check("/api/v1/admin/usage", admin, scoped+fmt.Sprintf("&api_key_id=%d&account_id=%d", kid, aid), 0, 2, 5)
	if w := call("GET", "/api/v1/usage?api_key_id="+fmt.Sprint(kid), user, nil); w.Code != 404 {
		t.Fatal("deleted Key explicit lookup", w.Code)
	}
}
