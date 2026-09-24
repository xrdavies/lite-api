package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

type convertedChatTool struct {
	ID    string `json:"id,omitempty"`
	Index int    `json:"index,omitempty"`
	Type  string `json:"type,omitempty"`
	// Namespace is retained in encrypted history and removed from Chat requests.
	Namespace string `json:"namespace,omitempty"`
	Search    bool   `json:"tool_search,omitempty"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type convertedChatMessage struct {
	Role        string                       `json:"role"`
	Content     any                          `json:"content"`
	Reasoning   string                       `json:"reasoning_content,omitempty"`
	Refusal     string                       `json:"refusal,omitempty"`
	Calls       []convertedChatTool          `json:"tool_calls,omitempty"`
	CallID      string                       `json:"tool_call_id,omitempty"`
	Discoveries []map[string]json.RawMessage `json:"discovered_tools,omitempty"`
	// Only authenticated encrypted history contains native blocks; client input cannot set these.
	Anthropic     []map[string]json.RawMessage `json:"anthropic_content,omitempty"`
	ResponseInput json.RawMessage              `json:"response_input,omitempty"`
}

type responsesChatRequest struct {
	Body             map[string]any
	Messages         []convertedChatMessage
	History          []convertedChatMessage
	Custom           map[string]bool
	Namespaces       map[string]responseToolName
	ToolSearch       bool
	Direct, Declared map[string]bool
}

type responseToolName struct{ Namespace, Name string }

func flattenedToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= 64 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	var prefix strings.Builder
	for _, r := range full {
		if prefix.Len()+len(string(r)) > 54 {
			break
		}
		prefix.WriteRune(r)
	}
	return prefix.String() + "__" + hex.EncodeToString(sum[:4])
}

func responsesToChatRequest(body map[string]json.RawMessage, history []convertedChatMessage) (*responsesChatRequest, error) {
	out := &responsesChatRequest{Body: map[string]any{"model": body["model"], "stream": false, "store": false}, Custom: map[string]bool{}, Namespaces: map[string]responseToolName{}}
	for _, name := range []string{"stream", "temperature", "top_p", "service_tier", "parallel_tool_calls", "metadata", "user", "safety_identifier", "prompt_cache_key", "prompt_cache_retention"} {
		if raw := body[name]; raw != nil {
			out.Body[name] = raw
		}
	}
	if raw := body["modalities"]; raw != nil && string(raw) != "null" {
		var modes []string
		if json.Unmarshal(raw, &modes) != nil || len(modes) < 1 || len(modes) > 2 {
			return nil, bad("invalid Responses modalities")
		}
		seen := map[string]bool{}
		for _, mode := range modes {
			if mode != "text" && mode != "image" || seen[mode] {
				return nil, bad("unsupported Responses modality")
			}
			seen[mode] = true
		}
		out.Body["modalities"] = raw
	}
	for _, name := range []string{"prompt", "context_management"} {
		if raw := body[name]; raw != nil && string(raw) != "null" {
			return nil, bad(name + " requires a native Responses account")
		}
	}
	if raw := body["truncation"]; raw != nil && string(raw) != "null" && string(raw) != `"disabled"` {
		return nil, bad("automatic truncation requires a native Responses account")
	}
	if raw := body["max_output_tokens"]; raw != nil && string(raw) != "null" {
		var n int64
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 2147483647 {
			return nil, bad("invalid max_output_tokens")
		}
		out.Body["max_completion_tokens"] = n
	}
	if string(body["stream"]) == "true" {
		out.Body["stream_options"] = map[string]bool{"include_usage": true}
	}
	effort, err := requestEffort(body, "responses")
	if err != nil {
		return nil, err
	}
	if effort != "" {
		out.Body["reasoning_effort"] = effort
	}
	if raw := body["text"]; raw != nil && string(raw) != "null" {
		var text map[string]json.RawMessage
		if json.Unmarshal(raw, &text) != nil || text == nil {
			return nil, bad("invalid text configuration")
		}
		if raw := text["verbosity"]; raw != nil {
			out.Body["verbosity"] = raw
		}
		if raw := text["format"]; raw != nil && string(raw) != "null" {
			var f map[string]json.RawMessage
			if json.Unmarshal(raw, &f) != nil || f == nil {
				return nil, bad("invalid text format")
			}
			kind := credentialString(f, "type")
			switch kind {
			case "json_schema":
				delete(f, "type")
				out.Body["response_format"] = map[string]any{"type": kind, "json_schema": f}
			case "text", "json_object":
				out.Body["response_format"] = f
			default:
				return nil, bad("unsupported text format")
			}
		}
	}
	historyStart := 0
	if raw := body["instructions"]; raw != nil && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil, bad("instructions must be text")
		}
		if text != "" {
			out.Messages = append(out.Messages, convertedChatMessage{Role: "system", Content: text})
			historyStart = 1
		}
	}
	out.Messages = append(out.Messages, history...)
	var items []map[string]json.RawMessage
	var text string
	if raw := body["input"]; len(raw) > 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &text) == nil {
			items = []map[string]json.RawMessage{{"role": json.RawMessage(`"user"`), "content": raw}}
		} else if json.Unmarshal(raw, &items) != nil {
			return nil, bad("invalid Responses input")
		}
	}
	if err := out.prepareTools(body, history, false); err != nil {
		return nil, err
	}
	reasoning := ""
	appendAssistant := func() *convertedChatMessage {
		if len(out.Messages) == 0 || out.Messages[len(out.Messages)-1].Role != "assistant" {
			out.Messages = append(out.Messages, convertedChatMessage{Role: "assistant", Content: nil})
		}
		if reasoning != "" {
			out.Messages[len(out.Messages)-1].Reasoning = reasoning
		}
		return &out.Messages[len(out.Messages)-1]
	}
	for _, item := range items {
		if item == nil {
			return nil, bad("invalid input item")
		}
		switch kind := credentialString(item, "type"); kind {
		case "additional_tools":
			continue
		case "item_reference":
			return nil, bad("item_reference requires a native Responses account")
		case "reasoning":
			var parts []struct{ Type, Text string }
			if raw := item["summary"]; raw != nil && json.Unmarshal(raw, &parts) != nil {
				return nil, bad("invalid reasoning summary")
			}
			reasoning = ""
			for _, part := range parts {
				reasoning += part.Text
			}
			if reasoning == "" && item["encrypted_content"] != nil {
				return nil, bad("encrypted reasoning requires its plaintext summary for Chat conversion")
			}
		case "tool_search_call":
			if err := clientSearchItem(item); err != nil {
				return nil, err
			}
			arguments, err := searchCallArguments(item["arguments"])
			if err != nil {
				return nil, bad("tool search arguments must be a JSON object")
			}
			call := convertedChatTool{ID: credentialString(item, "call_id"), Type: "function", Search: true}
			call.Function.Name, call.Function.Arguments = "tool_search", string(arguments)
			msg := appendAssistant()
			msg.Calls = append(msg.Calls, call)
		case "tool_search_output":
			discovered, err := searchOutputTools(item)
			if err != nil {
				return nil, err
			}
			raw := item["output"]
			if raw == nil || string(raw) == "null" {
				raw = item["tools"]
			}
			if raw == nil {
				return nil, bad("tool search output requires output or tools")
			}
			var text string
			if json.Unmarshal(raw, &text) != nil {
				text = string(raw)
			}
			out.Messages = append(out.Messages, convertedChatMessage{Role: "tool", CallID: credentialString(item, "call_id"), Content: text, Discoveries: discovered})
		case "function_call", "custom_tool_call":
			name, callID := credentialString(item, "name"), credentialString(item, "call_id")
			if name == "" || callID == "" {
				return nil, bad("tool call name and call ID required")
			}
			call := convertedChatTool{ID: callID, Type: "function"}
			call.Function.Name = name
			call.Namespace = credentialString(item, "namespace")
			if call.Namespace != "" && kind != "function_call" {
				return nil, bad("namespace requires a function call")
			}
			if kind == "custom_tool_call" {
				var input string
				if json.Unmarshal(item["input"], &input) != nil {
					return nil, bad("invalid custom tool input")
				}
				raw, _ := json.Marshal(map[string]string{"input": input})
				call.Function.Arguments = string(raw)
				if !out.Declared[name] {
					out.Custom[name] = true
				}
			} else {
				call.Function.Arguments = credentialString(item, "arguments")
				if call.Function.Arguments == "" {
					call.Function.Arguments = "{}"
				}
				if !json.Valid([]byte(call.Function.Arguments)) {
					return nil, bad("function arguments must be JSON")
				}
			}
			msg := appendAssistant()
			msg.Calls = append(msg.Calls, call)
		case "function_call_output", "custom_tool_call_output":
			callID := credentialString(item, "call_id")
			if callID == "" {
				return nil, bad("tool output requires a call ID")
			}
			content, err := responsesChatContent(item["output"], "tool")
			if err != nil {
				return nil, err
			}
			out.Messages = append(out.Messages, convertedChatMessage{Role: "tool", CallID: callID, Content: content})
		case "", "message":
			role := credentialString(item, "role")
			if role != "user" && role != "assistant" && role != "system" && role != "developer" {
				return nil, bad("invalid input message role")
			}
			content, err := responsesChatContent(item["content"], role)
			if err != nil {
				return nil, err
			}
			if role == "assistant" {
				msg := appendAssistant()
				if msg.Content == nil {
					msg.Content = content
				} else {
					msg.Content = msg.Content.(string) + content.(string)
				}
			} else {
				out.Messages = append(out.Messages, convertedChatMessage{Role: role, Content: content})
				reasoning = ""
			}
		default:
			return nil, bad("input item requires a native Responses account")
		}
	}
	if len(out.Messages) == 0 {
		return nil, bad("input is required")
	}
	// Only top-level instructions are replaced on continuation. Input messages,
	// including their system/developer roles, remain part of the conversation.
	out.History = append([]convertedChatMessage(nil), out.Messages[historyStart:]...)
	// History retains original tool identities. Only wire messages use flat names.
	for _, message := range out.Messages {
		for _, call := range message.Calls {
			if call.Namespace != "" {
				if _, err := out.registerNamespace(call.Namespace, call.Function.Name); err != nil {
					return nil, err
				}
			}
		}
	}
	for i := range out.Messages {
		out.Messages[i].Discoveries = nil
		out.Messages[i].Calls = append([]convertedChatTool(nil), out.Messages[i].Calls...)
		for j := range out.Messages[i].Calls {
			call := &out.Messages[i].Calls[j]
			if call.Search && out.Direct["tool_search"] || !call.Search && call.Namespace == "" && call.Function.Name == "tool_search" && out.ToolSearch {
				return nil, bad("tool search conflicts with history tool identity")
			}
			call.Search = false
			if call.Namespace != "" {
				call.Function.Name, call.Namespace = flattenedToolName(call.Namespace, call.Function.Name), ""
			} else if _, exists := out.Namespaces[call.Function.Name]; exists {
				return nil, bad("history tool identity collides with a namespace")
			}
		}
	}
	// Leading instruction items become one system message. Later instruction items
	// retain their position as user messages for strict Chat-compatible upstreams.
	leads := 0
	instructions := []string{}
	for leads < len(out.Messages) && (out.Messages[leads].Role == "system" || out.Messages[leads].Role == "developer") {
		text, ok := out.Messages[leads].Content.(string)
		if !ok {
			return nil, bad("Chat instruction content must be text")
		}
		instructions = append(instructions, text)
		leads++
	}
	if leads > 0 {
		out.Messages = append([]convertedChatMessage{{Role: "system", Content: strings.Join(instructions, "\n\n")}}, out.Messages[leads:]...)
	}
	for i := 1; i < len(out.Messages); i++ {
		if out.Messages[i].Role == "system" || out.Messages[i].Role == "developer" {
			out.Messages[i].Role = "user"
		}
	}
	out.Body["messages"] = out.Messages
	return out, nil
}

func (out *responsesChatRequest) prepareTools(body map[string]json.RawMessage, history []convertedChatMessage, keepCache bool) error {
	tools, err := responseClientTools(body)
	if err != nil {
		return err
	}
	// Saved discoveries remain callable in subsequent scoped continuations.
	for _, message := range history {
		if len(message.Discoveries) == 0 {
			continue
		}
		raw, _ := json.Marshal(message.Discoveries)
		callID, _ := json.Marshal(message.CallID)
		tools, err = mergeResponseDiscoveries(tools, []map[string]json.RawMessage{{"type": json.RawMessage(`"tool_search_output"`), "call_id": callID, "tools": raw}})
		if err != nil {
			return err
		}
	}
	// Reserve direct names before flattening so declaration order cannot hide a collision.
	out.Direct = map[string]bool{}
	for _, tool := range tools {
		if kind := credentialString(tool, "type"); kind != "namespace" && kind != "tool_search" {
			out.Direct[credentialString(tool, "name")] = true
		}
	}
	expanded := []map[string]json.RawMessage{}
	namespacedDefinitions := map[string]string{}
	for _, tool := range tools {
		if credentialString(tool, "type") != "namespace" {
			expanded = append(expanded, tool)
			continue
		}
		children, err := responseNamespaceChildren(tool)
		if err != nil {
			return err
		}
		for _, child := range children {
			flat, err := out.registerNamespace(credentialString(tool, "name"), credentialString(child, "name"))
			if err != nil {
				return err
			}
			definition := responseToolDefinition(child)
			if previous, exists := namespacedDefinitions[flat]; exists {
				if previous != definition {
					return bad("conflicting namespaced tool definitions")
				}
				continue
			}
			namespacedDefinitions[flat] = definition
			child["name"], _ = json.Marshal(flat)
			expanded = append(expanded, child)
		}
	}
	convertedTools := []any{}
	out.Declared = map[string]bool{}
	searchDefinition := ""
	for _, tool := range expanded {
		kind, name := credentialString(tool, "type"), credentialString(tool, "name")
		if kind == "tool_search" {
			if err := validateResponseClientTool(tool); err != nil {
				return err
			}
			name = "tool_search"
			if out.Direct[name] {
				return bad("tool search conflicts with a declared tool name")
			}
			definition := responseToolDefinition(tool)
			if out.ToolSearch {
				if definition != searchDefinition {
					return bad("conflicting tool search definitions")
				}
				continue
			}
			out.ToolSearch, searchDefinition = true, definition
		}
		if name == "" || len(name) > 256 || out.Declared[name] {
			return bad("invalid or duplicate tool name")
		}
		out.Declared[name] = true
		function := map[string]any{"name": name}
		switch kind {
		case "tool_search":
			function["description"] = "Find client tools for the current task."
			function["parameters"] = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`)
			for _, field := range []string{"description", "parameters", "strict"} {
				if raw := tool[field]; raw != nil {
					function[field] = raw
				}
			}
		case "function":
			for _, field := range []string{"description", "parameters", "strict"} {
				if raw := tool[field]; raw != nil {
					function[field] = raw
				}
			}
		case "custom":
			out.Custom[name] = true
			function["description"] = credentialString(tool, "description") + " Return the complete custom tool input in the input string."
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]string{"type": "string"}}, "required": []string{"input"}, "additionalProperties": false}
			function["strict"] = true
		default:
			return bad("this tool requires a native Responses account")
		}
		if keepCache && tool["cache_control"] != nil {
			function["cache_control"] = tool["cache_control"]
		}
		convertedTools = append(convertedTools, map[string]any{"type": "function", "function": function})
	}
	if len(convertedTools) > 0 {
		out.Body["tools"] = convertedTools
	}
	if raw := body["tool_choice"]; raw != nil && string(raw) != "null" {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			if name != "auto" && name != "none" && name != "required" {
				return bad("invalid tool choice")
			}
			out.Body["tool_choice"] = name
		} else {
			var choice map[string]json.RawMessage
			if json.Unmarshal(raw, &choice) != nil {
				return bad("invalid tool choice")
			}
			kind, name := credentialString(choice, "type"), credentialString(choice, "name")
			if kind == "tool_search" {
				if !out.ToolSearch {
					return bad("tool choice requires declared client tool search")
				}
				kind, name = "function", "tool_search"
			}
			if namespace := credentialString(choice, "namespace"); namespace != "" {
				flat := flattenedToolName(namespace, name)
				if kind != "function" || out.Namespaces[flat] != (responseToolName{namespace, name}) {
					return bad("tool choice must name a declared namespace function")
				}
				name = flat
			}
			if (kind != "function" && kind != "custom") || !out.Declared[name] {
				return bad("tool choice must name a declared tool")
			}
			out.Body["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": name}}
		}
	}
	return nil
}

func (out *responsesChatRequest) registerNamespace(namespace, name string) (string, error) {
	if namespace == "" || name == "" || len(namespace) > 256 || len(name) > 256 {
		return "", bad("invalid namespaced tool")
	}
	flat := flattenedToolName(namespace, name)
	owner := responseToolName{namespace, name}
	if previous, exists := out.Namespaces[flat]; out.Direct[flat] || exists && previous != owner {
		return "", bad("namespaced tool collides with another tool")
	}
	out.Namespaces[flat] = owner
	return flat, nil
}

func responsesChatContent(raw json.RawMessage, role string) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, bad("invalid Responses content")
	}
	out := []any{}
	var plain strings.Builder
	for _, part := range parts {
		switch credentialString(part, "type") {
		case "input_text", "output_text", "text":
			var v string
			if json.Unmarshal(part["text"], &v) != nil {
				return nil, bad("invalid text part")
			}
			plain.WriteString(v)
			out = append(out, map[string]string{"type": "text", "text": v})
		case "refusal":
			plain.WriteString(credentialString(part, "refusal"))
		case "input_image":
			if role != "user" {
				return nil, bad("image requires a user message")
			}
			u := credentialString(part, "image_url")
			if u == "" {
				return nil, bad("Chat image input requires image_url")
			}
			image := map[string]string{"url": u}
			if detail := credentialString(part, "detail"); detail != "" {
				image["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		case "input_file":
			if role != "user" {
				return nil, bad("file requires a user message")
			}
			file := map[string]json.RawMessage{}
			for _, key := range []string{"filename", "file_data", "file_id"} {
				if v := part[key]; v != nil {
					file[key] = v
				}
			}
			if file["file_data"] == nil && file["file_id"] == nil {
				return nil, bad("Chat file input requires file data or ID")
			}
			out = append(out, map[string]any{"type": "file", "file": file})
		default:
			return nil, bad("content part cannot be converted to Chat")
		}
	}
	if role != "user" {
		return plain.String(), nil
	}
	return out, nil
}

type chatResponsePart struct {
	Kind, ID, Text, CallID, Name, Arguments string
	ToolIndex                               int
	Announced                               bool
}

type chatResponsesStream struct {
	ID, Model, Tier, UpstreamID, Finish     string
	Created                                 int64
	Sequence, Size                          int
	Started                                 bool
	Quiet                                   bool
	Parts                                   []*chatResponsePart
	Tools                                   map[int]int
	TextIndex, ReasoningIndex, RefusalIndex int
	Custom                                  map[string]bool
	Namespaces                              map[string]responseToolName
	ToolSearch                              bool
	Usage                                   map[string]json.RawMessage
}

// ponytail: accumulated output is capped at 16 MiB/4096 items; use per-item
// buffers if profiling shows string copying dominates large stream handling.
func newChatResponsesStream(model string, custom map[string]bool) *chatResponsesStream {
	return &chatResponsesStream{ID: "resp_lite_" + randomToken(18), Model: model, Created: time.Now().Unix(), Tools: map[int]int{}, TextIndex: -1, ReasoningIndex: -1, RefusalIndex: -1, Custom: custom}
}

func (s *chatResponsesStream) emit(kind string, fields map[string]any) string {
	if s.Quiet {
		return ""
	}
	fields["type"], fields["sequence_number"] = kind, s.Sequence
	s.Sequence++
	raw, _ := json.Marshal(fields)
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}

func (s *chatResponsesStream) result(status string, output []any) map[string]any {
	v := map[string]any{"id": s.ID, "object": "response", "created_at": s.Created, "model": s.Model, "status": status, "output": output, "error": nil, "incomplete_details": nil, "usage": nil}
	if s.Tier != "" {
		v["service_tier"] = s.Tier
	}
	if status != "in_progress" {
		v["usage"] = s.Usage
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if s.Finish == "content_filter" {
			reason = "content_filter"
		}
		v["incomplete_details"] = map[string]string{"reason": reason}
	}
	return v
}

func (s *chatResponsesStream) item(p *chatResponsePart, full bool) (map[string]any, error) {
	status := "in_progress"
	value := ""
	if full {
		status = "completed"
		value = p.Text
	}
	v := map[string]any{"id": p.ID, "type": p.Kind, "status": status}
	switch p.Kind {
	case "message":
		v["role"] = "assistant"
		v["content"] = []any{}
		if full {
			v["content"] = []any{map[string]any{"type": "output_text", "text": value, "annotations": []any{}}}
		}
	case "refusal":
		v["type"], v["role"] = "message", "assistant"
		v["content"] = []any{}
		if full {
			v["content"] = []any{map[string]string{"type": "refusal", "refusal": value}}
		}
	case "reasoning":
		delete(v, "status")
		v["summary"] = []any{}
		if full {
			v["summary"] = []any{map[string]string{"type": "summary_text", "text": value}}
		}
	case "function_call":
		if p.CallID == "" || p.Name == "" {
			return nil, &apiError{502, "upstream tool identity is missing"}
		}
		v["call_id"], v["name"], v["arguments"] = p.CallID, p.Name, ""
		if name, ok := s.Namespaces[p.Name]; ok {
			v["name"], v["namespace"] = name.Name, name.Namespace
		}
		if s.Custom[p.Name] {
			v["type"], v["input"] = "custom_tool_call", ""
			delete(v, "arguments")
		}
		if s.ToolSearch && p.Name == "tool_search" {
			v["type"], v["execution"], v["arguments"] = "tool_search_call", "client", json.RawMessage(`{}`)
			delete(v, "name")
		}
		if full {
			args := p.Arguments
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, &apiError{502, "upstream tool arguments are incomplete"}
			}
			if s.ToolSearch && p.Name == "tool_search" {
				arguments, err := searchCallArguments([]byte(args))
				if err != nil {
					return nil, &apiError{502, "upstream tool search arguments are not an object"}
				}
				v["arguments"] = json.RawMessage(arguments)
			} else if s.Custom[p.Name] {
				var input struct {
					Input *string `json:"input"`
				}
				if json.Unmarshal([]byte(args), &input) != nil || input.Input == nil {
					return nil, &apiError{502, "invalid custom tool arguments"}
				}
				v["input"] = *input.Input
			} else {
				v["arguments"] = args
			}
		}
	}
	return v, nil
}

func (s *chatResponsesStream) add(kind string) int {
	s.Parts = append(s.Parts, &chatResponsePart{Kind: kind, ID: "item_" + randomToken(12)})
	return len(s.Parts) - 1
}

func (s *chatResponsesStream) start() string {
	if s.Started {
		return ""
	}
	s.Started = true
	return s.emit("response.created", map[string]any{"response": s.result("in_progress", []any{})}) + s.emit("response.in_progress", map[string]any{"response": s.result("in_progress", []any{})})
}

func (s *chatResponsesStream) appendText(kind, value string) string {
	if value == "" {
		return ""
	}
	index := &s.TextIndex
	partType, field, event, indexField := "output_text", "text", "output_text", "content_index"
	if kind == "reasoning" {
		index = &s.ReasoningIndex
		partType, event, indexField = "summary_text", "reasoning_summary_text", "summary_index"
	}
	if kind == "refusal" {
		index = &s.RefusalIndex
		partType, field, event = "refusal", "refusal", "refusal"
	}
	wire := ""
	if *index < 0 {
		*index = s.add(kind)
		p := s.Parts[*index]
		item, _ := s.item(p, false)
		wire += s.emit("response.output_item.added", map[string]any{"output_index": *index, "item": item})
		name := "response.content_part.added"
		if kind == "reasoning" {
			name = "response.reasoning_summary_part.added"
		}
		wire += s.emit(name, map[string]any{"output_index": *index, "item_id": p.ID, indexField: 0, "part": map[string]any{"type": partType, field: ""}})
	}
	p := s.Parts[*index]
	p.Text += value
	wire += s.emit("response."+event+".delta", map[string]any{"output_index": *index, "item_id": p.ID, indexField: 0, "delta": value})
	return wire
}

func (s *chatResponsesStream) observe(raw []byte, stream bool) (string, error) {
	var response struct {
		ID      string
		Created int64
		Tier    string `json:"service_tier"`
		Usage   map[string]json.RawMessage
		Choices []struct {
			Index   int
			Finish  *string `json:"finish_reason"`
			Message struct {
				Role           string
				Content        *string
				Reasoning      string `json:"reasoning_content"`
				ReasoningAlias string `json:"reasoning"`
				Refusal        string
				Calls          []convertedChatTool `json:"tool_calls"`
			}
			Delta struct {
				Role           string
				Content        *string
				Reasoning      string `json:"reasoning_content"`
				ReasoningAlias string `json:"reasoning"`
				Refusal        string
				Calls          []convertedChatTool `json:"tool_calls"`
			}
		}
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Choices) > 1 {
		return "", &apiError{502, "invalid Chat response for Responses conversion"}
	}
	if response.ID != "" {
		if s.UpstreamID != "" && s.UpstreamID != response.ID {
			return "", &apiError{502, "upstream Chat ID changed"}
		}
		s.UpstreamID = response.ID
	}
	if response.Created > 0 && !s.Started {
		s.Created = response.Created
	}
	if response.Tier != "" {
		s.Tier = response.Tier
	}
	if response.Usage != nil {
		s.Usage = map[string]json.RawMessage{}
		for k, v := range response.Usage {
			switch k {
			case "prompt_tokens":
				k = "input_tokens"
			case "completion_tokens":
				k = "output_tokens"
			case "prompt_tokens_details":
				k = "input_tokens_details"
			case "completion_tokens_details":
				k = "output_tokens_details"
			}
			s.Usage[k] = v
		}
	}
	wire := s.start()
	if len(response.Choices) == 0 {
		if !stream {
			return "", &apiError{502, "Chat response has no choice"}
		}
		return wire, nil
	}
	c := response.Choices[0]
	if c.Index != 0 {
		return "", &apiError{502, "unexpected Chat choice index"}
	}
	d := c.Delta
	if !stream {
		d = c.Message
	}
	if s.Finish != "" && (d.Content != nil && *d.Content != "" || d.Reasoning != "" || d.ReasoningAlias != "" || d.Refusal != "" || len(d.Calls) > 0) {
		return "", &apiError{502, "Chat content arrived after finish"}
	}
	reasoning := d.Reasoning
	if reasoning == "" {
		reasoning = d.ReasoningAlias
	}
	s.Size += len(reasoning) + len(d.Refusal)
	if d.Content != nil {
		s.Size += len(*d.Content)
	}
	for _, call := range d.Calls {
		s.Size += len(call.Function.Arguments) + len(call.ID) + len(call.Function.Name)
	}
	if s.Size > 16<<20 || len(s.Parts)+len(d.Calls) > 4096 {
		return "", &apiError{502, "converted response exceeds limit"}
	}
	wire += s.appendText("reasoning", reasoning)
	if d.Content != nil {
		wire += s.appendText("message", *d.Content)
	}
	wire += s.appendText("refusal", d.Refusal)
	for i, call := range d.Calls {
		index := call.Index
		if !stream {
			index = i
		}
		if index < 0 || index >= 4096 || len(call.ID) > 256 || len(call.Function.Name) > 256 || call.Type != "" && call.Type != "function" {
			return "", &apiError{502, "invalid Chat tool call"}
		}
		output, exists := s.Tools[index]
		if !exists {
			output = s.add("function_call")
			s.Tools[index] = output
		}
		p := s.Parts[output]
		if call.ID != "" {
			if p.CallID != "" && p.CallID != call.ID {
				return "", &apiError{502, "Chat tool ID changed"}
			}
			// ponytail: at most 4096 bounded items; index IDs if this scan profiles hot.
			for _, other := range s.Parts {
				if other != p && other.CallID == call.ID {
					return "", &apiError{502, "duplicate Chat tool ID"}
				}
			}
			p.CallID = call.ID
		}
		if call.Function.Name != "" {
			if p.Name != "" && p.Name != call.Function.Name {
				return "", &apiError{502, "Chat tool name changed"}
			}
			p.Name = call.Function.Name
		}
		p.Arguments += call.Function.Arguments
		if p.CallID == "" || p.Name == "" {
			continue
		}
		args := call.Function.Arguments
		if !p.Announced {
			if s.ToolSearch && p.Name == "tool_search" {
				p.ID = "tsc_" + randomToken(12)
			}
			item, _ := s.item(p, false)
			wire += s.emit("response.output_item.added", map[string]any{"output_index": output, "item": item})
			p.Announced = true
			args = p.Arguments
		}
		if args != "" && !s.Custom[p.Name] && !(s.ToolSearch && p.Name == "tool_search") {
			wire += s.emit("response.function_call_arguments.delta", map[string]any{"output_index": output, "item_id": p.ID, "delta": args})
		}
	}
	if c.Finish != nil && *c.Finish != "" {
		switch *c.Finish {
		case "stop", "tool_calls", "length", "content_filter":
		default:
			return "", &apiError{502, "unknown Chat finish reason"}
		}
		if s.Finish != "" && s.Finish != *c.Finish {
			return "", &apiError{502, "Chat finish reason changed"}
		}
		s.Finish = *c.Finish
	}
	if len(s.Parts) > 4096 {
		return "", &apiError{502, "converted response exceeds limit"}
	}
	return wire, nil
}

func (s *chatResponsesStream) finish() (string, []byte, error) {
	if s.Finish == "" {
		return "", nil, &apiError{502, "Chat stream ended without finish reason"}
	}
	status := "completed"
	if s.Finish == "length" || s.Finish == "content_filter" {
		status = "incomplete"
	}
	output := []any{}
	wire := s.start()
	if len(s.Parts) == 0 {
		wire += s.appendText("message", "")
		s.TextIndex = s.add("message")
		item, _ := s.item(s.Parts[s.TextIndex], false)
		wire += s.emit("response.output_item.added", map[string]any{"output_index": s.TextIndex, "item": item})
		wire += s.emit("response.content_part.added", map[string]any{"output_index": s.TextIndex, "item_id": s.Parts[s.TextIndex].ID, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	}
	for index, p := range s.Parts {
		item, err := s.item(p, true)
		if err != nil {
			return "", nil, err
		}
		output = append(output, item)
		fields := map[string]any{"output_index": index, "item_id": p.ID}
		switch p.Kind {
		case "function_call":
			if s.ToolSearch && p.Name == "tool_search" {
				break
			}
			event, field, value := "function_call_arguments", "arguments", item["arguments"]
			if s.Custom[p.Name] {
				event, field, value = "custom_tool_call_input", "input", item["input"]
				wire += s.emit("response."+event+".delta", map[string]any{"output_index": index, "item_id": p.ID, "delta": value})
			}
			fields[field] = value
			wire += s.emit("response."+event+".done", fields)
		default:
			event, field, partType, indexField := "output_text", "text", "output_text", "content_index"
			if p.Kind == "reasoning" {
				event, partType, indexField = "reasoning_summary_text", "summary_text", "summary_index"
			}
			if p.Kind == "refusal" {
				event, field, partType = "refusal", "refusal", "refusal"
			}
			fields[indexField], fields[field] = 0, p.Text
			wire += s.emit("response."+event+".done", fields)
			part := map[string]any{"type": partType, field: p.Text}
			if partType == "output_text" {
				part["annotations"] = []any{}
			}
			name := "response.content_part.done"
			if p.Kind == "reasoning" {
				name = "response.reasoning_summary_part.done"
			}
			wire += s.emit(name, map[string]any{"output_index": index, "item_id": p.ID, indexField: 0, "part": part})
		}
		wire += s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	response := s.result(status, output)
	wire += s.emit("response."+status, map[string]any{"response": response})
	raw, err := json.Marshal(response)
	return wire, raw, err
}

func (s *chatResponsesStream) assistant() convertedChatMessage {
	msg := convertedChatMessage{Role: "assistant", Content: ""}
	for _, p := range s.Parts {
		switch p.Kind {
		case "message":
			msg.Content = msg.Content.(string) + p.Text
		case "reasoning":
			msg.Reasoning += p.Text
		case "refusal":
			msg.Refusal += p.Text
		case "function_call":
			call := convertedChatTool{ID: p.CallID, Type: "function"}
			call.Search = s.ToolSearch && p.Name == "tool_search"
			call.Function.Name, call.Function.Arguments = p.Name, p.Arguments
			if name, ok := s.Namespaces[p.Name]; ok {
				call.Function.Name, call.Namespace = name.Name, name.Namespace
			}
			if call.Function.Arguments == "" {
				call.Function.Arguments = "{}"
			}
			msg.Calls = append(msg.Calls, call)
		}
	}
	return msg
}
