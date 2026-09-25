package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func testGroupChannelQueries(t *testing.T, a *App, admin, user string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.243:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	const groups = "/api/v1/admin/groups"
	const channels = "/api/v1/admin/channels"
	gids := []int64{}
	for i, row := range []struct {
		name, platform, status string
		order, rate            int
		exclusive              bool
	}{
		{"zulu", "openai", "active", 20, 1, false},
		{"alpha", "openai", "active", 10, 3, true},
		{"beta", "openai", "inactive", 10, 2, false},
		{"gamma", "composite", "active", 30, 0, true},
	} {
		v := must("POST", groups, map[string]any{"name": "query-group-" + row.name, "platform": row.platform, "status": row.status, "sort_order": row.order, "rate_multiplier": row.rate, "is_exclusive": row.exclusive})
		gid := int64(v["id"].(float64))
		gids = append(gids, gid)
		defer must("DELETE", fmt.Sprintf("%s/%d", groups, gid), nil)
		if v["account_count"] != float64(0) || v["active_account_count"] != float64(0) || v["rate_limited_account_count"] != float64(0) {
			t.Fatal("empty group counts", v)
		}
		exec("UPDATE groups SET created_at='2026-01-01'::timestamptz+$2*interval '1 hour' WHERE id=$1", gid, i)
	}
	must("PUT", fmt.Sprintf("%s/%d", groups, gids[0]), map[string]any{"description": `unique_%\描述`})
	check := func(root, query string, total int, want ...int64) []map[string]any {
		t.Helper()
		prefix := "query-group-"
		if root == channels {
			prefix = "query-channel-"
		}
		v := must("GET", root+"?search="+prefix+"&"+query, nil)
		got := []int64{}
		rows := []map[string]any{}
		for _, item := range v["items"].([]any) {
			row := item.(map[string]any)
			got = append(got, int64(row["id"].(float64)))
			rows = append(rows, row)
		}
		if v["total"] != float64(total) || !slices.Equal(got, want) {
			t.Fatalf("%s %s: total=%v rows=%v want=%v", root, query, v["total"], got, want)
		}
		return rows
	}
	check(groups, "", 4, gids[1], gids[2], gids[0], gids[3])
	check(groups, "is_exclusive=true&sort_by=rate_multiplier&sort_order=desc", 2, gids[1], gids[3])
	check(groups, "is_exclusive=false&status=active", 1, gids[0])
	check(groups, "platform=composite", 1, gids[3])
	check(groups, "platform=unavailable", 0)
	check(groups, "sort_by=name&sort_order=desc&page_size=1&page=2", 4, gids[3])
	check(groups, "sort_by=created_at&sort_order=desc", 4, gids[3], gids[2], gids[1], gids[0])
	check(groups, "sort_by=billing_type&sort_order=desc", 4, gids[3], gids[2], gids[1], gids[0])
	check(groups, "sort_by="+url.QueryEscape("id;DROP TABLE groups")+"&sort_order=invalid", 4, gids[1], gids[2], gids[0], gids[3])
	if v := must("GET", groups+"?search="+url.QueryEscape(` UNIQUE_%\描述 `), nil); v["total"] != float64(1) {
		t.Fatal("description literal search", v)
	}
	for _, row := range []struct {
		query string
		want  []int64
	}{
		{"", []int64{gids[1], gids[0], gids[3]}},
		{"include_inactive=true", []int64{gids[1], gids[2], gids[0], gids[3]}},
		{"include_inactive=true&platform=composite", []int64{gids[3]}},
		{"platform=openai&status=inactive", []int64{gids[1], gids[0]}},
	} {
		w := call("GET", groups+"/all?search=query-group-&"+row.query, admin, nil)
		var out struct{ Data []struct{ ID int64 } }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("all groups", w.Code, w.Body.String())
		}
		got := []int64{}
		for _, g := range out.Data {
			got = append(got, g.ID)
		}
		if !slices.Equal(got, row.want) {
			t.Fatal("all group scope", row.query, got)
		}
	}
	// Each scope exclusion and time window changes its own bucket only.
	accounts := []int64{}
	for i := 0; i < 13; i++ {
		associated := []int64{gids[0]}
		if i == 0 {
			associated = append(associated, gids[1], gids[2])
		}
		v := must("POST", "/api/v1/admin/accounts", map[string]any{"name": fmt.Sprintf("group-query-%d", i), "platform": "openai", "type": "apikey", "group_ids": associated, "credentials": map[string]any{"api_key": "group-query-upstream-secret"}})
		aid := int64(v["id"].(float64))
		accounts = append(accounts, aid)
		defer a.DB.Exec("UPDATE accounts SET deleted_at=now() WHERE id=$1", aid)
	}
	for i, clause := range map[int]string{
		0: "extra=jsonb_build_object('quota_limit',1,'quota_used',1)",
		1: "rate_limit_reset_at=now()+interval '1 hour',overload_until=now()+interval '1 hour'",
		2: "overload_until=now()+interval '1 hour'", 3: "temp_unschedulable_until=now()+interval '1 hour'",
		4: "status='inactive'", 5: "schedulable=false", 6: "expires_at=now()-interval '1 hour',auto_pause_on_expired=true",
		7: "expires_at=now()-interval '1 hour',auto_pause_on_expired=false", 8: "deleted_at=now()", 9: "type='oauth'",
		10: "credentials=credentials || '{\"account_mode\":\"plan\"}'::jsonb",
		11: "rate_limit_reset_at=now()-interval '1 hour',overload_until=now()-interval '1 hour',temp_unschedulable_until=now()-interval '1 hour'",
		12: "platform='unsupported'",
	} {
		exec("UPDATE accounts SET "+clause+" WHERE id=$1", accounts[i])
	}
	counts := func(row map[string]any, total, active, limited int) {
		t.Helper()
		if row["account_count"] != float64(total) || row["active_account_count"] != float64(active) || row["rate_limited_account_count"] != float64(limited) {
			t.Fatal("group counts", row["account_count"], row["active_account_count"], row["rate_limited_account_count"])
		}
	}
	counts(must("GET", fmt.Sprintf("%s/%d", groups, gids[0]), nil), 9, 3, 3)
	rows := check(groups, "sort_by=account_count&sort_order=desc", 4, gids[0], gids[1], gids[2], gids[3])
	counts(rows[0], 9, 3, 3)
	check(groups, "sort_by=account_count&sort_order=desc&page_size=1&page=2", 4, gids[1])
	check(groups, "sort_by=account_count&sort_order=asc", 4, gids[3], gids[1], gids[2], gids[0])
	exec("UPDATE accounts SET rate_limit_reset_at=now()-interval '1 hour',overload_until=now()-interval '1 hour',temp_unschedulable_until=now()-interval '1 hour' WHERE id IN ($1,$2,$3)", accounts[1], accounts[2], accounts[3])
	counts(must("PUT", fmt.Sprintf("%s/%d", groups, gids[0]), map[string]any{"description": "changed"}), 9, 6, 0)
	var unchanged bool
	if err := a.DB.QueryRow("SELECT rate_limit_reset_at IS NOT NULL AND overload_until IS NOT NULL AND temp_unschedulable_until IS NOT NULL FROM accounts WHERE id=$1", accounts[1]).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("listing cleared stored cooldown", err)
	}
	w := call("GET", "/api/v1/groups/available", user, nil)
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("account_count")) || bytes.Contains(w.Body.Bytes(), []byte("group-query-upstream-secret")) {
		t.Fatal("internal group counts leaked", w.Code)
	}
	// Channel list and detail share the exact child pricing projection.
	cids := []int64{}
	for i, name := range []string{"zulu", "alpha", "beta"} {
		body := map[string]any{"name": "query-channel-" + name}
		if i == 0 {
			body["description"] = `channel_%\描述`
			body["group_ids"] = []int64{gids[0]}
			body["model_pricing"] = []any{map[string]any{"platform": "openai", "models": []string{"model-a"}, "input_price": "0.000000000123", "intervals": []any{map[string]any{"min_tokens": 0, "input_price": "0.000000000321"}}}}
			body["account_stats_pricing_rules"] = []any{map[string]any{"name": "private cost", "group_ids": []int64{gids[0]}, "pricing": []any{map[string]any{"platform": "openai", "models": []string{"model-a"}, "input_price": "0.0000000001", "intervals": []any{map[string]any{"min_tokens": 0, "input_price": "0.000000000222"}}}}}}
		}
		v := must("POST", channels, body)
		cid := int64(v["id"].(float64))
		cids = append(cids, cid)
		defer must("DELETE", fmt.Sprintf("%s/%d", channels, cid), nil)
		exec("UPDATE channels SET created_at='2026-01-01'::timestamptz+$2*interval '1 hour' WHERE id=$1", cid, 2-i)
	}
	must("PUT", fmt.Sprintf("%s/%d", channels, cids[1]), map[string]any{"status": "disabled"})
	rows = check(channels, "", 3, cids[0], cids[1], cids[2])
	detail := must("GET", fmt.Sprintf("%s/%d", channels, cids[0]), nil)
	if !reflect.DeepEqual(rows[0], detail) || rows[0]["model_pricing"].([]any)[0].(map[string]any)["input_price"] != 0.000000000123 {
		t.Fatal("channel list/detail price snapshot mismatch")
	}
	for _, field := range []string{"group_ids", "model_pricing", "account_stats_pricing_rules"} {
		if len(rows[1][field].([]any)) != 0 {
			t.Fatal("empty channel config", field)
		}
	}
	check(channels, "sort_by=name&sort_order=asc", 3, cids[1], cids[2], cids[0])
	check(channels, "sort_by=name&sort_order=desc&page_size=1&page=2", 3, cids[2])
	check(channels, "sort_by=status&sort_order=asc", 3, cids[0], cids[2], cids[1])
	check(channels, "sort_by=id", 3, cids[2], cids[1], cids[0])
	check(channels, "sort_by="+url.QueryEscape("name;DROP TABLE channels"), 3, cids[0], cids[1], cids[2])
	check(channels, "status=disabled", 1, cids[1])
	check(channels, "status=nonexistent", 0)
	if v := must("GET", channels+"?search="+url.QueryEscape(` CHANNEL_%\描述 `), nil); v["total"] != float64(1) {
		t.Fatal("channel description literal search", v)
	}
	if v := must("GET", channels+"?search=query-channel-&page_size=1000", nil); v["page_size"] != float64(100) {
		t.Fatal("channel page limit", v["page_size"])
	}
	for _, root := range []string{groups, channels, groups + "/all"} {
		if w := call("GET", root, user, nil); w.Code != 403 {
			t.Fatal("ordinary user read administration", root, w.Code)
		}
		if w := call("GET", root, "", nil); w.Code != 401 {
			t.Fatal("anonymous read administration", root, w.Code)
		}
		if w := call("GET", root+"?search="+strings.Repeat("x", 101), admin, nil); w.Code != 400 {
			t.Fatal("unbounded search", root, w.Code)
		}
	}
	for _, path := range []string{groups + "?is_exclusive=bad", groups + "/all?include_inactive=bad"} {
		if w := call("GET", path, admin, nil); w.Code != 400 {
			t.Fatal("invalid boolean filter", path, w.Code)
		}
	}
	deleted := int64(must("POST", groups, map[string]any{"name": "query-group-deleted"})["id"].(float64))
	must("DELETE", fmt.Sprintf("%s/%d", groups, deleted), nil)
	check(groups, "sort_by=id", 4, gids[0], gids[1], gids[2], gids[3])
	if w := call("GET", fmt.Sprintf("%s/%d", groups, deleted), admin, nil); w.Code != 404 {
		t.Fatal("deleted group visible", w.Code)
	}
}

func testGroupReplacement(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.244:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	gids := []int64{}
	for _, name := range []string{"old", "new", "unrelated"} {
		group := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "replacement-" + name, "is_exclusive": true})
		gid := int64(group["id"].(float64))
		gids = append(gids, gid)
		defer exec("UPDATE groups SET deleted_at=now() WHERE id=$1", gid)
	}
	old, target, unrelated := gids[0], gids[1], gids[2]
	user := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "replace-group@example.test", "password": "replace-password", "balance": 12.34567891, "allowed_groups": []int64{old, unrelated}})
	uid := int64(user["id"].(float64))
	path := fmt.Sprintf("/api/v1/admin/users/%d/replace-group", uid)
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "replace-group@example.test", "password": "replace-password"})["access_token"].(string)
	keys := []int64{}
	for i, gid := range []int64{old, old, old, unrelated} {
		key := must("POST", "/api/v1/keys", token, map[string]any{"name": fmt.Sprint("replacement-", i), "group_id": gid})
		keys = append(keys, int64(key["id"].(float64)))
	}
	exec("UPDATE api_keys SET status='inactive' WHERE id=$1", keys[1])
	must("DELETE", fmt.Sprintf("/api/v1/keys/%d", keys[2]), token, nil)
	exec("UPDATE api_keys SET quota_used=1.23456789,usage_5h=0.1,usage_1d=0.2,usage_7d=0.3,window_5h_start=now(),window_1d_start=now(),window_7d_start=now() WHERE user_id=$1", uid)
	exec("INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) VALUES($1,$2,0.75)", uid, old)
	snapshot := func(includeAssignment bool) string {
		t.Helper()
		projection := "to_jsonb(k)-'updated_at'"
		if !includeAssignment {
			projection += "-'group_id'"
		}
		var raw string
		err := a.DB.QueryRow(`SELECT jsonb_build_object('keys',(SELECT jsonb_agg(`+projection+` ORDER BY id) FROM api_keys k WHERE user_id=$1),
 'balance',balance,'rates',(SELECT jsonb_agg(to_jsonb(m) ORDER BY group_id) FROM user_group_rate_multipliers m WHERE user_id=$1))::text FROM users WHERE id=$1`, uid).Scan(&raw)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	assignments := func(wantKeys, wantGroups []int64) {
		t.Helper()
		var raw []byte
		if err := a.DB.QueryRow(`SELECT jsonb_build_object('keys',(SELECT jsonb_agg(group_id ORDER BY id) FROM api_keys WHERE user_id=$1),
 'groups',(SELECT jsonb_agg(group_id ORDER BY group_id) FROM user_allowed_groups WHERE user_id=$1))`, uid).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var got struct{ Keys, Groups []int64 }
		if json.Unmarshal(raw, &got) != nil || !slices.Equal(got.Keys, wantKeys) || !slices.Equal(got.Groups, wantGroups) {
			t.Fatal("group assignment mismatch", string(raw))
		}
	}
	body := map[string]int64{"old_group_id": old, "new_group_id": target}
	original, preserved := snapshot(true), snapshot(false)
	for _, identity := range []struct {
		token string
		code  int
	}{{"", 401}, {token, 403}} {
		if w := call("POST", path, identity.token, body); w.Code != identity.code {
			t.Fatal("replacement authorization", w.Code)
		}
	}
	for _, invalid := range []map[string]int64{{"old_group_id": old, "new_group_id": old}, {"old_group_id": 0, "new_group_id": target}} {
		if w := call("POST", path, admin, invalid); w.Code != 400 {
			t.Fatal("invalid replacement", w.Code)
		}
	}
	for _, change := range []string{"is_exclusive=false", "status='inactive'", "subscription_type='subscription'", "require_oauth_only=true", "platform='unsupported'", "deleted_at=now()"} {
		exec("UPDATE groups SET "+change+" WHERE id=$1", target)
		want := 400
		if change == "deleted_at=now()" {
			want = 404
		}
		if w := call("POST", path, admin, body); w.Code != want {
			t.Fatal("ineligible target", change, w.Code, w.Body.String())
		}
		exec("UPDATE groups SET is_exclusive=true,status='active',subscription_type='standard',require_oauth_only=false,platform='openai',deleted_at=NULL WHERE id=$1", target)
	}
	// A failed Key update must also roll back the newly granted permission.
	exec(fmt.Sprintf("ALTER TABLE api_keys ADD CONSTRAINT test_group_replacement CHECK(group_id<>%d) NOT VALID", target))
	defer a.DB.Exec("ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS test_group_replacement")
	if w := call("POST", path, admin, body); w.Code != 500 {
		t.Fatal("replacement SQL failure", w.Code, w.Body.String())
	}
	exec("ALTER TABLE api_keys DROP CONSTRAINT test_group_replacement")
	assignments([]int64{old, old, old, unrelated}, []int64{old, unrelated})
	if snapshot(true) != original {
		t.Fatal("rejected replacement changed Key or financial state")
	}
	// Observe a real row-lock wait, then commit an incompatible target change.
	tx, err := a.DB.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var pid int
	if err = tx.QueryRow("SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE groups SET is_exclusive=false WHERE id=$1", target); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", path, admin, body) }()
	blocked := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if err = a.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("replacement did not lock target eligibility")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != 400 {
			t.Fatal("concurrent target change accepted", w.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement remained blocked")
	}
	exec("UPDATE groups SET is_exclusive=true WHERE id=$1", target)
	if v := must("POST", path, admin, body); v["migrated_keys"] != float64(2) {
		t.Fatal("replacement count", v)
	}
	assignments([]int64{target, target, old, unrelated}, []int64{target, unrelated})
	if snapshot(false) != preserved {
		t.Fatal("replacement changed Key settings, counters or balances")
	}
	if v := must("POST", path, admin, body); v["migrated_keys"] != float64(0) {
		t.Fatal("replacement repeated migration", v)
	}
	if w := call("POST", "/api/v1/keys", token, map[string]any{"name": "revoked", "group_id": old}); w.Code != 403 {
		t.Fatal("old group still authorized", w.Code)
	}
	must("POST", "/api/v1/keys", token, map[string]any{"name": "granted", "group_id": target})
	must("DELETE", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, nil)
	if w := call("POST", path, admin, body); w.Code != 404 {
		t.Fatal("deleted user replacement", w.Code)
	}
}
