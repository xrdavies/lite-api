package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestResponsesHealthStatus(t *testing.T) {
	for _, tt := range []struct {
		status, reason, text string
		success              bool
	}{
		{"completed", "", "OK", true},
		{"incomplete", "max_output_tokens", "OK", true},
		{"failed", "", "partial", false},
		{"cancelled", "", "partial", false},
		{"queued", "", "partial", false},
		{"in_progress", "", "partial", false},
		{"incomplete", "content_filter", "partial", false},
		{"incomplete", "", "partial", false},
		{"", "", "partial", false},
		{"completed", "", "", false},
	} {
		t.Run(tt.status+"/"+tt.reason+"/"+tt.text, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"id":"resp_health","object":"response","usage":{"input_tokens":4,"output_tokens":1},"status":%q,"incomplete_details":{"reason":%q},"output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]}`, tt.status, tt.reason, tt.text)
			}))
			defer up.Close()
			raw, _ := json.Marshal(map[string]any{"base_url": up.URL, "api_key": "test-secret", "api_protocol": "responses", "model_mapping": map[string]string{"test-model": "test-model"}})
			u := &upstreamAccount{ID: 1, Platform: "openai", Type: "apikey"}
			_ = json.Unmarshal(raw, &u.Credentials)
			a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
			result := a.runAccountTest(context.Background(), u, accountTestInput{Model: "test-model"})
			if (result.Status == "success") != tt.success {
				t.Fatalf("unexpected health result: %+v", result)
			}
		})
	}
}

func TestTextHealthGatewayCompatibility(t *testing.T) {
	for _, tc := range []struct{ protocol, body string }{
		{"chat_completions", `{"choices":[{"message":{"content":"OK"}}],"usage":{"prompt_tokens":210,"completion_tokens":1,"total_tokens":436,"completion_tokens_details":{"reasoning_tokens":225}}}`},
		{"responses", `{"id":"resp_health","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":4,"output_tokens":1}}`},
		{"anthropic", `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":4,"output_tokens":1}}`},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1}}`},
	} {
		for _, mode := range []string{"valid", "missing", "invalid"} {
			t.Run(tc.protocol+"/"+mode, func(t *testing.T) {
				var body map[string]json.RawMessage
				_ = json.Unmarshal([]byte(tc.body), &body)
				field := "usage"
				if tc.protocol == "gemini" {
					field = "usageMetadata"
				}
				if mode == "missing" {
					delete(body, field)
				} else if mode == "invalid" {
					body[field] = json.RawMessage(`{"prompt_tokens":-1,"input_tokens":-1,"promptTokenCount":-1,"completion_tokens":1,"output_tokens":1}`)
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(body) }))
				defer up.Close()
				u := &upstreamAccount{ID: 1, Platform: "openai", Type: "apikey", Credentials: map[string]json.RawMessage{"base_url": mustJSON(up.URL), "api_key": mustJSON("health-secret"), "api_protocol": mustJSON(tc.protocol)}}
				if tc.protocol == "gemini" {
					u.Platform = "gemini"
				}
				a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
				result := a.runAccountTest(context.Background(), u, accountTestInput{Model: "test-model", Mode: "text"})
				if result.GenerationStatus != "success" || (result.Status == "success") != (mode == "valid") || (result.GatewayStatus == "success") != (mode == "valid") {
					t.Fatal("generation hid incompatible gateway usage", result)
				}
				if mode != "valid" && !strings.HasPrefix(result.Error, "gateway compatibility check failed:") {
					t.Fatal("missing compatibility diagnosis", result)
				}
			})
		}
	}
}
