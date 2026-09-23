package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiImageCompatibilityConversions(t *testing.T) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"model":"gemini-3-pro-image","modalities":["text","image"],"messages":[{"role":"user","content":"draw"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	wire, _, _, err := chatToGemini(request)
	if err != nil || !strings.Contains(string(wire), `"responseModalities":["TEXT","IMAGE"]`) {
		t.Fatalf("chat image request: %s: %v", wire, err)
	}
	responseRequest, err := responsesToChatRequest(map[string]json.RawMessage{
		"model":      json.RawMessage(`"gemini-3-pro-image"`),
		"input":      json.RawMessage(`"draw"`),
		"modalities": json.RawMessage(`["text","image"]`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	responseWire, _, _, err := responsesGeminiRequest(responseRequest.Body)
	if err != nil || !strings.Contains(string(responseWire), `"responseModalities":["TEXT","IMAGE"]`) {
		t.Fatalf("Responses image request: %s: %v", responseWire, err)
	}

	const image = `{"candidates":[{"content":{"parts":[{"text":"caption"},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]},"finishReason":"STOP"}]}`
	chat := newGeminiChatStream("draw", false, nil)
	if _, err := chat.observe([]byte(image)); err != nil {
		t.Fatal(err)
	}
	_, result, err := chat.finish(priceUsage{})
	if err != nil || !strings.Contains(string(result), "![image](data:image/png;base64,aW1hZ2U=)") {
		t.Fatalf("chat image response: %s: %v", result, err)
	}

	messages := newGeminiMessagesStream("draw", func(json.RawMessage) (string, error) { return "signature", nil })
	if _, err := messages.observe([]byte(image)); err != nil {
		t.Fatal(err)
	}
	_, result, err = messages.finish(priceUsage{})
	if err != nil || !strings.Contains(string(result), "![image](data:image/png;base64,aW1hZ2U=)") {
		t.Fatalf("messages image response: %s: %v", result, err)
	}

	responses := newResponsesGeminiStream("draw", nil)
	if _, err := responses.observe([]byte(image)); err != nil {
		t.Fatal(err)
	}
	_, result, err = responses.finish(priceUsage{})
	if err != nil || !strings.Contains(string(result), "![image](data:image/png;base64,aW1hZ2U=)") {
		t.Fatalf("responses image response: %s: %v", result, err)
	}
}
