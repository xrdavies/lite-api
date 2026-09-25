package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLocalInputTokens(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{"model":"gpt-5","input":[{"role":"user","content":"hello world"}]}`, 6},
		{`{"model":"gpt-4.1","input":[{"role":"user","content":[{"type":"input_text","text":"first line"},{"type":"input_text","text":"second line"}]},{"type":"function_call_output","call_id":"call_123","output":"{\"ok\":true}"}]}`, 24},
		{`{"model":"grok-4","input":[]}`, 1},
	} {
		n, err := estimateInputTokens([]byte(tc.raw))
		if err != nil || n != tc.want {
			t.Fatal(n, tc.want, err)
		}
	}
	for _, model := range []string{"gpt-3.5", "gpt-4", "gpt-4o", "kimi-k2", "deepseek-chat"} {
		raw := mustJSON(map[string]any{"model": model, "input": strings.Repeat("你好世界 🌍 hello ", 800)})
		n, err := estimateInputTokens(raw)
		if err != nil || n < 800 {
			t.Fatal("long multilingual input", model, n, err)
		}
	}
	countImage := func(data string) int {
		t.Helper()
		raw := mustJSON(map[string]any{"model": "grok-4", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + data}}}}})
		n, err := estimateInputTokens(raw)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if countImage("AAAA") != countImage(strings.Repeat("AAAA", 100000)) {
		t.Fatal("image bytes were tokenized")
	}
	for _, raw := range []string{`null`, `{"input":[null]}`, `{"input":1}`, `{"input":[],"tools":true}`} {
		if _, err := estimateInputTokens([]byte(raw)); err == nil {
			t.Fatal("invalid count input", raw)
		}
	}
	// RawMessage preserves malformed bytes inside tool schemas. Reject these
	// before finding UTF-8 chunk boundaries, including long continuation runs.
	malformed := `{"input":[],"tools":[{"type":"function","description":"` + strings.Repeat("\x80", 5000) + `"}]}`
	if _, err := estimateInputTokens([]byte(malformed)); err == nil {
		t.Fatal("invalid UTF-8 in tool schema accepted")
	}
	for i := 0; i < 8; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			if n, err := estimateInputTokens([]byte(`{"model":"grok-4","input":"hello world"}`)); err != nil || n != 2 {
				t.Fatal(n, err)
			}
		})
	}
}

func testLocalTokenCounting(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, key string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.213:1234"
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, key string, body any) map[string]any {
		t.Helper()
		w := call(method, path, key, body, "")
		var result struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return result.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "local-count@example.test", "password": "local-count-password", "balance": 100}))
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "local-count@example.test", "password": "local-count-password"})["access_token"].(string)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	body := map[string]any{"model": "count-alias", "system": "Count accurately.", "messages": []any{map[string]any{"role": "user", "content": "Hello 你好"}}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}}
	expect := func(key string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := call("POST", "/messages/count_tokens", key, body, "")
		if w.Code != want {
			t.Fatalf("local count: %d %s; want %d", w.Code, w.Body.String(), want)
		}
		if want == 200 {
			var result struct {
				Input int `json:"input_tokens"`
			}
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Input <= 0 {
				t.Fatal("invalid estimate", w.Body.String())
			}
		}
		return w
	}
	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"actual-model"}]}`)
			return
		}
		upstreamCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer up.Close()
	for _, platform := range []string{"grok", "kimi", "zhipu", "deepseek", "minimax"} {
		gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Local count " + platform, "platform": platform}))
		gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		keyObject := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Local count", "group_id": gid})
		key, kid := keyObject["key"].(string), id(keyObject)
		if platform == "grok" {
			exec("UPDATE users SET balance=0 WHERE id=$1", uid)
			exec("UPDATE api_keys SET quota=1,quota_used=1,status='quota_exhausted' WHERE id=$1", kid)
			expect(key, 200) // No account, balance or remaining spending quota is needed.
		} else {
			expect(key, 503)
		}
		exec("UPDATE users SET balance=100 WHERE id=$1", uid)
		exec("UPDATE api_keys SET quota=0,quota_used=0,status='active' WHERE id=$1", kid)
		aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Local count " + platform, "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "unused-count-secret", "base_url": up.URL, "model_mapping": map[string]string{"count-alias": "actual-model"}}}))
		ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
		for _, protocol := range []string{"chat_completions", "responses", "anthropic"} {
			if platform == "grok" && protocol == "anthropic" {
				continue
			}
			manage("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": protocol}})
			expect(key, 200)
			native := call("POST", "/responses/input_tokens", key, map[string]any{"model": "count-alias", "input": "hello world"}, "")
			if native.Code != 200 || !strings.Contains(native.Body.String(), `"input_tokens":2`) {
				t.Fatal("native local count", platform, protocol, native.Code, native.Body.String())
			}
		}
		w := call("POST", "/v1/messages/count_tokens", key, body, "local-replay")
		replay := call("POST", "/messages/count_tokens", key, body, "local-replay")
		if w.Code != 200 || replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || w.Body.String() != replay.Body.String() {
			t.Fatal("local replay", w.Code, replay.Code)
		}
		if w := call("POST", "/messages/count_tokens", user, body, ""); w.Code != 401 {
			t.Fatal("JWT count", w.Code)
		}
		manage("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"not-count-alias"}}})
		expect(key, 403)
		manage("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
		exec("UPDATE api_keys SET status='inactive' WHERE id=$1", kid)
		expect(key, 401)
		exec("UPDATE api_keys SET status='active' WHERE id=$1", kid)
		manage("POST", ap+"/schedulable", admin, map[string]any{"schedulable": false})
		if platform == "grok" {
			expect(key, 200)
		} else {
			expect(key, 503)
		}
		manage("POST", ap+"/schedulable", admin, map[string]any{"schedulable": true})
		if platform != "grok" {
			exec("UPDATE users SET balance=0 WHERE id=$1", uid)
			expect(key, 402)
			exec("UPDATE users SET balance=100 WHERE id=$1", uid)
			exec("UPDATE api_keys SET quota=1,quota_used=1 WHERE id=$1", kid)
			expect(key, 429)
			exec("UPDATE api_keys SET quota=0,quota_used=0 WHERE id=$1", kid)
		}
		cgid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite count " + platform, "platform": "composite"}))
		manage("PUT", ap, admin, map[string]any{"group_ids": []int64{gid, cgid}})
		manage("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", cgid), admin, map[string]any{"public_model": "count-alias", "match_type": "exact", "target_platform": platform, "upstream_model": "count-alias", "endpoint": "count_tokens", "enabled": true})
		ckey := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Composite count", "group_id": cgid})["key"].(string)
		expect(ckey, 200)
		catalog := call("GET", "/v1/models", ckey, nil, "")
		if catalog.Code != 200 || !strings.Contains(catalog.Body.String(), `"id":"count-alias"`) {
			t.Fatal("count-only route missing from models", platform, catalog.Code, catalog.Body.String())
		}
		for _, invalid := range []any{nil, "invalid", []any{map[string]any{"role": "unknown", "content": "hi"}}} {
			if w := call("POST", "/messages/count_tokens", key, map[string]any{"model": "count-alias", "messages": invalid}, ""); w.Code != 400 {
				t.Fatal("invalid local input accepted", platform, w.Code)
			}
		}
		exec("UPDATE api_keys SET expires_at=now()-interval '1 minute' WHERE id=$1", kid)
		expect(key, 401)
		exec("UPDATE api_keys SET expires_at=NULL WHERE id=$1", kid)
		exec("UPDATE users SET status='inactive' WHERE id=$1", uid)
		expect(key, 401)
		exec("UPDATE users SET status='active' WHERE id=$1", uid)
		if platform == "grok" {
			sgid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Fallback local count", "platform": "openai", "claude_code_only": true, "fallback_group_id": gid}))
			skey := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Fallback count", "group_id": sgid})["key"].(string)
			exec("UPDATE users SET balance=0 WHERE id=$1", uid)
			expect(skey, 200)
			exec("UPDATE users SET balance=100 WHERE id=$1", uid)
		}
	}
	var count int
	var balance, used string
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&count); err != nil || count != 0 {
		t.Fatal("local count wrote usage", count, err)
	}
	if err := a.DB.QueryRow("SELECT balance::text,(SELECT sum(quota_used)::text FROM api_keys WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&balance, &used); err != nil || balance != "100.00000000" || rat(json.Number(used)).Sign() != 0 {
		t.Fatal("local count billed", balance, used, err)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatal("local counting called upstream", upstreamCalls.Load())
	}
}
