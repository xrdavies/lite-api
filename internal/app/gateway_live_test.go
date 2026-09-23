package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Optional paid verification. Run only against the disposable integration database;
// the upstream credential is supplied in the process environment and never logged.
func testLiveGateway(t *testing.T, a *App, admin string) {
	t.Helper()
	secret := os.Getenv("TEST_UPSTREAM_API_KEY")
	if secret == "" {
		return
	}
	base, model := os.Getenv("TEST_UPSTREAM_BASE_URL"), os.Getenv("TEST_UPSTREAM_MODEL")
	protocol := os.Getenv("TEST_UPSTREAM_PROTOCOL")
	if protocol == "" {
		protocol = "chat_completions"
	}
	if protocol != "chat_completions" && protocol != "responses" {
		t.Fatal("live gateway verification supports chat_completions or responses")
	}
	if base == "" || model == "" {
		t.Fatal("live gateway verification requires a base URL and model")
	}
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	client := &http.Client{Timeout: 90 * time.Second}
	call := func(method, path, token string, body any) []byte {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("live verification HTTP request failed")
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil || resp.StatusCode != 200 {
			var failure struct {
				Error struct{ Message string } `json:"error"`
			}
			_ = json.Unmarshal(data, &failure)
			message := strings.ReplaceAll(strings.ReplaceAll(failure.Error.Message, secret, "[redacted]"), token, "[redacted]")
			t.Fatalf("live verification %s %s failed (HTTP %d): %s", method, path, resp.StatusCode, message)
		}
		return data
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(call(method, path, token, body), &envelope); err != nil || envelope.Data == nil {
			t.Fatal("invalid management response during live verification")
		}
		return envelope.Data
	}
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "live-gateway@example.test", "password": "local-test-password", "balance": 10})
	uid := int64(user["id"].(float64))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "live-gateway@example.test", "password": "local-test-password"})["access_token"].(string)
	gid := int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Live verification", "platform": "openai"})["id"].(float64))
	// These are test tariffs, not the provider's advertised prices.
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Live test tariff", "group_ids": []int64{gid}, "billing_model_source": "requested", "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{model}, "input_price": json.Number("0.000001"), "output_price": json.Number("0.000002"), "cache_read_price": json.Number("0.0000005"), "cache_write_price": json.Number("0.000001")}}})
	manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Live verification account", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": secret, "base_url": base, "api_protocol": protocol}})
	key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "Live verification key", "group_id": gid, "quota": 10})["key"].(string)
	for _, stream := range []bool{false, true} {
		started := time.Now()
		path := "/v1/chat/completions"
		body := map[string]any{"model": model, "stream": stream, "max_completion_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "Reply only OK."}}}
		if protocol == "responses" {
			path = "/v1/responses"
			body = map[string]any{"model": model, "stream": stream, "max_output_tokens": 64, "input": "Reply only OK.", "store": false}
		}
		data := call("POST", path, key, body)
		if stream {
			completed := bytes.Contains(data, []byte("data: [DONE]"))
			failed := bytes.Contains(data, []byte(`"error"`))
			if protocol == "responses" {
				completed = bytes.Contains(data, []byte("event: response.completed")) || bytes.Contains(data, []byte("event: response.incomplete"))
				failed = bytes.Contains(data, []byte("event: error")) || bytes.Contains(data, []byte("event: response.failed"))
			}
			if !completed || failed || !bytes.Contains(data, []byte(`"usage"`)) {
				t.Fatal("live SSE did not complete with usage")
			}
		} else {
			var result struct {
				Usage json.RawMessage `json:"usage"`
			}
			if json.Unmarshal(data, &result) != nil || result.Usage == nil {
				t.Fatal("live JSON response has no usage")
			}
		}
		if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(key)) {
			t.Fatal("live response leaked a credential")
		}
		t.Logf("live gateway protocol=%s stream=%v completed in %d ms", protocol, stream, time.Since(started).Milliseconds())
	}
	var count int
	var matched bool
	err := a.DB.QueryRow(`SELECT count(*),bool_and(total_cost=input_tokens*0.000001+output_tokens*0.000002+cache_read_tokens*0.0000005+cache_creation_tokens*0.000001 AND actual_cost=total_cost) AND sum(round(actual_cost,8))=(SELECT 10-balance FROM users WHERE id=$1) AND sum(round(actual_cost,8))=(SELECT quota_used FROM api_keys WHERE key=$2) FROM usage_logs WHERE user_id=$1`, uid, key).Scan(&count, &matched)
	if err != nil || count != 2 || !matched {
		t.Fatal("live usage, prices, balance and key counters did not reconcile")
	}
	usage := manage("GET", "/api/v1/usage", token, nil)
	if usage["total"].(float64) != 2 {
		t.Fatal("live user usage query did not return both requests")
	}
	// The disposable database is deleted by the runner after this function returns.
	t.Logf("live gateway %s: JSON/SSE and both settlements reconciled", strings.TrimSpace(model))
}
