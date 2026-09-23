package app

import (
	"encoding/json"
	"strings"
)

// Normalize the client contract once; Gemini keeps its native content, usage and
// authentication on the wire, independent of the public model alias.
func chatToGemini(body map[string]json.RawMessage) ([]byte, map[string]bool, string, error) {
	copy := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		copy[k] = v
	}
	delete(copy, "stop")
	raw, err := normalizeChatInput(copy, false)
	if err != nil {
		return nil, nil, "", err
	}
	var in map[string]json.RawMessage
	_ = json.Unmarshal(raw, &in)
	effort, err := requestEffort(body, "chat_completions")
	if err != nil {
		return nil, nil, "", err
	}
	out, custom, effort, err := geminiOptions(in, body["stop"], effort)
	if err != nil {
		return nil, nil, "", err
	}
	names := map[string]bool{}
	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(in["tools"], &tools)
	for _, t := range tools {
		names[credentialString(t, "name")] = true
	}
	// Chat clients can round-trip Google's opaque function-call signatures. Calls
	// imported without one use the compatibility sentinel, as the native API allows.
	var original []struct {
		ToolCalls []struct {
			ID    string
			Extra struct {
				Google struct {
					Signature string `json:"thought_signature"`
				}
			} `json:"extra_content"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(body["messages"], &original) != nil {
		return nil, nil, "", bad("invalid tool signature metadata")
	}
	signatures := map[string]string{}
	for _, m := range original {
		for _, t := range m.ToolCalls {
			if t.Extra.Google.Signature != "" {
				signatures[t.ID] = t.Extra.Google.Signature
			}
		}
	}
	type content struct {
		Role  string `json:"role"`
		Parts []any  `json:"parts"`
	}
	contents := []content{}
	system := []any{}
	appendContent := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		if len(contents) > 0 && contents[len(contents)-1].Role == role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
		} else {
			contents = append(contents, content{role, parts})
		}
	}
	if instructions := credentialString(in, "instructions"); instructions != "" {
		system = append(system, map[string]any{"text": instructions})
	}
	var input []map[string]json.RawMessage
	_ = json.Unmarshal(in["input"], &input)
	calls := map[string]string{}
	results := map[string]bool{}
	for _, item := range input {
		kind, role := credentialString(item, "type"), credentialString(item, "role")
		switch kind {
		case "function_call", "custom_tool_call":
			id, name := credentialString(item, "call_id"), credentialString(item, "name")
			if id == "" || len(id) > 256 || !geminiFunctionName(name) || calls[id] != "" || names[name] && custom[name] != (kind == "custom_tool_call") {
				return nil, nil, "", bad("invalid historical tool identity")
			}
			var args map[string]json.RawMessage
			if kind == "custom_tool_call" {
				custom[name] = true
				value, _ := json.Marshal(credentialString(item, "input"))
				args = map[string]json.RawMessage{"input": value}
			} else if json.Unmarshal([]byte(credentialString(item, "arguments")), &args) != nil || args == nil {
				return nil, nil, "", bad("tool arguments must be a JSON object")
			}
			calls[id] = name
			signature := signatures[id]
			if signature == "" {
				signature = "skip_thought_signature_validator"
			}
			appendContent("model", []any{map[string]any{"functionCall": map[string]any{"name": name, "args": args}, "thoughtSignature": signature}})
		case "function_call_output", "custom_tool_call_output":
			id := credentialString(item, "call_id")
			name := calls[id]
			if name == "" || results[id] {
				return nil, nil, "", bad("tool result requires a matching call")
			}
			results[id] = true
			parts, err := geminiInputContent(item["output"])
			if err != nil {
				return nil, nil, "", err
			}
			var result strings.Builder
			media := []any{}
			for _, p := range parts {
				v := p.(map[string]any)
				if s, ok := v["text"].(string); ok {
					result.WriteString(s)
				} else {
					media = append(media, p)
				}
			}
			response := map[string]any{"functionResponse": map[string]any{"name": name, "response": map[string]any{"content": result.String()}}}
			appendContent("user", append([]any{response}, media...))
		default:
			parts, err := geminiInputContent(item["content"])
			if err != nil {
				return nil, nil, "", err
			}
			if role == "system" || role == "developer" {
				for _, p := range parts {
					if _, ok := p.(map[string]any)["text"]; !ok {
						return nil, nil, "", bad("system content must be text")
					}
				}
				system = append(system, parts...)
			} else {
				if role == "assistant" {
					role = "model"
				}
				appendContent(role, parts)
			}
		}
	}
	if len(contents) == 0 {
		return nil, nil, "", bad("messages require non-system content")
	}
	out["contents"] = contents
	if len(system) > 0 {
		out["systemInstruction"] = map[string]any{"parts": system}
	}
	raw, err = json.Marshal(out)
	return raw, custom, effort, err
}

// The two inbound protocols share only generation controls and tool declarations.
func geminiOptions(in map[string]json.RawMessage, stop json.RawMessage, effort string) (map[string]any, map[string]bool, string, error) {
	if tier := credentialString(in, "service_tier"); tier != "" && tier != "auto" {
		return nil, nil, "", bad("service_tier cannot be converted to Gemini")
	}
	config := map[string]any{"responseModalities": []string{"TEXT"}}
	for from, to := range map[string]string{"temperature": "temperature", "top_p": "topP", "max_output_tokens": "maxOutputTokens"} {
		if in[from] != nil {
			config[to] = in[from]
		}
	}
	if raw := stop; raw != nil && string(raw) != "null" {
		var single string
		var stops []string
		if json.Unmarshal(raw, &single) == nil {
			stops = []string{single}
		} else if json.Unmarshal(raw, &stops) != nil {
			return nil, nil, "", bad("invalid stop sequences")
		}
		if len(stops) > 5 {
			return nil, nil, "", bad("too many stop sequences")
		}
		for _, s := range stops {
			if s == "" {
				return nil, nil, "", bad("empty stop sequence")
			}
		}
		config["stopSequences"] = stops
	}
	if effort != "" {
		model := strings.TrimPrefix(strings.ToLower(credentialString(in, "model")), "models/")
		if effort == "xhigh" || effort == "max" {
			effort = "high"
		}
		thinking := map[string]any{"includeThoughts": true}
		switch effort {
		case "none", "minimal", "low", "medium", "high":
		default:
			return nil, nil, "", bad("unsupported Gemini reasoning effort")
		}
		if strings.HasPrefix(model, "gemini-2.5") {
			if effort == "none" && strings.Contains(model, "pro") {
				return nil, nil, "", bad("thinking cannot be disabled for this model")
			}
			budget := map[string]int{"none": 0, "minimal": 1024, "low": 1024, "medium": 8192, "high": 24576}[effort]
			thinking["thinkingBudget"] = budget
			// A token budget is not an explicit billable thinking level.
			effort = ""
		} else {
			if effort == "none" {
				return nil, nil, "", bad("thinking cannot be disabled for this model")
			}
			if effort == "minimal" && strings.Contains(model, "pro") {
				effort = "low"
			}
			thinking["thinkingLevel"] = strings.ToUpper(effort)
		}
		config["thinkingConfig"] = thinking
	}
	var text struct {
		Format    map[string]json.RawMessage
		Verbosity json.RawMessage
	}
	if raw := in["text"]; raw != nil && json.Unmarshal(raw, &text) != nil {
		return nil, nil, "", bad("invalid text configuration")
	}
	if text.Verbosity != nil {
		return nil, nil, "", bad("verbosity cannot be converted to Gemini")
	}
	switch credentialString(text.Format, "type") {
	case "", "text":
	case "json_object":
		config["responseMimeType"] = "application/json"
	case "json_schema":
		var schema map[string]json.RawMessage
		if json.Unmarshal(text.Format["schema"], &schema) != nil || schema == nil {
			return nil, nil, "", bad("JSON schema required")
		}
		config["responseMimeType"], config["responseJsonSchema"] = "application/json", schema
	default:
		return nil, nil, "", bad("unsupported response format")
	}
	out := map[string]any{"generationConfig": config}
	custom, names := map[string]bool{}, map[string]bool{}
	var tools []map[string]json.RawMessage
	_ = json.Unmarshal(in["tools"], &tools)
	declarations := []any{}
	for _, t := range tools {
		name := credentialString(t, "name")
		if !geminiFunctionName(name) || names[name] {
			return nil, nil, "", bad("invalid or duplicate Gemini tool name")
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
		declaration := map[string]any{"name": name, "parametersJsonSchema": schema}
		if t["description"] != nil {
			declaration["description"] = t["description"]
		}
		declarations = append(declarations, declaration)
	}
	if raw := in["parallel_tool_calls"]; raw != nil && string(raw) != "null" {
		var parallel bool
		if json.Unmarshal(raw, &parallel) != nil || !parallel && len(declarations) > 0 {
			return nil, nil, "", bad("Gemini cannot disable parallel function calls")
		}
	}
	if len(declarations) == 0 {
		if raw := in["tool_choice"]; raw != nil && string(raw) != "null" && string(raw) != `"auto"` && string(raw) != `"none"` {
			return nil, nil, "", bad("tool_choice requires declared tools")
		}
	}
	if len(declarations) > 0 {
		out["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
		choice := map[string]any{"mode": "AUTO"}
		if raw := in["tool_choice"]; raw != nil && string(raw) != "null" {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				mode := map[string]string{"auto": "AUTO", "none": "NONE", "required": "ANY"}[value]
				if mode == "" {
					return nil, nil, "", bad("invalid tool_choice")
				}
				choice["mode"] = mode
			} else {
				var v map[string]json.RawMessage
				_ = json.Unmarshal(raw, &v)
				name := credentialString(v, "name")
				if !names[name] {
					return nil, nil, "", bad("tool_choice must name a declared tool")
				}
				choice["mode"], choice["allowedFunctionNames"] = "ANY", []string{name}
			}
		}
		out["toolConfig"] = map[string]any{"functionCallingConfig": choice}
	}
	return out, custom, effort, nil
}

func geminiFunctionName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for i, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || i > 0 && (c >= '0' && c <= '9' || strings.ContainsRune(".-:", c))) {
			return false
		}
	}
	return true
}

func geminiInputContent(raw json.RawMessage) ([]any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []any{map[string]any{"text": text}}, nil
	}
	parts, err := anthropicInputContent(raw)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, p := range parts {
		b := p.(map[string]any)
		if b["type"] == "text" {
			out = append(out, map[string]any{"text": b["text"]})
			continue
		}
		source := b["source"].(map[string]any)
		if source["type"] == "base64" {
			out = append(out, map[string]any{"inlineData": map[string]any{"mimeType": source["media_type"], "data": source["data"]}})
		} else {
			// The provider fetches the URI; the gateway does not fetch client media URLs.
			out = append(out, map[string]any{"fileData": map[string]any{"fileUri": source["url"]}})
		}
	}
	return out, nil
}

type geminiChatStream struct {
	Chat            *responseChatStream
	Custom          map[string]bool
	Text, Thought   strings.Builder
	Tools           []map[string]any
	IDs             map[string]bool
	Finish          string
	Size, Parts     int
	ReasoningTokens int64
	Part            func(map[string]json.RawMessage, map[string]any) (string, error)
}

func newGeminiChatStream(model string, include bool, custom map[string]bool) *geminiChatStream {
	chat := newResponseChatStream(model, include)
	chat.ID = "chatcmpl-" + randomToken(18)
	return &geminiChatStream{Chat: chat, Custom: custom, IDs: map[string]bool{}}
}
func (s *geminiChatStream) observe(raw []byte) (string, error) {
	var e struct {
		Candidates []struct {
			Index   int
			Finish  string `json:"finishReason"`
			Content struct {
				Role  string
				Parts []map[string]json.RawMessage
			}
		}
		Feedback struct {
			Block string `json:"blockReason"`
		} `json:"promptFeedback"`
		Usage *struct {
			Thoughts int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if json.Unmarshal(raw, &e) != nil || len(e.Candidates) > 1 {
		return "", &apiError{502, "invalid Gemini candidate response"}
	}
	if e.Usage != nil {
		s.ReasoningTokens = e.Usage.Thoughts
	}
	s.Size += len(raw)
	if s.Size > 16<<20 {
		return "", &apiError{502, "converted stream exceeds limit"}
	}
	var wire strings.Builder
	if !s.Chat.Role && s.Part == nil {
		wire.WriteString(s.Chat.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil))
		s.Chat.Role = true
	}
	if e.Feedback.Block != "" {
		if s.Finish != "" || len(e.Candidates) > 0 {
			return "", &apiError{502, "invalid Gemini prompt block"}
		}
		s.Finish = "content_filter"
	}
	for _, c := range e.Candidates {
		if c.Index != 0 || c.Content.Role != "" && c.Content.Role != "model" || s.Finish != "" && (len(c.Content.Parts) > 0 || c.Finish != "") {
			return "", &apiError{502, "invalid Gemini candidate order"}
		}
		for _, p := range c.Content.Parts {
			s.Parts++
			if s.Parts > 4096 {
				return "", &apiError{502, "converted stream exceeds limit"}
			}
			var tool map[string]any
			switch {
			case p["text"] != nil:
				var text string
				var thought bool
				if json.Unmarshal(p["text"], &text) != nil || p["thought"] != nil && json.Unmarshal(p["thought"], &thought) != nil || p["functionCall"] != nil {
					return "", &apiError{502, "invalid Gemini text part"}
				}
				field := "content"
				if thought {
					field = "reasoning_content"
					s.Thought.WriteString(text)
				} else {
					s.Text.WriteString(text)
				}
				if s.Part == nil {
					wire.WriteString(s.Chat.chunk(map[string]any{field: text}, nil, nil))
				}
			case p["functionCall"] != nil:
				var call struct {
					ID, Name string
					Args     json.RawMessage
				}
				if json.Unmarshal(p["functionCall"], &call) != nil || !geminiFunctionName(call.Name) || len(call.ID) > 256 {
					return "", &apiError{502, "invalid Gemini function call"}
				}
				var args map[string]json.RawMessage
				if call.Args == nil {
					call.Args = json.RawMessage(`{}`)
				}
				if json.Unmarshal(call.Args, &args) != nil || args == nil {
					return "", &apiError{502, "invalid Gemini function arguments"}
				}
				if call.ID == "" {
					call.ID = "call_" + randomToken(18)
				}
				if s.IDs[call.ID] {
					return "", &apiError{502, "duplicate Gemini tool identity"}
				}
				s.IDs[call.ID] = true
				kind, field, value := "function", "arguments", string(call.Args)
				if s.Custom[call.Name] {
					kind, field = "custom", "input"
					if json.Unmarshal(args["input"], &value) != nil {
						return "", &apiError{502, "invalid custom tool input"}
					}
				}
				tool = map[string]any{"id": call.ID, "type": kind, kind: map[string]any{"name": call.Name, field: value}}
				if signature := credentialString(p, "thoughtSignature"); signature != "" {
					tool["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
				}
				s.Tools = append(s.Tools, tool)
			case s.Part != nil && len(p) == 1 && credentialString(p, "thoughtSignature") != "":
				// Native streams can carry a standalone signature part.
			default:
				return "", &apiError{502, "unsupported Gemini output part"}
			}
			if s.Part != nil {
				partWire, err := s.Part(p, tool)
				if err != nil {
					return "", err
				}
				wire.WriteString(partWire)
			}
		}
		if c.Finish != "" {
			switch c.Finish {
			case "STOP":
				s.Finish = "stop"
				if len(s.Tools) > 0 {
					s.Finish = "tool_calls"
				}
			case "MAX_TOKENS":
				s.Finish = "length"
			case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
				s.Finish = "content_filter"
			default:
				return "", &apiError{502, "Gemini generation did not complete successfully"}
			}
		}
	}
	return wire.String(), nil
}
func (s *geminiChatStream) finish(u priceUsage) (string, []byte, error) {
	if s.Finish == "" {
		return "", nil, &apiError{502, "Gemini response has no finish reason"}
	}
	usage := anthropicChatUsage(u)
	usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": s.ReasoningTokens}
	message := map[string]any{"role": "assistant", "content": s.Text.String()}
	if s.Thought.Len() > 0 {
		message["reasoning_content"] = s.Thought.String()
	}
	wire := ""
	if len(s.Tools) > 0 {
		message["tool_calls"] = s.Tools
		deltas := []any{}
		for i, t := range s.Tools {
			v := map[string]any{"index": i}
			for k, value := range t {
				v[k] = value
			}
			deltas = append(deltas, v)
		}
		wire = s.Chat.chunk(map[string]any{"tool_calls": deltas}, nil, nil)
	}
	wire += s.Chat.chunk(map[string]any{}, s.Finish, nil)
	if s.Chat.IncludeUsage {
		wire += s.Chat.chunk(nil, nil, usage)
	}
	wire += "data: [DONE]\n\n"
	raw, err := json.Marshal(map[string]any{"id": s.Chat.ID, "object": "chat.completion", "created": s.Chat.Created, "model": s.Chat.Model, "choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": s.Finish}}, "usage": usage})
	return wire, raw, err
}
