package app

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Reuse generation/tool validation while keeping native Messages content order.
func messagesToGemini(body map[string]json.RawMessage, saved map[string]json.RawMessage, count bool) ([]byte, string, error) {
	copy := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		copy[k] = v
	}
	delete(copy, "top_k")
	options, effort, err := messagesOpenAIOptions(copy)
	if err != nil {
		return nil, "", err
	}
	in := map[string]json.RawMessage{"model": body["model"]}
	for from, to := range map[string]string{"temperature": "temperature", "top_p": "top_p", "service_tier": "service_tier", "max_completion_tokens": "max_output_tokens", "parallel_tool_calls": "parallel_tool_calls"} {
		if v := options[from]; v != nil {
			raw, _ := json.Marshal(v)
			if string(raw) != "null" {
				in[to] = raw
			}
		}
	}
	if raw := in["max_output_tokens"]; raw != nil {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 2147483647 {
			return nil, "", bad("invalid output token limit")
		}
	}
	if f, ok := options["response_format"].(map[string]any); ok {
		format := f["json_schema"].(map[string]any)
		format["type"] = "json_schema"
		in["text"], _ = json.Marshal(map[string]any{"format": format})
	}
	if tools, ok := options["tools"].([]any); ok {
		list := []any{}
		for _, tool := range tools {
			f := tool.(map[string]any)["function"].(map[string]any)
			f["type"] = "function"
			list = append(list, f)
		}
		in["tools"], _ = json.Marshal(list)
	}
	if choice := options["tool_choice"]; choice != nil {
		if c, ok := choice.(map[string]any); ok {
			choice = map[string]any{"type": "function", "name": c["function"].(map[string]string)["name"]}
		}
		in["tool_choice"], _ = json.Marshal(choice)
	}
	var thinking struct {
		Type   string
		Budget *int64 `json:"budget_tokens"`
	}
	if raw := body["thinking"]; raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &thinking) != nil {
			return nil, "", bad("invalid thinking configuration")
		}
		switch thinking.Type {
		case "enabled":
			if thinking.Budget == nil || *thinking.Budget < 1024 || *thinking.Budget > 2147483647 {
				return nil, "", bad("thinking budget must be at least 1024")
			}
			var limit int64
			if raw := body["max_tokens"]; raw != nil && (json.Unmarshal(raw, &limit) != nil || limit <= *thinking.Budget) {
				return nil, "", bad("thinking budget must be below max_tokens")
			}
			if effort != "" {
				return nil, "", bad("choose thinking budget or effort, not both")
			}
		case "disabled":
			if thinking.Budget != nil || effort != "" && effort != "none" {
				return nil, "", bad("disabled thinking conflicts with budget or effort")
			}
			effort = "none"
		case "adaptive":
			if thinking.Budget != nil {
				return nil, "", bad("adaptive thinking does not accept a budget")
			}
		default:
			return nil, "", bad("unsupported thinking configuration")
		}
	}
	out, _, effort, err := geminiOptions(in, body["stop_sequences"], effort)
	if err != nil {
		return nil, "", err
	}
	config := out["generationConfig"].(map[string]any)
	if raw := body["top_k"]; raw != nil && string(raw) != "null" {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 2147483647 {
			return nil, "", bad("invalid top_k")
		}
		config["topK"] = n
	}
	if thinking.Type == "enabled" {
		config["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingBudget": *thinking.Budget}
		effort = ""
	} else if thinking.Type == "adaptive" && effort == "" {
		config["thinkingConfig"] = map[string]any{"includeThoughts": true}
	}
	if raw := body["system"]; raw != nil && string(raw) != "null" {
		blocks, err := messagesBlocks(raw)
		if err != nil {
			return nil, "", err
		}
		parts := []any{}
		for _, b := range blocks {
			if credentialString(b, "type") != "text" {
				return nil, "", bad("system content must be text")
			}
			part, err := messagesGeminiPart(b)
			if err != nil {
				return nil, "", err
			}
			if !strings.HasPrefix(credentialString(b, "text"), "x-anthropic-billing-header: ") {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			out["systemInstruction"] = map[string]any{"parts": parts}
		}
	}
	var messages []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return nil, "", bad("messages are required")
	}
	contents := []any{}
	calls := map[string]string{}
	functionIDs := map[string]string{}
	results := map[string]bool{}
	replayed := map[string]bool{}
	for _, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			return nil, "", bad("invalid Messages role")
		}
		blocks, err := messagesBlocks(m.Content)
		if err != nil {
			return nil, "", err
		}
		parts := []any{}
		for _, b := range blocks {
			kind := credentialString(b, "type")
			var part map[string]any
			var trailing []any
			switch kind {
			case "text", "image", "document":
				if m.Role == "assistant" && kind != "text" {
					return nil, "", bad("assistant media requires native Gemini content")
				}
				part, err = messagesGeminiPart(b)
				if err != nil {
					return nil, "", err
				}
			case "thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires an assistant role")
				}
				var thought string
				if json.Unmarshal(b["thinking"], &thought) != nil {
					return nil, "", bad("invalid thinking text")
				}
				if saved[credentialString(b, "signature")] == nil {
					continue
				}
				part = map[string]any{"text": thought, "thought": true}
			case "redacted_thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires an assistant role")
				}
				continue
			case "tool_use":
				id, name := credentialString(b, "id"), credentialString(b, "name")
				var args map[string]json.RawMessage
				if m.Role != "assistant" || id == "" || len(id) > 256 || !geminiFunctionName(name) || calls[id] != "" || json.Unmarshal(b["input"], &args) != nil || args == nil {
					return nil, "", bad("invalid or duplicate tool_use")
				}
				calls[id] = name
				part = map[string]any{"functionCall": map[string]any{"name": name, "args": args}, "thoughtSignature": "skip_thought_signature_validator"}
			case "tool_result":
				id := credentialString(b, "tool_use_id")
				name := calls[id]
				if m.Role != "user" || name == "" || results[id] {
					return nil, "", bad("tool_result requires a matching call")
				}
				results[id] = true
				var failed bool
				if raw := b["is_error"]; raw != nil && json.Unmarshal(raw, &failed) != nil {
					return nil, "", bad("invalid tool result error flag")
				}
				result := []string{}
				media := []any{}
				if raw := b["content"]; raw != nil && string(raw) != "null" {
					inner, err := messagesBlocks(raw)
					if err != nil {
						return nil, "", err
					}
					for _, p := range inner {
						converted, err := messagesGeminiPart(p)
						if err != nil {
							return nil, "", err
						}
						if text, ok := converted["text"].(string); ok {
							result = append(result, text)
						} else if converted["inlineData"] != nil {
							media = append(media, converted)
						} else {
							// FunctionResponsePart supports inline bytes only; URI media
							// stays in the same user turn immediately after the result.
							trailing = append(trailing, converted)
						}
					}
				}
				field := "output"
				if failed {
					field = "error"
				}
				function := map[string]any{"name": name, "response": map[string]any{field: strings.Join(result, "\n\n")}}
				if id := functionIDs[id]; id != "" {
					function["id"] = id
				}
				// Keep images/documents attached to their function result, not unrelated text.
				if len(media) > 0 {
					function["parts"] = media
				}
				part = map[string]any{"functionResponse": function}
			default:
				return nil, "", bad("content requires a native Messages account")
			}
			if sig := credentialString(b, "signature"); saved[sig] != nil {
				if replayed[sig] {
					return nil, "", bad("duplicate Gemini history signature")
				}
				replayed[sig] = true
				var state geminiMessagesState
				if json.Unmarshal(saved[sig], &state) != nil || state.Type != "gemini_part" || state.Part == nil {
					return nil, "", bad("reasoning signature is not Gemini state")
				}
				actual := map[string]json.RawMessage{}
				for _, key := range []string{"type", "text", "thinking", "id", "name", "input"} {
					if b[key] != nil {
						actual[key] = b[key]
					}
				}
				if responseToolDefinition(actual) != responseToolDefinition(state.Block) {
					return nil, "", bad("signed Gemini content changed")
				}
				if kind == "tool_use" {
					var call map[string]json.RawMessage
					_ = json.Unmarshal(state.Part["functionCall"], &call)
					functionIDs[credentialString(b, "id")] = credentialString(call, "id")
				}
				parts = append(parts, state.Part)
			} else {
				parts = append(parts, part)
			}
			parts = append(parts, trailing...)
		}
		if len(parts) > 0 {
			role := m.Role
			if role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	if len(contents) == 0 {
		return nil, "", bad("messages contain no convertible content")
	}
	out["contents"] = contents
	if count {
		out["model"] = "models/" + strings.TrimPrefix(credentialString(body, "model"), "models/")
		out = map[string]any{"generateContentRequest": out}
	}
	raw, err := json.Marshal(out)
	return raw, effort, err
}

func messagesGeminiPart(block map[string]json.RawMessage) (map[string]any, error) {
	if credentialString(block, "type") == "text" {
		var text string
		if json.Unmarshal(block["text"], &text) != nil {
			return nil, bad("invalid text content")
		}
		return map[string]any{"text": text}, nil
	}
	part, err := messagesResponsesPart(block, "user")
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal([]any{part})
	parts, err := geminiInputContent(raw)
	if err != nil {
		return nil, err
	}
	if len(parts) != 1 {
		return nil, bad("empty media content")
	}
	return parts[0].(map[string]any), nil
}

type geminiMessagesState struct {
	Type  string                     `json:"type"`
	Part  map[string]json.RawMessage `json:"part"`
	Block map[string]json.RawMessage `json:"block"`
}

type geminiMessagesStream struct {
	Native  *geminiChatStream
	Output  *responsesMessagesStream
	Blocks  []map[string]any
	Pending strings.Builder
	Held    bool
}

func newGeminiMessagesStream(model string, seal func(json.RawMessage) (string, error)) *geminiMessagesStream {
	s := &geminiMessagesStream{Native: newGeminiChatStream(model, false, nil), Output: newResponsesMessagesStream(model, seal)}
	s.Native.Part = s.part
	return s
}
func (s *geminiMessagesStream) part(part map[string]json.RawMessage, tool map[string]any) (string, error) {
	block := map[string]any{"type": "text", "text": credentialString(part, "text")}
	if string(part["thought"]) == "true" || part["text"] == nil && tool == nil {
		block = map[string]any{"type": "thinking", "thinking": credentialString(part, "text"), "signature": ""}
	}
	if tool != nil {
		function := tool["function"].(map[string]any)
		block = map[string]any{"type": "tool_use", "id": tool["id"], "name": function["name"], "input": json.RawMessage(function["arguments"].(string))}
		s.Held = true
	}
	if signature := credentialString(part, "thoughtSignature"); signature != "" {
		rawBlock, _ := json.Marshal(block)
		var expected map[string]json.RawMessage
		_ = json.Unmarshal(rawBlock, &expected)
		delete(expected, "signature")
		raw, _ := json.Marshal(geminiMessagesState{Type: "gemini_part", Part: part, Block: expected})
		sealed, err := s.Output.Seal(raw)
		if err != nil {
			return "", err
		}
		block["signature"] = sealed
	}
	s.Blocks = append(s.Blocks, block)
	wire := ""
	// Preserve native part order. Once a tool appears, all following blocks wait
	// for settlement, so a complete tool cannot trigger execution prematurely.
	if tool != nil {
		wire = s.Output.closeBlock()
		index := s.Output.BlockIndex
		s.Output.BlockIndex++
		start := map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}
		if sig := block["signature"]; sig != nil {
			start["signature"] = sig
		}
		wire += messagesEvent("content_block_start", map[string]any{"index": index, "content_block": start})
		wire += messagesEvent("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(block["input"].(json.RawMessage))}})
		wire += messagesEvent("content_block_stop", map[string]any{"index": index})
	} else if block["type"] == "thinking" {
		wire = s.Output.fullBlock(block)
	} else {
		key := strconv.Itoa(len(s.Blocks))
		if sig, ok := block["signature"].(string); ok && sig != "" {
			index := s.Output.BlockIndex
			s.Output.BlockIndex++
			wire = messagesEvent("content_block_start", map[string]any{"index": index, "content_block": block}) + messagesEvent("content_block_stop", map[string]any{"index": index})
		} else {
			wire = s.Output.text(key, block["text"].(string), "text") + s.Output.closeBlock()
		}
	}
	if s.Held {
		s.Pending.WriteString(wire)
		if s.Pending.Len() > 16<<20 {
			return "", &apiError{502, "converted stream exceeds limit"}
		}
		return "", nil
	}
	return wire, nil
}
func (s *geminiMessagesStream) observe(raw []byte) (string, error) {
	wire := ""
	if !s.Output.Started {
		s.Output.Started = true
		wire = messagesEvent("message_start", map[string]any{"message": s.Output.result([]any{}, nil, priceUsage{})})
	}
	next, err := s.Native.observe(raw)
	return wire + next, err
}
func (s *geminiMessagesStream) finish(u priceUsage) (string, []byte, error) {
	if s.Native.Finish == "" {
		return "", nil, &apiError{502, "Gemini response has no finish reason"}
	}
	reason := map[string]string{"stop": "end_turn", "tool_calls": "tool_use", "length": "max_tokens", "content_filter": "refusal"}[s.Native.Finish]
	content := []any{}
	for _, b := range s.Blocks {
		content = append(content, b)
	}
	wire := s.Pending.String() + s.Output.closeBlock() + messagesEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": messagesUsage(u)}) + messagesEvent("message_stop", map[string]any{})
	raw, err := json.Marshal(s.Output.result(content, reason, u))
	return wire, raw, err
}
