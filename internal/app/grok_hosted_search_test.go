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

	"github.com/coder/websocket"
)

func TestGrokHostedSearch(t *testing.T) {
	for _, raw := range []string{
		`{"type":"web_search","allowed_domains":["a"],"excluded_domains":["b"]}`,
		`{"type":"web_search","allowed_domains":["1","2","3","4","5","6"]}`,
		`{"type":"web_search","allowed_domains":null}`, `{"type":"web_search","enable_image_search":"true"}`,
		`{"type":"web_search","enable_video_understanding":true}`, `{"type":"x_search","enable_image_search":true}`,
		`{"type":"x_search","from_date":"2026-02-30"}`, `{"type":"x_search","from_date":"2026-02-20","to_date":"2026-02-19"}`,
		`{"type":"web_search","api_key":"private"}`,
	} {
		var tool map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &tool)
		if validateGrokHostedSearch(tool) == nil {
			t.Fatal("invalid tool accepted", raw)
		}
	}
	m := &hostedSearchMeter{}
	observe := func(raw string, want int64) {
		t.Helper()
		if err := m.observe([]byte(raw)); err != nil || m.count() != want {
			t.Fatal("search metering", raw, m.count(), want, err)
		}
	}
	observe(`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"a"}}`, 0)
	observe(`{"type":"response.output_item.done","item":{"type":"function_call","name":"web_search","id":"fn"}}`, 0)
	observe(`{"type":"response.output_item.done","item":{"type":"tool_search_call","execution":"client","id":"ts"}}`, 0)
	observe(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"a","status":"failed"}}`, 0)
	observe(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"a"}}`, 1)
	observe(`{"type":"response.output_item.done","item":{"type":"web_search_call","id":"a"}}`, 1)
	observe(`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`, 2)
	observe(`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`, 3)
	observe(`{"type":"response.completed","output":[{"type":"x_search_call"}],"response":{"status":"completed","output":[{"type":"web_search_call","id":"a"},{"type":"web_search_call"},{"type":"web_search_call"}]}}`, 3)
	observe(`{"status":"incomplete","usage":{"server_side_tool_usage_details":{"web_search_calls":4,"x_search_calls":2}}}`, 6)
	observe(`{"status":"completed","usage":{"server_side_tool_usage_details":{"web_search_calls":0,"x_search_calls":1}}}`, 6)
	// A bridge may supply IDs only in the final response; do not charge it twice.
	m = &hostedSearchMeter{}
	observe(`{"type":"response.output_item.done","item":{"type":"x_search_call"}}`, 1)
	observe(`{"status":"completed","output":[{"type":"x_search_call","id":"final"}]}`, 1)
	for _, count := range []string{`-1`, `1.5`, `null`, `"2"`, `10001`, `9223372036854775808`} {
		m = &hostedSearchMeter{}
		if m.observe([]byte(`{"status":"completed","usage":{"server_side_tool_usage_details":{"web_search_calls":`+count+`}}}`)) == nil {
			t.Fatal("invalid upstream count accepted", count)
		}
	}
	for _, price := range []*json.Number{nil, number("0"), number("123.45678901")} {
		p := modelPrice{Models: []string{"x"}, BillingMode: "token", Input: number("0.00000001"), Output: number("0.00000002"), CacheRead: number("0"), CacheWrite: number("0")}
		cost, err := calculatePrice(p, priceUsage{Input: 3, Output: 4}, "0.3333", "", "", "", time.Now(), false)
		if err != nil {
			t.Fatal(err)
		}
		if err = addHostedSearchCost(&cost, gatewayGroup{SearchPrice: price, Rate: "0.3333"}, 3); err != nil {
			t.Fatal(err)
		}
		value := "5"
		if price != nil {
			value = price.String()
		}
		want := rat("0.00000011")
		search := rat(json.Number(value))
		search.Mul(search, rat("0.003"))
		want.Add(want, search)
		want.Mul(want, rat("0.3333"))
		if cost.Actual != want.FloatString(10) || cost.Debit != want.FloatString(8) {
			t.Fatal("search rounding", cost, want)
		}
	}
}

func testHostedSearch(t *testing.T, a *App, admin, platform string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	ctx := context.Background()
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.174:1234"
		if platform == "openai" {
			r.RemoteAddr = "192.0.2.175:1234"
		}
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 && w.Code != 201 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		var out struct{ Data map[string]any }
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid management response")
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": platform + "-hosted-search@example.test", "password": "hosted-search-password", "balance": 100}))
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": platform + "-hosted-search@example.test", "password": "hosted-search-password"})["access_token"].(string)
	prices := []any{map[string]any{"platform": platform, "models": []string{"team-search"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": "0", "cache_write_price": "0"}}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": platform + " hosted search", "platform": platform, "rate_multiplier": 2, "search_price_per_1k": 10, "model_pricing": prices}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": platform + " hosted search", "group_id": gid, "quota": 100, "rate_limit_5h": 100})
	key, kid := k["key"].(string), id(k)
	var calls atomic.Int64
	var mode atomic.Int32
	var change atomic.Bool
	const web = `{"type":"web_search_call","id":"web_1","status":"completed","action":{"sources":[{"url":"https://example.test/"}]}}`
	x := `{"type":"x_search_call","id":"x_1","status":"completed"}`
	if platform == "openai" {
		x = `{"type":"web_search_call","id":"web_2","status":"completed"}`
	}
	var backgroundResult atomic.Value
	var forwardedTools atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.Header.Get("Upgrade") == "" {
			if r.Header.Get("Authorization") != "Bearer hosted-secret" {
				t.Error("background credential isolation")
			}
			fmt.Fprint(w, backgroundResult.Load().(string))
			return
		}
		n := calls.Add(1)
		if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer hosted-secret" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("hosted search upstream isolation")
		}
		var conn *websocket.Conn
		var body map[string]json.RawMessage
		if r.Header.Get("Upgrade") == "websocket" {
			var err error
			conn, err = websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			_, raw, err := conn.Read(r.Context())
			if err != nil || json.Unmarshal(raw, &body) != nil {
				t.Error("upstream websocket request", err)
				return
			}
		} else if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("upstream request")
			return
		}
		if credentialString(body, "model") != "native-search" {
			t.Error("model mapping", string(body["model"]))
		}
		forwardedTools.Store(string(body["tools"]))
		if change.Swap(false) {
			if _, err := a.DB.Exec("UPDATE groups SET search_price_per_1k=99 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		if mode.Load() == 5 {
			w.WriteHeader(400)
			return
		}
		output := "[" + web + "," + x + "]"
		details := ""
		status := "completed"
		if mode.Load() == 1 {
			output = `[{"type":"function_call","id":"fn","call_id":"fn","name":"web_search","arguments":"{}"}]`
		} else if mode.Load() == 2 {
			output = `[]`
			details = `,"server_side_tool_usage_details":{"web_search_calls":2,"x_search_calls":1}`
			if platform == "openai" {
				output = "[" + web + "," + x + `,{"type":"web_search_call","id":"web_3","status":"completed"}]`
				details = ""
			}
		} else if mode.Load() == 4 {
			status = "failed"
		}
		response := fmt.Sprintf(`{"id":"resp_hosted_%d","object":"response","model":"native-search","status":%q,"output":%s,"usage":{"input_tokens":2,"output_tokens":3%s}}`, n, status, output, details)
		if string(body["background"]) == "true" {
			backgroundResult.Store(response)
			fmt.Fprintf(w, `{"id":"resp_hosted_%d","object":"response","status":"queued"}`, n)
			return
		}
		if string(body["stream"]) != "true" && conn == nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, response)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(raw string) {
			if conn != nil {
				if err := conn.Write(r.Context(), websocket.MessageText, []byte(raw)); err != nil {
					t.Error(err)
				}
			} else {
				fmt.Fprintf(w, "data: %s\n\n", raw)
			}
		}
		emit(`{"type":"response.output_item.added","item":` + web + `}`)
		emit(`{"type":"response.output_item.done","item":` + web + `}`)
		emit(`{"type":"response.output_item.done","item":` + web + `}`)
		if mode.Load() == 3 {
			return // Known search consumption survives missing terminal token usage.
		}
		emit(`{"type":"response.output_item.done","item":` + x + `}`)
		emit(`{"type":"response.` + status + `","response":` + response + `}`)
		if conn != nil {
			_, _, _ = conn.Read(r.Context())
		}
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": platform + " hosted search", "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "rate_multiplier": 3, "extra": map[string]any{"quota_limit": 100}, "credentials": map[string]any{"api_key": "hosted-secret", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"team-search": "native-search"}}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "team-search", "input": "search the web and X", "store": false, "tools": []any{map[string]any{"type": "web_search", "allowed_domains": []string{"example.test"}}, map[string]any{"type": "x_search", "from_date": "2026-09-01"}}}
	if platform == "openai" {
		body["tools"] = []any{map[string]any{"type": "web_search", "filters": map[string]any{"allowed_domains": []string{"example.test"}}, "user_location": map[string]string{"type": "approximate", "country": "GB"}, "external_web_access": false, "return_token_budget": "default"}}
		body["tool_choice"] = map[string]string{"type": "web_search"}
	}
	call := func(path, token, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Cookie", "private-cookie")
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	checkCost := func(total, actual string, logs int) {
		t.Helper()
		var count int
		var gotTotal, gotActual, inputCost, outputCost string
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != logs {
			t.Fatal("hosted logs", count, logs, err)
		}
		if err := a.DB.QueryRow("SELECT total_cost::text,actual_cost::text,input_cost::text,output_cost::text FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&gotTotal, &gotActual, &inputCost, &outputCost); err != nil || rat(json.Number(gotTotal)).Cmp(rat(json.Number(total))) != 0 || rat(json.Number(gotActual)).Cmp(rat(json.Number(actual))) != 0 {
			t.Fatal("hosted cost", gotTotal, gotActual, inputCost, outputCost, total, actual, err)
		}
	}
	change.Store(true)
	w := call("/v1/responses", key, "hosted-json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "https://example.test/") {
		t.Fatal("hosted JSON", w.Code, w.Body.String())
	}
	wantTools, _ := json.Marshal(body["tools"])
	if forwardedTools.Load() != string(wantTools) {
		t.Fatal("hosted search options changed upstream")
	}
	checkCost("0.028", "0.056", 1)
	for _, path := range []string{"/responses", "/backend-api/codex/responses"} {
		w = call(path, key, "hosted-json")
		if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("hosted replay", w.Code, calls.Load())
		}
	}
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 10})
	body["stream"] = true
	w = call("/responses", key, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal("hosted SSE", w.Code, w.Body.String())
	}
	checkCost("0.028", "0.056", 2)
	delete(body, "stream")
	mode.Store(2)
	w = call("/v1/responses", key, "")
	if w.Code != 200 {
		t.Fatal("usage-only search count", w.Code, w.Body.String())
	}
	checkCost("0.038", "0.076", 3)
	mode.Store(1)
	originalTools := body["tools"]
	body["tools"] = []any{map[string]any{"type": "function", "name": "web_search", "parameters": map[string]string{"type": "object"}}}
	if w = call("/v1/responses", key, ""); w.Code != 200 {
		t.Fatal("client function", w.Code, w.Body.String())
	}
	checkCost("0.008", "0.016", 4)
	body["tools"], body["stream"] = originalTools, true
	mode.Store(3)
	w = call("/v1/responses", key, "")
	if !strings.Contains(w.Body.String(), "error") || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal("interrupted hosted search succeeded", w.Body.String())
	}
	checkCost("0.01", "0.02", 5)
	delete(body, "stream")
	mode.Store(4)
	w = call("/v1/responses", key, "")
	if w.Code != 502 {
		t.Fatal("failed response", w.Code, w.Body.String())
	}
	checkCost("0.028", "0.056", 6)
	mode.Store(0)
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 0})
	if w = call("/v1/responses", key, ""); w.Code != 200 {
		t.Fatal("free search", w.Code)
	}
	checkCost("0.008", "0.016", 7)
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 10})
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_hosted_search_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_hosted_search_failure")
	body["stream"] = true
	w = call("/v1/responses", key, "")
	if strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "settlement") {
		t.Fatal("unsettled success exposed", w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_hosted_search_failure"); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	fresh := &App{DB: a.DB, Redis: a.Redis}
	for range 2 {
		if err := fresh.recoverReceipts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	checkCost("0.028", "0.056", 8)
	if calls.Load() != before {
		t.Fatal("settlement repeated generation")
	}
	delete(body, "stream")
	// Native WebSocket turns share the same metering and transaction path.
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{"openai_apikey_responses_websockets_v2_mode": "passthrough"}})
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	wsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	body["type"] = "response.create"
	raw, _ := json.Marshal(body)
	if err = conn.Write(wsCtx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err = conn.Read(wsCtx)
		if err != nil || bytes.Contains(raw, []byte(`"error"`)) {
			conn.CloseNow()
			t.Fatal("hosted socket", string(raw), err)
		}
		if bytes.Contains(raw, []byte(`"response.completed"`)) {
			break
		}
	}
	conn.CloseNow()
	delete(body, "type")
	checkCost("0.028", "0.056", 9)
	// Consumption stays consistent across wallet, key, windows and account totals.
	var consistent bool
	if err = a.DB.QueryRow(`SELECT u.balance=100-round(s.cost,8) AND k.quota_used=round(s.cost,8) AND k.usage_5h=round(s.cost,8)
 AND (ac.extra->>'quota_used')::numeric=round(s.total*3,8)
 FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts ac ON ac.id=$2
 CROSS JOIN (SELECT sum(actual_cost) cost,sum(total_cost) total FROM usage_logs WHERE api_key_id=$1)s WHERE k.id=$1`, kid, aid).Scan(&consistent); err != nil || !consistent {
		t.Fatal("hosted balances inconsistent", err)
	}
	// Background lookup uses the creation-time search price and durable receipt.
	body["background"], body["store"] = true, true
	w = call("/responses", key, "hosted-background")
	var accepted struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" {
		t.Fatal("hosted background", w.Code, w.Body.String())
	}
	identity := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	taskID, err := a.Redis.Get(ctx, backgroundIndex(identity, accepted.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	task, err := a.loadBackgroundResponse(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 99})
	fresh = &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	for range 2 {
		if err = fresh.refreshBackgroundResponse(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	checkCost("0.028", "0.056", 10)
	delete(body, "background")
	body["store"] = false
	must("PUT", gp, admin, map[string]any{"search_price_per_1k": 10})
	// Composite admission uses the resolved platform, retaining native tool options.
	cgid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": platform + " composite search", "platform": "composite", "model_pricing": prices, "rate_multiplier": 2, "search_price_per_1k": 10}))
	must("PUT", ap, admin, map[string]any{"group_ids": []int64{gid, cgid}})
	must("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", cgid), admin, map[string]any{"public_model": "team-search", "match_type": "exact", "target_platform": platform, "upstream_model": "team-search", "endpoint": "responses", "enabled": true})
	ckey := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite search", "group_id": cgid})["key"].(string)
	if w = call("/responses", ckey, "composite-search"); w.Code != 200 {
		t.Fatal("composite hosted search", w.Code, w.Body.String())
	}
	if err = a.DB.QueryRow("SELECT count(*)=1 AND sum(actual_cost)=0.056 FROM usage_logs WHERE group_id=$1", cgid).Scan(&consistent); err != nil || !consistent {
		t.Fatal("composite hosted search billing", err)
	}

	before = calls.Load()
	invalidTool := map[string]any{"type": "web_search_preview", "filters": map[string]any{}}
	if platform == "openai" {
		invalidTool = map[string]any{"type": "x_search"}
	}
	for _, extra := range []bool{false, true} {
		body["tools"] = []any{invalidTool}
		if extra {
			delete(body, "tools")
			body["input"] = []any{map[string]any{"type": "additional_tools", "tools": []any{invalidTool}}}
		}
		if w = call("/responses", key, ""); w.Code != 400 || calls.Load() != before {
			t.Fatal("wrong-platform hosted tool dispatched", w.Code)
		}
	}
	body["tools"], body["input"] = originalTools, "search the web"
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
	if w = call("/responses", key, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("hosted search converted to Chat", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "responses"}})
	for _, path := range []string{"/v1/responses/input_tokens", "/responses/compact"} {
		if w = call(path, key, ""); w.Code != 400 {
			t.Fatal("hosted non-generation accepted", path, w.Code)
		}
	}
	if w = call("/v1/responses", user, ""); w.Code != 401 {
		t.Fatal("login token accepted as gateway key", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"different-model"}}})
	if w = call("/v1/responses", key, ""); w.Code != 403 {
		t.Fatal("hosted model restriction", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	if _, err := a.DB.Exec("UPDATE users SET balance=0 WHERE id=$1", uid); err != nil {
		t.Fatal(err)
	}
	if w = call("/v1/responses", key, ""); w.Code != 402 || calls.Load() != before {
		t.Fatal("hosted eligibility bypass", w.Code, calls.Load()-before)
	}
}
