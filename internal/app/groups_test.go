package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
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
