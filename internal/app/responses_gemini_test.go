package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesGeminiBridge(t *testing.T) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"model":"gemini-3.1-pro","stream":true,"max_output_tokens":32,"input":"hello"}`), &body); err != nil {
		t.Fatal(err)
	}
	wire, _, _, err := responsesGeminiRequest(map[string]any{
		"model":             body["model"],
		"stream":            body["stream"],
		"max_output_tokens": body["max_output_tokens"],
		"messages":          []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil || !strings.Contains(string(wire), `"contents"`) || strings.Contains(string(wire), `"messages"`) {
		t.Fatalf("request conversion: %s: %v", wire, err)
	}
	s := newResponsesGeminiStream("public", nil)
	for _, raw := range []string{
		`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello"}]}}]}`,
		`{"candidates":[{"index":0,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
	} {
		if _, err := s.observe([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	terminal, result, err := s.finish(priceUsage{Input: 3, Output: 2})
	if err != nil || !strings.Contains(terminal, `response.completed`) || !strings.Contains(string(result), `"status":"completed"`) || !strings.Contains(string(result), `hello`) {
		t.Fatalf("response conversion: terminal=%s result=%s err=%v", terminal, result, err)
	}
}
