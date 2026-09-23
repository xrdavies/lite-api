package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// CheckUpstream performs explicit, billable JSON and SSE probes without a database.
// Only sanitized summaries are returned; credentials never enter output or files.
func CheckUpstream(ctx context.Context, base, key, model string) (map[string]any, error) {
	if key == "" || model == "" || base == "" {
		return nil, errors.New("UPSTREAM_BASE_URL, UPSTREAM_MODEL and UPSTREAM_API_KEY are required")
	}
	a := &App{}
	credentials := map[string]json.RawMessage{}
	credentials["api_key"], _ = json.Marshal(key)
	credentials["base_url"], _ = json.Marshal(base)
	u := &upstreamAccount{Platform: "openai", Type: "apikey", Credentials: credentials}
	result := a.runAccountTest(ctx, u, model, "")
	if result.Status != "success" {
		return nil, errors.New(result.Error)
	}
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply with OK."}}, "max_completion_tokens": 64, "stream": true, "stream_options": map[string]any{"include_usage": true}})
	streamCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := a.upstreamRequest(streamCtx, u, http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("streaming probe did not return HTTP 200")
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return nil, errors.New("streaming probe did not return SSE")
	}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	chunks := 0
	hasText, hasUsage, done := false, false, false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if value == "[DONE]" {
			done = true
			break
		}
		var event struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct{ Delta struct{ Content string } }
			Usage   *struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			}
		}
		if json.Unmarshal([]byte(value), &event) != nil || event.Error != nil {
			return nil, errors.New("invalid streaming event")
		}
		chunks++
		for _, choice := range event.Choices {
			hasText = hasText || choice.Delta.Content != ""
		}
		hasUsage = hasUsage || (event.Usage != nil && event.Usage.Prompt > 0 && event.Usage.Completion > 0)
	}
	if scanner.Err() != nil || !done || !hasText || !hasUsage {
		return nil, errors.New("streaming probe did not finish with text, usage and DONE")
	}
	return map[string]any{"model": model, "json": "success", "json_latency_ms": result.Latency, "sse": "success", "sse_chunks": chunks, "sse_usage": true, "sse_done": true}, nil
}
