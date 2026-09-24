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

func TestGeminiImageBilling(t *testing.T) {
	for _, tc := range []struct {
		body, size, source string
	}{
		{`{"generationConfig":{"imageConfig":{"imageSize":"1K"}}}`, "1K", "input"},
		{`{"generationConfig":{"imageConfig":{"imageSize":"AUTO"}}}`, "2K", "default"},
		{`{"generationConfig":{"imageConfig":{"imageSize":"512"}}}`, "2K", "default"},
		{`{}`, "2K", "default"},
	} {
		var body map[string]json.RawMessage
		if json.Unmarshal([]byte(tc.body), &body) != nil {
			t.Fatal(tc.body)
		}
		size, source, err := geminiImageSize(body)
		if err != nil || size != tc.size || source != tc.source {
			t.Fatal(size, source, err)
		}
	}
	for _, raw := range []string{`{"generationConfig":{"imageConfig":{"imageSize":"8K"}}}`, `{"generationConfig":[]}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if _, _, err := geminiImageSize(body); err == nil {
			t.Fatal("invalid image size accepted", raw)
		}
	}
	raw := []byte(`{"candidates":[{"content":{"parts":[{"text":"caption"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},{"inline_data":{"mime_type":"image/jpeg","data":"/9j/4AAQ"}}]}}]}`)
	if count, err := countGeminiImages(raw); err != nil || count != 2 {
		t.Fatal("image outputs", count, err)
	}
	if _, err := countGeminiImages([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"not-base64"}}]}}]}`)); err == nil {
		t.Fatal("invalid image data accepted")
	}
	p := modelPrice{Platform: "gemini", Models: []string{"gemini-image"}, BillingMode: "image", PerRequest: number("0.2")}
	cost, err := calculatePrice(p, priceUsage{ImageCount: 2, Requests: 2, ImageSize: "1K"}, "1.5", "", "", "1K", time.Time{}, true)
	if err != nil || cost.Total != "0.4000000000" || cost.Actual != "0.6000000000" {
		t.Fatal("image cost", cost, err)
	}
	if !geminiImageModel("models/gemini-3-pro-image-preview") || geminiImageModel("gemini-2.5-pro") {
		t.Fatal("image model detection")
	}
	s := &gatewaySelection{Account: &upstreamAccount{Platform: "gemini"}}
	g := gatewayGroup{Rate: "2"}
	u := priceUsage{ImageCount: 2, ImageSize: "2K", Input: 10, Output: 15, ImageOutput: 10}
	check := func(want string) {
		t.Helper()
		cost, _, _, err := s.generatedImageCost(g, "alias", u, "", "", time.Time{})
		if err != nil || cost.Actual != want {
			t.Fatal("image price precedence", cost, err, want)
		}
	}
	check("0.8040000000")
	s.Pricing = []modelPrice{{Platform: "gemini", Models: []string{"alias"}, BillingMode: "image", PerRequest: number("0.1")}}
	check("0.4000000000")
	g.Image2K = number("0.2")
	check("0.8000000000")
	yes := true
	g.IndependentImage, g.ImageRate = &yes, number("0.5")
	check("0.2000000000")
	s.GroupPricing = []modelPrice{{Platform: "openai", Models: []string{"alias"}, BillingMode: "per_request", PerRequest: number("0.3")}}
	check("0.3000000000")
	s.GroupPricing[0] = modelPrice{Platform: "openai", Models: []string{"alias"}, BillingMode: "token", Input: number("0.01"), Output: number("0.02"), ImageOutput: number("0.03")}
	check("1.0000000000") // Explicit token cards use the shared rate, not the image rate.
	s.Restrict, s.Pricing = true, nil
	if _, _, err := s.generatedImagePrice(g, "alias", "2K"); err == nil {
		t.Fatal("image prices bypassed channel model restriction")
	}
}

func testGeminiImages(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.153:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "private-client-cookie")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "gemini-images@example.test", "password": "images-password", "balance": 100, "concurrency": 4}))
	token := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "gemini-images@example.test", "password": "images-password"})["access_token"].(string)
	gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Native images", "platform": "gemini", "rate_multiplier": 2}))
	gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	kobj := manage("POST", "/api/v1/keys", token, map[string]any{"name": "images", "group_id": gid, "quota": 100})
	key, kid := kobj["key"].(string), id(kobj)
	var calls, mode atomic.Int32
	const parts = `[{"text":"caption"},{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}},{"inline_data":{"mime_type":"image/jpeg","data":"/9j/4AAQ"}}]`
	const usage = `"usageMetadata":{"promptTokenCount":30,"cachedContentTokenCount":5,"candidatesTokenCount":20,"thoughtsTokenCount":2,"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":8},{"modality":"IMAGE","tokenCount":12}]}`
	entered, release := make(chan struct{}, 1), make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Goog-Api-Key") != "gemini-image-provider" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.Query().Get("key") != "" {
			t.Error("Gemini credentials not isolated")
		}
		if !strings.Contains(r.URL.Path, "/v1beta/models/gemini-3-pro-image:") {
			t.Error("Gemini model not mapped", r.URL.Path)
		}
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			fmt.Fprint(w, `{"totalTokens":12}`)
			return
		}
		if mode.Load() == 7 {
			entered <- struct{}{}
			<-release
		}
		if mode.Load() == 3 {
			fmt.Fprint(w, `{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":0}}`)
			return
		}
		if mode.Load() == 6 {
			fmt.Fprint(w, `{"candidates":[{"index":0,"finishReason":"NO_IMAGE"}]}`)
			return
		}
		metadata := "," + usage
		if mode.Load() == 2 {
			metadata = ""
		}
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			if mode.Load() == 4 {
				fmt.Fprintf(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"%s\"}}]}}]}\n\n", strings.Repeat("AAAA", 800000))
			} else {
				for i := 0; i < 2; i++ {
					fmt.Fprintf(w, "data: {\"candidates\":[{\"index\":0,\"content\":{\"parts\":%s}}],%s}\n\n", parts, usage)
				}
			}
			if mode.Load() != 5 {
				fmt.Fprintf(w, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}],%s}\n\n", usage)
			}
			return
		}
		fmt.Fprintf(w, `{"modelVersion":"gemini-3-pro-image","candidates":[{"index":0,"finishReason":"STOP","content":{"parts":%s}}]%s}`, parts, metadata)
	}))
	defer func() { close(release); provider.Close() }()
	aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Native image source", "platform": "gemini", "type": "apikey", "concurrency": 3, "group_ids": []int64{gid}, "rate_multiplier": 3, "credentials": map[string]any{"api_key": "gemini-image-provider", "base_url": provider.URL, "model_mapping": map[string]string{"draw": "gemini-3-pro-image"}}, "extra": map[string]any{"quota_limit": 100}}))
	prices := []any{map[string]any{"platform": "gemini", "models": []string{"draw"}, "billing_mode": "image", "per_request_price": "0.1"}}
	cid := id(manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Native image prices", "group_ids": []int64{gid}, "model_pricing": prices, "billing_model_source": "requested", "restrict_models": true}))
	cpath := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
	body := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "draw something"}}}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}, "imageConfig": map[string]string{"imageSize": "1K"}}}
	const path = "/v1beta/models/draw:generateContent"
	const streamPath = "/v1beta/models/draw:streamGenerateContent?alt=sse"
	check := func(w *httptest.ResponseRecorder, want string, count int, size, source string) string {
		t.Helper()
		request := w.Header().Get("X-Request-ID")
		var cost, billingSize, billingSource string
		var n, images int
		err := a.DB.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text,COALESCE(max(image_count),0),COALESCE(max(image_size),''),COALESCE(max(image_size_source),'') FROM usage_logs WHERE request_id=$1 AND user_id=$2", request, uid).Scan(&n, &cost, &images, &billingSize, &billingSource)
		if err != nil || n != 1 || cost != want || images != count || billingSize != size || billingSource != source {
			t.Fatalf("image receipt: %d %s %d %s %s %v want %s", n, cost, images, billingSize, billingSource, err, want)
		}
		return request
	}
	expect := func(path, idem string) *httptest.ResponseRecorder {
		t.Helper()
		w := call("POST", path, key, body, idem)
		if w.Code != 200 {
			t.Fatalf("images %d %s", w.Code, w.Body.String())
		}
		return w
	}
	first := expect(path, "image-json")
	check(first, "0.4000000000", 2, "1K", "input")
	if !strings.Contains(first.Body.String(), "iVBORw0KGgo=") {
		t.Fatal("image payload lost")
	}
	before := calls.Load()
	replay := expect(path, "image-json")
	if replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != before {
		t.Fatal("image replay charged upstream")
	}
	mode.Store(1)
	check(expect(streamPath, "image-stream"), "0.4000000000", 2, "1K", "input")
	// Input settings and native metadata remain distinct from billing size.
	var inputSize string
	if err := a.DB.QueryRow("SELECT image_input_size FROM usage_logs WHERE request_id=$1", first.Header().Get("X-Request-ID")).Scan(&inputSize); err != nil || inputSize != "1K" {
		t.Fatal(inputSize, err)
	}
	manage("PUT", gpath, admin, map[string]any{"image_price_1k": "0.2", "image_price_2k": "0.3", "image_price_4k": "0.4", "image_rate_independent": true, "image_rate_multiplier": "0.5"})
	mode.Store(2)
	check(expect(path, "image-no-token-usage"), "0.2000000000", 2, "1K", "input")
	mode.Store(4)
	check(expect(streamPath, "image-large-frame"), "0.1000000000", 1, "1K", "input")
	// Received images stay billable even when the stream fails before STOP.
	mode.Store(5)
	w := call("POST", streamPath, key, body, "image-interrupted")
	check(w, "0.2000000000", 2, "1K", "input")
	if strings.Contains(w.Body.String(), `"finishReason":"STOP"`) || !strings.Contains(w.Body.String(), "error") {
		t.Fatal("interrupted image stream reported success")
	}
	mode.Store(0)
	manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"draw"}, "billing_mode": "per_request", "per_request_price": "0.3"}}})
	check(expect(path, "image-group-card"), "0.3000000000", 2, "1K", "input")
	manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"draw"}, "billing_mode": "token", "input_price": "0.01", "output_price": "0.02", "cache_read_price": "0.005", "image_output_price": "0.03"}}})
	tokenRequest := check(expect(path, "image-token-card"), "1.6700000000", 2, "1K", "input")
	var output, images int
	var imageCost, rate string
	if err := a.DB.QueryRow("SELECT output_tokens,image_output_tokens,image_output_cost::text,rate_multiplier::text FROM usage_logs WHERE request_id=$1", tokenRequest).Scan(&output, &images, &imageCost, &rate); err != nil || output != 22 || images != 12 || imageCost != "0.3600000000" || rate != "2.0000" {
		t.Fatal("token image breakdown", output, images, imageCost, rate, err)
	}
	mode.Store(3)
	check(expect(path, "image-blocked"), "0.2000000000", 0, "", "")
	mode.Store(0)
	manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{}, "image_price_1k": -1, "image_price_2k": -1, "image_price_4k": -1})
	group := manage("GET", gpath, admin, nil)
	if group["image_price_1k"] != nil || group["image_price_2k"] != nil || group["image_price_4k"] != nil {
		t.Fatal("image override clear", group)
	}
	// Explicit token cards need real usage; image counts cannot fabricate tokens.
	manage("PUT", cpath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": "gemini", "models": []string{"draw"}, "billing_mode": "token", "input_price": "0.01", "output_price": "0.02", "cache_read_price": "0.005", "image_output_price": "0.03"}}})
	mode.Store(2)
	if w := call("POST", path, key, body, "missing-image-tokens"); w.Code != 502 {
		t.Fatal("missing token usage accepted", w.Code, w.Body.String())
	}
	mode.Store(0)
	manage("PUT", cpath, admin, map[string]any{"model_pricing": prices})
	mode.Store(3)
	check(expect(path, "image-blocked-per-image"), "0.0000000000", 0, "", "")
	mode.Store(6)
	check(expect(path, "image-no-output"), "0.0000000000", 0, "", "")
	mode.Store(0)
	// The request's tariff survives an administrator price change while upstream runs.
	mode.Store(7)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", path, key, body, "image-snapshot") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("image request did not start")
	}
	manage("PUT", gpath, admin, map[string]any{"image_price_1k": "9"})
	release <- struct{}{}
	check(<-done, "0.1000000000", 2, "1K", "input")
	mode.Store(0)
	manage("PUT", gpath, admin, map[string]any{"image_price_1k": -1})
	// Failure recovery uses the persisted image receipt and does not generate again.
	if _, err := a.DB.Exec("ALTER TABLE usage_logs ADD CONSTRAINT test_native_images CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID"); err != nil {
		t.Fatal(err)
	}
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_native_images")
	w = call("POST", streamPath, key, body, "image-recover")
	if !strings.Contains(w.Body.String(), "error") || strings.Contains(w.Body.String(), `"finishReason":"STOP"`) {
		t.Fatal("unsettled terminal image exposed", w.Body.String())
	}
	if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT test_native_images"); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	for i := 0; i < 2; i++ {
		if err := a.recoverReceipts(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	check(w, "0.1000000000", 2, "1K", "input")
	if calls.Load() != before {
		t.Fatal("recovery regenerated image")
	}
	// Native token counts remain free even with image modalities.
	expect("/v1beta/models/draw:countTokens", "image-count")
	before = calls.Load()
	manage("PUT", cpath, admin, map[string]any{"model_pricing": []any{}})
	if w := call("POST", path, key, body, "image-restrict"); w.Code != 403 || calls.Load() != before {
		t.Fatal("image bypassed restriction", w.Code, w.Body.String())
	}
	// Default image size uses the existing 2K billing tier, including mapped names.
	manage("PUT", cpath, admin, map[string]any{"restrict_models": false})
	delete(body, "generationConfig")
	check(expect(path, "image-default"), "0.2010000000", 2, "2K", "default")
	manage("PUT", gpath, admin, map[string]any{"image_price_2k": 0})
	check(expect(path, "image-free"), "0.0000000000", 2, "2K", "default")
	// The directory uses the same image tariff resolution as the gateway.
	manage("PUT", cpath, admin, map[string]any{"model_pricing": prices, "billing_model_source": "requested"})
	plaza := manage("GET", "/api/v1/model-plaza", token, nil)
	found := false
	for _, raw := range plaza["groups"].([]any) {
		group := raw.(map[string]any)
		if id(group) != gid {
			continue
		}
		model := group["models"].([]any)[0].(map[string]any)
		price := model["image_pricing"].(map[string]any)["2K"].(map[string]any)
		found = price["billing_mode"] == "image" && price["per_request_price"] == float64(0)
	}
	if !found {
		t.Fatal("image directory price differs from free gateway tariff")
	}
	// Existing usage endpoints expose image metadata only to its owner.
	var usageID int64
	if err := a.DB.QueryRow("SELECT id FROM usage_logs WHERE request_id=$1", tokenRequest).Scan(&usageID); err != nil {
		t.Fatal(err)
	}
	usagePath := fmt.Sprintf("/api/v1/usage/%d", usageID)
	visible := manage("GET", usagePath, token, nil)
	if visible["image_count"] != float64(2) || visible["image_input_size"] != "1K" || visible["image_size_source"] != "input" {
		t.Fatal("image usage metadata missing", visible)
	}
	if w := call("GET", usagePath, key, nil, ""); w.Code != 401 {
		t.Fatal("client key accessed login-only usage", w.Code)
	}
	// Invalid tariffs and modalities never dispatch or overwrite valid config.
	before = calls.Load()
	if w := call("PUT", gpath, admin, map[string]any{"image_rate_multiplier": -1}, ""); w.Code != 400 {
		t.Fatal("negative image rate", w.Code)
	}
	body["generationConfig"] = map[string]any{"imageConfig": map[string]string{"imageSize": "8K"}}
	if w := call("POST", path, key, body, "bad-image-size"); w.Code != 400 || calls.Load() != before {
		t.Fatal("invalid size dispatched", w.Code)
	}
	delete(body, "generationConfig")
	// Native composite routing retains actual Gemini image billing and origin.
	cgid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Composite native images", "platform": "composite", "image_price_2k": "0.05"}))
	manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, cgid}})
	manage("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", cgid), admin, map[string]any{"public_model": "draw", "match_type": "exact", "target_platform": "gemini", "upstream_model": "draw", "endpoint": "gemini", "enabled": true})
	ckey := manage("POST", "/api/v1/keys", token, map[string]any{"name": "composite-image", "group_id": cgid})["key"].(string)
	w = call("POST", path, ckey, body, "composite-image")
	if w.Code != 200 {
		t.Fatal("composite image", w.Code, w.Body.String())
	}
	check(w, "0.1000000000", 2, "2K", "default")
	// Compare ledger sums with all three independently persisted quota totals.
	var balanced bool
	if err := a.DB.QueryRow(`SELECT u.balance=100-COALESCE((SELECT sum(actual_cost) FROM usage_logs WHERE user_id=u.id),0) AND k.quota_used=COALESCE((SELECT sum(actual_cost) FROM usage_logs WHERE api_key_id=k.id),0) AND (a.extra->>'quota_used')::numeric=COALESCE((SELECT sum(total_cost*account_rate_multiplier) FROM usage_logs WHERE account_id=a.id),0) FROM users u JOIN api_keys k ON k.user_id=u.id CROSS JOIN accounts a WHERE u.id=$1 AND k.id=$2 AND a.id=$3`, uid, kid, aid).Scan(&balanced); err != nil || !balanced {
		t.Fatal("image ledger totals", balanced, err)
	}
}
