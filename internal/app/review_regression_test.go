package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the review regressions through real handlers, ledgers and worker timers.
func testReviewRegressions(t *testing.T, a *App, admin string) {
	ctx := context.Background()
	db := a.DB
	defer pauseTestWorkers(a)()
	oldTrusted := a.trustedProxies
	a.trustedProxies, _ = parseTrustedProxies("127.0.0.1/32")
	defer func() { a.trustedProxies = oldTrusted }()
	call := func(method, path, key string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "198.51.100.42:12345"
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, key string, body any) map[string]any {
		t.Helper()
		w := call(method, path, key, body)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("setup %s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var out struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Data
	}
	user := manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "review-user@example.test", "password": "review-password", "balance": 10})
	uid := int64(user["id"].(float64))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "review-user@example.test", "password": "review-password"})["access_token"].(string)
	var mode atomic.Int32
	healthStarted := make(chan struct{}, 1)
	releaseHealth := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			fmt.Fprint(w, `{"model":"review-chat","usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		case 2:
			fmt.Fprint(w, `{"modelVersion":"gemini-3-pro-image","candidates":[{"index":0,"finishReason":"STOP","content":{"parts":[{"text":"No image was generated."}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`)
		case 3:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"iVBORw0KGgo=\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/jpeg\",\"data\":\"/9j/4AAQ\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5}}\n\n")
		case 5:
			fmt.Fprint(w, `{"model":"review-chat","usage":{"prompt_tokens":10,"total_tokens":10}}`)
		case 6:
			fmt.Fprint(w, `{"model":"review-native","usage":{"input_tokens":10,"output_tokens":5}}`)
		case 7:
			fmt.Fprint(w, `{"id":"resp_review","object":"response","status":"cancelled","error":null,"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}]}`)
		case 8:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		case 9:
			select {
			case healthStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseHealth:
			case <-r.Context().Done():
			}
			fmt.Fprint(w, `{"choices":[{"message":{"content":"OK"}}]}`)
		case 4:
			fmt.Fprint(w, `{"id":"resp_review","object":"response","status":"failed","error":null,"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":10,"output_tokens":5}}`)
		case 10:
			var body struct{ Stream bool }
			_ = json.NewDecoder(r.Body).Decode(&body)
			usage := `{"prompt_tokens":210,"completion_tokens":1,"total_tokens":436,"completion_tokens_details":{"reasoning_tokens":225}}`
			if body.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"id\":\"chat_reasoning\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\ndata: [DONE]\n\n", usage)
			} else {
				fmt.Fprintf(w, `{"id":"chat_reasoning","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":%s}`, usage)
			}
		case 11:
			fmt.Fprint(w, `{"id":"resp_snapshot","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`)
		}
	}))
	defer up.Close()
	create := func(platform, model, protocol string) (string, int64) {
		group := manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "review-" + protocol, "platform": platform, "rate_multiplier": 1, "allow_messages_dispatch": platform == "openai"})
		gid := int64(group["id"].(float64))
		price := map[string]any{"platform": platform, "models": []string{model}, "input_price": "0.000001", "output_price": "0.000002", "cache_read_price": "0.000001", "cache_write_price": "0.000001"}
		if platform == "gemini" {
			price = map[string]any{"platform": platform, "models": []string{model}, "billing_mode": "image", "per_request_price": "0.1"}
		}
		manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "review-" + protocol, "group_ids": []int64{gid}, "billing_model_source": "requested", "model_pricing": []any{price}})
		creds := map[string]any{"api_key": "synthetic-review-secret", "base_url": up.URL, "model_mapping": map[string]string{model: model}}
		if platform != "gemini" {
			creds["api_protocol"] = protocol
		}
		account := manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "review-" + protocol, "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": creds})
		key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "review-" + protocol, "group_id": gid, "quota": 10})
		return key["key"].(string), int64(account["id"].(float64))
	}
	chat, chatID := create("openai", "review-chat", "chat_completions")
	native, _ := create("anthropic", "review-native", "anthropic")
	gemini, _ := create("gemini", "gemini-3-pro-image", "gemini")
	var chatGroup int64
	if err := db.QueryRow("SELECT group_id FROM api_keys WHERE key=$1", chat).Scan(&chatGroup); err != nil {
		t.Fatal(err)
	}
	responseKey, responseID := create("openai", "review-responses", "responses")
	t.Run("IndependentReasoningSettlesAllHTTPConversions", func(t *testing.T) {
		mode.Store(10)
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				body := map[string]any{"model": "review-chat", "stream": stream}
				if path == "/v1/responses" {
					body["input"], body["store"] = "hello", false
				} else {
					body["messages"] = []any{map[string]string{"role": "user", "content": "hello"}}
					if path == "/v1/messages" {
						body["max_tokens"] = 256
					}
				}
				w := call("POST", path, chat, body)
				var output int
				var cost string
				err := db.QueryRow("SELECT output_tokens,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&output, &cost)
				if w.Code != 200 || strings.Contains(w.Body.String(), `"error":{`) || strings.Contains(w.Body.String(), "event: error") || !strings.Contains(w.Body.String(), "OK") || err != nil || output != 226 || cost != "0.0006620000" {
					t.Fatal(path, stream, w.Code, w.Body.String(), output, cost, err)
				}
			}
		}
	})
	t.Run("MessagesSnapshotWithoutItemID", func(t *testing.T) {
		mode.Store(11)
		w := call("POST", "/v1/messages", responseKey, map[string]any{"model": "review-responses", "max_tokens": 256, "messages": []any{map[string]string{"role": "user", "content": "hello"}}})
		var output int
		if err := db.QueryRow("SELECT output_tokens FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&output); err != nil || output != 2 || w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"OK"`) {
			t.Fatal(w.Code, w.Body.String(), output, err)
		}
	})
	assertFailedUsage := func(t *testing.T, w *httptest.ResponseRecorder, cost string) {
		t.Helper()
		var usageRows, errorRows int
		var actual string
		request := w.Header().Get("X-Request-ID")
		if err := db.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE request_id=$1", request).Scan(&usageRows, &actual); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT count(*) FROM ops_error_logs WHERE request_id=$1 AND status_code=502", request).Scan(&errorRows); err != nil {
			t.Fatal(err)
		}
		if usageRows != 1 || errorRows != 1 || actual != cost {
			t.Fatal("failed request lost consumption or diagnostics", usageRows, errorRows, actual)
		}
	}
	t.Run("ChatUsageWithoutChoicesMustNotSucceed", func(t *testing.T) {
		mode.Store(1)
		w := call("POST", "/v1/chat/completions", chat, map[string]any{"model": "review-chat", "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
		var count int
		_ = db.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count)
		if w.Code != 502 || count != 1 {
			t.Errorf("invalid result or lost usage: HTTP=%d billing_rows=%d response=%s", w.Code, count, w.Body.String())
		}
		assertFailedUsage(t, w, "0.0000200000")
	})
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "draw two images"}}}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}, "imageConfig": map[string]string{"imageSize": "1K"}}}
	t.Run("GeminiTextOnlyMustNotChargeAnImage", func(t *testing.T) {
		mode.Store(2)
		w := call("POST", "/v1beta/models/gemini-3-pro-image:generateContent", gemini, body)
		var images int
		var cost string
		err := db.QueryRow("SELECT image_count,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&images, &cost)
		if err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || images != 0 || rat(json.Number(cost)).Sign() != 0 {
			t.Errorf("text-only response charged images=%d cost=%s HTTP=%d", images, cost, w.Code)
		}
	})
	t.Run("GeminiDistinctImageFramesMustCountBoth", func(t *testing.T) {
		mode.Store(3)
		w := call("POST", "/v1beta/models/gemini-3-pro-image:streamGenerateContent?alt=sse", gemini, body)
		var images int
		var cost string
		err := db.QueryRow("SELECT image_count,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&images, &cost)
		if err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || images != 2 || cost != "0.2000000000" {
			t.Errorf("two different image frames charged images=%d cost=%s HTTP=%d", images, cost, w.Code)
		}
	})
	t.Run("FailedResponseMustNotRecoverAccount", func(t *testing.T) {
		mode.Store(4)
		if _, err := db.Exec("UPDATE accounts SET status='error',error_message='upstream failed',updated_at=now() WHERE id=$1", responseID); err != nil {
			t.Fatal(err)
		}
		w := call("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/test", responseID), admin, map[string]any{"model_id": "review-responses"})
		var status string
		_ = db.QueryRow("SELECT status FROM accounts WHERE id=$1", responseID).Scan(&status)
		if status != "error" {
			t.Errorf("failed provider response recovered account=%s, probe=%s", status, w.Body.String())
		}
	})
	t.Run("ChatUsageOnlySSEMustNotSucceed", func(t *testing.T) {
		mode.Store(8)
		w := call("POST", "/v1/chat/completions", chat, map[string]any{"model": "review-chat", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": true})
		if w.Code == 200 && !strings.Contains(w.Body.String(), `"error"`) {
			t.Errorf("usage-only SSE reported success: %s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "[DONE]") {
			t.Error("invalid stream emitted success terminal")
		}
		assertFailedUsage(t, w, "0.0000200000")
	})
	t.Run("EmbeddingWithoutDataMustNotSucceed", func(t *testing.T) {
		mode.Store(5)
		w := call("POST", "/v1/embeddings", chat, map[string]any{"model": "review-chat", "input": "hello"})
		if w.Code == 200 {
			t.Errorf("embedding without vectors reported success: %s", w.Body.String())
		}
		assertFailedUsage(t, w, "0.0000100000")
	})
	t.Run("MessagesWithoutContentMustNotSucceed", func(t *testing.T) {
		mode.Store(6)
		w := call("POST", "/v1/messages", native, map[string]any{"model": "review-native", "max_tokens": 20, "messages": []any{map[string]string{"role": "user", "content": "hi"}}})
		if w.Code == 200 {
			t.Errorf("message without content reported success: %s", w.Body.String())
		}
		assertFailedUsage(t, w, "0.0000200000")
	})
	t.Run("CancelledResponseMustNotRecoverAccount", func(t *testing.T) {
		mode.Store(7)
		if _, err := db.Exec("UPDATE accounts SET status='error',updated_at=now() WHERE id=$1", responseID); err != nil {
			t.Fatal(err)
		}
		w := call("POST", fmt.Sprintf("/api/v1/admin/accounts/%d/test", responseID), admin, map[string]any{"model_id": "review-responses"})
		var status string
		if err := db.QueryRow("SELECT status FROM accounts WHERE id=$1", responseID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "error" {
			t.Errorf("cancelled response recovered account=%s body=%s", status, w.Body.String())
		}
	})
	t.Run("ReverseProxyBlacklistMustApply", func(t *testing.T) {
		key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "review-blacklist", "group_id": chatGroup, "ip_blacklist": []string{"198.51.100.42"}})["key"].(string)
		r := httptest.NewRequest("GET", "/v1/usage", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Forwarded-For", "198.51.100.42")
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("blacklisted client through reverse proxy accepted: HTTP %d", w.Code)
		}
	})
	t.Run("ReverseProxyIPRestriction", func(t *testing.T) {
		// A trusted reverse proxy resolves the allowlisted client, not its socket peer.
		key := manage("POST", "/api/v1/keys", token, map[string]any{"name": "review-ip", "group_id": chatGroup, "ip_whitelist": []string{"198.51.100.42"}})["key"].(string)
		r := httptest.NewRequest("GET", "/v1/usage", nil)
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Forwarded-For", "198.51.100.42")
		r.Header.Set("X-Real-IP", "198.51.100.42")
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("unexpected observation %d %s", w.Code, w.Body.String())
		}
	})
	t.Run("ProxyLoginAndAudit", func(t *testing.T) {
		request := func(ip string, body any) *httptest.ResponseRecorder {
			raw, _ := json.Marshal(body)
			r := httptest.NewRequest("POST", "/api/v1/auth/login", bytes.NewReader(raw))
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("X-Forwarded-For", ip)
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			return w
		}
		for i := range 10 {
			w := request("198.51.100.51", map[string]string{"email": fmt.Sprintf("absent-%d@example.test", i), "password": "wrong"})
			if w.Code != 401 {
				t.Fatal("login attempt", i, w.Code)
			}
		}
		if w := request("198.51.100.51", map[string]string{"email": "absent-final@example.test", "password": "wrong"}); w.Code != 429 {
			t.Fatal("client IP not rate limited", w.Code)
		}
		w := request("198.51.100.52", map[string]string{"email": "review-user@example.test", "password": "review-password"})
		if w.Code != 200 {
			t.Fatal("different client shares login IP bucket", w.Code)
		}
		var ip string
		if err := db.QueryRow("SELECT client_ip FROM audit_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&ip); err != nil || ip != "198.51.100.52" {
			t.Fatal("audit source", ip, err)
		}
	})
	t.Run("FailedScheduledResponseMustNotRecoverAccount", func(t *testing.T) {
		mode.Store(4)
		var plan testPlan
		if err := db.QueryRow("INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,auto_recover,next_run_at) VALUES($1,'review-responses','* * * * *',true,true,now()) RETURNING id,updated_at", responseID).Scan(&plan.ID, &plan.Updated); err != nil {
			t.Fatal(err)
		}
		defer db.Exec("DELETE FROM scheduled_test_plans WHERE id=$1", plan.ID)
		plan.AccountID, plan.Model, plan.Cron, plan.AutoRecover = responseID, "review-responses", "* * * * *", true
		if err := a.runTestPlan(ctx, plan); err != nil {
			t.Fatal(err)
		}
		var status, result string
		if err := db.QueryRow("SELECT a.status,r.status FROM accounts a JOIN scheduled_test_plans p ON p.account_id=a.id JOIN scheduled_test_results r ON r.plan_id=p.id WHERE p.id=$1", plan.ID).Scan(&status, &result); err != nil || status != "error" || result != "failed" {
			t.Fatal("scheduled response recovered account", status, result, err)
		}
	})
	t.Run("SlowHealthPlanMustNotBlockBillingRecovery", func(t *testing.T) {
		mode.Store(9)
		if _, err := db.Exec("INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,next_run_at) VALUES($1,'review-chat','* * * * *',true,now()-interval '1 minute')", chatID); err != nil {
			t.Fatal(err)
		}
		defer db.Exec("DELETE FROM scheduled_test_plans WHERE account_id=$1", chatID)
		a.startWorkers()
		defer func() { _ = pauseTestWorkers(a); close(releaseHealth) }()
		select {
		case <-healthStarted:
		case <-time.After(20 * time.Second):
			t.Fatal("health plan did not start")
		}
		var kid, gid int64
		if err := db.QueryRow("SELECT id,group_id FROM api_keys WHERE key=$1", chat).Scan(&kid, &gid); err != nil {
			t.Fatal(err)
		}
		receipt := usageReceipt{RequestID: "review-blocked-recovery", UserID: uid, KeyID: kid, AccountID: chatID, GroupID: gid, Platform: "openai", Model: "review-chat", Usage: priceUsage{Input: 1}, UserRate: "1", AccountRate: "1", AccountRaw: "0.1", AccountDebit: "0.1", At: time.Now(), BillingMode: "token", Cost: priceCost{Input: "0.1", Output: "0", CacheWrite: "0", CacheRead: "0", ImageInput: "0", ImageOutput: "0", Total: "0.1", Actual: "0.1", Debit: "0.1"}}
		receipt.Fingerprint = receipt.fingerprint()
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.Redis.HSet(ctx, "gateway:pending-billing", receipt.RequestID, string(raw)).Err(); err != nil {
			t.Fatal(err)
		}
		var count int
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if err = db.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", receipt.RequestID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count == 1 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Direct recovery succeeds, proving the receipt itself is valid.
		if err = a.recoverReceipts(ctx); err != nil {
			t.Fatal(err)
		}
		var after int
		if err = db.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", receipt.RequestID).Scan(&after); err != nil || after != 1 {
			t.Fatalf("invalid recovery fixture count=%d err=%v", after, err)
		}
		if count != 1 {
			t.Error("billing still pending after a complete 15-second worker period while health check blocks; direct recovery succeeded")
		}
	})
}
