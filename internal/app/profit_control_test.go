package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProfitControl(t *testing.T) {
	g := &gatewayIdentity{Group: gatewayGroup{Platform: "openai", Rate: "0.5", profitPolicy: profitPolicy{Enabled: true, Margin: "0.3", Buffer: "0.05"}}}
	threshold := g.profitThreshold(textRequest{Protocol: "responses", ResponseImage: &responseImageConfig{}})
	if threshold == nil || threshold.Cmp(big.NewRat(325, 1000)) != 0 {
		t.Fatal("wrong exact threshold", threshold)
	}
	for _, tc := range []struct {
		rate json.Number
		want bool
	}{{"0", true}, {"0.3250", true}, {"0.3251", false}, {"-1", false}, {"NaN", false}, {"", false}} {
		if profitAllows(threshold, tc.rate) != tc.want {
			t.Fatal("threshold boundary", tc)
		}
	}
	for _, protocol := range []string{"images", "videos", "seedance", "realtime", "tts", "stt", "alpha_search", "web_search", "embeddings", ""} {
		if g.profitThreshold(textRequest{Protocol: protocol}) != nil {
			t.Fatal("nontext gate", protocol)
		}
	}
	if g.profitThreshold(textRequest{Protocol: "anthropic", CountOnly: true}) != nil {
		t.Fatal("count gate")
	}
	for _, platform := range []string{"composite", "kimi", "deepseek", "zhipu", "minimax"} {
		g.SourcePlatform = platform
		if g.profitThreshold(textRequest{Protocol: "responses"}) != nil {
			t.Fatal("unsupported source gained a gate after target resolution", platform)
		}
	}
	g.SourcePlatform = "composite"
	g.RoutingGroup = &gatewayGroup{Platform: "anthropic", Rate: "9", profitPolicy: profitPolicy{Enabled: true, Margin: "0.2", Buffer: "0"}}
	if got := g.profitThreshold(textRequest{Protocol: "anthropic"}); got == nil || got.Cmp(big.NewRat(4, 10)) != 0 {
		t.Fatal("fallback used routing group billing rate", got)
	}
	g.RoutingGroup.Enabled = false
	if g.profitThreshold(textRequest{Protocol: "anthropic"}) != nil {
		t.Fatal("source gate leaked into disabled fallback")
	}
	for _, n := range []json.Number{"0", "0.9999", "0.2500", "1e-4"} {
		if !validProfitRatio(n) {
			t.Fatal("valid ratio", n)
		}
	}
	for _, n := range []json.Number{"-1", "1", "0.99999", "NaN", "1e99"} {
		if validProfitRatio(n) {
			t.Fatal("invalid ratio", n)
		}
	}
}

func testProfitControl(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.211:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "profit@example.test", "password": "profit-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "profit@example.test", "password": "profit-password"})["access_token"].(string)
	var calls, rejects, mutate atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer profit-reject" || r.Header.Get("X-Api-Key") == "profit-reject" || r.Header.Get("X-Goog-Api-Key") == "profit-reject" {
			rejects.Add(1)
			w.WriteHeader(503)
			return
		}
		if gid := mutate.Swap(0); gid != 0 {
			if _, err := a.DB.Exec("UPDATE groups SET profit_min_margin=0.99,rate_multiplier=9 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var result, stream string
		switch {
		case strings.HasSuffix(r.URL.Path, "count_tokens"):
			fmt.Fprint(w, `{"input_tokens":10}`)
			return
		case strings.HasSuffix(r.URL.Path, "countTokens"):
			fmt.Fprint(w, `{"totalTokens":10}`)
			return
		case strings.HasPrefix(r.URL.Path, "/v1beta/"):
			result = `{"modelVersion":"profit-model","candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}`
			stream = "data: " + result + "\n\n"
		case r.URL.Path == "/v1/messages":
			result = fmt.Sprintf(`{"id":"msg_profit_%d","type":"message","role":"assistant","model":"profit-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`, n)
		case r.URL.Path == "/v1/responses":
			result = fmt.Sprintf(`{"id":"resp_profit_%d","object":"response","status":"completed","model":"profit-model","output":[],"usage":{"input_tokens":10,"output_tokens":5}}`, n)
			stream = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + result + "}\n\n"
		default:
			result = fmt.Sprintf(`{"id":"chat_profit_%d","model":"profit-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, n)
			stream = "data: " + result + "\n\ndata: [DONE]\n\n"
		}
		if string(body["stream"]) == "true" || strings.HasSuffix(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, stream)
		} else {
			fmt.Fprint(w, result)
		}
	}))
	defer up.Close()
	for _, platform := range []string{"openai", "anthropic", "gemini", "grok"} {
		price := []modelPrice{{Platform: platform, Models: []string{"profit-model"}, Input: number("0.001"), Output: number("0.002"), CacheRead: number("0"), CacheWrite: number("0")}}
		group := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Profit " + platform, "platform": platform, "rate_multiplier": "2", "profit_control_enabled": true, "profit_min_margin": "0.2", "profit_safety_buffer": "0.1", "model_pricing": price})
		gid := id(group)
		gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		for _, patch := range []any{map[string]any{}, map[string]any{"profit_control_enabled": nil, "profit_min_margin": nil, "profit_safety_buffer": nil}} {
			v := must("PUT", gp, admin, patch)
			if v["profit_control_enabled"] != true || v["profit_min_margin"] != 0.2 || v["profit_safety_buffer"] != 0.1 {
				t.Fatal("profit partial update", v)
			}
		}
		for _, value := range []any{-1, 1, "0.9000", "0.20001"} {
			w := call("PUT", gp, admin, map[string]any{"rate_multiplier": "9", "profit_min_margin": value}, "")
			if w.Code != 400 || must("GET", gp, admin, nil)["rate_multiplier"] != float64(2) {
				t.Fatal("invalid merged profit partially stored", value, w.Code)
			}
		}
		must("PUT", fmt.Sprintf("/api/v1/admin/users/%d", uid), admin, map[string]any{"group_rates": map[string]any{fmt.Sprint(gid): "0.5"}})
		wire := "responses"
		if platform == "anthropic" || platform == "gemini" {
			wire = platform
		}
		if platform == "gemini" {
			wire = ""
		}
		newAccount := func(name, rate, secret string, priority int) int64 {
			credentials := map[string]any{"api_key": secret, "base_url": up.URL}
			if wire != "" {
				credentials["api_protocol"] = wire
			}
			return id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Profit " + platform + " " + name, "platform": platform, "type": "apikey", "concurrency": 1, "priority": priority, "rate_multiplier": rate, "group_ids": []int64{gid}, "credentials": credentials}))
		}
		expensive := newAccount("expensive", "0.3501", "profit-expensive", 0)
		cheap := newAccount("equal", "0.35", "profit-cheap", 50)
		ap := fmt.Sprintf("/api/v1/admin/accounts/%d", cheap)
		key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Profit", "group_id": gid})["key"].(string)
		path := "/v1/responses"
		body := map[string]any{"model": "profit-model", "input": "hi"}
		if platform == "anthropic" {
			path, body = "/v1/messages", map[string]any{"model": "profit-model", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "max_tokens": 16}
		} else if platform == "gemini" {
			path, body = "/v1beta/models/profit-model:generateContent", map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}}
		}
		check := func(w *httptest.ResponseRecorder, aid int64, rate string) {
			t.Helper()
			var selected int64
			var actual, usedRate string
			if w.Code != 200 {
				t.Fatal("profit dispatch", platform, w.Code, w.Body.String())
			}
			if err := a.DB.QueryRow("SELECT account_id,actual_cost::text,account_rate_multiplier::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&selected, &actual, &usedRate); err != nil || selected != aid || actual != "0.0100000000" || usedRate != rate {
				t.Fatal("profit selection or billing", platform, selected, aid, actual, usedRate, err)
			}
		}
		first := call("POST", path, key, body, "profit-once")
		check(first, cheap, "0.3500")
		must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.3501"})
		before := calls.Load()
		if w := call("POST", path, key, body, "profit-once"); w.Code != 200 || w.Body.String() != first.Body.String() || calls.Load() != before {
			t.Fatal("profit edit broke completed replay", w.Code)
		}
		if w := call("POST", path, key, body, ""); w.Code != 503 || calls.Load() != before {
			t.Fatal("expensive sticky account admitted", platform, w.Code, w.Body.String())
		}
		// Count endpoints do not consume balance or inherit the text margin gate.
		if platform == "anthropic" || platform == "gemini" {
			countPath := "/v1/messages/count_tokens"
			if platform == "gemini" {
				countPath = "/v1beta/models/profit-model:countTokens"
			}
			w := call("POST", countPath, key, body, "")
			var n int
			if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n); err != nil || w.Code != 200 || n != 0 {
				t.Fatal("profit count exemption", platform, w.Code, n, err)
			}
		}
		must("PUT", ap, admin, map[string]any{"rate_multiplier": "0"})
		check(call("POST", path, key, body, ""), cheap, "0.0000")
		must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.35"})
		if platform == "openai" {
			// Bound continuation cannot silently move to a cheap sibling account.
			var result struct{ ID string }
			_ = json.Unmarshal(first.Body.Bytes(), &result)
			must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.3501"})
			body["previous_response_id"] = result.ID
			before = calls.Load()
			if w := call("POST", path, key, body, ""); w.Code != 503 || calls.Load() != before {
				t.Fatal("bound response bypassed profit", w.Code)
			}
			delete(body, "previous_response_id")
			must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.35"})
			if !a.takeSlot("account", cheap, 1) {
				t.Fatal("cannot occupy account")
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- call("POST", path, key, body, "") }()
			waitForQueue(t, a, "account", cheap, 1)
			must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.3501"})
			a.releaseSlot("account", cheap)
			select {
			case w := <-done:
				if w.Code != 503 || calls.Load() != before {
					t.Fatal("queued request used stale account cost", w.Code)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("profit queue did not recheck")
			}
			must("PUT", ap, admin, map[string]any{"rate_multiplier": "0.35"})
			mutate.Store(gid)
			check(call("POST", path, key, body, ""), cheap, "0.3500")
			before = calls.Load()
			if w := call("POST", path, key, body, ""); w.Code != 503 || calls.Load() != before {
				t.Fatal("new request ignored changed margin", w.Code)
			}
			must("PUT", gp, admin, map[string]any{"profit_min_margin": "0.2", "rate_multiplier": "2"})
			retry := newAccount("retry", "0.1", "profit-reject", 0)
			must("PUT", gp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string]any{"profit-model": []int64{retry, expensive}}})
			body["stream"] = true
			check(call("POST", path, key, body, ""), cheap, "0.3500")
			if rejects.Load() != 1 {
				t.Fatal("profit retry not exercised")
			}
			must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", retry), admin, map[string]any{"status": "inactive"})
			delete(body, "stream")
		}
		must("PUT", gp, admin, map[string]any{"profit_control_enabled": false})
		if v := must("GET", gp, admin, nil); v["profit_min_margin"] != 0.2 || v["profit_safety_buffer"] != 0.1 {
			t.Fatal("disabled profit lost ratios", v)
		}
		must("PUT", ap, admin, map[string]any{"status": "inactive"})
		check(call("POST", path, key, body, ""), expensive, "0.3501")
		if w := call("PUT", gp, user, map[string]any{"profit_control_enabled": true}, ""); w.Code != 403 {
			t.Fatal("user changed profit policy", w.Code)
		}
	}
	for _, platform := range []string{"composite", "kimi", "deepseek", "zhipu", "minimax"} {
		body := map[string]any{"name": "No profit " + platform, "platform": platform, "profit_control_enabled": true}
		if w := call("POST", "/api/v1/admin/groups", admin, body, ""); w.Code != 400 {
			t.Fatal("unsupported profit create", platform, w.Code)
		}
		body["profit_control_enabled"] = false
		gid := id(must("POST", "/api/v1/admin/groups", admin, body))
		v := must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"profit_control_enabled": true, "profit_min_margin": "0.2"})
		if v["profit_control_enabled"] != false || v["profit_min_margin"] != float64(0) {
			t.Fatal("unsupported profit update normalization", v)
		}
	}
}
