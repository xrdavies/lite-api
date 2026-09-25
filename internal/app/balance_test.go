package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testBalanceHistory(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.208:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var result struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return result.Data
	}
	user := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "balance-history@example.test", "password": "history-password", "balance": json.Number("1.00000001")})
	uid := int64(user["id"].(float64))
	path := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	ordinary := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "balance-history@example.test", "password": "history-password"})["access_token"].(string)
	key := must("POST", "/api/v1/keys", ordinary, map[string]any{"name": "History"})["key"].(string)
	for range 2 {
		w := call("POST", path+"/balance", admin, map[string]any{"operation": "add", "balance": json.Number("2.00000002"), "notes": "precise credit"}, "history-credit")
		if w.Code != 200 {
			t.Fatal("balance adjustment", w.Code, w.Body.String())
		}
	}
	must("POST", path+"/balance", admin, map[string]any{"operation": "subtract", "balance": json.Number("0.50000003"), "notes": "debit"})
	must("POST", path+"/balance", admin, map[string]any{"operation": "set", "balance": json.Number("3.25000001"), "notes": "set balance"})
	must("POST", path+"/balance", admin, map[string]any{"operation": "set", "balance": json.Number("3.25000001")})
	must("PUT", path, admin, map[string]any{"concurrency": 7})
	must("PUT", path, admin, map[string]any{"concurrency": 7})
	type record struct {
		ID                 int64
		Code, Type, Status string
		Value              json.Number
		Notes              string
		GroupID            *int64 `json:"group_id"`
		UsedBy             int64  `json:"used_by"`
		Validity           int64  `json:"validity_days"`
	}
	type history struct {
		Items              []record
		Total, Page, Pages int
		Size               int         `json:"page_size"`
		Recharged          json.Number `json:"total_recharged"`
	}
	list := func(target, query string) history {
		t.Helper()
		w := call("GET", target+"/balance-history"+query, admin, nil, "")
		var result struct{ Data history }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Data.Items == nil {
			t.Fatalf("balance history: %d %s", w.Code, w.Body.String())
		}
		return result.Data
	}
	check := func(query string, total int, recharged string, ids ...int64) history {
		t.Helper()
		h := list(path, query)
		if h.Total != total || len(h.Items) != len(ids) || h.Recharged == "" || rat(h.Recharged).Cmp(rat(json.Number(recharged))) != 0 {
			t.Fatalf("%s: total=%d recharged=%s items=%v want=%v", query, h.Total, h.Recharged, h.Items, ids)
		}
		for i, row := range h.Items {
			if row.ID != ids[i] || row.UsedBy != uid || row.Code == "" || row.Status != "used" || row.Validity != 30 || row.GroupID != nil {
				t.Fatalf("history order/ledger DTO: %+v want id=%d owner=%d", row, ids[i], uid)
			}
		}
		return h
	}
	h := list(path, "")
	if h.Total != 5 || len(h.Items) != 5 || h.Recharged != "3.75000004" {
		t.Fatal("initial/add/subtract/set/concurrency history", h)
	}
	ids := make([]int64, len(h.Items))
	for i, row := range h.Items {
		ids[i] = row.ID
	}
	for i, want := range []struct{ value, notes string }{{"2", "Concurrency adjustment"}, {"0.75000001", "set balance"}, {"-0.50000003", "debit"}, {"2.00000002", "precise credit"}, {"1.00000001", "Initial balance"}} {
		if rat(h.Items[i].Value).Cmp(rat(json.Number(want.value))) != 0 || h.Items[i].Notes != want.notes {
			t.Fatal("history changed original amount or notes", h.Items[i], want)
		}
	}
	check("?type=admin_balance", 4, "3.75000004", ids[1:]...)
	check("?type=admin_concurrency", 1, "3.75000004", ids[0])
	for _, codeType := range []string{"balance", "concurrency", "subscription", "affiliate_balance", "unknown", "' OR true --"} {
		check("?type="+url.QueryEscape(codeType), 0, "3.75000004")
	}
	if h := check("?page_size=2&page=2", 5, "3.75000004", ids[2:4]...); h.Page != 2 || h.Size != 2 || h.Pages != 3 {
		t.Fatal("history pagination", h)
	}
	check("?page=9", 5, "3.75000004")
	// Explicitly shuffle timestamps: unfiltered history falls back to creation
	// time, while a type filter keeps the ledger's used_at DESC NULL ordering.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range ids {
		if _, err := a.DB.Exec("UPDATE redeem_codes SET used_at=$2,created_at=$2 WHERE id=$1", id, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.DB.Exec("UPDATE redeem_codes SET used_at=NULL,notes=NULL WHERE id=$1", ids[1]); err != nil {
		t.Fatal(err)
	}
	check("", 5, "3.75000004", ids[4], ids[3], ids[2], ids[1], ids[0])
	if got := check("?type=admin_balance", 4, "3.75000004", ids[1], ids[4], ids[3], ids[2]); got.Items[0].Notes != "" {
		t.Fatal("NULL ledger notes did not normalize to empty text")
	}
	if _, err := a.DB.Exec("UPDATE redeem_codes SET used_at=$2 WHERE id=$1", ids[2], base.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check("?type=admin_balance&page_size=3", 4, "3.75000004", ids[1], ids[4], ids[2])
	// Excluded products and other users must not inflate the internal total.
	for _, codeType := range []string{"balance", "concurrency", "subscription", "affiliate_balance"} {
		if _, err := a.DB.Exec("INSERT INTO redeem_codes(code,type,value,status,used_by,used_at) VALUES($1,$2,999,'used',$3,now())", randomToken(24), codeType, uid); err != nil {
			t.Fatal(err)
		}
	}
	check("?type=admin_balance", 4, "3.75000004", ids[1], ids[4], ids[2], ids[3])
	emptyUser := must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "empty-history@example.test", "password": "empty-history-password"})
	emptyID := int64(emptyUser["id"].(float64))
	for _, target := range []string{"9223372036854775807", fmt.Sprint(emptyID)} {
		empty := list("/api/v1/admin/users/"+target, "")
		if empty.Total != 0 || len(empty.Items) != 0 || empty.Recharged != "0" || empty.Pages != 1 {
			t.Fatal("history scope or empty result", target, empty)
		}
	}
	otherPath := fmt.Sprintf("/api/v1/admin/users/%d", emptyID)
	must("POST", otherPath+"/balance", admin, map[string]any{"operation": "add", "balance": 123})
	check("?type=admin_balance", 4, "3.75000004", ids[1], ids[4], ids[2], ids[3])
	if got := list(otherPath, ""); got.Total != 1 || got.Recharged != "123.00000000" {
		t.Fatal("other user's history", got)
	}
	for _, query := range []string{"?type=%00", "?type=%FF", "?type=" + strings.Repeat("a", 21)} {
		if w := call("GET", path+"/balance-history"+query, admin, nil, ""); w.Code != 400 {
			t.Fatal("invalid history type", query, w.Code)
		}
	}
	for _, auth := range []struct {
		token  string
		status int
	}{{ordinary, 403}, {key, 401}, {"", 401}} {
		if w := call("GET", path+"/balance-history", auth.token, nil, ""); w.Code != auth.status {
			t.Fatal("history authorization", w.Code, auth.status)
		}
	}
	var preciseID int64
	if err := a.DB.QueryRow("INSERT INTO redeem_codes(code,type,value,status,used_by,used_at) VALUES($1,'admin_balance',999999999999.12345678,'used',$2,now()) RETURNING id", randomToken(24), uid).Scan(&preciseID); err != nil {
		t.Fatal(err)
	}
	check("?type=admin_concurrency", 1, "1000000000002.87345682", ids[0])
	if _, err := a.DB.Exec("DELETE FROM redeem_codes WHERE id=$1", preciseID); err != nil {
		t.Fatal(err)
	}
	// Snapshot count, aggregate and page must agree during concurrent changes.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		for ctx.Err() == nil {
			var id int64
			if err := a.DB.QueryRow("INSERT INTO redeem_codes(code,type,value,status,used_by,used_at) VALUES($1,'admin_balance',999999999999.12345678,'used',$2,now()) RETURNING id", randomToken(24), uid).Scan(&id); err != nil {
				done <- err
				return
			}
			if _, err := a.DB.Exec("DELETE FROM redeem_codes WHERE id=$1", id); err != nil {
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
		h := list(path, "?page_size=100")
		sum := rat(json.Number("0"))
		for _, row := range h.Items {
			if row.Type == "admin_balance" && rat(row.Value).Sign() > 0 {
				sum.Add(sum, rat(row.Value))
			}
		}
		if h.Total != len(h.Items) || h.Total < 5 || h.Total > 6 || rat(h.Recharged).Cmp(sum) != 0 {
			t.Fatal("history count/amount/page snapshot mismatch", h)
		}
	}
	cancel()
	// Soft deletion must not erase the administrator's historical ledger.
	must("DELETE", path, admin, nil)
	h = list(path, "?type=admin_concurrency")
	if h.Total != 1 || len(h.Items) != 1 || h.Items[0].ID != ids[0] {
		t.Fatal("deleted user's history lost", h)
	}
}
