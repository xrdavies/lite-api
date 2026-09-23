package app

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
)

func chatAnthropicPlatform(platform string) bool {
	return platform == "anthropic" || chatResponsesPlatform(platform)
}

// Reuse the existing Chat input normalization, then translate the resulting
// content and tools. The account platform and billing identity stay unchanged.
func chatToAnthropic(body map[string]json.RawMessage) ([]byte, map[string]bool, string, error) {
	copy := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		copy[k] = v
	}
	delete(copy, "stop")
	raw, err := normalizeChatInput(copy, true)
	if err != nil {
		return nil, nil, "", err
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(raw, &in)
	return normalizedResponsesToAnthropic(in, body["stop"])
}

func normalizedResponsesToAnthropic(in map[string]json.RawMessage, stop json.RawMessage) ([]byte, map[string]bool, string, error) {
	out := map[string]any{"model": in["model"], "stream": false, "max_tokens": 8192}
	if raw := in["stream"]; raw != nil && string(raw) != "null" {
		out["stream"] = raw
	}
	for _, name := range []string{"temperature", "top_p"} {
		if in[name] != nil {
			out[name] = in[name]
		}
	}
	if in["max_output_tokens"] != nil && string(in["max_output_tokens"]) != "null" {
		out["max_tokens"] = in["max_output_tokens"]
	}
	if stop != nil && string(stop) != "null" {
		var stops []string
		var single string
		if json.Unmarshal(stop, &single) == nil {
			stops = []string{single}
		} else if json.Unmarshal(stop, &stops) != nil {
			return nil, nil, "", bad("invalid stop sequences")
		}
		if len(stops) > 4 {
			return nil, nil, "", bad("too many stop sequences")
		}
		for _, s := range stops {
			if s == "" {
				return nil, nil, "", bad("empty stop sequence")
			}
		}
		out["stop_sequences"] = stops
	}
	config := map[string]any{}
	var reasoning struct{ Effort string }
	_ = json.Unmarshal(in["reasoning"], &reasoning)
	effort := reasoning.Effort
	if effort == "xhigh" {
		effort = "max"
	}
	if effort != "" {
		if effort == "none" {
			out["thinking"] = map[string]any{"type": "disabled"}
		} else {
			config["effort"] = effort
			if effort != "low" && effort != "minimal" {
				budget := int64(10240)
				if effort == "medium" {
					budget = 4096
				}
				if effort == "max" {
					budget = 32768
				}
				limit := int64(8192)
				if in["max_output_tokens"] != nil && string(in["max_output_tokens"]) != "null" {
					_ = json.Unmarshal(in["max_output_tokens"], &limit)
				}
				// Anthropic requires the thinking budget to fit inside the output limit.
				if limit <= 1024 {
					return nil, nil, "", bad("thinking requires max_tokens above 1024")
				}
				out["thinking"] = map[string]any{"type": "enabled", "budget_tokens": min(budget, limit-1)}
			}
		}
	}
	var text struct {
		Format    map[string]json.RawMessage
		Verbosity json.RawMessage
	}
	if raw := in["text"]; raw != nil && json.Unmarshal(raw, &text) != nil {
		return nil, nil, "", bad("invalid text configuration")
	}
	if text.Verbosity != nil {
		return nil, nil, "", bad("verbosity cannot be converted to Anthropic")
	}
	switch credentialString(text.Format, "type") {
	case "", "text":
	case "json_schema":
		if !json.Valid(text.Format["schema"]) || string(text.Format["schema"]) == "null" {
			return nil, nil, "", bad("JSON schema required")
		}
		config["format"] = map[string]any{"type": "json_schema", "schema": text.Format["schema"]}
	case "json_object":
		return nil, nil, "", bad("Anthropic structured output requires an explicit JSON schema")
	default:
		return nil, nil, "", bad("unsupported text format")
	}
	if len(config) > 0 {
		out["output_config"] = config
	}
	if tier := credentialString(in, "service_tier"); tier != "" && tier != "auto" {
		return nil, nil, "", bad("service_tier cannot be converted to Anthropic")
	}
	custom := map[string]bool{}
	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(in["tools"], &tools)
	declarations := []any{}
	names := map[string]bool{}
	for _, t := range tools {
		name := credentialString(t, "name")
		if names[name] {
			return nil, nil, "", bad("duplicate tool name")
		}
		names[name] = true
		schema := t["parameters"]
		if credentialString(t, "type") == "custom" {
			custom[name] = true
			schema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
		}
		if schema == nil || string(schema) == "null" {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(schema, &object) != nil || credentialString(object, "type") != "object" {
			return nil, nil, "", bad("tool parameters must be an object schema")
		}
		d := map[string]any{"name": name, "input_schema": schema}
		for _, key := range []string{"description", "strict", "cache_control"} {
			if t[key] != nil {
				d[key] = t[key]
			}
		}
		declarations = append(declarations, d)
	}
	if len(declarations) > 0 {
		out["tools"] = declarations
	}
	choice := map[string]any{"type": "auto"}
	if raw := in["tool_choice"]; raw != nil {
		var kind string
		if json.Unmarshal(raw, &kind) == nil {
			if kind == "required" {
				kind = "any"
			}
			choice["type"] = kind
		} else {
			var value map[string]json.RawMessage
			_ = json.Unmarshal(raw, &value)
			name := credentialString(value, "name")
			if !names[name] {
				return nil, nil, "", bad("tool_choice must name a declared tool")
			}
			choice = map[string]any{"type": "tool", "name": name}
		}
	}
	if raw := in["parallel_tool_calls"]; raw != nil && string(raw) != "null" {
		var parallel bool
		if json.Unmarshal(raw, &parallel) != nil {
			return nil, nil, "", bad("invalid parallel_tool_calls")
		}
		choice["disable_parallel_tool_use"] = !parallel
	}
	if len(declarations) > 0 {
		out["tool_choice"] = choice
	}
	type message struct {
		Role    string `json:"role"`
		Content []any  `json:"content"`
	}
	messages := []message{}
	system := []any{}
	if raw := in["instructions"]; raw != nil && string(raw) != "null" {
		var instructions string
		if json.Unmarshal(raw, &instructions) != nil {
			return nil, nil, "", bad("instructions must be text")
		}
		if instructions != "" {
			system = append(system, map[string]any{"type": "text", "text": instructions})
		}
	}
	appendMessage := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, blocks...)
		} else {
			messages = append(messages, message{role, blocks})
		}
	}
	var input []map[string]json.RawMessage
	_ = json.Unmarshal(in["input"], &input)
	for _, item := range input {
		kind, role := credentialString(item, "type"), credentialString(item, "role")
		switch kind {
		case "native_assistant":
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(item["content"], &blocks) != nil {
				return nil, nil, "", bad("invalid saved native content")
			}
			parts := make([]any, 0, len(blocks))
			for _, block := range blocks {
				parts = append(parts, block)
			}
			appendMessage("assistant", parts)
		case "function_call", "custom_tool_call":
			name := credentialString(item, "name")
			if names[name] && custom[name] != (kind == "custom_tool_call") {
				return nil, nil, "", bad("historical tool type conflicts with declaration")
			}
			var args map[string]json.RawMessage
			if kind == "custom_tool_call" {
				custom[name] = true
				value, _ := json.Marshal(credentialString(item, "input"))
				args = map[string]json.RawMessage{"input": value}
			} else if json.Unmarshal([]byte(credentialString(item, "arguments")), &args) != nil || args == nil {
				return nil, nil, "", bad("tool arguments must be a JSON object")
			}
			appendMessage("assistant", []any{map[string]any{"type": "tool_use", "id": credentialString(item, "call_id"), "name": name, "input": args}})
		case "function_call_output", "custom_tool_call_output":
			parts, err := anthropicInputContent(item["output"])
			if err != nil {
				return nil, nil, "", err
			}
			appendMessage("user", []any{map[string]any{"type": "tool_result", "tool_use_id": credentialString(item, "call_id"), "content": parts}})
		default:
			parts, err := anthropicInputContent(item["content"])
			if err != nil {
				return nil, nil, "", err
			}
			if role == "system" || role == "developer" {
				for _, part := range parts {
					if part.(map[string]any)["type"] != "text" {
						return nil, nil, "", bad("system content must be text")
					}
				}
				system = append(system, parts...)
			} else {
				appendMessage(role, parts)
			}
		}
	}
	if len(messages) == 0 {
		return nil, nil, "", bad("messages require non-system content")
	}
	if len(system) > 0 {
		out["system"] = system
	}
	out["messages"] = messages
	raw, err := json.Marshal(out)
	return raw, custom, effort, err
}

func anthropicInputContent(raw json.RawMessage) ([]any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return []any{}, nil
		}
		return []any{map[string]any{"type": "text", "text": text}}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, bad("invalid Anthropic conversion content")
	}
	out := []any{}
	for _, p := range parts {
		var b map[string]any
		switch credentialString(p, "type") {
		case "input_text", "output_text", "refusal":
			text := credentialString(p, "text")
			if credentialString(p, "type") == "refusal" {
				text = credentialString(p, "refusal")
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			b = map[string]any{"type": "text", "text": text}
		case "input_image":
			source, err := anthropicMediaSource(credentialString(p, "image_url"), true)
			if err != nil {
				return nil, err
			}
			b = map[string]any{"type": "image", "source": source}
		case "input_file":
			if p["file_id"] != nil {
				return nil, bad("file_id cannot be used on a different provider")
			}
			source, err := anthropicMediaSource(credentialString(p, "file_data"), false)
			if err != nil {
				return nil, err
			}
			b = map[string]any{"type": "document", "source": source}
			if title := credentialString(p, "filename"); title != "" {
				b["title"] = title
			}
		default:
			return nil, bad("unsupported content for Anthropic conversion")
		}
		if p["cache_control"] != nil {
			b["cache_control"] = p["cache_control"]
		}
		out = append(out, b)
	}
	return out, nil
}

func anthropicMediaSource(value string, image bool) (map[string]any, error) {
	if strings.HasPrefix(value, "data:") {
		header, data, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
		media, encoded := strings.CutSuffix(header, ";base64")
		allowed := media == "application/pdf" && !image || image && (media == "image/png" || media == "image/jpeg" || media == "image/gif" || media == "image/webp")
		if !ok || !encoded || !allowed || data == "" {
			return nil, bad("invalid media data URI")
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, bad("invalid base64 media")
		}
		return map[string]any{"type": "base64", "media_type": media, "data": data}, nil
	}
	parsed, err := url.Parse(value)
	if !image || err != nil || parsed.Host == "" || parsed.User != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, bad("invalid image URL or document data")
	}
	return map[string]any{"type": "url", "url": value}, nil
}

type anthropicChatBlock struct {
	Type, ID, Name, Text, Thinking, Signature, Data string
	Input                                           json.RawMessage
	TextDelta, ThinkingDelta, SignatureDelta        strings.Builder `json:"-"`
	Args                                            strings.Builder `json:"-"`
	Closed                                          bool            `json:"-"`
	ToolIndex                                       int             `json:"-"`
}

type anthropicChatStream struct {
	Chat             *responseChatStream
	Custom           map[string]bool
	Blocks           []*anthropicChatBlock
	IDs              map[string]bool
	Finish           string
	Started, Stopped bool
	Capture          bool
	Size             int
}

func newAnthropicChatStream(model string, includeUsage bool, custom map[string]bool) *anthropicChatStream {
	return &anthropicChatStream{Chat: newResponseChatStream(model, includeUsage), Custom: custom, IDs: map[string]bool{}}
}

func anthropicChatUsage(u priceUsage) map[string]any {
	input := u.Input + u.CacheRead + u.CacheWrite
	return map[string]any{"prompt_tokens": input, "completion_tokens": u.Output, "total_tokens": input + u.Output, "prompt_tokens_details": map[string]any{"cached_tokens": u.CacheRead, "cache_write_tokens": u.CacheWrite}, "cache_creation_input_tokens": u.CacheWrite}
}

func (s *anthropicChatStream) startBlock(index int, b *anthropicChatBlock) (string, error) {
	if !s.Started || s.Finish != "" || index != len(s.Blocks) || index >= 4096 || index > 0 && !s.Blocks[index-1].Closed {
		return "", &apiError{502, "invalid Anthropic content block order"}
	}
	var delta map[string]any
	switch b.Type {
	case "text":
		delta = map[string]any{"content": b.Text}
	case "thinking":
		delta = map[string]any{"reasoning_content": b.Thinking}
	case "redacted_thinking": // Opaque signatures have no Chat content equivalent.
	case "tool_use":
		if b.ID == "" || b.Name == "" || len(b.ID) > 256 || len(b.Name) > 256 || s.IDs[b.ID] {
			return "", &apiError{502, "invalid Anthropic tool identity"}
		}
		b.ToolIndex = len(s.IDs)
		s.IDs[b.ID] = true
		kind, field := "function", "arguments"
		if s.Custom[b.Name] {
			kind, field = "custom", "input"
		}
		delta = map[string]any{"tool_calls": []any{map[string]any{"index": b.ToolIndex, "id": b.ID, "type": kind, kind: map[string]any{"name": b.Name, field: ""}}}}
	default:
		return "", &apiError{502, "unsupported Anthropic content block"}
	}
	s.Size += len(b.Text) + len(b.Thinking) + len(b.Input) + len(b.Signature) + len(b.Data)
	if s.Size > 16<<20 {
		return "", &apiError{502, "converted stream exceeds limit"}
	}
	s.Blocks = append(s.Blocks, b)
	if delta == nil {
		return "", nil
	}
	return s.Chat.chunk(delta, nil, nil), nil
}

func (s *anthropicChatStream) stopBlock(index int) (string, error) {
	if index < 0 || index >= len(s.Blocks) || s.Blocks[index].Closed {
		return "", &apiError{502, "invalid Anthropic content block stop"}
	}
	b := s.Blocks[index]
	b.Closed = true
	if b.Type != "tool_use" {
		return "", nil
	}
	args := b.Args.String()
	if args == "" {
		args = string(b.Input)
	}
	if args == "" {
		args = "{}"
	}
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &object) != nil || object == nil {
		return "", &apiError{502, "invalid Anthropic tool input"}
	}
	b.Input = json.RawMessage(args)
	field, kind, value := "arguments", "function", args
	if s.Custom[b.Name] {
		field, kind = "input", "custom"
		if json.Unmarshal(object["input"], &value) != nil {
			return "", &apiError{502, "custom tool input must be a string"}
		}
	} else if b.Args.Len() > 0 {
		return "", nil
	} // Argument deltas were already emitted.
	return s.Chat.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": b.ToolIndex, kind: map[string]any{field: value}}}}, nil, nil), nil
}

func (s *anthropicChatStream) setFinish(reason string) error {
	if reason == "" {
		return nil
	}
	for _, b := range s.Blocks {
		if !b.Closed {
			return &apiError{502, "Anthropic message ended with an open content block"}
		}
	}
	finish := ""
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		finish = "stop"
	case "tool_use":
		finish = "tool_calls"
	case "max_tokens":
		finish = "length"
	case "refusal":
		finish = "content_filter"
	default:
		return &apiError{502, "unsupported Anthropic stop reason"}
	}
	if s.Finish != "" && s.Finish != finish {
		return &apiError{502, "Anthropic stop reason changed"}
	}
	s.Finish = finish
	return nil
}

func (s *anthropicChatStream) event(raw []byte, usage priceUsage) (string, error) {
	var e struct {
		Type    string
		Index   int
		Message struct {
			ID, Type string
			Content  []json.RawMessage
		}
		Block anthropicChatBlock `json:"content_block"`
		Delta struct {
			Type, Text, Thinking, Signature string
			Partial                         string `json:"partial_json"`
			Stop                            string `json:"stop_reason"`
		}
	}
	if json.Unmarshal(raw, &e) != nil || s.Stopped {
		return "", &apiError{502, "invalid Anthropic stream event"}
	}
	if e.Type == "ping" {
		return "", nil
	}
	if e.Type == "message_start" {
		if s.Started || e.Message.ID == "" || len(e.Message.ID) > 256 || len(e.Message.Content) > 0 {
			return "", &apiError{502, "invalid Anthropic message start"}
		}
		s.Started = true
		s.Chat.ID = e.Message.ID
		return s.Chat.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil), nil
	}
	if !s.Started {
		return "", &apiError{502, "Anthropic event has no message start"}
	}
	switch e.Type {
	case "content_block_start":
		return s.startBlock(e.Index, &e.Block)
	case "content_block_stop":
		return s.stopBlock(e.Index)
	case "content_block_delta":
		if e.Index < 0 || e.Index >= len(s.Blocks) || s.Blocks[e.Index].Closed || s.Finish != "" {
			return "", &apiError{502, "invalid Anthropic content delta"}
		}
		b := s.Blocks[e.Index]
		s.Size += len(e.Delta.Text) + len(e.Delta.Thinking) + len(e.Delta.Partial) + len(e.Delta.Signature)
		if s.Size > 16<<20 {
			return "", &apiError{502, "converted stream exceeds limit"}
		}
		switch e.Delta.Type {
		case "text_delta":
			if b.Type != "text" {
				break
			}
			if s.Capture {
				b.TextDelta.WriteString(e.Delta.Text)
			}
			return s.Chat.chunk(map[string]any{"content": e.Delta.Text}, nil, nil), nil
		case "thinking_delta":
			if b.Type != "thinking" {
				break
			}
			if s.Capture {
				b.ThinkingDelta.WriteString(e.Delta.Thinking)
			}
			return s.Chat.chunk(map[string]any{"reasoning_content": e.Delta.Thinking}, nil, nil), nil
		case "signature_delta":
			if b.Type == "thinking" {
				if s.Capture {
					b.SignatureDelta.WriteString(e.Delta.Signature)
				}
				return "", nil
			}
		case "input_json_delta":
			if b.Type != "tool_use" {
				break
			}
			if b.Args.Len() == 0 && len(b.Input) > 0 && string(b.Input) != "{}" {
				return "", &apiError{502, "Anthropic tool input changed"}
			}
			b.Args.WriteString(e.Delta.Partial)
			if s.Custom[b.Name] {
				return "", nil
			}
			return s.Chat.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": b.ToolIndex, "function": map[string]any{"arguments": e.Delta.Partial}}}}, nil, nil), nil
		default:
			return "", nil // Forward-compatible metadata deltas do not invent content.
		}
		return "", &apiError{502, "Anthropic delta does not match content block"}
	case "message_delta":
		return "", s.setFinish(e.Delta.Stop)
	case "message_stop":
		if s.Finish == "" {
			return "", &apiError{502, "Anthropic stream has no stop reason"}
		}
		s.Stopped = true
		wire := s.Chat.chunk(map[string]any{}, s.Finish, nil)
		if s.Chat.IncludeUsage {
			wire += s.Chat.chunk(nil, nil, anthropicChatUsage(usage))
		}
		return wire + "data: [DONE]\n\n", nil
	case "error":
		return "", &apiError{502, "upstream returned an error"}
	}
	return "", nil
}

func (s *anthropicChatStream) response(raw []byte, usage priceUsage) ([]byte, error) {
	var in struct {
		ID, Type string
		Stop     string `json:"stop_reason"`
		Content  []*anthropicChatBlock
	}
	if json.Unmarshal(raw, &in) != nil || in.Type != "message" || in.ID == "" || len(in.ID) > 256 || in.Content == nil {
		return nil, &apiError{502, "invalid Anthropic message"}
	}
	s.Started = true
	s.Chat.ID = in.ID
	var text, thought strings.Builder
	tools := []any{}
	for i, b := range in.Content {
		if b == nil {
			return nil, &apiError{502, "invalid Anthropic content block"}
		}
		if _, err := s.startBlock(i, b); err != nil {
			return nil, err
		}
		if _, err := s.stopBlock(i); err != nil {
			return nil, err
		}
		text.WriteString(b.Text)
		thought.WriteString(b.Thinking)
		if b.Type == "tool_use" {
			kind, field, value := "function", "arguments", string(b.Input)
			if s.Custom[b.Name] {
				kind, field = "custom", "input"
				var v struct{ Input string }
				_ = json.Unmarshal(b.Input, &v)
				value = v.Input
			}
			tools = append(tools, map[string]any{"id": b.ID, "type": kind, kind: map[string]any{"name": b.Name, field: value}})
		}
	}
	if err := s.setFinish(in.Stop); err != nil {
		return nil, err
	}
	if s.Finish == "" {
		return nil, &apiError{502, "Anthropic message has no stop reason"}
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if thought.Len() > 0 {
		message["reasoning_content"] = thought.String()
	}
	if len(tools) > 0 {
		message["tool_calls"] = tools
	}
	return json.Marshal(map[string]any{"id": in.ID, "object": "chat.completion", "created": s.Chat.Created, "model": s.Chat.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": s.Finish}}, "usage": anthropicChatUsage(usage)})
}
