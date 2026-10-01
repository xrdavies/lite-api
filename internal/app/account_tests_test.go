package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
				fmt.Fprintf(w, `{"status":%q,"incomplete_details":{"reason":%q},"output":[{"type":"message","content":[{"type":"output_text","text":%q}]}]}`, tt.status, tt.reason, tt.text)
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
