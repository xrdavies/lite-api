package app

import (
	"encoding/json"
	"strings"
)

// Responses has richer output events than Gemini. Reuse the existing, bounded
// Gemini and Chat translators in sequence instead of creating a second protocol
// implementation.
type responsesGeminiStream struct {
	Native *geminiChatStream
	Output *chatResponsesStream
}

func newResponsesGeminiStream(model string, request *responsesChatRequest) *responsesGeminiStream {
	output := newChatResponsesStream(model, nil)
	if request != nil {
		output.Custom, output.Namespaces, output.ToolSearch = request.Custom, request.Namespaces, request.ToolSearch
	}
	// Keep native calls as functions until the Responses converter restores their
	// original namespace, custom input or client tool-search contract.
	return &responsesGeminiStream{Native: newGeminiChatStream(model, true, nil), Output: output}
}

func (s *responsesGeminiStream) feed(wire string, stream bool) (string, error) {
	var out strings.Builder
	for _, frame := range strings.Split(wire, "\n\n") {
		var data []string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if len(data) == 0 {
			continue
		}
		payload := strings.Join(data, "\n")
		if payload == "" || payload == "[DONE]" {
			continue
		}
		converted, err := s.Output.observe([]byte(payload), stream)
		if err != nil {
			return "", err
		}
		out.WriteString(converted)
	}
	return out.String(), nil
}

func (s *responsesGeminiStream) observe(raw []byte) (string, error) {
	wire, err := s.Native.observe(raw)
	if err != nil {
		return "", err
	}
	return s.feed(wire, true)
}

func (s *responsesGeminiStream) finish(u priceUsage) (string, []byte, error) {
	wire, _, err := s.Native.finish(u)
	if err != nil {
		return "", nil, err
	}
	converted, err := s.feed(wire, true)
	if err != nil {
		return "", nil, err
	}
	terminal, raw, err := s.Output.finish()
	return converted + terminal, raw, err
}

func (s *responsesGeminiStream) response(raw []byte, u priceUsage) ([]byte, error) {
	if _, err := s.observe(raw); err != nil {
		return nil, err
	}
	wire, result, err := s.finish(u)
	if err != nil {
		return nil, err
	}
	_ = wire
	return result, nil
}

func (s *responsesGeminiStream) assistant() convertedChatMessage {
	message := s.Output.assistant()
	metadata := map[string]json.RawMessage{}
	for _, tool := range s.Native.Tools {
		if extra := tool["extra_content"]; extra != nil {
			metadata[tool["id"].(string)], _ = json.Marshal(extra)
		}
	}
	for i := range message.Calls {
		message.Calls[i].ExtraContent = metadata[message.Calls[i].ID]
	}
	return message
}

func responsesGeminiRequest(body map[string]any) ([]byte, map[string]bool, string, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, "", err
	}
	var normalized map[string]json.RawMessage
	if err := json.Unmarshal(raw, &normalized); err != nil {
		return nil, nil, "", err
	}
	return chatToGemini(normalized)
}
