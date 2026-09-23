package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Keep the upstream protocol explicit: conversion does not change the account's
// platform, model routing, price snapshot, or the client's idempotency scope.
func chatResponsesPlatform(platform string) bool {
	return platform == "openai" || platform == "kimi" || platform == "zhipu" || platform == "deepseek" || platform == "minimax"
}

func chatToResponses(body map[string]json.RawMessage) ([]byte, error) {
	return normalizeChatInput(body, false)
}

func normalizeChatInput(body map[string]json.RawMessage, keepCache bool) ([]byte, error) {
	var messages []struct {
		Role, Name string
		Content    json.RawMessage
		ToolCallID string `json:"tool_call_id"`
		Reasoning  string `json:"reasoning_content"`
		Refusal    string
		ToolCalls  []struct {
			ID, Type string
			Function struct{ Name, Arguments string }
			Custom   struct{ Name, Input string }
		} `json:"tool_calls"`
		FunctionCall *struct{ Name, Arguments string } `json:"function_call"`
	}
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return nil, bad("invalid Chat messages for Responses conversion")
	}
	out := map[string]any{"model": body["model"], "store": false, "stream": false}
	if raw := body["stream"]; raw != nil {
		out["stream"] = raw
	}
	for _, field := range []string{"temperature", "top_p", "service_tier", "parallel_tool_calls", "metadata", "user", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "instructions"} {
		if raw := body[field]; raw != nil {
			out[field] = raw
		}
	}
	// These controls have no equivalent on the Responses endpoint.
	for _, field := range []string{"stop", "logit_bias", "audio", "web_search_options", "seed", "top_logprobs"} {
		if raw := body[field]; raw != nil && string(raw) != "null" {
			return nil, bad(field + " cannot be converted to Responses")
		}
	}
	for _, field := range []string{"n", "frequency_penalty", "presence_penalty", "logprobs"} {
		if raw := body[field]; raw != nil && string(raw) != "null" {
			want := "0"
			if field == "n" {
				want = "1"
			}
			if field == "logprobs" {
				want = "false"
			}
			if string(raw) != want {
				return nil, bad(field + " cannot be converted to Responses")
			}
		}
	}
	if raw := body["modalities"]; raw != nil && string(raw) != "null" {
		var modes []string
		if json.Unmarshal(raw, &modes) != nil || len(modes) != 1 || modes[0] != "text" {
			return nil, bad("Responses conversion requires text output")
		}
	}
	if raw := body["max_completion_tokens"]; raw != nil && string(raw) != "null" {
		out["max_output_tokens"] = raw
	} else if raw := body["max_tokens"]; raw != nil && string(raw) != "null" {
		out["max_output_tokens"] = raw
	}
	if raw, ok := out["max_output_tokens"].(json.RawMessage); ok {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n <= 0 || n > 2147483647 {
			return nil, bad("invalid output token limit")
		}
	}
	if effort := credentialString(body, "reasoning_effort"); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	input := []any{}
	customCalls := map[string]bool{}
	for _, msg := range messages {
		switch msg.Role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return nil, bad("invalid Chat message role")
		}
		content, err := chatResponsesContent(msg.Content, msg.Role, keepCache)
		if err != nil {
			return nil, err
		}
		if msg.Role == "tool" || msg.Role == "function" {
			callID := msg.ToolCallID
			if msg.Role == "function" {
				callID = msg.Name
			}
			if callID == "" {
				return nil, bad("tool result requires a call ID")
			}
			kind := "function_call_output"
			if customCalls[callID] {
				kind = "custom_tool_call_output"
			}
			input = append(input, map[string]any{"type": kind, "call_id": callID, "output": content})
			continue
		}
		if msg.Role == "assistant" {
			parts := content.([]any)
			if msg.Reasoning != "" {
				parts = append([]any{map[string]any{"type": "output_text", "text": "<thinking>" + msg.Reasoning + "</thinking>"}}, parts...)
			}
			if msg.Refusal != "" {
				parts = append(parts, map[string]any{"type": "refusal", "refusal": msg.Refusal})
			}
			if len(parts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": msg.Role, "content": parts})
			}
			for _, tool := range msg.ToolCalls {
				if tool.ID == "" {
					return nil, bad("tool call ID is required")
				}
				switch tool.Type {
				case "function":
					if tool.Function.Name == "" {
						return nil, bad("function name is required")
					}
					args := tool.Function.Arguments
					if args == "" {
						args = "{}"
					}
					input = append(input, map[string]any{"type": "function_call", "call_id": tool.ID, "name": tool.Function.Name, "arguments": args})
				case "custom":
					if tool.Custom.Name == "" {
						return nil, bad("custom tool name is required")
					}
					customCalls[tool.ID] = true
					input = append(input, map[string]any{"type": "custom_tool_call", "call_id": tool.ID, "name": tool.Custom.Name, "input": tool.Custom.Input})
				default:
					return nil, bad("unsupported tool call")
				}
			}
			if msg.FunctionCall != nil {
				if msg.FunctionCall.Name == "" {
					return nil, bad("function name is required")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": msg.FunctionCall.Name, "name": msg.FunctionCall.Name, "arguments": msg.FunctionCall.Arguments})
			}
		} else {
			input = append(input, map[string]any{"role": msg.Role, "content": content})
		}
	}
	out["input"] = input
	var tools []map[string]json.RawMessage
	if raw := body["tools"]; raw != nil && json.Unmarshal(raw, &tools) != nil {
		return nil, bad("invalid tools")
	}
	var functions []map[string]json.RawMessage
	if raw := body["functions"]; raw != nil && json.Unmarshal(raw, &functions) != nil {
		return nil, bad("invalid functions")
	}
	for _, f := range functions {
		raw, _ := json.Marshal(f)
		tools = append(tools, map[string]json.RawMessage{"type": json.RawMessage(`"function"`), "function": raw})
	}
	convertedTools := []any{}
	for _, tool := range tools {
		kind := credentialString(tool, "type")
		if kind != "function" && kind != "custom" {
			return nil, bad("hosted tool billing is not yet available for conversion")
		}
		var definition map[string]json.RawMessage
		if json.Unmarshal(tool[kind], &definition) != nil || definition == nil || credentialString(definition, "name") == "" {
			return nil, bad("invalid tool definition")
		}
		definition["type"], _ = json.Marshal(kind)
		if kind == "function" && (definition["strict"] == nil || string(definition["strict"]) == "null") {
			definition["strict"] = json.RawMessage("false")
		}
		convertedTools = append(convertedTools, definition)
	}
	if len(convertedTools) > 0 {
		out["tools"] = convertedTools
	}
	choice := body["tool_choice"]
	legacy := false
	if choice == nil {
		choice, legacy = body["function_call"], true
	}
	if choice != nil && string(choice) != "null" {
		var value string
		if json.Unmarshal(choice, &value) == nil {
			if value != "auto" && value != "none" && value != "required" {
				return nil, bad("invalid tool_choice")
			}
			out["tool_choice"] = value
		} else {
			var v map[string]json.RawMessage
			if json.Unmarshal(choice, &v) != nil || v == nil {
				return nil, bad("invalid tool_choice")
			}
			kind, name := credentialString(v, "type"), credentialString(v, "name")
			if legacy {
				kind = "function"
			} else if kind == "function" || kind == "custom" {
				var nested map[string]json.RawMessage
				if json.Unmarshal(v[kind], &nested) == nil {
					name = credentialString(nested, "name")
				}
			}
			if (kind != "function" && kind != "custom") || name == "" {
				return nil, bad("invalid tool_choice")
			}
			out["tool_choice"] = map[string]string{"type": kind, "name": name}
		}
	}
	text := map[string]any{}
	if raw := body["verbosity"]; raw != nil {
		text["verbosity"] = raw
	}
	if raw := body["response_format"]; raw != nil && string(raw) != "null" {
		var format map[string]json.RawMessage
		if json.Unmarshal(raw, &format) != nil || format == nil {
			return nil, bad("invalid response_format")
		}
		kind := credentialString(format, "type")
		if kind == "json_schema" {
			var schema map[string]json.RawMessage
			if json.Unmarshal(format["json_schema"], &schema) != nil || schema == nil {
				return nil, bad("invalid response_format schema")
			}
			schema["type"] = json.RawMessage(`"json_schema"`)
			text["format"] = schema
		} else if kind == "text" || kind == "json_object" {
			text["format"] = format
		} else {
			return nil, bad("invalid response_format type")
		}
	}
	if len(text) > 0 {
		out["text"] = text
	}
	return json.Marshal(out)
}

func chatResponsesContent(raw json.RawMessage, role string, keepCache bool) (any, error) {
	var text string
	if len(raw) == 0 || string(raw) == "null" {
		text = ""
	} else if json.Unmarshal(raw, &text) != nil {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(raw, &parts) != nil {
			return nil, bad("invalid message content")
		}
		out := []any{}
		for _, part := range parts {
			switch credentialString(part, "type") {
			case "text":
				var text string
				if json.Unmarshal(part["text"], &text) != nil {
					return nil, bad("invalid text content")
				}
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				v := map[string]any{"type": kind, "text": text}
				if keepCache && part["cache_control"] != nil {
					v["cache_control"] = part["cache_control"]
				}
				out = append(out, v)
			case "image_url":
				if role != "user" {
					return nil, bad("image content requires a user message")
				}
				var image struct{ URL, Detail string }
				if json.Unmarshal(part["image_url"], &image) != nil || image.URL == "" {
					return nil, bad("invalid image content")
				}
				v := map[string]any{"type": "input_image", "image_url": image.URL}
				if keepCache && part["cache_control"] != nil {
					v["cache_control"] = part["cache_control"]
				}
				if image.Detail != "" {
					v["detail"] = image.Detail
				}
				out = append(out, v)
			case "file":
				var file map[string]json.RawMessage
				if role != "user" || json.Unmarshal(part["file"], &file) != nil || file == nil || (file["file_id"] == nil && file["file_data"] == nil) {
					return nil, bad("invalid file content")
				}
				file["type"] = json.RawMessage(`"input_file"`)
				if keepCache && part["cache_control"] != nil {
					file["cache_control"] = part["cache_control"]
				}
				out = append(out, file)
			default:
				return nil, bad("unsupported message content for Responses conversion")
			}
		}
		return out, nil
	}
	if role == "assistant" {
		if text == "" {
			return []any{}, nil
		}
		return []any{map[string]any{"type": "output_text", "text": text}}, nil
	}
	return text, nil
}

type responseChatItem struct {
	ID, Type, Name, Arguments, Input string
	CallID                           string `json:"call_id"`
	Content                          []struct{ Type, Text, Refusal string }
	Summary                          []struct{ Type, Text string }
}

type responseChatBody struct {
	ID, Model, Status string
	Created           int64  `json:"created_at"`
	Tier              string `json:"service_tier"`
	Output            []responseChatItem
	Usage             map[string]json.RawMessage
	Incomplete        struct{ Reason string } `json:"incomplete_details"`
}

func responseChatUsage(usage map[string]json.RawMessage) map[string]json.RawMessage {
	if usage == nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for key, value := range usage {
		switch key {
		case "input_tokens":
			key = "prompt_tokens"
		case "output_tokens":
			key = "completion_tokens"
		case "input_tokens_details":
			key = "prompt_tokens_details"
		case "output_tokens_details":
			key = "completion_tokens_details"
		}
		out[key] = value
	}
	return out
}

func responseChatFinish(response responseChatBody, tools bool) string {
	if response.Status == "incomplete" {
		if response.Incomplete.Reason == "max_output_tokens" {
			return "length"
		}
		if response.Incomplete.Reason == "content_filter" {
			return "content_filter"
		}
		return "stop"
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

func responsesToChat(raw []byte, model string) ([]byte, error) {
	var response responseChatBody
	if json.Unmarshal(raw, &response) != nil || response.Output == nil {
		return nil, &apiError{502, "Responses output is missing"}
	}
	text, reasoning, refusal := "", "", ""
	tools := []any{}
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text += part.Text
				}
				if part.Type == "refusal" {
					refusal += part.Refusal
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				reasoning += part.Text
			}
		case "function_call", "custom_tool_call":
			if item.CallID == "" || item.Name == "" {
				return nil, &apiError{502, "invalid upstream tool call"}
			}
			kind, value := "function", map[string]any{"name": item.Name, "arguments": item.Arguments}
			if item.Type == "custom_tool_call" {
				kind, value = "custom", map[string]any{"name": item.Name, "input": item.Input}
			}
			tools = append(tools, map[string]any{"id": item.CallID, "type": kind, kind: value})
		default:
			return nil, &apiError{502, "unsupported Responses output for Chat conversion"}
		}
	}
	message := map[string]any{"role": "assistant", "content": nil}
	if text != "" {
		message["content"] = text
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if refusal != "" {
		message["refusal"] = refusal
	}
	if len(tools) > 0 {
		message["tool_calls"] = tools
	}
	result := map[string]any{"id": response.ID, "object": "chat.completion", "created": response.Created, "model": model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": responseChatFinish(response, len(tools) > 0)}}, "usage": responseChatUsage(response.Usage)}
	if response.Tier != "" {
		result["service_tier"] = response.Tier
	}
	return json.Marshal(result)
}

type responseChatStream struct {
	ID, Model, Tier    string
	Created            int64
	Role, IncludeUsage bool
	Text               map[string]string
	Tools              map[int]responseChatItem
	ToolIndex          map[int]int
	Size               int
}

// ponytail: string accumulation is bounded to 16 MiB and 4096 parts; use per-part
// buffers if profiling shows large streams spend significant time copying.
func newResponseChatStream(model string, includeUsage bool) *responseChatStream {
	return &responseChatStream{Model: model, IncludeUsage: includeUsage, Created: time.Now().Unix(), Text: map[string]string{}, Tools: map[int]responseChatItem{}, ToolIndex: map[int]int{}}
}

func (s *responseChatStream) chunk(delta map[string]any, finish any, usage any) string {
	choices := []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
	if usage != nil {
		choices = []any{}
	}
	value := map[string]any{"id": s.ID, "object": "chat.completion.chunk", "model": s.Model, "created": s.Created, "choices": choices}
	if s.Tier != "" {
		value["service_tier"] = s.Tier
	}
	if usage != nil {
		value["usage"] = usage
	}
	raw, _ := json.Marshal(value)
	return "data: " + string(raw) + "\n\n"
}

func (s *responseChatStream) event(raw []byte) (string, error) {
	var e struct {
		Type, Delta, Arguments, Input, Text, Refusal string
		OutputIndex                                  int `json:"output_index"`
		ContentIndex                                 int `json:"content_index"`
		SummaryIndex                                 int `json:"summary_index"`
		Item                                         responseChatItem
		Response                                     *responseChatBody
	}
	if json.Unmarshal(raw, &e) != nil {
		return "", &apiError{502, "invalid Responses event"}
	}
	if e.OutputIndex < 0 || e.ContentIndex < 0 || e.SummaryIndex < 0 || e.OutputIndex > 4096 || e.ContentIndex > 4096 || e.SummaryIndex > 4096 {
		return "", &apiError{502, "Responses event index exceeds limit"}
	}
	if e.Response != nil {
		s.ID, s.Tier = e.Response.ID, e.Response.Tier
		if e.Response.Created != 0 {
			s.Created = e.Response.Created
		}
	}
	if s.ID == "" {
		return "", &apiError{502, "Responses stream missing creation event"}
	}
	var out strings.Builder
	if !s.Role {
		out.WriteString(s.chunk(map[string]any{"role": "assistant"}, nil, nil))
		s.Role = true
	}
	text := func(key, field, value string, full bool) error {
		if _, exists := s.Text[key]; !exists && len(s.Text) >= 4096 {
			return &apiError{502, "Responses stream has too many parts"}
		}
		previous := s.Text[key]
		if full {
			if !strings.HasPrefix(value, previous) {
				return &apiError{502, "Responses stream content changed"}
			}
			value = strings.TrimPrefix(value, previous)
		}
		s.Size += len(value)
		if s.Size > 16<<20 {
			return &apiError{502, "converted stream exceeds limit"}
		}
		s.Text[key] = previous + value
		if value != "" {
			out.WriteString(s.chunk(map[string]any{field: value}, nil, nil))
		}
		return nil
	}
	tool := func(index int, item responseChatItem) error {
		if item.CallID == "" || item.Name == "" || len(item.CallID) > 256 || len(item.Name) > 256 {
			return &apiError{502, "invalid upstream tool call"}
		}
		old, exists := s.Tools[index]
		if exists && (old.CallID != item.CallID || old.Name != item.Name || old.Type != item.Type) {
			return &apiError{502, "Responses tool identity changed"}
		}
		kind, field, value := "function", "arguments", item.Arguments
		if item.Type == "custom_tool_call" {
			kind, field, value = "custom", "input", item.Input
		}
		if !exists {
			if len(s.Tools) >= 4096 {
				return &apiError{502, "Responses stream has too many tools"}
			}
			s.ToolIndex[index] = len(s.Tools)
			s.Tools[index] = item
			out.WriteString(s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": s.ToolIndex[index], "id": item.CallID, "type": kind, kind: map[string]string{"name": item.Name, field: ""}}}}, nil, nil))
		}
		key := fmt.Sprintf("tool:%d", index)
		previous := s.Text[key]
		if !strings.HasPrefix(value, previous) {
			return &apiError{502, "Responses tool arguments changed"}
		}
		suffix := strings.TrimPrefix(value, previous)
		s.Size += len(suffix)
		if s.Size > 16<<20 {
			return &apiError{502, "converted stream exceeds limit"}
		}
		s.Text[key] = value
		if suffix != "" {
			out.WriteString(s.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": s.ToolIndex[index], kind: map[string]string{field: suffix}}}}, nil, nil))
		}
		return nil
	}
	item := func(index int, value responseChatItem) error {
		switch value.Type {
		case "function_call", "custom_tool_call":
			return tool(index, value)
		case "message":
			for i, part := range value.Content {
				field, content := "content", part.Text
				if part.Type == "refusal" {
					field, content = "refusal", part.Refusal
				}
				if err := text(fmt.Sprintf("%s:%d:%d", field, index, i), field, content, true); err != nil {
					return err
				}
			}
		case "reasoning":
			for i, part := range value.Summary {
				if err := text(fmt.Sprintf("reasoning_content:%d:%d", index, i), "reasoning_content", part.Text, true); err != nil {
					return err
				}
			}
		default:
			return &apiError{502, "unsupported Responses stream output"}
		}
		return nil
	}
	var err error
	switch e.Type {
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done":
		field, index, value := "content", e.ContentIndex, e.Delta
		full := strings.HasSuffix(e.Type, ".done")
		if full {
			value = e.Text
		}
		if strings.HasPrefix(e.Type, "response.refusal.") {
			field = "refusal"
			if full {
				value = e.Refusal
			}
		}
		if strings.HasPrefix(e.Type, "response.reasoning_summary_text.") {
			field, index = "reasoning_content", e.SummaryIndex
		}
		key := fmt.Sprintf("%s:%d:%d", field, e.OutputIndex, index)
		if strings.HasPrefix(e.Type, "response.reasoning_text.") {
			field, key = "reasoning_content", fmt.Sprintf("raw_reasoning:%d:%d", e.OutputIndex, index)
		}
		err = text(key, field, value, full)
	case "response.output_item.added", "response.output_item.done":
		err = item(e.OutputIndex, e.Item)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		v, exists := s.Tools[e.OutputIndex]
		if !exists {
			return "", &apiError{502, "Responses tool delta has no call"}
		}
		value := e.Arguments
		if v.Type == "custom_tool_call" {
			value = e.Input
		}
		if strings.HasSuffix(e.Type, ".delta") {
			value = s.Text[fmt.Sprintf("tool:%d", e.OutputIndex)] + e.Delta
		}
		v.Arguments, v.Input = value, value
		err = tool(e.OutputIndex, v)
	case "response.completed", "response.incomplete":
		if e.Response == nil || e.Response.Output == nil {
			return "", &apiError{502, "Responses terminal output is missing"}
		}
		for i, value := range e.Response.Output {
			if err = item(i, value); err != nil {
				return "", err
			}
		}
		out.WriteString(s.chunk(map[string]any{}, responseChatFinish(*e.Response, len(s.Tools) > 0), nil))
		if s.IncludeUsage && e.Response.Usage != nil {
			out.WriteString(s.chunk(nil, nil, responseChatUsage(e.Response.Usage)))
		}
		out.WriteString("data: [DONE]\n\n")
	}
	return out.String(), err
}
