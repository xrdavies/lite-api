package app

import "encoding/json"

// Input and stored native content take a direct path. Shared tool preparation
// handles names, discoveries and collisions without degrading messages to Chat.
func responsesToAnthropicRequest(body map[string]json.RawMessage, history []convertedChatMessage) (*responsesChatRequest, string, error) {
	out := &responsesChatRequest{Body: map[string]any{}, Custom: map[string]bool{}, Namespaces: map[string]responseToolName{}}
	for _, name := range []string{"prompt", "context_management"} {
		if raw := body[name]; raw != nil && string(raw) != "null" {
			return nil, "", bad(name + " requires a native Responses account")
		}
	}
	if raw := body["truncation"]; raw != nil && string(raw) != "null" && string(raw) != `"disabled"` {
		return nil, "", bad("automatic truncation requires a native Responses account")
	}
	if raw := body["max_output_tokens"]; raw != nil && string(raw) != "null" {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 2147483647 {
			return nil, "", bad("invalid max_output_tokens")
		}
	}
	if _, err := requestEffort(body, "responses"); err != nil {
		return nil, "", err
	}
	if err := out.prepareTools(body, history, true); err != nil {
		return nil, "", err
	}
	in := map[string]json.RawMessage{}
	for k, v := range body {
		in[k] = v
	}
	var tools []map[string]json.RawMessage
	if raw, err := json.Marshal(out.Body["tools"]); err == nil {
		_ = json.Unmarshal(raw, &tools)
	}
	definitions := []map[string]json.RawMessage{}
	for _, tool := range tools {
		var definition map[string]json.RawMessage
		_ = json.Unmarshal(tool["function"], &definition)
		definition["type"] = json.RawMessage(`"function"`)
		definitions = append(definitions, definition)
	}
	in["tools"], _ = json.Marshal(definitions)
	if choice := out.Body["tool_choice"]; choice != nil {
		raw, _ := json.Marshal(choice)
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			var fn map[string]json.RawMessage
			_ = json.Unmarshal(object["function"], &fn)
			raw, _ = json.Marshal(map[string]any{"type": "function", "name": credentialString(fn, "name")})
		}
		in["tool_choice"] = raw
	}
	items := []map[string]json.RawMessage{}
	for _, m := range history {
		if m.ResponseInput != nil {
			var item map[string]json.RawMessage
			if json.Unmarshal(m.ResponseInput, &item) != nil || item == nil {
				return nil, "", bad("invalid saved Anthropic input")
			}
			items = append(items, item)
		} else if m.Anthropic != nil {
			raw, _ := json.Marshal(m.Anthropic)
			items = append(items, map[string]json.RawMessage{"type": json.RawMessage(`"native_assistant"`), "content": raw})
		} else {
			return nil, "", bad("response history has no native Anthropic content")
		}
	}
	var input []map[string]json.RawMessage
	var text string
	raw := body["input"]
	if raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &text) == nil {
			input = []map[string]json.RawMessage{{"role": json.RawMessage(`"user"`), "content": raw}}
		} else if json.Unmarshal(raw, &input) != nil {
			return nil, "", bad("invalid Responses input")
		}
	}
	out.History = append(out.History, history...)
	for _, item := range input {
		if item == nil {
			return nil, "", bad("invalid input item")
		}
		kind := credentialString(item, "type")
		switch kind {
		case "additional_tools":
			continue
		case "reasoning":
			continue // Foreign encrypted reasoning is not an Anthropic signature.
		case "", "message":
			role := credentialString(item, "role")
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return nil, "", bad("invalid input role")
			}
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output", "tool_search_call", "tool_search_output":
		default:
			return nil, "", bad("input item requires a native Responses account")
		}
		saved, _ := json.Marshal(item)
		m := convertedChatMessage{Role: credentialString(item, "role"), ResponseInput: saved}
		if kind == "tool_search_output" {
			var err error
			m.Discoveries, err = searchOutputTools(item)
			if err != nil {
				return nil, "", err
			}
			m.CallID = credentialString(item, "call_id")
		}
		out.History = append(out.History, m)
		items = append(items, item)
	}
	// Reserve every historical namespace before validating direct tool identities.
	for _, item := range items {
		if credentialString(item, "type") == "native_assistant" {
			var blocks []map[string]json.RawMessage
			_ = json.Unmarshal(item["content"], &blocks)
			for _, b := range blocks {
				if ns := credentialString(b, "namespace"); ns != "" {
					if _, err := out.registerNamespace(ns, credentialString(b, "name")); err != nil {
						return nil, "", err
					}
				}
			}
		} else if ns := credentialString(item, "namespace"); ns != "" {
			if credentialString(item, "type") != "function_call" {
				return nil, "", bad("namespace requires a function call")
			}
			if _, err := out.registerNamespace(ns, credentialString(item, "name")); err != nil {
				return nil, "", err
			}
		}
	}
	nameFor := func(name, ns string, search bool) (string, error) {
		if name == "" || len(name) > 256 {
			return "", bad("invalid tool name")
		}
		if ns != "" {
			return out.registerNamespace(ns, name)
		}
		if _, ok := out.Namespaces[name]; ok {
			return "", bad("history tool identity collides with a namespace")
		}
		if search && out.Direct["tool_search"] || !search && name == "tool_search" && out.ToolSearch {
			return "", bad("tool search conflicts with history tool identity")
		}
		return name, nil
	}
	for _, item := range items {
		kind := credentialString(item, "type")
		if kind == "native_assistant" {
			var blocks []map[string]json.RawMessage
			_ = json.Unmarshal(item["content"], &blocks)
			for _, b := range blocks {
				if credentialString(b, "type") != "tool_use" {
					continue
				}
				name := credentialString(b, "name")
				flat, err := nameFor(name, credentialString(b, "namespace"), string(b["tool_search"]) == "true")
				if err != nil {
					return nil, "", err
				}
				if string(b["custom"]) == "true" && !out.Declared[flat] {
					out.Custom[flat] = true
				}
				b["name"], _ = json.Marshal(flat)
				delete(b, "namespace")
				delete(b, "tool_search")
				delete(b, "custom")
			}
			item["content"], _ = json.Marshal(blocks)
			continue
		}
		if kind == "function_call" || kind == "custom_tool_call" || kind == "tool_search_call" {
			callID := credentialString(item, "call_id")
			if callID == "" || len(callID) > 256 {
				return nil, "", bad("tool call ID required")
			}
			name := credentialString(item, "name")
			if kind == "tool_search_call" {
				name = "tool_search"
				if err := clientSearchItem(item); err != nil {
					return nil, "", err
				}
			}
			flat, err := nameFor(name, credentialString(item, "namespace"), kind == "tool_search_call")
			if err != nil {
				return nil, "", err
			}
			item["name"], _ = json.Marshal(flat)
			delete(item, "namespace")
			if kind == "custom_tool_call" {
				var value string
				if json.Unmarshal(item["input"], &value) != nil {
					return nil, "", bad("custom tool input must be a string")
				}
				args, _ := json.Marshal(map[string]string{"input": value})
				item["arguments"], _ = json.Marshal(string(args))
				if !out.Declared[flat] {
					out.Custom[flat] = true
				}
			} else if kind == "tool_search_call" {
				args, err := searchCallArguments(item["arguments"])
				if err != nil {
					return nil, "", err
				}
				item["arguments"], _ = json.Marshal(string(args))
			}
			if kind == "function_call" && credentialString(item, "arguments") == "" {
				item["arguments"] = json.RawMessage(`"{}"`)
			}
			item["type"] = json.RawMessage(`"function_call"`)
		}
		if kind == "function_call_output" || kind == "custom_tool_call_output" || kind == "tool_search_output" {
			if id := credentialString(item, "call_id"); id == "" || len(id) > 256 {
				return nil, "", bad("tool output requires a call ID")
			}
			if kind == "tool_search_output" {
				raw := item["output"]
				if raw == nil || string(raw) == "null" {
					raw = item["tools"]
				}
				if raw == nil {
					return nil, "", bad("tool search output requires output or tools")
				}
				var value string
				if json.Unmarshal(raw, &value) != nil {
					value = string(raw)
				}
				item["output"], _ = json.Marshal(value)
			}
			item["type"] = json.RawMessage(`"function_call_output"`)
		}
	}
	in["input"], _ = json.Marshal(items)
	wire, _, effort, err := normalizedResponsesToAnthropic(in, nil)
	if err != nil {
		return nil, "", err
	}
	// Preserve decimal values and native blocks verbatim when assembling the body.
	var native map[string]json.RawMessage
	_ = json.Unmarshal(wire, &native)
	out.Body = map[string]any{}
	for k, v := range native {
		out.Body[k] = v
	}
	return out, effort, nil
}

type anthropicResponsesStream struct {
	Native      *anthropicChatStream
	Output      *chatResponsesStream
	BlockOutput map[int]int
}

func newAnthropicResponsesStream(model string, request *responsesChatRequest) *anthropicResponsesStream {
	output := newChatResponsesStream(model, request.Custom)
	output.Namespaces = request.Namespaces
	output.ToolSearch = request.ToolSearch
	native := newAnthropicChatStream(model, false, nil)
	native.Capture = true
	return &anthropicResponsesStream{Native: native, Output: output, BlockOutput: map[int]int{}}
}

func (s *anthropicResponsesStream) beginBlock(index int, block *anthropicChatBlock) (string, error) {
	out := s.Output
	// Each native text/thinking block retains its own position, including interleaving.
	switch block.Type {
	case "text":
		out.TextIndex = -1
		wire := out.appendText("message", block.Text)
		if out.TextIndex < 0 {
			out.TextIndex = out.add("message")
			item, _ := out.item(out.Parts[out.TextIndex], false)
			wire += out.emit("response.output_item.added", map[string]any{"output_index": out.TextIndex, "item": item})
			wire += out.emit("response.content_part.added", map[string]any{"output_index": out.TextIndex, "item_id": out.Parts[out.TextIndex].ID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		}
		s.BlockOutput[index] = out.TextIndex
		return wire, nil
	case "thinking":
		out.ReasoningIndex = -1
		wire := out.appendText("reasoning", block.Thinking)
		if out.ReasoningIndex < 0 {
			out.ReasoningIndex = out.add("reasoning")
			item, _ := out.item(out.Parts[out.ReasoningIndex], false)
			wire += out.emit("response.output_item.added", map[string]any{"output_index": out.ReasoningIndex, "item": item})
			wire += out.emit("response.reasoning_summary_part.added", map[string]any{"output_index": out.ReasoningIndex, "item_id": out.Parts[out.ReasoningIndex].ID, "summary_index": 0, "part": map[string]string{"type": "summary_text", "text": ""}})
		}
		s.BlockOutput[index] = out.ReasoningIndex
		return wire, nil
	case "tool_use":
		n := out.add("function_call")
		p := out.Parts[n]
		p.CallID, p.Name = block.ID, block.Name
		if out.ToolSearch && p.Name == "tool_search" {
			p.ID = "tsc_" + randomToken(12)
		}
		item, err := out.item(p, false)
		if err != nil {
			return "", err
		}
		s.BlockOutput[index] = n
		return out.emit("response.output_item.added", map[string]any{"output_index": n, "item": item}), nil
	}
	return "", nil
}

func (s *anthropicResponsesStream) usage(u priceUsage) {
	values := anthropicChatUsage(u)
	values["input_tokens"] = values["prompt_tokens"]
	delete(values, "prompt_tokens")
	values["output_tokens"] = values["completion_tokens"]
	delete(values, "completion_tokens")
	values["input_tokens_details"] = values["prompt_tokens_details"]
	delete(values, "prompt_tokens_details")
	values["output_tokens_details"] = map[string]int{"reasoning_tokens": 0}
	raw, _ := json.Marshal(values)
	_ = json.Unmarshal(raw, &s.Output.Usage)
}

func (s *anthropicResponsesStream) event(raw []byte, u priceUsage) (string, error) {
	if _, err := s.Native.event(raw, u); err != nil {
		return "", err
	}
	var e struct {
		Type  string
		Index int
		Delta struct {
			Type, Text, Thinking string
			Partial              string `json:"partial_json"`
		}
	}
	_ = json.Unmarshal(raw, &e)
	switch e.Type {
	case "message_start":
		return s.Output.start(), nil
	case "content_block_start":
		return s.beginBlock(e.Index, s.Native.Blocks[e.Index])
	case "content_block_delta":
		index, ok := s.BlockOutput[e.Index]
		if !ok {
			return "", nil
		}
		p := s.Output.Parts[index]
		switch e.Delta.Type {
		case "text_delta":
			s.Output.TextIndex = index
			return s.Output.appendText("message", e.Delta.Text), nil
		case "thinking_delta":
			s.Output.ReasoningIndex = index
			return s.Output.appendText("reasoning", e.Delta.Thinking), nil
		case "input_json_delta":
			p.Arguments += e.Delta.Partial
			if s.Output.Custom[p.Name] || s.Output.ToolSearch && p.Name == "tool_search" {
				return "", nil
			}
			return s.Output.emit("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": p.ID, "delta": e.Delta.Partial}), nil
		}
	case "content_block_stop":
		if n, ok := s.BlockOutput[e.Index]; ok && s.Output.Parts[n].Kind == "function_call" {
			p := s.Output.Parts[n]
			if p.Arguments == "" {
				p.Arguments = string(s.Native.Blocks[e.Index].Input)
				if !s.Output.Custom[p.Name] && !(s.Output.ToolSearch && p.Name == "tool_search") {
					return s.Output.emit("response.function_call_arguments.delta", map[string]any{"output_index": n, "item_id": p.ID, "delta": p.Arguments}), nil
				}
			}
		}
	case "message_stop":
		s.Output.Finish = s.Native.Finish
		s.usage(u)
		wire, _, err := s.Output.finish()
		return wire, err
	}
	return "", nil
}

func (s *anthropicResponsesStream) response(raw []byte, u priceUsage) ([]byte, error) {
	if _, err := s.Native.response(raw, u); err != nil {
		return nil, err
	}
	for i, b := range s.Native.Blocks {
		if _, err := s.beginBlock(i, b); err != nil {
			return nil, err
		}
		if b.Type == "tool_use" {
			s.Output.Parts[s.BlockOutput[i]].Arguments = string(b.Input)
		}
	}
	s.Output.Finish = s.Native.Finish
	s.usage(u)
	_, result, err := s.Output.finish()
	return result, err
}

func (s *anthropicResponsesStream) assistant() convertedChatMessage {
	blocks := []map[string]json.RawMessage{}
	for _, b := range s.Native.Blocks {
		item := map[string]any{"type": b.Type}
		switch b.Type {
		case "text":
			item["text"] = b.Text + b.TextDelta.String()
		case "thinking":
			item["thinking"] = b.Thinking + b.ThinkingDelta.String()
			item["signature"] = b.Signature + b.SignatureDelta.String()
		case "redacted_thinking":
			item["data"] = b.Data
		case "tool_use":
			item["id"], item["name"], item["input"] = b.ID, b.Name, b.Input
			if name, ok := s.Output.Namespaces[b.Name]; ok {
				item["name"], item["namespace"] = name.Name, name.Namespace
			}
			if s.Output.Custom[b.Name] {
				item["custom"] = true
			}
			if s.Output.ToolSearch && b.Name == "tool_search" {
				item["tool_search"] = true
			}
		}
		raw, _ := json.Marshal(item)
		var saved map[string]json.RawMessage
		_ = json.Unmarshal(raw, &saved)
		blocks = append(blocks, saved)
	}
	return convertedChatMessage{Role: "assistant", Anthropic: blocks}
}
