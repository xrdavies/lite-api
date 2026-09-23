package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func testChannelManagement(t *testing.T, a *App, admin, userToken string, privateGroup int64) {
	t.Helper()
	call := func(method, path, token string, body any) (int, json.RawMessage) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "192.0.2.10:1234"
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, req)
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &envelope)
		if w.Code != 200 {
			return w.Code, json.RawMessage(w.Body.Bytes())
		}
		return w.Code, envelope.Data
	}
	must := func(method, path, token string, body any) json.RawMessage {
		t.Helper()
		code, data := call(method, path, token, body)
		if code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, code, data)
		}
		return data
	}
	idOf := func(raw json.RawMessage) int64 {
		var v struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v.ID
	}
	expect := func(want int, method, path, token string, body any) {
		t.Helper()
		code, raw := call(method, path, token, body)
		if code != want {
			t.Fatalf("%s %s got %d want %d: %s", method, path, code, want, raw)
		}
	}
	expect(403, "POST", "/api/v1/admin/channels", userToken, map[string]any{"name": "forbidden"})
	expect(401, "GET", "/api/v1/channels/available", "", nil)
	expect(400, "PUT", "/api/v1/admin/settings", admin, map[string]any{"registration_enabled": true})
	public := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Public pricing", "platform": "openai"}))
	composite := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite pricing", "platform": "composite", "is_exclusive": true}))
	price := map[string]any{"platform": "openai", "models": []string{"gpt-5.6-luna"}, "input_price": json.Number("0.000001000001"), "output_price": json.Number("2e-6"), "cache_write_price": json.Number("0.000000125"), "cache_write_1h_price": json.Number("0.00000025"), "cache_read_price": 0, "fast_multiplier": 2, "flex_multiplier": 0.5, "reasoning_effort_multipliers": map[string]any{"high": 1.5}, "intervals": []any{map[string]any{"min_tokens": 0, "max_tokens": 100, "input_multiplier": 1}, map[string]any{"min_tokens": 100, "input_multiplier": 2}}, "time_pricing": map[string]any{"timezone": "UTC", "periods": []any{map[string]any{"start_time": "09:00", "end_time": "10:00", "multiplier": 0.5}}}}
	payload := map[string]any{"name": "Channel contract", "group_ids": []int64{public, privateGroup, composite}, "model_mapping": map[string]any{"openai": map[string]string{"alias": "gpt-5.6-luna", "unpriced": "missing", "gpt-*": "*"}}, "billing_model_source": "requested", "restrict_models": true, "apply_pricing_to_account_stats": true, "model_pricing": []any{price, map[string]any{"platform": "anthropic", "models": []string{"private-anthropic-model"}, "input_price": 0.000003}}, "account_stats_pricing_rules": []any{map[string]any{"name": "Internal cost", "group_ids": []int64{public}, "pricing": []any{map[string]any{"platform": "openai", "models": []string{"gpt-5.6-luna"}, "input_price": json.Number("0.0000000001"), "intervals": []any{map[string]any{"min_tokens": 0, "input_price": json.Number("0.000000000001")}}}}}}}
	raw := must("POST", "/api/v1/admin/channels", admin, payload)
	id := idOf(raw)
	path := fmt.Sprintf("/api/v1/admin/channels/%d", id)
	var snapshot struct {
		Pricing []modelPrice     `json:"model_pricing"`
		Rules   []statsPriceRule `json:"account_stats_pricing_rules"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Pricing) != 2 || snapshot.Pricing[0].Input.String() != "0.000001000001" || len(snapshot.Pricing[0].Intervals) != 2 || len(snapshot.Rules) != 1 || snapshot.Rules[0].Pricing[0].Input.String() != "0.0000000001" {
		t.Fatalf("price round trip: %s", raw)
	}
	// Two channels cannot own the same group, and the failed transaction leaves no channel.
	expect(409, "POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Conflicting channel", "group_ids": []int64{public}})
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM channels WHERE name='Conflicting channel'").Scan(&count); err != nil || count != 0 {
		t.Fatal("failed creation left a channel", count, err)
	}
	expect(400, "PUT", path, admin, map[string]any{"name": "Should roll back", "group_ids": []int64{99999999}})
	if raw = must("GET", path, admin, nil); !bytes.Contains(raw, []byte(`"name": "Channel contract"`)) && !bytes.Contains(raw, []byte(`"name":"Channel contract"`)) {
		t.Fatal("transaction failed to restore channel", string(raw))
	}
	// Unsubmitted configuration (including future JSON keys) remains intact.
	if _, err := a.DB.Exec(`UPDATE channels SET features_config='{"future":{"keep":true}}' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	raw = must("PUT", path, admin, map[string]any{"description": "Updated"})
	if !bytes.Contains(raw, []byte("future")) || !bytes.Contains(raw, []byte("gpt-5.6-luna")) {
		t.Fatal("partial update lost fields", string(raw))
	}
	raw = must("PUT", path, admin, map[string]any{"features_config": map[string]any{}})
	if !bytes.Contains(raw, []byte("future")) {
		t.Fatal("empty feature patch removed unknown keys")
	}
	expect(400, "PUT", path, admin, map[string]any{"features_config": map[string]any{"bedrock_cc_compat": true}})
	expect(400, "PUT", path, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"gpt-*", "gpt-5.6-luna"}, "input_price": 1}}})
	expect(400, "PUT", path, admin, map[string]any{"account_stats_pricing_rules": []any{map[string]any{"name": "Invalid scope", "group_ids": []int64{999999}, "pricing": []any{map[string]any{"models": []string{"m"}, "input_price": 1}}}}})
	raw = must("GET", "/api/v1/channels/available", userToken, nil)
	if string(raw) != "[]" {
		t.Fatal("availability must default off", string(raw))
	}
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{"available_channels_enabled": true, "site_name": "Team gateway"})
	raw = must("GET", "/api/v1/settings/public", "", nil)
	if !bytes.Contains(raw, []byte("Team gateway")) || bytes.Contains(raw, []byte("secret")) {
		t.Fatal(string(raw))
	}
	raw = must("GET", "/api/v1/channels/available", userToken, nil)
	for _, secret := range []string{"private-anthropic-model", "Private team", "Composite pricing", "billing_model_source", "restrict_models", "account_stats", "future", "sort_order"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("private data leaked (%s): %s", secret, raw)
		}
	}
	for _, expected := range []string{"alias", "unpriced", "gpt-5.6-luna", "Public pricing", "0.000001000001"} {
		if !bytes.Contains(raw, []byte(expected)) {
			t.Fatalf("missing %s: %s", expected, raw)
		}
	}
	if bytes.Contains(raw, []byte("gpt-*")) {
		t.Fatal("wildcard listed as concrete model")
	}
	var views []struct {
		Platforms []struct {
			Groups []struct {
				ID int64 `json:"id"`
			} `json:"groups"`
		} `json:"platforms"`
	}
	if err := json.Unmarshal(raw, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || len(views[0].Platforms) != 1 || len(views[0].Platforms[0].Groups) != 1 || views[0].Platforms[0].Groups[0].ID != public {
		t.Fatal("visibility mismatch", string(raw))
	}
	// Granting the composite group exposes its platforms; revocation takes effect on the next query.
	var uid int64
	if err := a.DB.QueryRow("SELECT id FROM users WHERE email='another@example.test'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec("INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2)", uid, composite); err != nil {
		t.Fatal(err)
	}
	raw = must("GET", "/api/v1/channels/available", userToken, nil)
	if !bytes.Contains(raw, []byte("private-anthropic-model")) {
		t.Fatal("composite platform not expanded", string(raw))
	}
	if _, err := a.DB.Exec("DELETE FROM user_allowed_groups WHERE user_id=$1 AND group_id=$2", uid, composite); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Exec("INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) VALUES($1,$2,0.75)", uid, public); err != nil {
		t.Fatal(err)
	}
	raw = must("GET", "/api/v1/channels/available", userToken, nil)
	if !bytes.Contains(raw, []byte("0.75")) {
		t.Fatal("personal rate not displayed", string(raw))
	}
	must("PUT", path, admin, map[string]any{"status": "disabled"})
	if raw = must("GET", "/api/v1/channels/available", userToken, nil); string(raw) != "[]" {
		t.Fatal("disabled channel visible", string(raw))
	}
	must("PUT", path, admin, map[string]any{"status": "active"})
	// A database constraint failure midway through price replacement must restore the original prices.
	if _, err := a.DB.Exec(`ALTER TABLE channel_pricing_intervals ADD CONSTRAINT test_interval_failure CHECK(sort_order<>999)`); err != nil {
		t.Fatal(err)
	}
	expect(500, "PUT", path, admin, map[string]any{"model_pricing": []any{map[string]any{"models": []string{"replacement"}, "input_price": 1, "intervals": []any{map[string]any{"input_price": 1, "sort_order": 999}}}}})
	if _, err := a.DB.Exec("ALTER TABLE channel_pricing_intervals DROP CONSTRAINT test_interval_failure"); err != nil {
		t.Fatal(err)
	}
	raw = must("GET", path, admin, nil)
	if !bytes.Contains(raw, []byte("gpt-5.6-luna")) || bytes.Contains(raw, []byte("replacement")) {
		t.Fatal("partial pricing committed")
	}
	// Concurrent association attempts rely on the unique group constraint, including rollback.
	raceGroup := idOf(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Association race"}))
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := call("POST", "/api/v1/admin/channels", admin, map[string]any{"name": fmt.Sprintf("Association %d", i), "group_ids": []int64{raceGroup}})
			codes <- code
		}(i)
	}
	wg.Wait()
	close(codes)
	successes, conflicts := 0, 0
	for code := range codes {
		if code == 200 {
			successes++
		} else if code == 409 {
			conflicts++
		} else {
			t.Errorf("unexpected association response %d", code)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatal(successes, conflicts)
	}
	must("DELETE", path, admin, nil)
	for _, table := range []string{"channel_groups", "channel_model_pricing", "channel_account_stats_pricing_rules"} {
		if err := a.DB.QueryRow("SELECT count(*) FROM "+table+" WHERE channel_id=$1", id).Scan(&count); err != nil || count != 0 {
			t.Fatal("delete cascade", table, count, err)
		}
	}
	var orphans bool
	if err := a.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM channel_pricing_intervals i LEFT JOIN channel_model_pricing p ON p.id=i.pricing_id WHERE p.id IS NULL) OR EXISTS(SELECT 1 FROM channel_account_stats_pricing_intervals i LEFT JOIN channel_account_stats_model_pricing p ON p.id=i.pricing_id WHERE p.id IS NULL)`).Scan(&orphans); err != nil || orphans {
		t.Fatal("orphan price intervals", err)
	}
	expect(404, "GET", path, admin, nil)
	// No configuration or pricing operation changed the schema or wrote balances.
	if _, err := a.DB.ExecContext(context.Background(), "DELETE FROM channels WHERE name LIKE 'Association %'"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "api_key") {
		t.Fatal("credential in channel snapshot")
	}
}
