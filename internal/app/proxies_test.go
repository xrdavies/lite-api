package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func testProxyQueries(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	const root = "/api/v1/admin/proxies"
	const prefix = "proxy-query "
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.205:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	object := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
		return out.Data
	}
	ids := []int64{}
	for i, protocol := range []string{"http", "socks5", "https", "http"} {
		name := prefix + string(rune('A'+i))
		if i == 0 {
			name += "_%"
		}
		p := object(call("POST", root, admin, map[string]any{"name": name, "protocol": protocol, "host": "127.0.0.1", "port": 29100 + i, "password": "proxy-query-secret"}))
		ids = append(ids, int64(p["id"].(float64)))
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range ids[:3] {
		exec("UPDATE proxies SET created_at='2026-01-01'::timestamptz+$2*interval '1 day' WHERE id=$1", id, 3-i)
	}
	exec("UPDATE proxies SET status='inactive',expires_at='2027-06-05'::timestamptz WHERE id=$1", ids[1])
	exec("UPDATE proxies SET expires_at='2027-06-02'::timestamptz WHERE id=$1", ids[2])
	object(call("DELETE", fmt.Sprintf("%s/%d", root, ids[3]), admin, nil))
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO accounts(name,platform,type,credentials,proxy_id,status,notes,deleted_at)
VALUES($1,'openai','apikey','{"api_key":"account-query-secret"}'::jsonb,$2,$3,$4,CASE WHEN $5 THEN now() END)`, fmt.Sprintf("proxy-query-account-%d", i), ids[0], []string{"active", "inactive", "active"}[i], fmt.Sprintf("proxy-account-note-%d", i), i == 2)
	}
	defer func() {
		a.DB.Exec("UPDATE accounts SET deleted_at=now() WHERE name LIKE 'proxy-query-account-%'")
		a.DB.Exec("UPDATE proxies SET deleted_at=now() WHERE name LIKE 'proxy-query %'")
	}()
	read := func(path string, all bool) ([]map[string]any, int) {
		t.Helper()
		w := call("GET", path, admin, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "query-secret") || strings.Contains(w.Body.String(), `"password":`) || strings.Contains(w.Body.String(), `"credentials":`) {
			t.Fatalf("invalid or unsafe proxy query: %d %s", w.Code, w.Body.String())
		}
		var out struct{ Data json.RawMessage }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var page struct {
			Items []map[string]any
			Total int
		}
		if all {
			if err := json.Unmarshal(out.Data, &page.Items); err != nil {
				t.Fatal(err)
			}
			page.Total = len(page.Items)
		} else if err := json.Unmarshal(out.Data, &page); err != nil {
			t.Fatal(err)
		}
		return page.Items, page.Total
	}
	list := root + "?search=" + url.QueryEscape(prefix)
	for _, tc := range []struct {
		field, direction string
		order            []int
	}{
		{"id", "asc", []int{0, 1, 2}},
		{"name", "asc", []int{0, 1, 2}},
		{"protocol", "asc", []int{0, 2, 1}},
		{"status", "desc", []int{1, 2, 0}},
		{"created_at", "asc", []int{2, 1, 0}},
		{"expiry", "asc", []int{2, 1, 0}},
		{"expiry", "desc", []int{0, 1, 2}},
		{"account_count", "asc", []int{2, 1, 0}},
		{"account_count", "desc", []int{0, 2, 1}},
		{"id;DROP TABLE proxies", "invalid", []int{2, 1, 0}},
	} {
		for index, wanted := range tc.order {
			path := list + "&sort_by=" + url.QueryEscape(tc.field) + "&sort_order=" + tc.direction + fmt.Sprintf("&page_size=1&page=%d", index+1)
			rows, total := read(path, false)
			if total != 3 || len(rows) != 1 || int64(rows[0]["id"].(float64)) != ids[wanted] || rows[0]["has_password"] != true {
				t.Fatal("sorting must precede pagination", tc, index, total, rows)
			}
			wantCount := float64(0)
			if wanted == 0 {
				wantCount = 2
			}
			if rows[0]["account_count"] != wantCount {
				t.Fatal("proxy account count", rows)
			}
		}
	}
	for _, search := range []string{"proxy-query A_%", "_%", "%", "  PROXY-QUERY A  "} {
		rows, total := read(root+"?search="+url.QueryEscape(search), false)
		if total != 1 || int64(rows[0]["id"].(float64)) != ids[0] {
			t.Fatal("search is not a case-insensitive literal substring", search, rows)
		}
	}
	rows, total := read(list+"&status=inactive&protocol=socks5", false)
	if total != 1 || int64(rows[0]["id"].(float64)) != ids[1] {
		t.Fatal("combined filters", rows)
	}
	for _, withCount := range []string{"", "&with_count=true"} {
		rows, _ = read(root+"/all?search="+url.QueryEscape(prefix)+"&status=inactive"+withCount, true)
		got := []int64{}
		for _, row := range rows {
			got = append(got, int64(row["id"].(float64)))
		}
		if !reflect.DeepEqual(got, []int64{ids[0], ids[2]}) {
			t.Fatal("all proxies must be active and newest first", got)
		}
	}
	accountsPath := fmt.Sprintf("%s/%d/accounts", root, ids[0])
	rows, total = read(accountsPath, true)
	if total != 2 || rows[0]["name"] != "proxy-query-account-1" || rows[0]["notes"] != "proxy-account-note-1" || rows[0]["type"] != "apikey" || rows[0]["status"] != "inactive" {
		t.Fatal("incomplete proxy account summary", rows)
	}
	for _, path := range []string{list, root + "/all", accountsPath} {
		for _, tc := range []struct {
			token string
			code  int
		}{{ordinary, 403}, {"", 401}} {
			if w := call("GET", path, tc.token, nil); w.Code != tc.code {
				t.Fatal("proxy management access", path, w.Code)
			}
		}
	}
	for _, query := range []string{strings.Repeat("a", 101), "bad\x00search"} {
		if w := call("GET", root+"?search="+url.QueryEscape(query), admin, nil); w.Code != 400 {
			t.Fatal("proxy search bound", w.Code)
		}
	}
}
