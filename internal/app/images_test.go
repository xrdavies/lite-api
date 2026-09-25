package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectImageUsage(t *testing.T) {
	for _, tc := range []struct {
		body, size, source string
		count              int64
		usage, invalid     bool
	}{
		{`{"data":[{"url":"https://example.test/a"},{"b64_json":"same"},{"b64_json":"same"},{}]}`, "1K", "input", 3, false, false},
		{`{"size":"2048x1024","data":[{"url":"https://example.test/a"},{"b64_json":"a","size":"4096x2048"}]}`, "4K", "output", 2, false, false},
		{`{"data":[{"url":"https://example.test/a"}],"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":2,"image_tokens":3},"output_tokens_details":{"image_tokens":15}}}`, "1K", "input", 1, true, false},
		{`{"data":[{"url":"https://example.test/a"}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`, "1K", "input", 1, true, false},
		{`{"data":[{"url":"https://example.test/a"}],"usage":{"input_tokens":0,"output_tokens":0}}`, "1K", "input", 1, true, false},
		{`{"data":[{}]}`, "1K", "input", 0, false, true},
		{`{"data":[{"url":"https://example.test/a"}],"usage":{"input_tokens":-1,"output_tokens":0}}`, "1K", "input", 1, false, true},
		{`{"data":[{"url":"https://example.test/a"}],"usage":{}}`, "1K", "input", 1, false, true},
	} {
		o := textObservation{Protocol: "images", Usage: priceUsage{ImageInputSize: "1024x1024"}}
		err := o.observe([]byte(tc.body))
		if (err != nil) != tc.invalid || o.Usage.ImageCount != tc.count || o.HasUsage != tc.usage || o.Usage.ImageSize != tc.size || o.Usage.ImageSizeSource != tc.source {
			t.Fatalf("%s: %+v usage=%v err=%v", tc.body, o.Usage, o.HasUsage, err)
		}
		if !tc.usage && (o.Usage.Input != 0 || o.Usage.Output != 0) {
			t.Fatal("invented token usage")
		}
		if strings.Contains(tc.body, `"cached_tokens"`) && (o.Usage.Input != 8 || o.Usage.CacheRead != 2 || o.Usage.ImageInput != 3 || o.Usage.ImageOutput != 15) {
			t.Fatal("image token breakdown", o.Usage)
		}
		if tc.size == "4K" && (o.Usage.ImageSizes != [3]int64{0, 1, 1} || o.Usage.ImageOutputSize != "2048x1024") {
			t.Fatal("mixed output sizes", o.Usage)
		}
	}
	for model, want := range map[string]string{"grok-imagine-image": "0.0200000000", "grok-imagine-image-quality": "0.0700000000", "grok-imagine-image-2.0": "0.0800000000"} {
		s := gatewaySelection{Account: &upstreamAccount{Platform: "grok"}}
		cost, _, _, err := s.generatedImageCost(gatewayGroup{Rate: "1"}, model, priceUsage{ImageCount: 1, ImageSize: "4K"}, "", "", time.Time{})
		if err != nil || cost.Actual != want {
			t.Fatal("Grok image compatibility price", model, cost, err)
		}
	}
}

func TestImageEditRequestParsing(t *testing.T) {
	jsonBody := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(`{"model":"image-edit","prompt":"replace","images":[{"image_url":{"url":"data:image/png;base64,AA=="}}]}`), &jsonBody); err != nil {
		t.Fatal(err)
	}
	in, err := parseTextRequest(httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil), "images", jsonBody)
	if err != nil || in.Action != "edits" || in.Scope != "images.edits" {
		t.Fatalf("json edit parse: %#v %v", in, err)
	}
	if path, err := in.upstreamPath("image-edit"); err != nil || path != "/v1/images/edits" {
		t.Fatalf("edit upstream path: %q %v", path, err)
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("model", "image-edit")
	_ = form.WriteField("prompt", "replace")
	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="image"; filename="source.png"`},
		"Content-Type":        {"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	part.Write([]byte("png-bytes"))
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	multipartBody, err := parseImageMultipart(body.Bytes(), form.FormDataContentType())
	if err != nil {
		t.Fatal(err)
	}
	in, err = parseTextRequest(httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil), "images", multipartBody)
	if err != nil || in.Action != "edits" {
		t.Fatalf("multipart edit parse: %#v %v", in, err)
	}
	for _, raw := range []string{
		`{"model":"image-edit","prompt":"x","images":[{"file_id":"file_1"}]}`,
		`{"model":"image-edit","prompt":"x","images":[]}`,
	} {
		var invalid map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &invalid)
		if _, err := parseTextRequest(httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil), "images", invalid); err == nil {
			t.Fatalf("invalid edit accepted: %s", raw)
		}
	}
}

func TestImagesToGrok(t *testing.T) {
	body := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(`{"model":"m","prompt":"edit","images":[{"url":"https://example.test/a.png"}]}`), &body); err != nil {
		t.Fatal(err)
	}
	out, err := imagesToGrok(body)
	if err != nil || out["images"] != nil {
		t.Fatalf("grok image conversion: %v %#v", err, out)
	}
	var image map[string]any
	if json.Unmarshal(out["image"], &image) != nil || image["type"] != "image_url" || image["url"] != "https://example.test/a.png" {
		t.Fatalf("grok image shape: %s", out["image"])
	}
	for _, tc := range []struct{ body, resolution, aspect string }{
		{`{"size":"1024x1024"}`, "1k", "1:1"},
		{`{"size":"2048x1152"}`, "2k", "16:9"},
		{`{"size":"2160x3840"}`, "2k", "9:16"},
		{`{"size":"1672x941"}`, "2k", "16:9"},
		{`{"size":"1K"}`, "1k", ""},
		{`{"size":"auto"}`, "", ""},
		{`{"size":"1024x1024","resolution":" 2K ","aspect_ratio":"auto"}`, "2k", "auto"},
		{`{"size":"4096x2048","resolution":"1k","aspect_ratio":"21:9"}`, "1k", "21:9"},
	} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(tc.body), &body)
		out, err := imagesToGrok(body)
		if err != nil || out["size"] != nil || credentialString(out, "resolution") != tc.resolution || credentialString(out, "aspect_ratio") != tc.aspect {
			t.Fatal("Grok geometry", tc.body, out, err)
		}
	}
	for _, raw := range []string{`{"resolution":"4k"}`, `{"resolution":1}`, `{"aspect_ratio":"2:0"}`, `{"aspect_ratio":2}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		if _, err := imagesToGrok(body); err == nil {
			t.Fatal("invalid Grok geometry", raw)
		}
	}
}

func TestImageStreamContracts(t *testing.T) {
	for _, stream := range []bool{true, false} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"model":"m","prompt":"draw","stream":%t,"partial_images":3}`, stream)), &body)
		in, err := parseTextRequest(httptest.NewRequest("POST", "/v1/images/generations", nil), "images", body)
		if err != nil || in.Stream != stream {
			t.Fatal("image stream parsing", in, err)
		}
	}
	for _, extra := range []string{`"stream":"true"`, `"partial_images":4`, `"partial_images":1.5`, `"partial_images":-1`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(`{"model":"m","prompt":"draw",`+extra+`}`), &body)
		if _, err := parseTextRequest(httptest.NewRequest("POST", "/v1/images/generations", nil), "images", body); err == nil {
			t.Fatal("invalid stream option", extra)
		}
	}
	o := textObservation{Protocol: "images", ImageStream: &responseImageMeter{}, Usage: priceUsage{ImageInputSize: "1536x1024"}}
	if err := o.observe([]byte(`{"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"eA=="}`)); err != nil || o.Usage.ImageCount != 0 || o.complete() {
		t.Fatal("preview counted", o.Usage, err)
	}
	const completed = `{"type":"image_generation.completed","id":"image_1","b64_json":"eA==","size":"1024x1024","usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"image_tokens":15}}}`
	for range 2 {
		if err := o.observe([]byte(completed)); err != nil || o.Usage.ImageCount != 1 || o.Usage.Output != 20 || !o.HasUsage || !o.complete() {
			t.Fatal("completion or duplicate", o.Usage, err)
		}
	}
	if err := o.observe([]byte(`{"type":"image_edit.completed","b64_json":"eQ==","size":"3840x2160"}`)); err != nil || o.Usage.ImageCount != 2 || o.Usage.ImageSize != "4K" || o.Usage.Output != 20 || !o.HasUsage {
		t.Fatal("multiple completed images", o.Usage, err)
	}
	for _, raw := range []string{`{"type":"error","message":"private"}`, `{"type":"image_generation.completed"}`, `{"type":"image_generation.completed","b64_json":"invalid!"}`, `{"type":"image_generation.partial_image","b64_json":"eA=="}`, `{"type":"image_generation.partial_image","b64_json":"eA==","partial_image_index":3}`} {
		if err := o.observe([]byte(raw)); err == nil || o.Usage.ImageCount != 2 || o.Usage.Output != 20 {
			t.Fatal("invalid event lost known usage", raw, o.Usage, err)
		}
	}
}

func testGrokImages(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.189:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		w := call(path, token, body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var out struct {
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("invalid management response")
		}
		return out.Data
	}
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer grok-image-key" || (r.URL.Path != "/v1/images/generations" && r.URL.Path != "/v1/images/edits") {
			t.Errorf("grok image upstream auth/path: %s %s", r.Header.Get("Authorization"), r.URL.Path)
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if credentialString(body, "model") != "grok-imagine-image-2.0" || credentialString(body, "prompt") == "" {
			t.Errorf("grok image request: %s", mustJSON(body))
		}
		if body["image"] != nil {
			var image map[string]any
			_ = json.Unmarshal(body["image"], &image)
			if image["type"] != "image_url" {
				t.Errorf("grok edit object: %v", image)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1710000000,"data":[{"url":"https://cdn.example/grok.png"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	manage("/api/v1/admin/users", admin, map[string]any{"email": "grok-images@example.test", "password": "grok-images-password", "balance": 1})
	user := manage("/api/v1/auth/login", "", map[string]any{"email": "grok-images@example.test", "password": "grok-images-password"})["access_token"].(string)
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Grok images", "platform": "grok", "allow_image_generation": true, "profit_control_enabled": true, "profit_min_margin": "0.9"})["id"].(float64))
	manage("/api/v1/admin/channels", admin, map[string]any{"name": "Grok image tariff", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "grok", "models": []string{"client-grok-image"}, "billing_mode": "image", "per_request_price": json.Number("0.02")}}})
	manage("/api/v1/admin/accounts", admin, map[string]any{"name": "Grok image account", "platform": "grok", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "grok-image-key", "base_url": upstream.URL, "api_protocol": "chat_completions", "model_mapping": map[string]string{"client-grok-image": "grok-imagine-image-2.0"}}})
	key := manage("/api/v1/keys", user, map[string]any{"name": "Grok image key", "group_id": gid})["key"].(string)
	gen := call("/v1/images/generations", key, map[string]any{"model": "client-grok-image", "prompt": "draw a fox", "n": 1})
	if gen.Code != http.StatusOK || requests.Load() != 1 {
		t.Fatalf("grok image generation: %d %s calls=%d", gen.Code, gen.Body.String(), requests.Load())
	}
	edit := call("/v1/images/edits", key, map[string]any{"model": "client-grok-image", "prompt": "make it blue", "images": []any{map[string]any{"url": "https://example.test/source.png"}}})
	if edit.Code != http.StatusOK || requests.Load() != 2 {
		t.Fatalf("grok image edit: %d %s calls=%d", edit.Code, edit.Body.String(), requests.Load())
	}
	var logged int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE model='client-grok-image'").Scan(&logged); err != nil || logged != 2 {
		t.Fatalf("grok image usage: %d %v", logged, err)
	}
}

func testImages(t *testing.T, a *App, admin string) {
	t.Helper()
	for _, raw := range []string{`{"model":"m","prompt":"draw","n":0}`, `{"model":"m","prompt":"draw","n":11}`, `{"model":"m","prompt":"draw","response_format":"xml"}`} {
		var body map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &body) != nil {
			t.Fatal("test image json")
		}
		if _, err := parseTextRequest(httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil), "images", body); err == nil {
			t.Fatalf("invalid image body accepted: %s", raw)
		}
	}
	call := func(path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.88:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if idem != "" {
			r.Header.Set("Idempotency-Key", idem)
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		t.Helper()
		w := call(path, token, body, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var envelope struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/images/generations" && r.URL.Path != "/v1/images/edits" || r.Header.Get("Authorization") != "Bearer image-upstream" || r.Header.Get("Cookie") != "" {
			t.Errorf("image upstream isolation: path=%s auth=%q cookie=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			N      int    `json:"n"`
		}
		raw, decodeErr := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &body) != nil || decodeErr != nil || body.Model != "upstream-image" || (r.URL.Path == "/v1/images/generations" && (body.Prompt != "draw a small lighthouse" || body.N != 2)) || (r.URL.Path == "/v1/images/edits" && body.Prompt != "replace the sky") {
			t.Errorf("image request changed: %+v raw=%s err=%v", body, raw, decodeErr)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"created":1710000000,"data":[{"b64_json":"one"},{"b64_json":"two"}]}`)
	}))
	defer upstream.Close()
	manage("/api/v1/admin/users", admin, map[string]any{"email": "images@example.test", "password": "images-password", "balance": 1})
	token := manage("/api/v1/auth/login", "", map[string]any{"email": "images@example.test", "password": "images-password"})["access_token"].(string)
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Image generation", "platform": "openai", "allow_image_generation": true})["id"].(float64))
	manage("/api/v1/admin/channels", admin, map[string]any{
		"name": "Image generation tariff", "group_ids": []int64{gid},
		"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"client-image"}, "billing_mode": "image", "per_request_price": json.Number("0.02")}},
	})
	manage("/api/v1/admin/accounts", admin, map[string]any{"name": "Image upstream", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "image-upstream", "base_url": upstream.URL + "/v1", "api_protocol": "chat_completions", "model_mapping": map[string]string{"client-image": "upstream-image"}}})
	key := manage("/api/v1/keys", token, map[string]any{"name": "Image key", "group_id": gid})["key"].(string)
	body := map[string]any{"model": "client-image", "prompt": "draw a small lighthouse", "n": 2, "response_format": "b64_json"}
	first := call("/v1/images/generations", key, body, "image-request")
	second := call("/images/generations", key, body, "image-request")
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("image forwarding or alias replay failed: first=%d second=%d calls=%d", first.Code, second.Code, calls.Load())
	}
	if !strings.Contains(first.Body.String(), `"b64_json":"one"`) {
		t.Fatal("image response was not preserved")
	}
	var cost, balance string
	var logged int
	if err := a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", first.Header().Get("X-Request-ID")).Scan(&cost); err != nil || cost != "0.0400000000" {
		t.Fatal("image settlement cost", cost, err)
	}
	if err := a.DB.QueryRow("SELECT balance::text FROM users WHERE email='images@example.test'").Scan(&balance); err != nil || balance != "0.96000000" {
		t.Fatal("image balance", balance, err)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE model='client-image'").Scan(&logged); err != nil || logged != 1 {
		t.Fatal("image duplicate billing", logged, err)
	}
	editBody := map[string]any{"model": "client-image", "prompt": "replace the sky", "images": []any{map[string]any{"url": "https://example.test/source.png"}}}
	edit := call("/v1/images/edits", key, editBody, "image-edit")
	if edit.Code != http.StatusOK || !strings.Contains(edit.Body.String(), `"b64_json":"one"`) || calls.Load() != 2 {
		t.Fatalf("json image edit failed: %d %s calls=%d", edit.Code, edit.Body.String(), calls.Load())
	}
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	_ = mw.WriteField("model", "client-image")
	_ = mw.WriteField("prompt", "replace the sky")
	part, err := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image"; filename="source.png"`}, "Content-Type": {"image/png"}})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("source-bytes"))
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	multipartRequest := httptest.NewRequest(http.MethodPost, "/images/edits", bytes.NewReader(form.Bytes()))
	multipartRequest.Header.Set("Content-Type", mw.FormDataContentType())
	multipartRequest.Header.Set("Authorization", "Bearer "+key)
	multipartRequest.Header.Set("Idempotency-Key", "image-edit-multipart")
	multipartRequest.RemoteAddr = "192.0.2.88:1234"
	multipartResponse := httptest.NewRecorder()
	a.Handler().ServeHTTP(multipartResponse, multipartRequest)
	if multipartResponse.Code != http.StatusOK || !strings.Contains(multipartResponse.Body.String(), `"b64_json":"one"`) || calls.Load() != 3 {
		t.Fatalf("multipart image edit failed: %d %s calls=%d", multipartResponse.Code, multipartResponse.Body.String(), calls.Load())
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE model='client-image'").Scan(&logged); err != nil || logged != 3 {
		t.Fatal("image edit settlement count", logged, err)
	}
	for _, invalid := range []map[string]any{
		{"model": "client-image", "prompt": "", "n": 1},
		{"model": "client-image", "prompt": "draw", "n": 0},
		{"model": "client-image", "prompt": "draw", "n": 11},
		{"model": "client-image", "prompt": "draw", "response_format": "xml"},
	} {
		if w := call("/v1/images/generations", key, invalid, ""); w.Code != http.StatusBadRequest || calls.Load() != 3 {
			t.Fatalf("invalid image request dispatched: %#v -> %d %s", invalid, w.Code, w.Body.String())
		}
	}
	deniedGroup := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Images disabled", "platform": "openai"})["id"].(float64))
	deniedKey := manage("/api/v1/keys", token, map[string]any{"name": "Disabled image key", "group_id": deniedGroup})["key"].(string)
	if w := call("/v1/images/generations", deniedKey, body, ""); w.Code != http.StatusForbidden || calls.Load() != 3 {
		t.Fatalf("disabled image group accepted: %d %s", w.Code, w.Body.String())
	}
}

func testDirectImageBilling(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.190:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
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
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	for _, platform := range []string{"openai", "grok"} {
		gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": platform + " image billing", "platform": platform, "allow_image_generation": true, "rate_multiplier": 2, "image_rate_independent": true, "image_rate_multiplier": "0.5", "image_price_1k": "0.1", "image_price_2k": "0.2", "image_price_4k": "0.4"}))
		gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		uid := id(manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": platform + "-image-billing@example.test", "password": "image-billing-password", "balance": 100}))
		user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": platform + "-image-billing@example.test", "password": "image-billing-password"})["access_token"].(string)
		k := manage("POST", "/api/v1/keys", user, map[string]any{"name": "image billing", "group_id": gid, "quota": 100})
		key, kid := k["key"].(string), id(k)
		var mode, calls atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var in map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&in) != nil || credentialString(in, "model") != "upstream-image-billing" || r.Header.Get("Authorization") != "Bearer image-billing-provider" {
				t.Error("image billing dispatch")
			}
			if mode.Load() == 3 {
				if _, err := a.DB.Exec("UPDATE groups SET image_price_4k=9,image_rate_multiplier=7 WHERE id=$1", gid); err != nil {
					t.Error(err)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if mode.Load() == 4 {
				fmt.Fprint(w, `{"data":[{}]}`)
				return
			}
			usage, sizes := "", `,"size":"4096x2048"`
			if mode.Load() == 1 {
				usage = `,"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":2,"image_tokens":3},"output_tokens_details":{"image_tokens":15}}`
			}
			if mode.Load() == 2 {
				sizes = ""
			}
			fmt.Fprintf(w, `{"model":"response-image-billing","data":[{"url":"https://example.test/a","size":"1024x1024"},{"url":"https://example.test/b"%s}]%s}`, sizes, usage)
		}))
		defer provider.Close()
		aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": platform + " image billing", "platform": platform, "type": "apikey", "rate_multiplier": 3, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "image-billing-provider", "base_url": provider.URL, "model_mapping": map[string]string{"mapped-image-billing": "upstream-image-billing"}}, "extra": map[string]any{"quota_limit": 100}}))
		price := func(model, billing, value string) map[string]any {
			return map[string]any{"platform": platform, "models": []string{model}, "billing_mode": billing, "per_request_price": value}
		}
		cid := id(manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": platform + " image billing", "group_ids": []int64{gid}, "model_mapping": map[string]any{platform: map[string]string{"public-image-billing": "mapped-image-billing"}}, "model_pricing": []any{price("mapped-image-billing", "image", "0.9")}}))
		cpath := fmt.Sprintf("/api/v1/admin/channels/%d", cid)
		body := map[string]any{"model": "public-image-billing", "prompt": "private image billing prompt", "size": "1024x1024", "n": 2}
		check := func(w *httptest.ResponseRecorder, want, billing, size, source, rate string) {
			t.Helper()
			if w.Code != 200 {
				t.Fatal("image request", w.Code, w.Body.String())
			}
			var cost, mode, gotSize, gotSource, gotRate, model string
			var count int
			if err := a.DB.QueryRow("SELECT actual_cost::text,billing_mode,image_count,image_size,image_size_source,rate_multiplier::text,model FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&cost, &mode, &count, &gotSize, &gotSource, &gotRate, &model); err != nil || cost != want || mode != billing || count != 2 || gotSize != size || gotSource != source || gotRate != rate {
				t.Fatal("direct image billing", cost, mode, count, gotSize, gotSource, gotRate, model, err)
			}
		}
		first := call("POST", "/v1/images/generations", key, body, "sized-images")
		check(first, "0.4000000000", "image", "4K", "output", "0.5000")
		var balance, used, accountUsed string
		if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text,a.extra->>'quota_used' FROM users u JOIN api_keys k ON k.user_id=u.id CROSS JOIN accounts a WHERE u.id=$1 AND k.id=$2 AND a.id=$3", uid, kid, aid).Scan(&balance, &used, &accountUsed); err != nil || balance != "99.60000000" || used != "0.40000000" || accountUsed != "2.40000000" {
			t.Fatal("image balance/quota", balance, used, accountUsed, err)
		}
		if replay := call("POST", "/images/generations", key, body, "sized-images"); replay.Code != 200 || replay.Header().Get("Idempotency-Replayed") != "true" || calls.Load() != 1 {
			t.Fatal("image billing replay", replay.Code, calls.Load())
		}
		mode.Store(3)
		check(call("POST", "/images/generations", key, body, "old-image-price"), "0.4000000000", "image", "4K", "output", "0.5000")
		manage("PUT", gpath, admin, map[string]any{"image_price_4k": "0.4", "image_rate_multiplier": "0.5"})
		mode.Store(0)
		manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{price("mapped-image-billing", "per_request", "0.3")}})
		check(call("POST", "/images/generations", key, body, "group-image-card"), "0.3000000000", "per_request", "4K", "output", "0.5000")
		manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{map[string]any{"platform": platform, "models": []string{"mapped-image-billing"}, "billing_mode": "token", "input_price": "0.01", "output_price": "0.02", "cache_read_price": "0.005", "image_input_price": "0.03", "image_output_price": "0.04"}}})
		missing := call("POST", "/images/generations", key, body, "missing-image-usage")
		var n int
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", missing.Header().Get("X-Request-ID")).Scan(&n); err != nil || n != 0 || missing.Code != 502 {
			t.Fatal("missing token usage billed", missing.Code, n, err)
		}
		mode.Store(1)
		body["images"] = []any{map[string]any{"url": "https://example.test/input.png"}}
		edited := call("POST", "/v1/images/edits", key, body, "image-tokens")
		check(edited, "1.7000000000", "token", "4K", "output", "2.0000")
		var input, output, imageIn, imageOut, cached int
		if err := a.DB.QueryRow("SELECT input_tokens,output_tokens,image_input_tokens,image_output_tokens,cache_read_tokens FROM usage_logs WHERE request_id=$1", edited.Header().Get("X-Request-ID")).Scan(&input, &output, &imageIn, &imageOut, &cached); err != nil || input != 8 || output != 20 || imageIn != 3 || imageOut != 15 || cached != 2 {
			t.Fatal("direct image tokens", input, output, imageIn, imageOut, cached, err)
		}
		delete(body, "images")
		mode.Store(0)
		manage("PUT", gpath, admin, map[string]any{"model_pricing": []any{}, "image_price_1k": -1, "image_price_2k": -1, "image_price_4k": -1})
		for _, source := range []string{"requested", "upstream", "response_model"} {
			billingModel := map[string]string{"requested": "public-image-billing", "upstream": "upstream-image-billing", "response_model": "response-image-billing"}[source]
			manage("PUT", cpath, admin, map[string]any{"billing_model_source": source, "model_pricing": []any{price("mapped-image-billing", "image", "0.9"), price(billingModel, "image", "0.6")}})
			w := call("POST", "/images/generations", key, body, "source-"+source)
			check(w, "0.6000000000", "image", "4K", "output", "0.5000")
			var got string
			if err := a.DB.QueryRow("SELECT model FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&got); err != nil || got != billingModel {
				t.Fatal("image billing source", got, err)
			}
		}
		manage("PUT", cpath, admin, map[string]any{"billing_model_source": "channel_mapped"})
		manage("PUT", gpath, admin, map[string]any{"image_price_4k": 0})
		check(call("POST", "/images/generations", key, body, "free-image"), "0.0000000000", "image", "4K", "output", "0.5000")
		manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": true})
		plaza := manage("GET", "/api/v1/model-plaza", user, nil)
		found := false
		for _, item := range plaza["groups"].([]any) {
			group := item.(map[string]any)
			if int64(group["id"].(float64)) != gid {
				continue
			}
			for _, item := range group["models"].([]any) {
				model := item.(map[string]any)
				if model["name"] != "public-image-billing" {
					continue
				}
				p := model["image_pricing"].(map[string]any)["4K"].(map[string]any)
				found = p["per_request_price"] == float64(0)
			}
		}
		if !found {
			t.Fatal("direct image prices absent from plaza")
		}
		manage("PUT", "/api/v1/admin/settings", admin, map[string]any{"model_plaza_enabled": false})
		mode.Store(4)
		w := call("POST", "/images/generations", key, body, "no-image-output")
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n); err != nil || w.Code != 502 || n != 0 {
			t.Fatal("empty image result billed", w.Code, n, err)
		}
	}
}

func testImageStreams(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.191:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
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
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	for _, platform := range []string{"openai", "grok"} {
		gid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": platform + " image stream", "platform": platform, "allow_image_generation": true, "rate_multiplier": 2, "image_price_2k": "0.2", "image_rate_independent": true, "image_rate_multiplier": "0.5"}))
		gpath := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
		manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": platform + "-image-stream@example.test", "password": "image-stream-password", "balance": 100})
		user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": platform + "-image-stream@example.test", "password": "image-stream-password"})["access_token"].(string)
		key := manage("POST", "/api/v1/keys", user, map[string]any{"name": "stream", "group_id": gid, "quota": 100})["key"].(string)
		var mode, calls atomic.Int32
		var retryCalls atomic.Int32
		drain := make(chan struct{})
		var release sync.Once
		defer release.Do(func() { close(drain) })
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "upstream-stream-image" || r.Header.Get("Authorization") != "Bearer stream-image-provider" {
				t.Error("image stream dispatch")
			}
			if platform == "grok" && (body["size"] != nil || credentialString(body, "resolution") != "2k" || credentialString(body, "aspect_ratio") != "16:9") {
				t.Error("Grok geometry was not translated", body)
			}
			if platform == "openai" && (credentialString(body, "size") != "2048x1152" || body["resolution"] != nil) {
				t.Error("OpenAI geometry changed")
			}
			if mode.Load() == 8 && retryCalls.Add(1) == 1 {
				w.WriteHeader(429)
				return
			}
			if mode.Load() == 5 {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"data":[{"b64_json":"eA=="}]}`)
				return
			}
			if string(body["stream"]) != "true" || string(body["partial_images"]) != "1" {
				t.Error("stream controls not forwarded", body)
			}
			if mode.Load() == 4 {
				if _, err := a.DB.Exec("UPDATE groups SET image_price_2k=9,image_rate_multiplier=7 WHERE id=$1", gid); err != nil {
					t.Error(err)
				}
			}
			kind := "image_generation"
			if strings.HasSuffix(r.URL.Path, "/edits") {
				kind = "image_edit"
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: %s.partial_image\ndata: {\"type\":%q,\"b64_json\":\"cHJldmlldw==\",\"partial_image_index\":0}\n\n", kind, kind+".partial_image")
			w.(http.Flusher).Flush()
			if mode.Load() == 7 {
				select {
				case <-drain:
				case <-r.Context().Done():
					t.Error("image generation canceled before usage drain")
					return
				}
			}
			if mode.Load() == 2 {
				return
			}
			data := "ZmluYWw="
			if mode.Load() == 3 {
				data = strings.Repeat("AAAA", 800000)
			}
			usage := `,"usage":{"input_tokens":10,"output_tokens":20,"output_tokens_details":{"image_tokens":15}}`
			if mode.Load() == 6 {
				usage = ""
			}
			completed := fmt.Sprintf("event: %s.completed\ndata: {\"type\":%q,\"id\":\"img_1\",\"b64_json\":%q,\"size\":\"2048x1152\"%s}\n\n", kind, kind+".completed", data, usage)
			fmt.Fprint(w, completed)
			if mode.Load() == 1 {
				fmt.Fprint(w, "data: {\"type\":\"error\",\"message\":\"private-provider-error\"}\n\n")
				return
			}
			if mode.Load() != 3 {
				fmt.Fprint(w, completed)
			} // Relay duplicate must not double bill.
			// Native images finish at EOF; some relays append DONE.
			if platform == "grok" {
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		}))
		defer provider.Close()
		aid := id(manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": platform + " stream source", "platform": platform, "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "stream-image-provider", "base_url": provider.URL, "model_mapping": map[string]string{"stream-image": "upstream-stream-image"}}}))
		body := map[string]any{"model": "stream-image", "prompt": "stream a lighthouse", "size": "2048x1152", "stream": true, "partial_images": 1}
		checkBill := func(w *httptest.ResponseRecorder, count int, want string) {
			t.Helper()
			var n int
			var cost string
			if err := a.DB.QueryRow("SELECT count(*),COALESCE(sum(actual_cost),0)::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&n, &cost); err != nil || n != count || count > 0 && cost != want {
				t.Fatal("stream billing", n, cost, err, w.Body.String())
			}
		}
		first := call("POST", "/v1/images/generations", key, body, "stream-one")
		if first.Code != 200 || !strings.Contains(first.Body.String(), "partial_image") || !strings.Contains(first.Body.String(), "completed") || strings.Contains(first.Body.String(), `"error"`) {
			t.Fatal("image stream", first.Code, first.Body.String())
		}
		checkBill(first, 1, "0.1000000000")
		replay := call("POST", "/images/generations", key, body, "stream-one")
		if replay.Code != 200 || replay.Body.String() != first.Body.String() || replay.Header().Get("Content-Type") != "text/event-stream" || calls.Load() != 1 {
			t.Fatal("image SSE replay", replay.Code, calls.Load())
		}
		for _, m := range []int32{1, 2, 3, 4} {
			mode.Store(m)
			w := call("POST", "/images/generations", key, body, fmt.Sprintf("image-stream-%d", m))
			if m == 1 || m == 2 {
				if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), ".completed") || strings.Contains(w.Body.String(), "private-provider-error") {
					t.Fatal("failed stream reported completion", w.Code, w.Body.String())
				}
			}
			if m == 2 {
				checkBill(w, 0, "")
			} else {
				checkBill(w, 1, "0.1000000000")
			}
		}
		manage("PUT", gpath, admin, map[string]any{"image_price_2k": "0.2", "image_rate_multiplier": "0.5"})
		mode.Store(7)
		gateway := httptest.NewServer(a.Handler())
		ctx, cancel := context.WithCancel(context.Background())
		raw, _ := json.Marshal(body)
		request, _ := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/images/generations", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer "+key)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			gateway.Close()
			t.Fatal(err)
		}
		requestID := response.Header.Get("X-Request-ID")
		cancel()
		response.Body.Close()
		release.Do(func() { close(drain) })
		deadline := time.Now().Add(5 * time.Second)
		var drained bool
		for time.Now().Before(deadline) {
			var cost string
			if a.DB.QueryRow("SELECT actual_cost::text FROM usage_logs WHERE request_id=$1", requestID).Scan(&cost) == nil {
				drained = cost == "0.1000000000"
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		gateway.Close()
		if !drained {
			t.Fatal("disconnected image stream lost consumption")
		}
		mode.Store(0)
		// Multipart edits preserve stream and geometry controls.
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		for k, v := range map[string]string{"model": "stream-image", "prompt": "edit a lighthouse", "size": "2048x1152", "stream": "true", "partial_images": "1"} {
			_ = mw.WriteField(k, v)
		}
		part, _ := mw.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image"; filename="source.png"`}, "Content-Type": {"image/png"}})
		_, _ = part.Write([]byte("source"))
		_ = mw.Close()
		r := httptest.NewRequest("POST", "/images/edits", &form)
		r.RemoteAddr = "192.0.2.191:1234"
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("Authorization", "Bearer "+key)
		edited := httptest.NewRecorder()
		a.Handler().ServeHTTP(edited, r)
		if edited.Code != 200 || !strings.Contains(edited.Body.String(), "image_edit.completed") {
			t.Fatal("multipart image stream", edited.Code, edited.Body.String())
		}
		checkBill(edited, 1, "0.1000000000")
		// Failed SQL stores a receipt before withholding completed output.
		if _, err := a.DB.Exec(fmt.Sprintf("ALTER TABLE usage_logs ADD CONSTRAINT stream_image_bill_failure CHECK (account_id<>%d) NOT VALID", aid)); err != nil {
			t.Fatal(err)
		}
		failed := call("POST", "/images/generations", key, body, "stream-sql-failure")
		if !strings.Contains(failed.Body.String(), `"error"`) || strings.Contains(failed.Body.String(), ".completed") {
			t.Fatal("unsettled final image exposed")
		}
		if _, err := a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT stream_image_bill_failure"); err != nil {
			t.Fatal(err)
		}
		manage("PUT", gpath, admin, map[string]any{"image_price_2k": 9})
		fresh := &App{DB: a.DB, Redis: a.Redis}
		for range 2 {
			if err := fresh.recoverReceipts(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		checkBill(failed, 1, "0.1000000000")
		manage("PUT", gpath, admin, map[string]any{"image_price_2k": "0.2", "model_pricing": []any{map[string]any{"platform": platform, "models": []string{"stream-image"}, "billing_mode": "token", "input_price": "0.01", "output_price": "0.02", "image_output_price": "0.04"}}})
		billed := call("POST", "/images/generations", key, body, "image-stream-tokens")
		checkBill(billed, 1, "1.6000000000")
		mode.Store(6)
		missing := call("POST", "/images/generations", key, body, "image-stream-no-tokens")
		checkBill(missing, 0, "")
		if !strings.Contains(missing.Body.String(), `"error"`) || strings.Contains(missing.Body.String(), ".completed") {
			t.Fatal("missing tokens published as success")
		}
		// Composite dispatch and synchronous generation use the same conversion.
		mode.Store(5)
		cgid := id(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": platform + " composite image stream", "platform": "composite", "allow_image_generation": true, "image_price_2k": "0.2"}))
		manage("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", aid), admin, map[string]any{"group_ids": []int64{gid, cgid}})
		manage("POST", fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", cgid), admin, map[string]any{"public_model": "stream-image", "match_type": "exact", "target_platform": platform, "upstream_model": "stream-image", "endpoint": "images", "enabled": true})
		ckey := manage("POST", "/api/v1/keys", user, map[string]any{"name": "composite images", "group_id": cgid})["key"].(string)
		body["stream"] = false
		checkBill(call("POST", "/images/generations", ckey, body, "sync-geometry"), 1, "0.2000000000")
		mode.Store(0)
		body["stream"] = true
		checkBill(call("POST", "/images/generations", ckey, body, "composite-stream"), 1, "0.2000000000")
		manage("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": platform + " stream retry", "platform": platform, "type": "apikey", "priority": 100, "group_ids": []int64{cgid}, "credentials": map[string]any{"api_key": "stream-image-provider", "base_url": provider.URL, "model_mapping": map[string]string{"stream-image": "upstream-stream-image"}}})
		mode.Store(8)
		body["images"] = []any{map[string]any{"url": "https://example.test/input.png"}}
		checkBill(call("POST", "/images/edits", ckey, body, "retry-stream-geometry"), 1, "0.2000000000")
		if retryCalls.Load() != 2 {
			t.Fatal("stream retry did not try both accounts", retryCalls.Load())
		}
	}
}
