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

func TestResponseImages(t *testing.T) {
	parse := func(tool string, additional bool) (textRequest, error) {
		body := map[string]json.RawMessage{"model": json.RawMessage(`"text-model"`), "input": json.RawMessage(`"draw"`), "tools": json.RawMessage("[" + tool + "]")}
		if additional {
			delete(body, "tools")
			body["input"] = json.RawMessage(`[{"type":"additional_tools","tools":[` + tool + `]}]`)
		}
		return parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
	}
	for _, additional := range []bool{false, true} {
		in, err := parse(`{"type":"image_generation","model":"team-image","size":"1536x1024","action":"edit","quality":"high","background":"auto","output_format":"webp","input_fidelity":null,"moderation":"auto","partial_images":3,"output_compression":80,"input_image_mask":{"image_url":"data:image/png;base64,AA=="}}`, additional)
		if err != nil || in.ResponseImage == nil || in.ResponseImage.Model != "team-image" || in.ResponseImage.Size != "1536x1024" {
			t.Fatal("image tool rejected", additional, in, err)
		}
	}
	for _, option := range []string{`"partial_images":4`, `"partial_images":null`, `"output_compression":-1`, `"output_compression":1.5`, `"size":"0x1024"`, `"size":null`, `"quality":"bad"`, `"quality":"low|high"`, `"model":"a*"`, `"action":"remove"`, `"input_image_mask":{"file_id":"../foreign"}`, `"input_image_mask":null`, `"api_key":"secret"`} {
		if _, err := parse(`{"type":"image_generation",`+option+`}`, false); err == nil {
			t.Fatal("invalid option accepted", option)
		}
	}
	for _, raw := range []string{
		`{"model":"m","input":"draw","tools":[{"type":"image_generation"},{"type":"image_generation"}]}`,
		`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","file_id":"../foreign"}]}],"tools":[{"type":"image_generation"}]}`,
		`{"model":"m","input":[{"type":"tool_search_output","call_id":"call_1","tools":[{"type":"image_generation"}]}]}`,
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if _, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body); err == nil {
			t.Fatal("ambiguous or unscoped image input accepted", raw)
		}
	}
	meter := responseImageMeter{}
	const item = `{"type":"image_generation_call","id":"ig_1","result":"aW1hZ2U=","size":"1024x1024","status":"generating"}`
	for _, raw := range []string{
		`{"type":"response.image_generation_call.partial_image","partial_image_b64":"aW1hZ2U="}`,
		`{"type":"response.output_item.added","item":` + item + `}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","name":"image_generation","result":{},"size":10}}`,
	} {
		if err := meter.observe([]byte(raw)); err != nil || len(meter.seen) != 0 {
			t.Fatal("partial output billed", err)
		}
	}
	for _, raw := range []string{
		`{"type":"response.output_item.done","item":` + item + `}`,
		`{"type":"response.output_item.done","item":` + item + `}`,
		`{"type":"response.completed","response":{"status":"completed","output":[` + item + `,{"type":"image_generation_call","id":"ig_2","result":"aW1hZ2Uy","size":"3840x2160"}]}}`,
	} {
		if err := meter.observe([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	u := priceUsage{}
	for range 2 {
		meter.apply(&u, &responseImageConfig{Size: "1536x1024"})
		if u.ImageCount != 2 || u.ImageSize != "4K" || u.ImageSizeSource != "output" || u.ImageOutputSize != "1024x1024" || u.ImageSizes != [3]int64{1, 0, 1} {
			t.Fatal("image metering", u)
		}
	}
	if meter.observe([]byte(`{"status":"completed","output":[{"type":"image_generation_call","result":"invalid!"}]}`)) == nil {
		t.Fatal("malformed output accepted")
	}
	in, err := parse(`{"type":"image_generation"}`, false)
	if err != nil || in.ResponseImage.Model != "gpt-image-2" {
		t.Fatal("billing fallback changed", in, err)
	}
	yes := true
	g := gatewayIdentity{Key: gatewayKey{Quota: "0", Limit5h: "0", Limit1d: "0", Limit7d: "0"}, Group: gatewayGroup{Rate: "2", imagePrices: imagePrices{IndependentImage: &yes, ImageRate: number("0.5"), Image4K: number("0.4")}, SearchPrice: number("10")}}
	s := gatewaySelection{Account: &upstreamAccount{Platform: "openai"}, ResponseImage: in.ResponseImage, Rate: "3", BillingSource: "channel_mapped", ChannelModel: "text"}
	u.SearchCalls = 1
	r, err := (&App{}).makeReceipt("receipt", &g, &s, "text", "text", "", "", u, false, 0, 0, time.Time{}, "", "", "", "", "")
	if err != nil || r.Cost.Total != "0.8100000000" || r.Cost.Actual != "0.4200000000" || r.UserRate != "0.5" {
		t.Fatal("image and search multiplier separation", r, err)
	}
	u.SearchCalls, u.Input, u.Output, u.ImageOutput = 0, 2, 4, 2
	s.GroupPricing = []modelPrice{{Platform: "openai", Models: []string{"gpt-image-2"}, BillingMode: "token", Input: number("0.001"), Output: number("0.002"), ImageOutput: number("0.003")}}
	r, err = (&App{}).makeReceipt("token", &g, &s, "text", "text", "", "", u, false, 0, 0, time.Time{}, "", "", "", "", "")
	if err != nil || r.Cost.Actual != "0.0240000000" || r.UserRate != "2" || r.BillingMode != "token" {
		t.Fatal("explicit token price did not override image rate", r, err)
	}
	s.GroupPricing = nil
	g.Group.Image4K = number("0")
	r, err = (&App{}).makeReceipt("free", &g, &s, "text", "text", "", "", u, false, 0, 0, time.Time{}, "", "", "", "", "")
	if err != nil || r.Cost.Actual != "0.0000000000" {
		t.Fatal("explicit free image price", r, err)
	}
	for source, expected := range map[string]string{"requested": "request", "channel_mapped": "channel", "upstream": "upstream", "response_model": "response"} {
		s.BillingSource, s.ChannelModel, s.UpstreamModel = source, "channel", "upstream"
		if got := s.responseImageModel("request", "response"); got != expected {
			t.Fatal("image billing source", source, got)
		}
	}
}

func testResponseImages(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	ctx := context.Background()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.176:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-cookie")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "response-images@example.test", "password": "image-password", "balance": 100})
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "response-images@example.test", "password": "image-password"})["access_token"].(string)
	prices := []any{map[string]any{"platform": "openai", "models": []string{"team-text"}, "input_price": "0.001", "output_price": "0.002", "cache_read_price": 0, "cache_write_price": 0}}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Responses images", "platform": "openai", "allow_image_generation": true, "rate_multiplier": 2, "image_rate_independent": true, "image_rate_multiplier": "0.5", "image_price_1k": "0.1", "image_price_2k": "0.2", "image_price_4k": "0.4", "search_price_per_1k": 10, "model_pricing": prices}))
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	k := must("POST", "/api/v1/keys", user, map[string]any{"name": "images", "group_id": gid, "quota": 100})
	key, kid := k["key"].(string), id(k)
	var calls atomic.Int64
	var mode atomic.Int32
	var change atomic.Bool
	var pending, toolsSeen atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer image-provider" || r.Header.Get("Cookie") != "" {
			t.Error("image upstream credential isolation")
		}
		if r.Method == "GET" && r.Header.Get("Upgrade") == "" {
			fmt.Fprint(w, pending.Load().(string))
			return
		}
		n := calls.Add(1)
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
				t.Error("upstream WS input", err)
				return
			}
		} else if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("upstream body")
			return
		}
		if credentialString(body, "model") != "native-text" {
			t.Error("text model mapping")
		}
		toolsSeen.Store(string(body["tools"]))
		if change.Swap(false) {
			if _, err := a.DB.Exec("UPDATE groups SET image_price_4k=9 WHERE id=$1", gid); err != nil {
				t.Error(err)
			}
		}
		first := fmt.Sprintf(`{"type":"image_generation_call","id":"ig_%d_a","status":"completed","result":"aW1hZ2U=","size":"1024x1024"}`, n)
		second := fmt.Sprintf(`{"type":"image_generation_call","id":"ig_%d_b","status":"completed","result":"aW1hZ2Uy","size":"3840x2160"}`, n)
		output := "[" + first + "," + second + "]"
		if mode.Load() == 1 {
			output = `[]`
		}
		status := "completed"
		if mode.Load() == 3 {
			status = "failed"
		}
		response := fmt.Sprintf(`{"id":"resp_image_%d","object":"response","model":"native-text","status":%q,"output":%s,"usage":{"input_tokens":2,"output_tokens":4,"output_tokens_details":{"image_tokens":2}}}`, n, status, output)
		if mode.Load() == 1 {
			response = strings.Replace(response, `,"output_tokens_details":{"image_tokens":2}`, "", 1)
		}
		if string(body["background"]) == "true" {
			pending.Store(response)
			fmt.Fprintf(w, `{"id":"resp_image_%d","object":"response","status":"queued"}`, n)
			return
		}
		if string(body["stream"]) != "true" && conn == nil {
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
		emit(`{"type":"response.image_generation_call.partial_image","partial_image_index":0,"partial_image_b64":"` + strings.Repeat("A", (2<<20)+4) + `"}`)
		emit(`{"type":"response.output_item.done","item":` + first + `}`)
		emit(`{"type":"response.output_item.done","item":` + first + `}`)
		if mode.Load() == 2 {
			return
		}
		emit(`{"type":"response.output_item.done","item":` + second + `}`)
		emit(`{"type":"response.` + status + `","response":` + response + `}`)
		if conn != nil {
			_, _, _ = conn.Read(r.Context())
		}
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Responses image provider", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "rate_multiplier": 3, "extra": map[string]any{"quota_limit": 100, "openai_apikey_responses_websockets_v2_mode": "passthrough"}, "credentials": map[string]any{"api_key": "image-provider", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"team-text": "native-text"}}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "team-text", "input": "draw", "store": false, "tools": []any{map[string]any{"type": "image_generation", "model": "team-image", "size": "1536x1024", "partial_images": 2}}}
	check := func(logs int, want string, images int64) {
		t.Helper()
		var count int
		var cost string
		var gotImages int64
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count); err != nil || count != logs {
			t.Fatal("image log count", count, logs, err)
		}
		if err := a.DB.QueryRow("SELECT actual_cost::text,image_count FROM usage_logs WHERE api_key_id=$1 ORDER BY id DESC LIMIT 1", kid).Scan(&cost, &gotImages); err != nil || cost != want || gotImages != images {
			t.Fatal("image cost", cost, want, gotImages, err)
		}
	}
	change.Store(true)
	w := call("POST", "/responses", key, body, "image-json")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "aW1hZ2Uy") {
		t.Fatal("image JSON", w.Code, w.Body.String())
	}
	check(1, "0.4000000000", 2)
	rawTools, _ := json.Marshal(body["tools"])
	if toolsSeen.Load() != string(rawTools) {
		t.Fatal("image tool options changed")
	}
	var metadata bool
	if err := a.DB.QueryRow(`SELECT model='team-image' AND requested_model='team-text' AND image_size='4K' AND image_size_source='output' AND image_input_size='1536x1024' AND image_output_size='1024x1024' AND image_size_breakdown='{"1K":1,"4K":1}'::jsonb FROM usage_logs WHERE api_key_id=$1`, kid).Scan(&metadata); err != nil || !metadata {
		t.Fatal("image metadata", err)
	}
	for _, path := range []string{"/v1/responses", "/backend-api/codex/responses"} {
		w = call("POST", path, key, body, "image-json")
		if w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("image replay", w.Code, calls.Load())
		}
	}
	must("PUT", gp, admin, map[string]any{"image_price_4k": "0.4"})
	body["stream"] = true
	w = call("POST", "/responses", key, body, "")
	if !strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "partial_image") {
		t.Fatal("image SSE", w.Body.String())
	}
	check(2, "0.4000000000", 2)
	mode.Store(2)
	w = call("POST", "/responses", key, body, "")
	if !strings.Contains(w.Body.String(), "error") || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal("truncated stream succeeded")
	}
	check(3, "0.0500000000", 1)
	delete(body, "stream")
	mode.Store(1)
	if w = call("POST", "/responses", key, body, ""); w.Code != 200 {
		t.Fatal("text-only result", w.Code, w.Body.String())
	}
	check(4, "0.0200000000", 0)
	mode.Store(3)
	if w = call("POST", "/responses", key, body, ""); w.Code != 502 {
		t.Fatal("failed response succeeded", w.Code)
	}
	check(5, "0.4000000000", 2)
	mode.Store(0)
	// WebSocket uses the same image meter and receipt transaction.
	server := httptest.NewServer(a.Handler())
	defer server.Close()
	wsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(wsCtx, "ws"+strings.TrimPrefix(server.URL, "http")+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	conn.SetReadLimit(16 << 20)
	body["type"] = "response.create"
	raw, _ := json.Marshal(body)
	if err = conn.Write(wsCtx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	for {
		_, raw, err = conn.Read(wsCtx)
		if err != nil || bytes.Contains(raw, []byte(`"error"`)) {
			conn.CloseNow()
			t.Fatal("image socket", string(raw), err)
		}
		if bytes.Contains(raw, []byte(`"response.completed"`)) {
			break
		}
	}
	conn.CloseNow()
	delete(body, "type")
	check(6, "0.4000000000", 2)
	// Background execution retains metadata and original prices through SQL failure.
	body["background"], body["store"] = true, true
	w = call("POST", "/responses", key, body, "image-background")
	var accepted struct{ ID string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &accepted) != nil || accepted.ID == "" {
		t.Fatal("image background", w.Code, w.Body.String())
	}
	identity := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	taskID, err := a.Redis.Get(ctx, backgroundIndex(identity, accepted.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	must("PUT", gp, admin, map[string]any{"image_price_4k": 9})
	if _, err = a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_response_image_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_response_image_failure")
	task, err := a.loadBackgroundResponse(ctx, taskID)
	if err != nil || a.refreshBackgroundResponse(ctx, task) == nil {
		t.Fatal("unsettled background success", err)
	}
	if _, err = a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_response_image_failure"); err != nil {
		t.Fatal(err)
	}
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret}
	before := calls.Load()
	for range 2 {
		task, err = fresh.loadBackgroundResponse(ctx, taskID)
		if err != nil || fresh.refreshBackgroundResponse(ctx, task) != nil {
			t.Fatal("image recovery", err)
		}
	}
	check(7, "0.4000000000", 2)
	if calls.Load() != before {
		t.Fatal("image recovery generated again")
	}
	var consistent bool
	if err = a.DB.QueryRow(`SELECT u.balance=100-round(s.cost,8) AND k.quota_used=round(s.cost,8) AND (ac.extra->>'quota_used')::numeric=round(s.total*3,8) FROM users u JOIN api_keys k ON k.user_id=u.id JOIN accounts ac ON ac.id=$2 CROSS JOIN (SELECT sum(actual_cost) cost,sum(total_cost) total FROM usage_logs WHERE api_key_id=$1)s WHERE k.id=$1`, kid, aid).Scan(&consistent); err != nil || !consistent {
		t.Fatal("image wallet/key/account drift", err)
	}
	delete(body, "background")
	must("PUT", gp, admin, map[string]any{"image_price_4k": "0.4"})
	// Explicit image history and inherited tools keep the original Key boundary.
	other := must("POST", "/api/v1/keys", user, map[string]any{"name": "other-images", "group_id": gid})["key"].(string)
	body["previous_response_id"] = accepted.ID
	if w = call("POST", "/responses", other, body, ""); w.Code != 404 {
		t.Fatal("foreign image continuation", w.Code)
	}
	savedTools := body["tools"]
	delete(body, "tools")
	if w = call("POST", "/responses", key, body, ""); w.Code != 400 {
		t.Fatal("inherited image tool bypassed admission", w.Code)
	}
	body["tools"] = savedTools
	delete(body, "previous_response_id")
	imageID := fmt.Sprintf("ig_%d_a", before)
	body["input"] = []any{map[string]string{"type": "image_generation_call", "id": imageID}, map[string]string{"role": "user", "content": "edit"}}
	if w = call("POST", "/responses", other, body, ""); w.Code != 404 {
		t.Fatal("foreign explicit image history", w.Code)
	}
	if calls.Load() != before {
		t.Fatal("foreign or unconfigured tool dispatched")
	}
	if w = call("POST", "/responses", key, body, ""); w.Code != 200 {
		t.Fatal("owned explicit image history", w.Code, w.Body.String())
	}
	check(8, "0.4000000000", 2)
	body["input"] = "draw"
	// Synchronous settlement failure cannot publish JSON success or lose usage.
	if _, err = a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_response_image_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	w = call("POST", "/responses", key, body, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "aW1hZ2U") {
		t.Fatal("unsettled JSON exposed", w.Code)
	}
	if _, err = a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_response_image_failure"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = fresh.recoverReceipts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	check(9, "0.4000000000", 2)
	// Composite routes enforce the resolved target and use the same tariff.
	cgid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite hosted images", "platform": "composite", "allow_image_generation": true, "image_price_4k": "0.4", "model_pricing": prices}))
	cp := fmt.Sprintf("/api/v1/admin/groups/%d", cgid)
	route := must("POST", cp+"/composite-routes", admin, map[string]any{"public_model": "team-text", "match_type": "exact", "target_platform": "openai", "upstream_model": "team-text", "endpoint": "responses", "enabled": true})
	must("PUT", ap, admin, map[string]any{"group_ids": []int64{gid, cgid}})
	ckey := must("POST", "/api/v1/keys", user, map[string]any{"name": "composite-images", "group_id": cgid})["key"].(string)
	if w = call("POST", "/responses", ckey, body, ""); w.Code != 200 {
		t.Fatal("composite images", w.Code, w.Body.String())
	}
	must("PUT", cp+"/composite-routes/"+fmt.Sprint(id(route)), admin, map[string]any{"public_model": "team-text", "match_type": "exact", "target_platform": "grok", "upstream_model": "team-text", "endpoint": "responses", "enabled": true})
	before = calls.Load()
	if w = call("POST", "/responses", ckey, body, ""); w.Code != 400 {
		t.Fatal("Grok image tool admitted", w.Code)
	}
	// Unsupported group, disabled media and conversion-only accounts never dispatch.
	must("PUT", gp, admin, map[string]any{"allow_image_generation": false})
	if w = call("POST", "/responses", key, body, ""); w.Code != 403 {
		t.Fatal("image gate", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"allow_image_generation": true})
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_protocol": "chat_completions"}})
	if w = call("POST", "/responses", key, body, ""); w.Code != 503 {
		t.Fatal("image tool converted", w.Code)
	}
	if w = call("POST", "/responses", admin, body, ""); w.Code != 401 {
		t.Fatal("JWT accepted", w.Code)
	}
	if calls.Load() != before {
		t.Fatal("rejected image dispatched")
	}
}
