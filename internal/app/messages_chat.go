package app

import (
	"encoding/json"
	"strings"
)

func messagesChatPlatform(platform string) bool {
	return chatResponsesPlatform(platform) || platform == "grok"
}

// Shared controls retain the same validation and effort mapping on both targets.
func messagesOpenAIOptions(body map[string]json.RawMessage) (map[string]any, string, error) {
	out := map[string]any{"model": body["model"], "stream": false, "store": false, "max_completion_tokens": body["max_tokens"]}
	for _, name := range []string{"stream", "temperature", "top_p", "service_tier"} {
		if body[name] != nil {
			out[name] = body[name]
		}
	}
	if string(body["stream"]) == "true" {
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	for _, name := range []string{"top_k", "context_management", "container", "mcp_servers"} {
		if raw := body[name]; raw != nil && string(raw) != "null" {
			return nil, "", bad(name + " requires a native Messages account")
		}
	}
	if raw := body["stop_sequences"]; raw != nil && string(raw) != "null" {
		var stops []string
		if json.Unmarshal(raw, &stops) != nil || len(stops) > 4 {
			return nil, "", bad("invalid stop_sequences")
		}
		for _, stop := range stops {
			if stop == "" {
				return nil, "", bad("empty stop sequence")
			}
		}
		if len(stops) > 0 {
			out["stop"] = stops
		}
	}
	effort, err := requestEffort(body, "anthropic")
	if err != nil {
		return nil, "", err
	}
	if effort == "max" {
		model := strings.ToLower(credentialString(body, "model"))
		if i := strings.LastIndex(model, "/"); i >= 0 {
			model = model[i+1:]
		}
		model = strings.ReplaceAll(model, "_", "-")
		keep := model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-") || model == "gpt-6-astra" || strings.HasPrefix(model, "gpt-6-astra-") || strings.HasPrefix(model, "deepseek-v4") || strings.HasPrefix(model, "deepseek-flash") || strings.HasPrefix(model, "glm-") || strings.HasPrefix(model, "kimi-") || strings.HasPrefix(model, "moonshot-") || model == "k3" || strings.HasPrefix(model, "k3-")
		if !keep {
			effort = "xhigh"
		}
	}
	if effort != "" {
		out["reasoning_effort"] = effort
	}
	var config map[string]json.RawMessage
	if raw := body["output_config"]; raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return nil, "", bad("invalid output_config")
		}
		if raw := config["format"]; raw != nil && string(raw) != "null" {
			var f map[string]json.RawMessage
			if json.Unmarshal(raw, &f) != nil || credentialString(f, "type") != "json_schema" || f["schema"] == nil {
				return nil, "", bad("invalid output format")
			}
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": f["schema"], "strict": true}}
		}
	}
	var tools []map[string]json.RawMessage
	if raw := body["tools"]; raw != nil && json.Unmarshal(raw, &tools) != nil {
		return nil, "", bad("invalid tools")
	}
	declared := map[string]bool{}
	converted := []any{}
	for _, tool := range tools {
		kind, name := credentialString(tool, "type"), credentialString(tool, "name")
		if kind != "" && kind != "custom" {
			return nil, "", bad("hosted tools require native tool billing")
		}
		var schema map[string]json.RawMessage
		if name == "" || len(name) > 256 || declared[name] || json.Unmarshal(tool["input_schema"], &schema) != nil || schema == nil {
			return nil, "", bad("invalid or duplicate tool definition")
		}
		declared[name] = true
		f := map[string]any{"name": name, "parameters": tool["input_schema"], "strict": false}
		for _, key := range []string{"description", "strict"} {
			if tool[key] != nil {
				f[key] = tool[key]
			}
		}
		converted = append(converted, map[string]any{"type": "function", "function": f})
	}
	if len(converted) > 0 {
		out["tools"] = converted
	}
	if raw := body["tool_choice"]; raw != nil && string(raw) != "null" {
		var c struct {
			Type, Name string
			Disable    *bool `json:"disable_parallel_tool_use"`
		}
		if json.Unmarshal(raw, &c) != nil {
			return nil, "", bad("invalid tool_choice")
		}
		switch c.Type {
		case "auto", "none":
			out["tool_choice"] = c.Type
		case "any":
			if len(declared) == 0 {
				return nil, "", bad("tool_choice requires tools")
			}
			out["tool_choice"] = "required"
		case "tool":
			if !declared[c.Name] {
				return nil, "", bad("tool_choice references an undeclared tool")
			}
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": c.Name}}
		default:
			return nil, "", bad("invalid tool_choice")
		}
		if c.Disable != nil {
			out["parallel_tool_calls"] = !*c.Disable
		}
	}
	return out, effort, nil
}

func messagesToChat(body map[string]json.RawMessage) ([]byte, string, error) {
	out, effort, err := messagesOpenAIOptions(body)
	if err != nil {
		return nil, "", err
	}
	messages := []convertedChatMessage{}
	if raw := body["system"]; raw != nil && string(raw) != "null" {
		blocks, err := messagesBlocks(raw)
		if err != nil {
			return nil, "", err
		}
		texts := []string{}
		for _, b := range blocks {
			if credentialString(b, "type") != "text" {
				return nil, "", bad("system content must be text")
			}
			var text string
			if json.Unmarshal(b["text"], &text) != nil {
				return nil, "", bad("invalid system text")
			}
			if !strings.HasPrefix(text, "x-anthropic-billing-header: ") {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			messages = append(messages, convertedChatMessage{Role: "system", Content: strings.Join(texts, "\n\n")})
		}
	}
	var input []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(body["messages"], &input) != nil || len(input) == 0 {
		return nil, "", bad("messages are required")
	}
	seenCalls := map[string]bool{}
	for _, m := range input {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, "", bad("invalid Messages role")
		}
		blocks, err := messagesBlocks(m.Content)
		if err != nil {
			return nil, "", err
		}
		msg := convertedChatMessage{Role: m.Role, Content: nil}
		parts := []any{}
		text := []string{}
		thoughts := []string{}
		resultMedia := []any{}
		flush := func() {
			if len(parts) > 0 {
				msg.Content = parts
			}
			if m.Role == "assistant" {
				msg.Content = strings.Join(text, "")
			}
			if len(msg.Calls) > 0 {
				msg.Reasoning = strings.Join(thoughts, "\n")
			}
			if len(parts) > 0 || len(msg.Calls) > 0 {
				messages = append(messages, msg)
			}
			msg = convertedChatMessage{Role: m.Role, Content: nil}
			parts = nil
			text = nil
			thoughts = nil
		}
		for _, b := range blocks {
			switch kind := credentialString(b, "type"); kind {
			case "text", "image", "document":
				p, err := messagesChatPart(b)
				if err != nil {
					return nil, "", err
				}
				if m.Role == "assistant" && kind != "text" {
					return nil, "", bad("assistant media requires a native Messages account")
				}
				parts = append(parts, p)
				if kind == "text" {
					text = append(text, credentialString(b, "text"))
				}
			case "thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires assistant role")
				}
				var thought string
				if json.Unmarshal(b["thinking"], &thought) != nil {
					return nil, "", bad("invalid thinking text")
				}
				thoughts = append(thoughts, thought)
			case "redacted_thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires assistant role")
				}
			case "tool_use":
				id, name := credentialString(b, "id"), credentialString(b, "name")
				var args map[string]json.RawMessage
				if m.Role != "assistant" || id == "" || name == "" || len(id) > 256 || len(name) > 256 || seenCalls[id] || json.Unmarshal(b["input"], &args) != nil || args == nil {
					return nil, "", bad("invalid or duplicate tool_use")
				}
				seenCalls[id] = true
				call := convertedChatTool{ID: id, Type: "function"}
				call.Function.Name = name
				call.Function.Arguments = string(b["input"])
				msg.Calls = append(msg.Calls, call)
			case "tool_result":
				id := credentialString(b, "tool_use_id")
				if m.Role != "user" || id == "" || len(id) > 256 {
					return nil, "", bad("tool_result requires a user role and tool_use_id")
				}
				inner := []map[string]json.RawMessage{}
				if raw := b["content"]; raw != nil && string(raw) != "null" {
					inner, err = messagesBlocks(raw)
					if err != nil {
						return nil, "", err
					}
				}
				resultText := []string{}
				media := []any{}
				for _, p := range inner {
					value, err := messagesChatPart(p)
					if err != nil {
						return nil, "", err
					}
					if credentialString(p, "type") == "text" {
						resultText = append(resultText, credentialString(p, "text"))
					} else {
						media = append(media, value)
					}
				}
				failed := false
				if raw := b["is_error"]; raw != nil && json.Unmarshal(raw, &failed) != nil {
					return nil, "", bad("invalid tool result error flag")
				}
				result := strings.Join(resultText, "\n\n")
				if failed {
					result = "Tool error: " + result
				}
				messages = append(messages, convertedChatMessage{Role: "tool", CallID: id, Content: result})
				resultMedia = append(resultMedia, media...)
			default:
				return nil, "", bad("content requires a native Messages account")
			}
		}
		// All parallel tool results must precede user media/content on Chat.
		if len(resultMedia) > 0 {
			messages = append(messages, convertedChatMessage{Role: "user", Content: resultMedia})
		}
		flush()
	}
	if len(messages) == 0 {
		return nil, "", bad("messages contain no convertible content")
	}
	out["messages"] = messages
	raw, err := json.Marshal(out)
	return raw, effort, err
}

func messagesBlocks(raw json.RawMessage) ([]map[string]json.RawMessage, error) {
	var text string
	if string(raw) != "null" && json.Unmarshal(raw, &text) == nil {
		return []map[string]json.RawMessage{{"type": json.RawMessage(`"text"`), "text": raw}}, nil
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return nil, bad("invalid Messages content")
	}
	return blocks, nil
}

func messagesChatPart(b map[string]json.RawMessage) (map[string]any, error) {
	kind := credentialString(b, "type")
	if kind == "text" {
		var text string
		if json.Unmarshal(b["text"], &text) != nil {
			return nil, bad("invalid text content")
		}
		return map[string]any{"type": "text", "text": text}, nil
	}
	if kind != "image" && kind != "document" {
		return nil, bad("unsupported tool result content")
	}
	var src map[string]json.RawMessage
	if json.Unmarshal(b["source"], &src) != nil {
		return nil, bad("invalid media source")
	}
	value := credentialString(src, "url")
	if credentialString(src, "type") == "base64" {
		value = "data:" + credentialString(src, "media_type") + ";base64," + credentialString(src, "data")
	} else if credentialString(src, "type") != "url" {
		return nil, bad("unsupported media source")
	}
	if _, err := anthropicMediaSource(value, kind == "image"); err != nil {
		return nil, err
	}
	if kind == "image" {
		return map[string]any{"type": "image_url", "image_url": map[string]string{"url": value}}, nil
	}
	return map[string]any{"type": "file", "file": map[string]string{"filename": "document.pdf", "file_data": value}}, nil
}

type chatMessagesStream struct {
	State                  *chatResponsesStream
	ID, Model              string
	Started                bool
	Sent                   []int
	ActiveKind             string
	ActiveIndex, NextIndex int
}

func newChatMessagesStream(model string) *chatMessagesStream {
	state := newChatResponsesStream(model, nil)
	state.Quiet = true
	return &chatMessagesStream{State: state, ID: "msg_lite_" + randomToken(18), Model: model}
}
func messagesEvent(kind string, fields map[string]any) string {
	fields["type"] = kind
	raw, _ := json.Marshal(fields)
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}
func messagesUsage(u priceUsage) map[string]any {
	return map[string]any{"input_tokens": u.Input, "output_tokens": u.Output, "cache_read_input_tokens": u.CacheRead, "cache_creation_input_tokens": u.CacheWrite}
}
func (s *chatMessagesStream) result(content []any, reason any, u priceUsage) map[string]any {
	return map[string]any{"id": s.ID, "type": "message", "role": "assistant", "model": s.Model, "content": content, "stop_reason": reason, "stop_sequence": nil, "usage": messagesUsage(u)}
}
func (s *chatMessagesStream) observe(raw []byte, stream bool) (string, error) {
	if _, err := s.State.observe(raw, stream); err != nil {
		return "", err
	}
	if !stream {
		return "", nil
	}
	wire := ""
	if !s.Started {
		s.Started = true
		wire = messagesEvent("message_start", map[string]any{"message": s.result([]any{}, nil, priceUsage{})})
	}
	// Chat can interleave parallel tool fragments. Emit actionable tool blocks
	// after validation and settlement, while text/thinking remain incremental.
	for i, p := range s.State.Parts {
		if i >= len(s.Sent) {
			s.Sent = append(s.Sent, 0)
		}
		if p.Kind == "function_call" || len(p.Text) == s.Sent[i] {
			continue
		}
		kind, field := "text_delta", "text"
		block := map[string]any{"type": "text", "text": ""}
		if p.Kind == "reasoning" {
			kind, field = "thinking_delta", "thinking"
			block = map[string]any{"type": "thinking", "thinking": "", "signature": ""}
		}
		if s.ActiveKind != kind {
			if s.ActiveKind != "" {
				wire += messagesEvent("content_block_stop", map[string]any{"index": s.ActiveIndex})
			}
			s.ActiveKind, s.ActiveIndex = kind, s.NextIndex
			s.NextIndex++
			wire += messagesEvent("content_block_start", map[string]any{"index": s.ActiveIndex, "content_block": block})
		}
		wire += messagesEvent("content_block_delta", map[string]any{"index": s.ActiveIndex, "delta": map[string]any{"type": kind, field: p.Text[s.Sent[i]:]}})
		s.Sent[i] = len(p.Text)
	}
	return wire, nil
}
func (s *chatMessagesStream) finish(u priceUsage) (string, []byte, error) {
	if s.State.Finish == "" {
		return "", nil, &apiError{502, "Chat stream ended without finish reason"}
	}
	content := []any{}
	wire := ""
	hasTools := false
	if s.ActiveKind != "" {
		wire += messagesEvent("content_block_stop", map[string]any{"index": s.ActiveIndex})
	}
	for _, p := range s.State.Parts {
		b := map[string]any{"type": "text", "text": p.Text}
		switch p.Kind {
		case "reasoning":
			b = map[string]any{"type": "thinking", "thinking": p.Text, "signature": ""}
		case "function_call":
			var args map[string]json.RawMessage
			raw := p.Arguments
			if raw == "" {
				raw = "{}"
			}
			if p.CallID == "" || p.Name == "" || json.Unmarshal([]byte(raw), &args) != nil || args == nil {
				return "", nil, &apiError{502, "upstream tool identity or arguments are invalid"}
			}
			b = map[string]any{"type": "tool_use", "id": p.CallID, "name": p.Name, "input": json.RawMessage(raw)}
			hasTools = true
		}
		content = append(content, b)
		if s.Started && p.Kind == "function_call" {
			index := s.NextIndex
			s.NextIndex++
			wire += messagesEvent("content_block_start", map[string]any{"index": index, "content_block": map[string]any{"type": "tool_use", "id": p.CallID, "name": p.Name, "input": map[string]any{}}})
			wire += messagesEvent("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(b["input"].(json.RawMessage))}})
			wire += messagesEvent("content_block_stop", map[string]any{"index": index})
		}
	}
	reason := "end_turn"
	if hasTools || s.State.Finish == "tool_calls" {
		reason = "tool_use"
	}
	if s.State.Finish == "length" {
		reason = "max_tokens"
	}
	if s.State.Finish == "content_filter" {
		reason = "refusal"
	}
	wire += messagesEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": messagesUsage(u)}) + messagesEvent("message_stop", map[string]any{})
	raw, err := json.Marshal(s.result(content, reason, u))
	return wire, raw, err
}
