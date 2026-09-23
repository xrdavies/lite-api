package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
		if r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer image-upstream" || r.Header.Get("Cookie") != "" {
			t.Errorf("image upstream isolation: path=%s auth=%q cookie=%q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"))
		}
		var body struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			N      int    `json:"n"`
		}
		raw, decodeErr := io.ReadAll(r.Body)
		if json.Unmarshal(raw, &body) != nil || decodeErr != nil || body.Model != "upstream-image" || body.Prompt != "draw a small lighthouse" || body.N != 2 {
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
	for _, invalid := range []map[string]any{
		{"model": "client-image", "prompt": "", "n": 1},
		{"model": "client-image", "prompt": "draw", "n": 0},
		{"model": "client-image", "prompt": "draw", "n": 11},
		{"model": "client-image", "prompt": "draw", "response_format": "xml"},
	} {
		if w := call("/v1/images/generations", key, invalid, ""); w.Code != http.StatusBadRequest || calls.Load() != 1 {
			t.Fatalf("invalid image request dispatched: %#v -> %d %s", invalid, w.Code, w.Body.String())
		}
	}
	deniedGroup := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Images disabled", "platform": "openai"})["id"].(float64))
	deniedKey := manage("/api/v1/keys", token, map[string]any{"name": "Disabled image key", "group_id": deniedGroup})["key"].(string)
	if w := call("/v1/images/generations", deniedKey, body, ""); w.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("disabled image group accepted: %d %s", w.Code, w.Body.String())
	}
}
