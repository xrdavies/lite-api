package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// A client can replay a reasoning item only to its original tenant and source.
// The token contains encrypted provider state, never the API key or plaintext prompt.
type messagesReasoning struct {
	AccountID int64
	Target    string
	Expires   int64
	Item      json.RawMessage
}

const messagesSignaturePrefix = "lite_reasoning_v1."

func messagesSignatureAAD(g *gatewayIdentity) []byte {
	return []byte(fmt.Sprintf("lite-api/messages-reasoning/v1/%d/%d", g.Key.ID, g.Key.GroupID))
}
func (a *App) sealMessagesReasoning(g *gatewayIdentity, u *upstreamAccount, item json.RawMessage) (string, error) {
	raw, err := json.Marshal(messagesReasoning{AccountID: u.ID, Target: responseTarget(u), Expires: time.Now().Add(30 * 24 * time.Hour).Unix(), Item: item})
	if err != nil {
		return "", err
	}
	if len(raw) > 1<<20 {
		return "", &apiError{502, "reasoning state exceeds limit"}
	}
	cipher := a.chatHistoryCipher()
	nonce := make([]byte, cipher.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return messagesSignaturePrefix + base64.RawURLEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, messagesSignatureAAD(g))), nil
}
func (a *App) messagesReasoningInput(g *gatewayIdentity, body map[string]json.RawMessage) (map[string]json.RawMessage, *responseBinding, error) {
	var messages []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(body["messages"], &messages) != nil {
		return nil, nil, bad("invalid messages")
	}
	saved := map[string]json.RawMessage{}
	var binding *responseBinding
	for _, m := range messages {
		blocks, err := messagesBlocks(m.Content)
		if err != nil {
			return nil, nil, err
		}
		for _, b := range blocks {
			sig := credentialString(b, "signature")
			if !strings.HasPrefix(sig, messagesSignaturePrefix) {
				continue
			}
			if m.Role != "assistant" || credentialString(b, "type") != "thinking" {
				return nil, nil, bad("reasoning signature requires an assistant thinking block")
			}
			if _, ok := saved[sig]; ok {
				continue
			}
			cipher := a.chatHistoryCipher()
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sig, messagesSignaturePrefix))
			if err != nil || len(raw) < cipher.NonceSize() || len(raw) > (1<<20)+64 {
				return nil, nil, bad("invalid reasoning signature")
			}
			plain, err := cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], messagesSignatureAAD(g))
			if err != nil {
				return nil, nil, bad("reasoning signature does not belong to this Key and group")
			}
			var state messagesReasoning
			if json.Unmarshal(plain, &state) != nil || state.AccountID < 1 || state.Target == "" || state.Expires <= time.Now().Unix() {
				return nil, nil, bad("reasoning signature is invalid or expired")
			}
			if binding != nil && (binding.AccountID != state.AccountID || binding.Target != state.Target) {
				return nil, nil, bad("reasoning history spans different upstream sources")
			}
			binding = &responseBinding{AccountID: state.AccountID, Target: state.Target}
			saved[sig] = state.Item
		}
	}
	return saved, binding, nil
}

func messagesToResponses(body map[string]json.RawMessage, saved map[string]json.RawMessage) ([]byte, string, error) {
	options, effort, err := messagesOpenAIOptions(body)
	if err != nil {
		return nil, "", err
	}
	if options["stop"] != nil {
		return nil, "", bad("stop_sequences cannot be converted to Responses")
	}
	out := map[string]any{"model": options["model"], "stream": options["stream"], "store": false, "max_output_tokens": options["max_completion_tokens"], "include": []string{"reasoning.encrypted_content"}}
	for _, key := range []string{"temperature", "top_p", "service_tier", "parallel_tool_calls"} {
		if options[key] != nil {
			out[key] = options[key]
		}
	}
	if effort != "" {
		out["reasoning"] = map[string]any{"effort": effort, "summary": "auto"}
	}
	if f, ok := options["response_format"].(map[string]any); ok {
		format := f["json_schema"].(map[string]any)
		format["type"] = "json_schema"
		out["text"] = map[string]any{"format": format}
	}
	if tools, ok := options["tools"].([]any); ok {
		converted := []any{}
		for _, tool := range tools {
			f := tool.(map[string]any)["function"].(map[string]any)
			f["type"] = "function"
			converted = append(converted, f)
		}
		out["tools"] = converted
	}
	if choice := options["tool_choice"]; choice != nil {
		if c, ok := choice.(map[string]any); ok {
			out["tool_choice"] = map[string]string{"type": "function", "name": c["function"].(map[string]string)["name"]}
		} else {
			out["tool_choice"] = choice
		}
	}
	input := []any{}
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
			if strings.HasPrefix(credentialString(b, "text"), "x-anthropic-billing-header: ") {
				continue
			}
			part, err := messagesResponsesPart(b, "developer")
			if err != nil {
				return nil, "", err
			}
			parts = append(parts, part)
		}
		if len(parts) > 0 {
			input = append(input, map[string]any{"type": "message", "role": "developer", "content": parts})
		}
	}
	var messages []struct {
		Role    string
		Content json.RawMessage
	}
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return nil, "", bad("messages are required")
	}
	calls := map[string]bool{}
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
		flush := func() {
			if len(parts) > 0 {
				input = append(input, map[string]any{"type": "message", "role": m.Role, "content": parts})
				parts = nil
			}
		}
		for _, b := range blocks {
			switch credentialString(b, "type") {
			case "text", "image", "document":
				p, err := messagesResponsesPart(b, m.Role)
				if err != nil {
					return nil, "", err
				}
				parts = append(parts, p)
			case "thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires an assistant role")
				}
				flush()
				sig := credentialString(b, "signature")
				if item := saved[sig]; item != nil {
					if replayed[sig] {
						return nil, "", bad("duplicate reasoning history item")
					}
					replayed[sig] = true
					input = append(input, item)
				}
			case "redacted_thinking":
				if m.Role != "assistant" {
					return nil, "", bad("thinking requires an assistant role")
				}
			case "tool_use":
				flush()
				id, name := credentialString(b, "id"), credentialString(b, "name")
				var args map[string]json.RawMessage
				if m.Role != "assistant" || id == "" || name == "" || len(id) > 256 || len(name) > 256 || calls[id] || json.Unmarshal(b["input"], &args) != nil || args == nil {
					return nil, "", bad("invalid or duplicate tool_use")
				}
				calls[id] = true
				input = append(input, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(b["input"])})
			case "tool_result":
				flush()
				id := credentialString(b, "tool_use_id")
				if m.Role != "user" || id == "" || len(id) > 256 {
					return nil, "", bad("invalid tool_result")
				}
				result := []any{}
				failed := false
				if raw := b["is_error"]; raw != nil && json.Unmarshal(raw, &failed) != nil {
					return nil, "", bad("invalid tool result error flag")
				}
				if failed {
					result = append(result, map[string]string{"type": "input_text", "text": "Tool error:"})
				}
				if raw := b["content"]; raw != nil && string(raw) != "null" {
					inner, err := messagesBlocks(raw)
					if err != nil {
						return nil, "", err
					}
					for _, p := range inner {
						part, err := messagesResponsesPart(p, "user")
						if err != nil {
							return nil, "", err
						}
						result = append(result, part)
					}
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": result})
			default:
				return nil, "", bad("content requires a native Messages account")
			}
		}
		flush()
	}
	if len(input) == 0 {
		return nil, "", bad("messages contain no convertible content")
	}
	out["input"] = input
	raw, err := json.Marshal(out)
	return raw, effort, err
}
func messagesResponsesPart(b map[string]json.RawMessage, role string) (map[string]any, error) {
	part, err := messagesChatPart(b)
	if err != nil {
		return nil, err
	}
	switch part["type"] {
	case "text":
		part["type"] = "input_text"
		if role == "assistant" {
			part["type"] = "output_text"
		}
	case "image_url":
		if role != "user" {
			return nil, bad("image requires a user message")
		}
		part = map[string]any{"type": "input_image", "image_url": part["image_url"].(map[string]string)["url"]}
	case "file":
		if role != "user" {
			return nil, bad("file requires a user message")
		}
		f := part["file"].(map[string]string)
		part = map[string]any{"type": "input_file", "filename": f["filename"], "file_data": f["file_data"]}
	}
	return part, nil
}

type messagesResponseItem struct {
	ID, Type, Name, Arguments string
	CallID                    string `json:"call_id"`
	Encrypted                 string `json:"encrypted_content"`
	Content                   []struct {
		Type    string `json:"type"`
		Text    string `json:"text,omitempty"`
		Refusal string `json:"refusal,omitempty"`
	}
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
}
type messagesResponseBody struct {
	ID, Status string
	Output     []messagesResponseItem
	Incomplete struct{ Reason string } `json:"incomplete_details"`
}
type messagesResponseSlot struct {
	Item messagesResponseItem
	Text map[string]string
	Done bool
}

// ponytail: at most 16 MiB/4096 parts are accumulated; replace per-part strings
// with builders if long-stream profiling shows copying is a bottleneck.
type responsesMessagesStream struct {
	Model, ID           string
	Slots               map[int]*messagesResponseSlot
	IDs                 map[string]int
	Blocks              map[int][]map[string]any
	Started, Stopped    bool
	Current, BlockIndex int
	Open                bool
	OpenKey             string
	Sent                map[string]string
	Size, Parts         int
	Seal                func(json.RawMessage) (string, error)
}

func newResponsesMessagesStream(model string, seal func(json.RawMessage) (string, error)) *responsesMessagesStream {
	return &responsesMessagesStream{Model: model, ID: "msg_lite_" + randomToken(18), Slots: map[int]*messagesResponseSlot{}, IDs: map[string]int{}, Blocks: map[int][]map[string]any{}, Sent: map[string]string{}, Seal: seal}
}
func (s *responsesMessagesStream) result(content []any, reason any, u priceUsage) map[string]any {
	return map[string]any{"id": s.ID, "type": "message", "role": "assistant", "model": s.Model, "content": content, "stop_reason": reason, "stop_sequence": nil, "usage": messagesUsage(u)}
}
func (s *responsesMessagesStream) closeBlock() string {
	if !s.Open {
		return ""
	}
	s.Open = false
	wire := messagesEvent("content_block_stop", map[string]any{"index": s.BlockIndex})
	s.BlockIndex++
	return wire
}
func (s *responsesMessagesStream) text(key, value, kind string) string {
	before := s.Sent[key]
	delta := strings.TrimPrefix(value, before)
	s.Sent[key] = value
	if delta == "" {
		return ""
	}
	wire := ""
	if !s.Open || s.OpenKey != key {
		wire = s.closeBlock()
		s.Open = true
		s.OpenKey = key
		wire += messagesEvent("content_block_start", map[string]any{"index": s.BlockIndex, "content_block": map[string]string{"type": kind, kind: ""}})
	}
	return wire + messagesEvent("content_block_delta", map[string]any{"index": s.BlockIndex, "delta": map[string]string{"type": kind + "_delta", kind: delta}})
}
func (s *responsesMessagesStream) fullBlock(block map[string]any) string {
	wire := s.closeBlock()
	index := s.BlockIndex
	s.BlockIndex++
	start := map[string]any{"type": block["type"]}
	delta := map[string]any{}
	switch block["type"] {
	case "thinking":
		start["thinking"] = ""
		delta["type"], delta["thinking"] = "thinking_delta", block["thinking"]
	case "tool_use":
		start["id"], start["name"], start["input"] = block["id"], block["name"], map[string]any{}
		delta["type"], delta["partial_json"] = "input_json_delta", string(block["input"].(json.RawMessage))
	}
	wire += messagesEvent("content_block_start", map[string]any{"index": index, "content_block": start}) + messagesEvent("content_block_delta", map[string]any{"index": index, "delta": delta})
	if sig, ok := block["signature"].(string); ok && sig != "" {
		wire += messagesEvent("content_block_delta", map[string]any{"index": index, "delta": map[string]string{"type": "signature_delta", "signature": sig}})
	}
	return wire + messagesEvent("content_block_stop", map[string]any{"index": index})
}
func (s *responsesMessagesStream) put(index int, item messagesResponseItem, done bool) error {
	if index < 0 || index >= 4096 || item.ID == "" || len(item.ID) > 256 {
		return &apiError{502, "invalid Responses item identity"}
	}
	switch item.Type {
	case "message", "reasoning", "function_call":
	default:
		return &apiError{502, "unsupported Responses item for Messages"}
	}
	if old, ok := s.IDs[item.ID]; ok && old != index {
		return &apiError{502, "duplicate Responses item ID"}
	}
	s.IDs[item.ID] = index
	slot := s.Slots[index]
	if slot == nil {
		slot = &messagesResponseSlot{Text: map[string]string{}}
		s.Slots[index] = slot
	} else if slot.Item.ID != item.ID || slot.Item.Type != item.Type || slot.Item.Name != item.Name || slot.Item.CallID != item.CallID {
		return &apiError{502, "Responses item identity changed"}
	}
	if slot.Item.Encrypted != "" && item.Encrypted != "" && slot.Item.Encrypted != item.Encrypted {
		return &apiError{502, "reasoning ciphertext changed"}
	}
	if item.Encrypted != "" && slot.Item.Encrypted == "" {
		s.Size += len(item.Encrypted)
	}
	if slot.Done {
		before, _ := json.Marshal(slot.Item)
		after, _ := json.Marshal(item)
		if string(before) != string(after) {
			return &apiError{502, "completed Responses item changed"}
		}
	}
	set := func(key, value string) error {
		if _, ok := slot.Text[key]; !ok {
			s.Parts++
		}
		old := slot.Text[key]
		if !strings.HasPrefix(value, old) {
			return &apiError{502, "Responses content changed"}
		}
		s.Size += len(value) - len(old)
		slot.Text[key] = value
		return nil
	}
	if item.Type == "function_call" {
		if err := set("args", item.Arguments); err != nil {
			return err
		}
	}
	for i, p := range item.Content {
		if !(item.Type == "message" && (p.Type == "output_text" || p.Type == "refusal") || item.Type == "reasoning" && p.Type == "reasoning_text") {
			return &apiError{502, "unsupported Responses content"}
		}
		value := p.Text
		if p.Type == "refusal" {
			value = p.Refusal
		}
		if err := set(fmt.Sprintf("%s:%d", p.Type, i), value); err != nil {
			return err
		}
	}
	for i, p := range item.Summary {
		if p.Type != "summary_text" {
			return &apiError{502, "unsupported reasoning summary"}
		}
		if err := set(fmt.Sprintf("summary:%d", i), p.Text); err != nil {
			return err
		}
	}
	if s.Size > 16<<20 || s.Parts > 4096 {
		return &apiError{502, "converted response exceeds limit"}
	}
	// Terminal snapshots must not omit previously observed parts.
	if done {
		for key := range slot.Text {
			if strings.HasPrefix(key, "output_text:") || strings.HasPrefix(key, "refusal:") || strings.HasPrefix(key, "reasoning_text:") {
				var kind string
				var n int
				kind, tail, _ := strings.Cut(key, ":")
				_, _ = fmt.Sscanf(tail, "%d", &n)
				if n >= len(item.Content) || item.Content[n].Type != kind {
					return &apiError{502, "Responses terminal content is missing"}
				}
			}
			if strings.HasPrefix(key, "summary:") {
				var n int
				_, _ = fmt.Sscanf(key, "summary:%d", &n)
				if n >= len(item.Summary) {
					return &apiError{502, "Responses terminal reasoning is missing"}
				}
			}
		}
	}
	if item.Encrypted == "" {
		item.Encrypted = slot.Item.Encrypted
	}
	slot.Item, slot.Done = item, slot.Done || done
	return nil
}
func (s *responsesMessagesStream) blocks(item messagesResponseItem) ([]map[string]any, error) {
	result := []map[string]any{}
	switch item.Type {
	case "message":
		for _, p := range item.Content {
			value := p.Text
			if p.Type == "refusal" {
				value = p.Refusal
			}
			result = append(result, map[string]any{"type": "text", "text": value})
		}
	case "reasoning":
		var text strings.Builder
		for _, p := range item.Summary {
			text.WriteString(p.Text)
		}
		if text.Len() == 0 {
			for _, p := range item.Content {
				if p.Type == "reasoning_text" {
					text.WriteString(p.Text)
				}
			}
		}
		sig := ""
		if item.Encrypted != "" {
			// Retain the exact replayable item shape, including its provider ID.
			raw, _ := json.Marshal(map[string]any{"id": item.ID, "type": "reasoning", "summary": item.Summary, "encrypted_content": item.Encrypted})
			var err error
			sig, err = s.Seal(raw)
			if err != nil {
				return nil, err
			}
		}
		if text.Len() > 0 || sig != "" {
			result = append(result, map[string]any{"type": "thinking", "thinking": text.String(), "signature": sig})
		}
	case "function_call":
		var args map[string]json.RawMessage
		if item.CallID == "" || item.Name == "" || len(item.CallID) > 256 || len(item.Name) > 256 || json.Unmarshal([]byte(item.Arguments), &args) != nil || args == nil {
			return nil, &apiError{502, "invalid Responses tool call"}
		}
		result = append(result, map[string]any{"type": "tool_use", "id": item.CallID, "name": item.Name, "input": json.RawMessage(item.Arguments)})
	}
	return result, nil
}

// Render items in order. Tool calls and following items wait for settlement.
func (s *responsesMessagesStream) render(final bool) (string, error) {
	wire := ""
	for {
		slot := s.Slots[s.Current]
		if slot == nil {
			return wire, nil
		}
		item := slot.Item
		if item.Type == "function_call" && !final {
			return wire, nil
		}
		if item.Type == "message" {
			for n := 0; n < 4096; n++ {
				key := fmt.Sprintf("output_text:%d", n)
				value, ok := slot.Text[key]
				if !ok {
					key = fmt.Sprintf("refusal:%d", n)
					value, ok = slot.Text[key]
				}
				if !ok {
					break
				}
				wire += s.text(fmt.Sprintf("%d/%s", s.Current, key), value, "text")
			}
		}
		if item.Type == "reasoning" {
			prefix := "summary"
			// Prefer the summary. Raw reasoning is the fallback only once the
			// item is complete, so a late summary cannot replace emitted text.
			if _, ok := slot.Text["summary:0"]; !ok && slot.Done {
				prefix = "reasoning_text"
			}
			var thought strings.Builder
			for i := 0; i < 4096; i++ {
				value, ok := slot.Text[fmt.Sprintf("%s:%d", prefix, i)]
				if !ok {
					break
				}
				thought.WriteString(value)
			}
			wire += s.text(fmt.Sprintf("%d/thinking", s.Current), thought.String(), "thinking")
		}
		if !slot.Done {
			return wire, nil
		}
		if item.Type != "message" {
			blocks, err := s.itemBlocks(s.Current)
			if err != nil {
				return "", err
			}
			for _, b := range blocks {
				if item.Type == "reasoning" && s.Open && s.OpenKey == fmt.Sprintf("%d/thinking", s.Current) {
					if sig, _ := b["signature"].(string); sig != "" {
						wire += messagesEvent("content_block_delta", map[string]any{"index": s.BlockIndex, "delta": map[string]string{"type": "signature_delta", "signature": sig}})
					}
					wire += s.closeBlock()
				} else {
					wire += s.fullBlock(b)
				}
			}
		} else {
			wire += s.closeBlock()
		}
		s.Current++
	}
}
func (s *responsesMessagesStream) itemBlocks(index int) ([]map[string]any, error) {
	if blocks, ok := s.Blocks[index]; ok {
		return blocks, nil
	}
	blocks, err := s.blocks(s.Slots[index].Item)
	if err == nil {
		s.Blocks[index] = blocks
	}
	return blocks, err
}
func (s *responsesMessagesStream) finish(response messagesResponseBody, u priceUsage) (string, []byte, error) {
	if s.Stopped || response.Output == nil || len(response.Output) > 4096 {
		return "", nil, &apiError{502, "invalid Responses terminal output"}
	}
	for i, item := range response.Output {
		if err := s.put(i, item, true); err != nil {
			return "", nil, err
		}
	}
	if len(s.Slots) != len(response.Output) {
		return "", nil, &apiError{502, "Responses terminal output omitted an item"}
	}
	content := []any{}
	calls := map[string]bool{}
	hasTools := false
	for i, item := range response.Output {
		blocks, err := s.itemBlocks(i)
		if err != nil {
			return "", nil, err
		}
		for _, b := range blocks {
			content = append(content, b)
		}
		if item.Type == "function_call" {
			if calls[item.CallID] {
				return "", nil, &apiError{502, "duplicate Responses tool ID"}
			}
			calls[item.CallID] = true
			hasTools = true
		}
	}
	reason := "end_turn"
	if hasTools {
		reason = "tool_use"
	}
	if response.Status == "incomplete" {
		switch response.Incomplete.Reason {
		case "max_output_tokens":
			reason = "max_tokens"
		case "content_filter":
			reason = "refusal"
		default:
			reason = "end_turn"
		}
	} else if response.Status != "completed" {
		return "", nil, &apiError{502, "Responses result is not complete"}
	}
	wire, err := s.render(true)
	if err != nil {
		return "", nil, err
	}
	wire += s.closeBlock() + messagesEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": messagesUsage(u)}) + messagesEvent("message_stop", map[string]any{})
	s.Stopped = true
	raw, err := json.Marshal(s.result(content, reason, u))
	return wire, raw, err
}
func (s *responsesMessagesStream) response(raw []byte, u priceUsage) ([]byte, error) {
	var response messagesResponseBody
	if json.Unmarshal(raw, &response) != nil {
		return nil, &apiError{502, "invalid Responses JSON"}
	}
	_, result, err := s.finish(response, u)
	return result, err
}
func (s *responsesMessagesStream) event(raw []byte, u priceUsage) (string, error) {
	var e struct {
		Type, Delta, Text, Refusal, Arguments string
		ItemID                                string `json:"item_id"`
		OutputIndex                           int    `json:"output_index"`
		ContentIndex                          int    `json:"content_index"`
		SummaryIndex                          int    `json:"summary_index"`
		Item                                  messagesResponseItem
		Response                              *messagesResponseBody
	}
	if json.Unmarshal(raw, &e) != nil || s.Stopped || e.OutputIndex < 0 || e.OutputIndex >= 4096 || e.ContentIndex < 0 || e.ContentIndex >= 4096 || e.SummaryIndex < 0 || e.SummaryIndex >= 4096 {
		return "", &apiError{502, "invalid Responses event"}
	}
	wire := ""
	if !s.Started {
		if e.Response == nil || e.Response.ID == "" {
			return "", &apiError{502, "Responses stream has no start"}
		}
		s.Started = true
		wire = messagesEvent("message_start", map[string]any{"message": s.result([]any{}, nil, priceUsage{})})
	}
	switch e.Type {
	case "response.output_item.added", "response.output_item.done":
		if err := s.put(e.OutputIndex, e.Item, e.Type == "response.output_item.done"); err != nil {
			return "", err
		}
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta", "response.reasoning_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done":
		slot := s.Slots[e.OutputIndex]
		if slot == nil || slot.Done {
			return "", &apiError{502, "Responses delta has no open item"}
		}
		key, kind, value := fmt.Sprintf("output_text:%d", e.ContentIndex), "message", e.Text
		if strings.HasPrefix(e.Type, "response.refusal.") {
			key, value = fmt.Sprintf("refusal:%d", e.ContentIndex), e.Refusal
		}
		if strings.HasPrefix(e.Type, "response.reasoning_summary_text.") {
			key, kind = fmt.Sprintf("summary:%d", e.SummaryIndex), "reasoning"
		}
		if strings.HasPrefix(e.Type, "response.reasoning_text.") {
			key, kind = fmt.Sprintf("reasoning_text:%d", e.ContentIndex), "reasoning"
		}
		if strings.HasPrefix(e.Type, "response.function_call_arguments.") {
			key, kind, value = "args", "function_call", e.Arguments
		}
		if slot.Item.Type != kind || e.ItemID != "" && e.ItemID != slot.Item.ID {
			return "", &apiError{502, "Responses delta type does not match item"}
		}
		if _, ok := slot.Text[key]; !ok {
			s.Parts++
		}
		old := slot.Text[key]
		if strings.HasSuffix(e.Type, ".delta") {
			value = old + e.Delta
		}
		if !strings.HasPrefix(value, old) {
			return "", &apiError{502, "Responses content changed"}
		}
		s.Size += len(value) - len(old)
		slot.Text[key] = value
		if s.Size > 16<<20 || s.Parts > 4096 {
			return "", &apiError{502, "converted stream exceeds limit"}
		}
	case "response.completed", "response.incomplete":
		if e.Response == nil {
			return "", &apiError{502, "Responses terminal output missing"}
		}
		terminal, _, err := s.finish(*e.Response, u)
		return wire + terminal, err
	case "response.failed", "error":
		return "", &apiError{502, "Responses upstream failed"}
	}
	more, err := s.render(false)
	return wire + more, err
}
