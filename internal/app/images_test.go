package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync/atomic"
	"testing"
)

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
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Grok images", "platform": "grok", "allow_image_generation": true})["id"].(float64))
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
