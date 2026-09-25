package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKeyManagement(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token, idem string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.225:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	data := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("key management status %d", w.Code)
		}
		return out.Data
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		return data(call(method, path, token, "", body))
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "key-management@example.test", "password": "key-management-password", "balance": 10})
	uid := id(user)
	token := manage("POST", "/api/v1/auth/login", "", map[string]string{"email": "key-management@example.test", "password": "key-management-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Key management", "platform": "openai"}))
	const idem = "key-create-parallel"
	body := map[string]any{"name": "zeta", "group_id": gid, "expires_in_days": 7}
	results := make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = call("POST", "/api/v1/keys", token, idem, body)
		}()
	}
	wg.Wait()
	key := data(results[0])
	kid := id(key)
	keyPath := fmt.Sprintf("/api/v1/keys/%d", kid)
	replays := 0
	for _, w := range results {
		if v := data(w); id(v) != kid || v["key"] != key["key"] || v["expires_at"] != key["expires_at"] {
			t.Fatal("creation replay changed the credential or expiry")
		}
		if w.Header().Get("Idempotency-Replayed") == "true" && w.Header().Get("X-Idempotency-Replayed") == "true" {
			replays++
		}
	}
	if replays != len(results)-1 {
		t.Fatal("concurrent key creation did not replay", replays)
	}
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM api_keys WHERE user_id=$1", uid).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate credential persisted", count, err)
	}
	changed := map[string]any{"name": "different", "group_id": gid, "expires_in_days": 7}
	for _, request := range []struct {
		token string
		body  any
	}{{token, changed}, {admin, body}} {
		w := call("POST", "/api/v1/keys", request.token, idem, request.body)
		if w.Code != 409 || strings.Contains(w.Body.String(), key["key"].(string)) {
			t.Fatal("idempotency actor/payload isolation failed", w.Code)
		}
	}
	for _, invalid := range []string{strings.Repeat("a", 129), "two words"} {
		if w := call("POST", "/api/v1/keys", token, invalid, body); w.Code != 400 {
			t.Fatal("invalid idempotency key accepted", w.Code)
		}
	}
	// A replay-record failure must roll back the credential as well.
	exec(`ALTER TABLE idempotency_records ADD CONSTRAINT test_key_replay_failure CHECK(scope<>'user.api_keys.create') NOT VALID`)
	defer a.DB.Exec("ALTER TABLE idempotency_records DROP CONSTRAINT IF EXISTS test_key_replay_failure")
	if w := call("POST", "/api/v1/keys", token, "key-create-rollback", map[string]string{"name": "rollback"}); w.Code != 500 {
		t.Fatal("replay persistence failure accepted", w.Code)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM api_keys WHERE user_id=$1", uid).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed transaction left a usable credential", count, err)
	}
	exec("ALTER TABLE idempotency_records DROP CONSTRAINT test_key_replay_failure")
	rollback := data(call("POST", "/api/v1/keys", token, "key-create-rollback", map[string]string{"name": "rollback"}))
	manage("DELETE", fmt.Sprintf("/api/v1/keys/%d", id(rollback)), token, nil)
	// Replay is durable and must not recreate a deleted key.
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock}
	raw, _ := json.Marshal(map[string]string{"name": "rollback"})
	r := httptest.NewRequest("POST", "/api/v1/keys", bytes.NewReader(raw))
	r.Header.Set("Idempotency-Key", "key-create-rollback")
	r = r.WithContext(context.WithValue(r.Context(), identityKey{}, &identity{ID: uid}))
	w := httptest.NewRecorder()
	if err := fresh.createKey(w, r); err != nil || id(data(w)) != id(rollback) {
		t.Fatal("durable creation replay failed", err)
	}
	if w := call("GET", fmt.Sprintf("/api/v1/keys/%d", id(rollback)), token, "", nil); w.Code != 404 {
		t.Fatal("replay resurrected a deleted key", w.Code)
	}
	// List filters and sorting apply before pagination and never widen the owner.
	alpha := manage("POST", "/api/v1/keys", token, map[string]any{"name": "alpha", "group_id": gid, "status": "inactive", "custom_key": "list-needle-credential"})
	tied := manage("POST", "/api/v1/keys", token, map[string]any{"name": "alpha", "group_id": gid})
	loose := manage("POST", "/api/v1/keys", token, map[string]any{"name": "loose_%", "custom_key": "loose-list-credential"})
	manage("POST", "/api/v1/keys", admin, map[string]any{"name": "alpha", "group_id": gid})
	checkList := func(path, actor string, total int, want ...int64) {
		t.Helper()
		v := manage("GET", path, actor, nil)
		var got []int64
		for _, item := range v["items"].([]any) {
			row := item.(map[string]any)
			got = append(got, id(row))
			if row["user_id"] != float64(uid) {
				t.Fatal("key list crossed user scope")
			}
		}
		if int(v["total"].(float64)) != total || !slices.Equal(got, want) {
			t.Fatal("key filtering/sorting/pagination", path, got, want, v["total"])
		}
	}
	checkList(fmt.Sprintf("/api/v1/keys?group_id=%d&sort_by=name&sort_order=asc&page_size=1&page=2", gid), token, 3, id(tied))
	checkList("/api/v1/keys?group_id=0", token, 1, id(loose))
	checkList("/api/v1/keys?search=needle&status=inactive", token, 1, id(alpha))
	checkList("/api/v1/keys?search="+url.QueryEscape("_%"), token, 1, id(loose))
	checkList("/api/v1/keys?search=does-not-exist", token, 0)
	checkList(fmt.Sprintf("/api/v1/admin/users/%d/api-keys?sort_by=name&sort_order=asc", uid), admin, 4, id(alpha), id(tied), id(loose), kid)
	checkList("/api/v1/keys?sort_by="+url.QueryEscape("id; DROP TABLE api_keys"), token, 4, id(loose), id(tied), id(alpha), kid)
	for _, query := range []string{"group_id=-1", "group_id=invalid", "search=" + strings.Repeat("a", 101)} {
		if w := call("GET", "/api/v1/keys?"+query, token, "", nil); w.Code != 400 {
			t.Fatal("invalid key list filter accepted", w.Code)
		}
	}
	// Effective windows are a projection; queries and unrelated edits must not
	// reset the stored counters, including a stale counter without a start time.
	exec(`UPDATE api_keys SET usage_5h=1.12345678,window_5h_start=now(),
 usage_1d=2,window_1d_start=now()-interval '25 hours',usage_7d=3,window_7d_start=NULL WHERE id=$1`, kid)
	for _, v := range []map[string]any{
		manage("GET", keyPath, token, nil),
		manage("PUT", keyPath, token, map[string]string{"name": "window-view"}),
		manage("GET", "/api/v1/keys?search=window-view", token, nil)["items"].([]any)[0].(map[string]any),
		manage("GET", fmt.Sprintf("/api/v1/admin/users/%d/api-keys?search=window-view", uid), admin, nil)["items"].([]any)[0].(map[string]any),
	} {
		if v["usage_5h"] != 1.12345678 || v["usage_1d"] != float64(0) || v["usage_7d"] != float64(0) || v["reset_5h_at"] == nil || v["reset_1d_at"] != nil || v["reset_7d_at"] != nil || v["current_concurrency"] != float64(0) || v["last_used_ip"] != nil {
			t.Fatal("invalid effective key window projection")
		}
		start, e1 := time.Parse(time.RFC3339Nano, v["window_5h_start"].(string))
		reset, e2 := time.Parse(time.RFC3339Nano, v["reset_5h_at"].(string))
		if e1 != nil || e2 != nil || reset.Sub(start) != 5*time.Hour {
			t.Fatal("incorrect window reset time", e1, e2)
		}
	}
	var stale bool
	if err := a.DB.QueryRow("SELECT usage_1d=2 AND usage_7d=3 FROM api_keys WHERE id=$1", kid).Scan(&stale); err != nil || !stale {
		t.Fatal("key projection changed stored usage", err)
	}
	// Expiry changes only recover expired keys, preserving manual disables,
	// exhausted quotas, explicit status choices and concurrent consumption fields.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		status string
		body   map[string]any
		want   string
	}{
		{"expired", map[string]any{"expires_at": future}, "active"},
		{"expired", map[string]any{"expires_at": "", "quota": 10}, "active"},
		{"expired", map[string]any{"expires_at": past}, "expired"},
		{"expired", map[string]any{"expires_at": future, "status": "inactive"}, "inactive"},
		{"inactive", map[string]any{"expires_at": "", "quota": 10}, "inactive"},
		{"quota_exhausted", map[string]any{"expires_at": ""}, "quota_exhausted"},
		{"quota_exhausted", map[string]any{"expires_at": "", "quota": 10}, "active"},
	} {
		exec("UPDATE api_keys SET status=$2,expires_at=$3,quota=3,quota_used=3.12345678,usage_5h=1,window_5h_start=now() WHERE id=$1", kid, tc.status, past)
		v := manage("PUT", keyPath, token, tc.body)
		if v["status"] != tc.want || v["quota_used"] != 3.12345678 || v["usage_5h"] != float64(1) {
			t.Fatal("expiry recovery changed status or usage", tc.status, tc.want)
		}
	}
	// Shared balance idempotency remains independent of the creation operation.
	for i := 0; i < 2; i++ {
		v := data(call("POST", fmt.Sprintf("/api/v1/admin/users/%d/balance", uid), admin, idem, map[string]any{"operation": "add", "balance": "0.12345678"}))
		if v["balance"] != 10.12345678 {
			t.Fatal("write scopes collided or balance duplicated")
		}
	}

	// Lists, detail and mutation responses share public relation fields. Loading
	// an owner through an administrator's key endpoint must not expose notes.
	userPath := fmt.Sprintf("/api/v1/admin/users/%d", uid)
	groupPath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	adminKeyPath := fmt.Sprintf("/api/v1/admin/api-keys/%d", kid)
	manage("PUT", groupPath, admin, map[string]any{"rate_multiplier": 2, "rpm_limit": 40, "allow_image_generation": true, "video_price_720p": "0.01234567", "image_price_1k": "0.02345678"})
	exec(`UPDATE groups SET model_routing='{"internal-model":[999]}'::jsonb,
 profit_control_enabled=true,profit_min_margin=0.2,profit_safety_buffer=0.1 WHERE id=$1`, gid)
	manage("PUT", userPath, admin, map[string]any{"notes": "private administrator note", "restrict_public_groups": true, "allowed_groups": []int64{gid}, "group_rates": map[string]any{fmt.Sprint(gid): "0.25"}})
	checkOwner := func(v map[string]any, adminView bool) {
		t.Helper()
		groups, ok := v["allowed_groups"].([]any)
		if !ok || !slices.Contains(groups, any(float64(gid))) || v["id"] != float64(uid) || v["email"] != "key-management@example.test" {
			t.Fatal("owner identity or grants missing")
		}
		for _, field := range []string{"password_hash", "totp_secret", "api_keys", "subscriptions", "deleted_at"} {
			if _, present := v[field]; present {
				t.Fatal("private or recursive owner field", field)
			}
		}
		if adminView {
			rates, ok := v["group_rates"].(map[string]any)
			if !ok || rates[fmt.Sprint(gid)] != 0.25 || v["notes"] != "private administrator note" || v["restrict_public_groups"] != true {
				t.Fatal("administrator owner fields missing")
			}
		} else {
			for _, field := range []string{"notes", "restrict_public_groups", "group_rates", "last_used_at"} {
				if _, present := v[field]; present {
					t.Fatal("administrator owner field exposed", field)
				}
			}
		}
	}
	checkGroup := func(v map[string]any) {
		t.Helper()
		if v["id"] != float64(gid) || v["rate_multiplier"] != float64(2) || v["rpm_limit"] != float64(40) || v["allow_image_generation"] != true || v["video_price_720p"] != 0.01234567 || v["image_price_1k"] != 0.02345678 {
			t.Fatal("public group pricing or capabilities missing")
		}
		for _, field := range []string{"model_routing", "model_routing_enabled", "model_pricing", "profit_control_enabled", "profit_min_margin", "profit_safety_buffer", "codex_models_manifest_config", "model_allowlist", "account_groups", "account_count", "force_openai_fast", "free_openai_fast", "deleted_at"} {
			if _, present := v[field]; present {
				t.Fatal("internal group field exposed", field)
			}
		}
	}
	checkRelations := func(v map[string]any, visible bool) {
		t.Helper()
		owner, ok := v["user"].(map[string]any)
		if !ok {
			t.Fatal("missing key owner")
		}
		checkOwner(owner, false)
		if !visible {
			if v["group"] != nil {
				t.Fatal("unavailable group exposed")
			}
			return
		}
		group, ok := v["group"].(map[string]any)
		if !ok {
			t.Fatal("missing key group")
		}
		checkGroup(group)
	}
	for _, v := range []map[string]any{
		manage("PUT", keyPath, token, map[string]any{"name": "relations"}),
		manage("GET", keyPath, token, nil),
		manage("GET", "/api/v1/keys?search=relations", token, nil)["items"].([]any)[0].(map[string]any),
		manage("GET", userPath+"/api-keys?search=relations", admin, nil)["items"].([]any)[0].(map[string]any),
		manage("GET", groupPath+"/api-keys?search=relations", admin, nil)["items"].([]any)[0].(map[string]any),
		manage("PUT", adminKeyPath, admin, map[string]any{"reset_rate_limit_usage": true})["api_key"].(map[string]any),
	} {
		checkRelations(v, true)
	}
	checkRelations(manage("POST", "/api/v1/keys", token, map[string]any{"name": "unassigned relation"}), false)
	checkRelations(manage("POST", "/api/v1/keys", token, map[string]any{"name": "assigned relation", "group_id": gid}), true)
	for _, path := range []string{"/api/v1/user/profile", "/api/v1/auth/me"} {
		checkOwner(manage("GET", path, token, nil), false)
	}
	checkOwner(manage("PUT", "/api/v1/user", token, map[string]any{"username": "Updated owner"}), false)
	checkOwner(manage("POST", "/api/v1/auth/login", "", map[string]string{"email": "key-management@example.test", "password": "key-management-password"})["user"].(map[string]any), false)
	for _, v := range []map[string]any{
		manage("GET", userPath, admin, nil),
		manage("GET", "/api/v1/admin/users?search=key-management@example.test", admin, nil)["items"].([]any)[0].(map[string]any),
	} {
		checkOwner(v, true)
		if v["last_used_at"] != nil {
			t.Fatal("login incorrectly counted as model usage")
		}
	}
	var available struct{ Data []map[string]any }
	w = call("GET", "/api/v1/groups/available", token, "", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &available) != nil || len(available.Data) != 1 {
		t.Fatal("restricted group visibility", w.Code)
	}
	checkGroup(available.Data[0])
	if rates := manage("GET", "/api/v1/groups/rates", token, nil); len(rates) != 1 || rates[fmt.Sprint(gid)] != 0.25 {
		t.Fatal("dedicated multiplier replaced base group rate")
	}
	// A real gateway request verifies both effective pricing and activity derived
	// from consumption. Removing its key must not erase the owner's last use.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"relations","model":"relations-model","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer provider.Close()
	manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Relations account", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "relations-upstream", "base_url": provider.URL}})
	channel := manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Relations pricing", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"relations-model"}, "billing_mode": "per_request", "per_request_price": "0.01"}}})
	defer manage("PUT", fmt.Sprintf("/api/v1/admin/channels/%d", id(channel)), admin, map[string]any{"status": "disabled"})
	w = call("POST", "/v1/chat/completions", tied["key"].(string), "", map[string]any{"model": "relations-model", "messages": []any{map[string]string{"role": "user", "content": "ok"}}})
	if w.Code != 200 {
		t.Fatal("relation pricing gateway request", w.Code, w.Body.String())
	}
	var lastUsed time.Time
	var cost string
	if err := a.DB.QueryRow("SELECT created_at,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&lastUsed, &cost); err != nil || cost != "0.0025000000" {
		t.Fatal("relation effective pricing", cost, err)
	}
	manage("DELETE", fmt.Sprintf("/api/v1/keys/%d", id(tied)), token, nil)
	for _, v := range []map[string]any{
		manage("GET", userPath, admin, nil),
		manage("GET", "/api/v1/admin/users?search=key-management@example.test", admin, nil)["items"].([]any)[0].(map[string]any),
	} {
		checkOwner(v, true)
		stamp, _ := v["last_used_at"].(string)
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil || !at.Equal(lastUsed) {
			t.Fatal("owner activity lost after key deletion", err)
		}
	}
	// Unavailable groups are hidden from the owner's key response, while the
	// administrator can still diagnose its assignment using public group fields.
	manage("PUT", groupPath, admin, map[string]any{"status": "inactive"})
	checkRelations(manage("GET", keyPath, token, nil), false)
	checkRelations(manage("GET", userPath+"/api-keys?search=relations", admin, nil)["items"].([]any)[0].(map[string]any), true)
	manage("PUT", groupPath, admin, map[string]any{"status": "active"})
	manage("PUT", userPath, admin, map[string]any{"allowed_groups": []int64{}})
	w = call("GET", keyPath, token, "", nil)
	if v := data(w); v["group"] != nil || v["group_id"] != float64(gid) {
		t.Fatal("revoked group relation visible or assignment changed")
	}
	if w := call("GET", groupPath+"/api-keys", token, "", nil); w.Code != 403 {
		t.Fatal("owner queried administrative relations", w.Code)
	}
	// A grant and assignment must roll back together; retry returns the granted
	// group identity and the newly visible owner grants in the same response.
	private := manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Relations private", "platform": "openai", "is_exclusive": true})
	privateID := id(private)
	exec("ALTER TABLE api_keys ADD CONSTRAINT test_relation_assignment CHECK(group_id<>" + fmt.Sprint(privateID) + ") NOT VALID")
	defer a.DB.Exec("ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS test_relation_assignment")
	if w := call("PUT", adminKeyPath, admin, "", map[string]any{"group_id": privateID}); w.Code != 500 {
		t.Fatal("failed assignment accepted", w.Code)
	}
	var grants int
	if err := a.DB.QueryRow("SELECT count(*) FROM user_allowed_groups WHERE user_id=$1", uid).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("failed assignment committed grant", grants, err)
	}
	exec("ALTER TABLE api_keys DROP CONSTRAINT test_relation_assignment")
	granted := manage("PUT", adminKeyPath, admin, map[string]any{"group_id": privateID})
	if granted["auto_granted_group_access"] != true || granted["granted_group_id"] != float64(privateID) || granted["granted_group_name"] != "Relations private" {
		t.Fatal("automatic grant identity missing")
	}
	assigned := granted["api_key"].(map[string]any)
	ownerGroups := assigned["user"].(map[string]any)["allowed_groups"].([]any)
	if assigned["group"].(map[string]any)["id"] != float64(privateID) || !slices.Equal(ownerGroups, []any{float64(privateID)}) {
		t.Fatal("assignment response used stale relations")
	}
	for _, body := range []map[string]any{{"group_id": privateID}, {"group_id": 0}} {
		v := manage("PUT", adminKeyPath, admin, body)
		if v["auto_granted_group_access"] != false || v["granted_group_id"] != nil || v["granted_group_name"] != nil {
			t.Fatal("no-op assignment reported a new grant")
		}
	}
	manage("PUT", adminKeyPath, admin, map[string]any{"group_id": privateID})
	manage("DELETE", fmt.Sprintf("/api/v1/admin/groups/%d", privateID), admin, nil)
	if v := manage("GET", keyPath, token, nil); v["group"] != nil || v["group_id"] != float64(privateID) {
		t.Fatal("deleted group exposed or historical assignment lost")
	}
}
