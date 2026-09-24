package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func webSearchMode(raw json.RawMessage) string {
	if string(raw) == "true" {
		return "enabled"
	}
	var mode string
	_ = json.Unmarshal(raw, &mode)
	if mode == "enabled" || mode == "disabled" {
		return mode
	}
	return "default"
}

func onlyWebSearch(request map[string]json.RawMessage) bool {
	var tools []struct{ Type, Name string }
	if json.Unmarshal(request["tools"], &tools) != nil || len(tools) != 1 {
		return false
	}
	t := tools[0]
	return strings.HasPrefix(t.Type, "web_search") || t.Type == "google_search" || t.Name == "web_search" || t.Name == "google_search" || t.Name == "web_search_20250305"
}

func webSearchQuery(request map[string]json.RawMessage) string {
	var messages []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(request["messages"], &messages) != nil || len(messages) == 0 || messages[len(messages)-1].Role != "user" {
		return ""
	}
	content := messages[len(messages)-1].Content
	var query string
	if json.Unmarshal(content, &query) == nil {
		return query
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

func (a *App) searchEmulation(ctx context.Context, in textRequest, s *gatewaySelection, request map[string]json.RawMessage) (*webSearchConfig, error) {
	// The reference API Key execution branch is Anthropic Messages only.
	if in.Protocol != "anthropic" || in.CountOnly || s.Account.Platform != "anthropic" || s.Account.Type != "apikey" || !onlyWebSearch(request) {
		return nil, nil
	}
	mode := webSearchMode(s.Account.Extra[webSearchFeature])
	var platforms map[string]bool
	_ = json.Unmarshal(s.Features[webSearchFeature], &platforms)
	if mode == "disabled" || mode != "enabled" && !platforms[s.Account.Platform] {
		return nil, nil
	}
	cfg, err := loadWebSearch(ctx, a.DB)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || len(cfg.Providers) == 0 {
		return nil, nil
	}
	query := webSearchQuery(request)
	if strings.TrimSpace(query) == "" || len(query) > 4096 || strings.ContainsRune(query, 0) {
		return nil, bad("search requires a final user text of 1 to 4096 bytes")
	}
	return &cfg, nil
}

func webSearchMessages(result *webSearchResponse, model string, stream bool) *http.Response {
	id, tool := "msg_ws_"+randomToken(18), "srvtoolu_ws_"+randomToken(12)
	blocks := make([]map[string]string, 0, len(result.Results))
	var summary strings.Builder
	if len(result.Results) == 0 {
		summary.WriteString("No search results found for: " + result.Query)
	} else {
		fmt.Fprintf(&summary, "Here are the search results for %q:\n\n", result.Query)
	}
	for i, r := range result.Results {
		block := map[string]string{"type": "web_search_result", "url": r.URL, "title": r.Title}
		if r.Snippet != "" {
			block["page_content"] = r.Snippet
		}
		if r.PageAge != "" {
			block["page_age"] = r.PageAge
		}
		blocks = append(blocks, block)
		fmt.Fprintf(&summary, "%d. **%s**\n   %s\n   %s\n\n", i+1, r.Title, r.URL, r.Snippet)
	}
	content := []any{
		map[string]any{"type": "server_tool_use", "id": tool, "name": "web_search", "input": map[string]string{"query": result.Query}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": tool, "content": blocks},
		map[string]any{"type": "text", "text": summary.String()},
	}
	// This estimate is presentation metadata, never observed model consumption.
	usage := map[string]int{"input_tokens": 0, "output_tokens": summary.Len() / 4}
	msg := map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": content, "stop_reason": "end_turn", "stop_sequence": nil, "usage": usage}
	raw, _ := json.Marshal(msg)
	body, contentType := string(raw), "application/json"
	if stream {
		msg["content"], msg["stop_reason"], msg["usage"] = []any{}, nil, map[string]int{"input_tokens": 0, "output_tokens": 0}
		var wire strings.Builder
		wire.WriteString(messagesEvent("message_start", map[string]any{"message": msg}))
		for i, block := range content {
			if i == 2 {
				block = map[string]string{"type": "text", "text": ""}
			}
			wire.WriteString(messagesEvent("content_block_start", map[string]any{"index": i, "content_block": block}))
			if i == 2 {
				wire.WriteString(messagesEvent("content_block_delta", map[string]any{"index": i, "delta": map[string]string{"type": "text_delta", "text": summary.String()}}))
			}
			wire.WriteString(messagesEvent("content_block_stop", map[string]any{"index": i}))
		}
		wire.WriteString(messagesEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usage}))
		wire.WriteString(messagesEvent("message_stop", map[string]any{}))
		body, contentType = wire.String(), "text/event-stream"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}
