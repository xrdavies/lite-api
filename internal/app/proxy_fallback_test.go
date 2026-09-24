package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func testProxyFallback(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	const proxies, accounts = "/api/v1/admin/proxies/", "/api/v1/admin/accounts/"
	var calls [3]atomic.Int32
	servers := make([]*httptest.Server, 3)
	for i := range servers {
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			if r.Header.Get("Authorization") != "Bearer fallback-key" || i == 2 && r.Header.Get("Proxy-Authorization") != "" {
				t.Error("credential isolation")
			}
			if r.URL.Path == "/v1/responses" {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"session.created"}`))
				return
			}
			fmt.Fprint(w, `{"data":[]}`)
		}))
		defer servers[i].Close()
	}
	newProxy := func(name string, index int, mode string, backup *int64) int64 {
		t.Helper()
		host, port, _ := net.SplitHostPort(strings.TrimPrefix(servers[index].URL, "http://"))
		n, _ := strconv.Atoi(port)
		out := manage("POST", strings.TrimSuffix(proxies, "/"), map[string]any{"name": name, "protocol": "http", "host": host, "port": n, "username": "proxy-user", "password": "proxy-secret", "fallback_mode": mode, "backup_proxy_id": backup})
		return int64(out["id"].(float64))
	}
	backup := newProxy("Fallback backup", 1, "direct", nil)
	primary := newProxy("Fallback primary", 0, "proxy", &backup)
	var ids []int64
	defer func() {
		for _, id := range ids {
			exec("UPDATE accounts SET deleted_at=now() WHERE id=$1", id)
		}
	}()
	newAccount := func(proxy int64) int64 {
		t.Helper()
		out := manage("POST", strings.TrimSuffix(accounts, "/"), map[string]any{"name": "Proxy expiry test", "platform": "openai", "type": "apikey", "proxy_id": proxy, "credentials": map[string]any{"base_url": servers[2].URL, "api_key": "fallback-key"}})
		id := int64(out["id"].(float64))
		ids = append(ids, id)
		return id
	}
	id := newAccount(primary)
	path, primaryPath := accounts+fmt.Sprint(id), proxies+fmt.Sprint(primary)
	request := func(accountID int64, want int) {
		t.Helper()
		u, err := a.loadAccount(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		before := calls[want].Load()
		resp, err := a.upstreamRequest(ctx, u, "GET", "/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || calls[want].Load() != before+1 {
			t.Fatal("wrong proxy destination", want, resp.StatusCode)
		}
	}
	socket := func(accountID int64, want int) {
		t.Helper()
		u, err := a.loadAccount(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		before := calls[want].Load()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		conn, _, err := a.dialUpstreamSocket(ctx, u, "/v1/responses", http.Header{"Authorization": []string{"Bearer fallback-key"}})
		if err != nil {
			t.Fatal("WebSocket fallback handshake", err)
		}
		defer conn.CloseNow()
		_, raw, err := conn.Read(ctx)
		if err != nil || string(raw) != `{"type":"session.created"}` || calls[want].Load() != before+1 {
			t.Fatal("WebSocket routed incorrectly", string(raw), err)
		}
	}
	state := func(accountID int64, wantProxy, wantOrigin *int64) {
		t.Helper()
		var proxy, origin *int64
		if err := a.DB.QueryRow("SELECT proxy_id,proxy_fallback_origin_id FROM accounts WHERE id=$1", accountID).Scan(&proxy, &origin); err != nil {
			t.Fatal(err)
		}
		equal := func(a, b *int64) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		if !equal(proxy, wantProxy) || !equal(origin, wantOrigin) {
			t.Fatalf("unexpected proxy binding: %v %v, wanted %v %v", proxy, origin, wantProxy, wantOrigin)
		}
	}
	seed := func() {
		exec(`UPDATE accounts SET extra=extra || '{"upstream_billing_probe":{"status":"ok"},"upstream_model_metadata":{"models":[]},"quota_used":12.12345678,"keep":"yes"}'::jsonb WHERE id=$1`, id)
	}
	clean := func() {
		t.Helper()
		var good bool
		if err := a.DB.QueryRow(`SELECT NOT extra ? 'upstream_billing_probe' AND NOT extra ? 'upstream_model_metadata' AND extra->>'quota_used'='12.12345678' AND extra->>'keep'='yes' FROM accounts WHERE id=$1`, id).Scan(&good); err != nil || !good {
			t.Fatal("proxy change lost counters or kept obsolete snapshot", good, err)
		}
	}
	expire := func(proxy int64) {
		manage("PUT", proxies+fmt.Sprint(proxy), map[string]any{"expires_at": time.Now().Add(-time.Minute).Unix()})
	}
	sweep := func() {
		t.Helper()
		if err := a.expireProxies(ctx); err != nil {
			t.Fatal(err)
		}
	}
	request(id, 0)
	socket(id, 0)
	seed()
	// A backup edit invalidates the original account's observations too.
	manage("PUT", proxies+fmt.Sprint(backup), map[string]any{"password": "backup-secret"})
	clean()
	seed()
	expire(primary)
	request(id, 1) // Runtime fallback works before the periodic write.
	socket(id, 1)
	sweep()
	state(id, &backup, &primary)
	clean()
	if got := manage("GET", primaryPath, nil); got["status"] != "expired" {
		t.Fatal("expired state not persisted", got)
	}
	if w := call("POST", path+"/revert-proxy-fallback", ordinary, nil); w.Code != 403 {
		t.Fatal("ordinary user reverted proxy", w.Code)
	}
	if w := call("POST", path+"/revert-proxy-fallback", admin, nil); w.Code != 409 {
		t.Fatal("unavailable origin accepted", w.Code)
	}
	if w := call("DELETE", primaryPath, admin, nil); w.Code != 409 {
		t.Fatal("original proxy deleted while needed for revert", w.Code)
	}
	// A second fallback keeps the first origin and persists direct routing.
	expire(backup)
	sweep()
	state(id, nil, &primary)
	request(id, 2)
	socket(id, 2)
	// Renewing the original does not move accounts until explicitly reverted.
	manage("PUT", primaryPath, map[string]any{"status": "active", "expires_at": nil})
	sweep()
	state(id, nil, &primary)
	seed()
	manage("POST", path+"/revert-proxy-fallback", nil)
	state(id, &primary, nil)
	clean()
	request(id, 0)
	socket(id, 0)
	if w := call("POST", path+"/revert-proxy-fallback", admin, nil); w.Code != 409 {
		t.Fatal("repeat revert accepted", w.Code)
	}
	if w := call("POST", accounts+"999999999/revert-proxy-fallback", admin, nil); w.Code != 404 {
		t.Fatal("missing account", w.Code)
	}
	// Manual reassignment means the operator has chosen a new origin.
	expire(primary)
	sweep()
	state(id, nil, &primary)
	manage("PUT", path, map[string]any{"proxy_id": 0})
	state(id, nil, nil)
	// With no fallback, expiry blocks dispatch and keeps the binding.
	none := newProxy("No fallback", 0, "none", nil)
	blocked := newAccount(none)
	expire(none)
	sweep()
	state(blocked, &none, nil)
	if _, err := a.resolveProxy(ctx, none); err == nil {
		t.Fatal("unavailable proxy silently became direct")
	}
	// Inactive backup nodes can lead to an explicit direct fallback, but a
	// disabled root never grants permission to bypass itself.
	unavailable := newProxy("Disabled backup", 1, "direct", nil)
	chain := newProxy("Through disabled backup", 0, "proxy", &unavailable)
	chainAccount := newAccount(chain)
	manage("PUT", proxies+fmt.Sprint(unavailable), map[string]any{"status": "inactive"})
	expire(chain)
	request(chainAccount, 2)
	sweep()
	state(chainAccount, nil, &chain)
	if _, err := a.resolveProxy(ctx, unavailable); err == nil {
		t.Fatal("disabled root enabled direct fallback")
	}
	// Malformed stored cycles terminate and leave the account bound/blocked.
	cycle := newProxy("Stored cycle", 0, "none", nil)
	cycleAccount := newAccount(cycle)
	exec("UPDATE proxies SET fallback_mode='proxy',backup_proxy_id=id,expires_at=now()-interval '1 minute' WHERE id=$1", cycle)
	sweep()
	state(cycleAccount, &cycle, nil)
	if _, err := a.resolveProxy(ctx, cycle); err == nil {
		t.Fatal("cycle accepted")
	}
	exec("UPDATE proxies SET fallback_mode='none',backup_proxy_id=NULL WHERE id=$1", cycle)
	// Proxy status and account binding must roll back together on storage errors.
	fault := newProxy("Atomic expiry", 0, "direct", nil)
	faultAccount := newAccount(fault)
	expire(fault)
	exec(fmt.Sprintf(`CREATE FUNCTION reject_proxy_expiry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id=%d AND NEW.proxy_id IS DISTINCT FROM OLD.proxy_id THEN RAISE EXCEPTION 'injected expiry failure'; END IF; RETURN NEW; END $$`, faultAccount))
	exec("CREATE TRIGGER reject_proxy_expiry BEFORE UPDATE ON accounts FOR EACH ROW EXECUTE FUNCTION reject_proxy_expiry()")
	err := a.expireProxies(ctx)
	exec("DROP TRIGGER reject_proxy_expiry ON accounts")
	exec("DROP FUNCTION reject_proxy_expiry()")
	if err == nil {
		t.Fatal("storage error ignored")
	}
	state(faultAccount, &fault, nil)
	if got := manage("GET", proxies+fmt.Sprint(fault), nil); got["status"] != "active" {
		t.Fatal("expiry status committed despite failed account move")
	}
	sweep()
	state(faultAccount, nil, &fault)
	// A concurrent administrator renewal is observed after the graph lock,
	// so the sweep cannot execute against a stale expiry snapshot.
	renew := newProxy("Concurrent renewal", 0, "direct", nil)
	renewAccount := newAccount(renew)
	expire(renew)
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT pg_advisory_xact_lock(720034)"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("UPDATE proxies SET expires_at=now()+interval '1 day',updated_at=clock_timestamp() WHERE id=$1", renew); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- a.expireProxies(ctx) }()
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	state(renewAccount, &renew, nil)
	request(renewAccount, 0)
	// Explicit reassignment releases the original-proxy deletion guard.
	manage("PUT", accounts+fmt.Sprint(chainAccount), map[string]any{"proxy_id": 0})
	manage("DELETE", proxies+fmt.Sprint(chain), nil)
}
