package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderBalanceContracts(t *testing.T) {
	policy, err := parseBalancePolicy(Config{})
	if err != nil || !policy.Enabled || policy.Threshold != "0.5" || policy.Interval != 10*time.Minute {
		t.Fatal(policy, err)
	}
	for _, cfg := range []Config{{BalanceCheckEnabled: "unknown"}, {BalanceThreshold: "-1"}, {BalanceThreshold: "NaN"}, {BalanceThreshold: "1e100"}, {BalanceCheckIntervalMinutes: "0"}, {BalanceCheckIntervalMinutes: "1441"}} {
		if _, err := parseBalancePolicy(cfg); err == nil {
			t.Fatal("invalid policy accepted", cfg)
		}
	}
	for _, v := range []struct {
		Platform, Body, Balance string
		Low                     bool
	}{
		{"kimi", `{"code":0,"data":{"available_balance":0.500000000001}}`, "0.500000000001", false},
		{"kimi", `{"code":0,"data":{"available_balance":"-0.01"}}`, "-0.01", true},
		{"deepseek", `{"balance_infos":[{"currency":"CNY","total_balance":"0.1"},{"currency":"USD","total_balance":"0.5"}]}`, "0.1", false},
		{"deepseek", `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"100"}]}`, "100", true},
	} {
		got, err := parseProviderBalance(v.Platform, []byte(v.Body))
		if err != nil || !got.Success || got.Balance.String() != v.Balance || got.below("0.5") != v.Low {
			t.Fatal(v, got, err)
		}
	}
	for _, v := range []struct{ Platform, Body string }{
		{"kimi", `{}`}, {"kimi", `{"code":1,"data":{"available_balance":100}}`},
		{"kimi", `{"code":0,"data":{"available_balance":null}}`}, {"kimi", `{"code":0,"data":{"available_balance":"NaN"}}`},
		{"deepseek", `{"balance_infos":[]}`}, {"deepseek", `{"is_available":null,"balance_infos":[{"total_balance":1}]}`},
		{"deepseek", `{"balance_infos":[{"total_balance":"0.5"},{"total_balance":"0.6"}]}`},
		{"deepseek", `{"balance_infos":[{"total_balance":"1e100"}]}`}, {"zhipu", `{}`},
	} {
		if _, err := parseProviderBalance(v.Platform, []byte(v.Body)); err == nil {
			t.Fatal("invalid provider result accepted", v)
		}
	}
	for _, platform := range []string{"kimi", "deepseek"} {
		for _, suffix := range []string{"", "/v1", "/anthropic", "/anthropic/v1"} {
			base, _ := json.Marshal("https://relay.example/team" + suffix)
			u := &upstreamAccount{Platform: platform, Type: "apikey", Credentials: map[string]json.RawMessage{"base_url": base, "api_key": json.RawMessage(`"secret"`)}}
			account, path, err := balanceEndpoint(u)
			if err != nil {
				t.Fatal(err)
			}
			baseURL, _ := account.baseURL()
			target, err := upstreamURL(baseURL, path)
			want := "https://relay.example/team/v1/users/me/balance"
			if platform == "deepseek" {
				want = "https://relay.example/team/user/balance"
				if suffix == "/v1" {
					want = "https://relay.example/team/v1/user/balance"
				}
			}
			if err != nil || target != want || account.protocol() != "chat_completions" || string(u.Credentials["base_url"]) != string(base) {
				t.Fatal("balance URL or credential mutation", target, want, err)
			}
		}
	}
	for _, platform := range []string{"kimi", "deepseek", "zhipu", "minimax"} {
		if !balanceFailure(platform, 402, nil) || !balanceFailure(platform, 429, []byte(`{"error":{"message":"Insufficient balance"}}`)) || balanceFailure(platform, 429, []byte("too many requests")) || balanceFailure(platform, 403, []byte("insufficient balance")) {
			t.Fatal("incorrect balance classification", platform)
		}
	}
	if balanceFailure("openai", 402, nil) {
		t.Fatal("balance policy crossed provider boundary")
	}
}

func testAccountBalances(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Session-Id", randomToken(12))
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]json.RawMessage {
		t.Helper()
		w := call(method, path, token, body)
		var result struct{ Data map[string]json.RawMessage }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return result.Data
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	var mode, calls atomic.Int32
	entered, unblock := make(chan struct{}, 1), make(chan struct{}, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer balance-secret" || r.Header.Get("X-Api-Key") != "" {
			t.Error("balance request authentication or method")
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			w.WriteHeader(403)
			fmt.Fprint(w, "balance-secret")
			return
		case 2:
			fmt.Fprint(w, `{"code":0,"data":{"available_balance":0.1}}`)
			return
		case 3:
			entered <- struct{}{}
			select {
			case <-unblock:
			case <-r.Context().Done():
				return
			}
		case 4:
			fmt.Fprint(w, `{}`)
			return
		case 5:
			fmt.Fprint(w, strings.Repeat(" ", maxBalanceBody+1))
			return
		}
		switch r.URL.Path {
		case "/kimi/v1/users/me/balance":
			fmt.Fprint(w, `{"code":0,"data":{"available_balance":12.123456789012}}`)
		case "/deepseek/user/balance":
			fmt.Fprint(w, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.1"},{"currency":"USD","total_balance":"2.25"}]}`)
		default:
			t.Error("unexpected balance URL", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	ids := map[string]string{}
	for _, platform := range []string{"kimi", "deepseek"} {
		ids[platform] = string(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "balance-" + platform, "platform": platform, "type": "apikey", "credentials": map[string]any{"api_key": "balance-secret", "base_url": provider.URL + "/" + platform + "/anthropic", "api_protocol": "anthropic"}})["id"])
		defer exec("UPDATE accounts SET status='inactive',updated_at=clock_timestamp() WHERE id=$1", ids[platform])
	}
	path := "/api/v1/admin/cn-providers/accounts/" + ids["kimi"] + "/balance"
	for _, v := range []struct {
		Token  string
		Status int
	}{{"", 401}, {ordinary, 403}, {"sk-not-a-user-session", 401}} {
		if w := call("GET", path, v.Token, nil); w.Code != v.Status {
			t.Fatal("balance permissions", w.Code)
		}
	}
	if w := call("GET", "/api/v1/admin/cn-providers/accounts/9223372036854775807/balance", admin, nil); w.Code != 404 {
		t.Fatal("missing balance account", w.Code)
	}
	get := func(id string) providerBalance {
		t.Helper()
		w := call("GET", "/api/v1/admin/cn-providers/accounts/"+id+"/balance", admin, nil)
		var result struct{ Data providerBalance }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || strings.Contains(w.Body.String(), "balance-secret") {
			t.Fatalf("balance result: %d %s", w.Code, w.Body.String())
		}
		return result.Data
	}
	exec(`UPDATE accounts SET extra='{"keep":{"unknown":true},"quota_used":0.12345678,"kimi_balance_low":true}'::jsonb WHERE id=$1`, ids["kimi"])
	if got := get(ids["kimi"]); !got.Success || !got.Persisted || got.Balance != "12.123456789012" || got.FetchedAt == 0 {
		t.Fatal("Kimi balance", got)
	}
	if got := get(ids["deepseek"]); !got.Success || len(got.Balances) != 2 || got.below("0.5") {
		t.Fatal("DeepSeek balance", got)
	}
	var snapshot string
	if err := a.DB.QueryRow("SELECT extra::text FROM accounts WHERE id=$1", ids["kimi"]).Scan(&snapshot); err != nil || !strings.Contains(snapshot, "0.12345678") || !strings.Contains(snapshot, "12.123456789012") || !strings.Contains(snapshot, `"unknown": true`) || !strings.Contains(snapshot, `"kimi_balance_low": false`) {
		t.Fatal("balance snapshot corrupted", snapshot, err)
	}
	for _, m := range []int32{1, 4, 5} {
		mode.Store(m)
		if got := get(ids["kimi"]); got.Success || got.Persisted || got.Error == "" {
			t.Fatal("invalid probe accepted", got)
		}
		var after string
		if err := a.DB.QueryRow("SELECT extra::text FROM accounts WHERE id=$1", ids["kimi"]).Scan(&after); err != nil || after != snapshot {
			t.Fatal("failed probe overwrote snapshot", after, err)
		}
	}
	mode.Store(2)
	if err := a.runBalanceChecks(ctx); err != nil {
		t.Fatal(err)
	}
	checkState := func(reason string, paused bool) {
		t.Helper()
		var got string
		var blocked bool
		if err := a.DB.QueryRow("SELECT COALESCE(temp_unschedulable_reason,''),COALESCE(temp_unschedulable_until>now(),false) FROM accounts WHERE id=$1", ids["kimi"]).Scan(&got, &blocked); err != nil || !strings.HasPrefix(got, reason) || blocked != paused || reason == "" && got != "" {
			t.Fatal("balance cooldown", got, blocked, err)
		}
	}
	checkState("cn_balance_low", true)
	mode.Store(0)
	get(ids["kimi"])
	checkState("cn_balance_low", true) // Manual snapshots never change scheduling.
	if err := a.runBalanceChecks(ctx); err != nil {
		t.Fatal(err)
	}
	checkState("", false)
	exec("UPDATE accounts SET temp_unschedulable_until=now()+interval '1 hour',temp_unschedulable_reason='manual',rate_limit_reset_at=now()+interval '1 hour' WHERE id=$1", ids["kimi"])
	if err := a.runBalanceChecks(ctx); err != nil {
		t.Fatal(err)
	}
	checkState("manual", true)
	mode.Store(3)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- call("GET", path, admin, nil) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	if w := call("GET", path, admin, nil); w.Code != 409 {
		t.Fatal("concurrent probe was dispatched", w.Code)
	}
	manage("PUT", "/api/v1/admin/accounts/"+ids["kimi"], admin, map[string]any{"status": "inactive"})
	unblock <- struct{}{}
	w := <-result
	var got struct{ Data providerBalance }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Data.Persisted || !got.Data.Success {
		t.Fatal("stale probe applied", w.Code, w.Body.String())
	}
	checkState("manual", true)
	mode.Store(0)
	// Use a short-lived worker to exercise real start/cancel without changing the
	// running application's configuration or its ten-minute timer.
	worker := &App{DB: a.DB, Redis: a.Redis, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams, balancePolicy: balanceCheckPolicy{Enabled: true, Threshold: "0.5", Interval: 10 * time.Millisecond}}
	workerCtx, cancel := context.WithCancel(ctx)
	before := calls.Load()
	worker.startBalanceChecks(workerCtx)
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-worker.balanceWorkerDone
	if calls.Load() == before {
		t.Fatal("balance worker did not run")
	}
	worker.balancePolicy.Enabled = false
	before = calls.Load()
	if err := worker.runBalanceChecks(ctx); err != nil || calls.Load() != before {
		t.Fatal("disabled balance worker ran", err)
	}

	// Exercise reactive cooldown and account failover through the HTTP gateway,
	// including providers without an active balance endpoint.
	var rejectStatus atomic.Int32
	var rejected atomic.Int32
	textProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer rejected-balance-secret" {
			rejected.Add(1)
			w.WriteHeader(int(rejectStatus.Load()))
			fmt.Fprint(w, `{"error":{"message":"Insufficient balance rejected-balance-secret"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"balance-ok","model":"balance-model","choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer textProvider.Close()
	manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "balance-probe@example.test", "password": "balance-probe-password", "balance": 10})
	token := credentialString(manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "balance-probe@example.test", "password": "balance-probe-password"}), "access_token")
	for _, platform := range []string{"kimi", "deepseek", "zhipu", "minimax"} {
		gid := json.Number(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "balance-" + platform, "platform": platform})["id"])
		manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "balance-" + platform, "group_ids": []json.Number{gid}, "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"balance-model"}, "billing_mode": "per_request", "per_request_price": "0.01"}}})
		var rejectedID int64
		for i, key := range []string{"rejected-balance-secret", "healthy-balance-secret"} {
			account := manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "reactive-" + platform, "platform": platform, "type": "apikey", "group_ids": []json.Number{gid}, "priority": i + 1, "credentials": map[string]any{"api_key": key, "base_url": textProvider.URL}})
			id, _ := json.Number(account["id"]).Int64()
			defer exec("UPDATE accounts SET status='inactive',updated_at=clock_timestamp() WHERE id=$1", id)
			if i == 0 {
				rejectedID = id
			}
		}
		key := credentialString(manage("POST", "/api/v1/keys", token, map[string]any{"name": "balance", "group_id": gid}), "key")
		for _, status := range []int32{402, 429} {
			exec("UPDATE accounts SET temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,rate_limit_reset_at=NULL,updated_at=clock_timestamp() WHERE id=$1", rejectedID)
			rejectStatus.Store(status)
			before := rejected.Load()
			w := call("POST", "/v1/chat/completions", key, map[string]any{"model": "balance-model", "messages": []any{map[string]string{"role": "user", "content": "check"}}})
			if w.Code != 200 || rejected.Load() != before+1 || strings.Contains(w.Body.String(), "secret") {
				t.Fatal("balance failover", platform, status, w.Code, w.Body.String())
			}
			var healthy, low, blocked, rateFree bool
			if err := a.DB.QueryRow("SELECT status='active',COALESCE((extra->>$2)::boolean,false),temp_unschedulable_until>now(),rate_limit_reset_at IS NULL FROM accounts WHERE id=$1", rejectedID, platform+"_balance_low").Scan(&healthy, &low, &blocked, &rateFree); err != nil || !healthy || !low || !blocked || !rateFree {
				t.Fatal("reactive cooldown", healthy, low, blocked, rateFree, err)
			}
		}
		if platform == "zhipu" || platform == "minimax" {
			if w := call("GET", fmt.Sprintf("/api/v1/admin/cn-providers/accounts/%d/balance", rejectedID), admin, nil); w.Code != 400 {
				t.Fatal("unsupported active balance provider", w.Code)
			}
		}
		// A probe loaded before a newer balance failure may not erase that signal.
		stale, err := a.loadAccount(ctx, rejectedID)
		if err != nil {
			t.Fatal(err)
		}
		a.markBalanceFailure(ctx, stale)
		var unchanged bool
		if err := a.DB.QueryRow("SELECT updated_at=$2 FROM accounts WHERE id=$1", rejectedID, stale.UpdatedAt).Scan(&unchanged); err != nil || unchanged {
			t.Fatal("reactive failure did not invalidate stale probe", err)
		}
	}
}
