package app

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesToolOutputMedia(t *testing.T) {
	parse := func(input string) map[string]json.RawMessage {
		return map[string]json.RawMessage{"model": json.RawMessage(`"gemini-3.1-pro"`), "input": json.RawMessage(input)}
	}
	const image = "data:image/png;base64,YQ=="
	const call = `{"type":"function_call","call_id":"view","name":"view_image","arguments":"{}"}`
	for _, output := range []string{
		`[{"type":"input_image","image_url":"` + image + `"}]`,
		`{"type":"image_url","image_url":{"url":"` + image + `"}}`,
		`{"status":"ok","content":[{"type":"input_image","image_url":"` + image + `"}],"number":9007199254740993}`,
		`"[{\"type\":\"input_image\",\"image_url\":\"` + image + `\"}]"`,
		`"` + image + `"`,
	} {
		request, err := responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":`+output+`}]`), nil)
		if err != nil {
			t.Fatal("image result rejected", output, err)
		}
		if len(request.Messages) != 3 || request.Messages[1].Role != "tool" || request.Messages[2].Role != "user" || strings.Contains(request.Messages[1].Content.(string), image) || !strings.Contains(request.Messages[1].Content.(string), toolMediaMarker) {
			t.Fatal("image not lifted after tool", request.Messages)
		}
		wire, _ := json.Marshal(request.Body)
		if strings.Count(string(wire), image) != 1 || strings.Contains(string(wire), "tool_output_media") || !strings.Contains(string(wire), "Tool output media for call view") || strings.Contains(output, "9007199254740993") && !strings.Contains(string(wire), "9007199254740993") {
			t.Fatal("lost media attribution or numeric data", string(wire))
		}
		// Authenticated history survives JSON persistence and emits the image once again.
		encoded, _ := json.Marshal(request.History)
		var history []convertedChatMessage
		if json.Unmarshal(encoded, &history) != nil {
			t.Fatal("invalid stored history")
		}
		next, err := responsesToChatRequest(parse(`"continue"`), history)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ = json.Marshal(next.Body)
		if strings.Count(string(wire), image) != 1 || strings.Contains(string(wire), "tool_output_media") {
			t.Fatal("lost or duplicated history image", string(wire))
		}
		wire, _, _, err = responsesGeminiRequest(next.Body)
		if err != nil || !strings.Contains(string(wire), `"inlineData":{"data":"YQ==","mimeType":"image/png"}`) || !strings.Contains(string(wire), "Tool output media for call view") || strings.Count(string(wire), `"id":"view"`) != 2 {
			t.Fatal("Gemini lost attributed image", string(wire), err)
		}
	}
	for _, output := range []string{`{"value":9007199254740993}`, `[ { "type": "input_text", "text": "ok" }, {"unknown":true} ]`, `"{ \"ok\": true }"`, `"prefix data:image/png;base64,YQ== suffix"`} {
		request, err := responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":`+output+`}]`), nil)
		var want string
		if json.Unmarshal([]byte(output), &want) != nil {
			want = output
		}
		if err != nil || len(request.Messages) != 2 || request.Messages[1].Content != want {
			t.Fatal("changed media-free output", output, err)
		}
	}
	input := `[{"type":"function_call","call_id":"a","name":"view","arguments":"{}"},{"type":"custom_tool_call","call_id":"b","name":"edit","input":"patch"},{"role":"developer","content":"approval saved"},{"type":"custom_tool_call_output","call_id":"b","output":{"content":[{"type":"image_url","image_url":{"url":"https://example.test/b.png"}}]}},{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"https://example.test/a.png"}]}]`
	request, err := responsesToChatRequest(parse(input), nil)
	if err != nil || len(request.Messages) != 5 || request.Messages[1].CallID != "a" || request.Messages[2].CallID != "b" || request.Messages[3].Role != "user" || request.Messages[4].Content != "approval saved" {
		t.Fatal("parallel media batch interrupted", request, err)
	}
	wire, _ := json.Marshal(request.Messages[3])
	if strings.Index(string(wire), "a.png") > strings.Index(string(wire), "b.png") || !strings.Contains(string(wire), "for call a") || !strings.Contains(string(wire), "for call b") {
		t.Fatal("parallel image attribution", string(wire))
	}
	// The last duplicate result wins, including clearing an earlier image.
	request, err = responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":"`+image+`"},{"type":"function_call_output","call_id":"view","output":"latest"}]`), nil)
	wire, _ = json.Marshal(request.Body)
	if err != nil || len(request.Messages) != 2 || request.Messages[1].Content != "latest" || strings.Contains(string(wire), image) || strings.Contains(string(wire), "tool_output_media") {
		t.Fatal("obsolete media survived replacement", string(wire), err)
	}
	for _, output := range []string{`{"type":"input_image","image_url":"file:///etc/passwd"}`, `{"type":"input_image","image_url":"data:image/png;base64,invalid!"}`, `{"content":[{"type":"input_image","file_id":"file_foreign"}]}`, `{"type":"input_file","file_id":"file_foreign"}`, strings.Repeat(`{"content":`, 66) + `"text"` + strings.Repeat(`}`, 66)} {
		if _, err := responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":`+output+`}]`), nil); err == nil {
			t.Fatal("unsafe or excessive image accepted", output)
		}
	}
	if _, err := responsesToChatRequest(parse(`[{"type":"function_call_output","call_id":"foreign","output":"`+image+`"}]`), nil); err == nil {
		t.Fatal("orphan media accepted")
	}
	for _, part := range []string{
		`{"type":"input_image","image_url":"` + image + `","detail":"high"}`,
		`{"type":"image_url","image_url":{"url":"` + image + `","detail":"high"}}`,
	} {
		request, err := responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":[`+part+`]}]`), nil)
		if err != nil {
			t.Fatal(err)
		}
		wire, _ := json.Marshal(request.Body)
		if !strings.Contains(string(wire), `"detail":"high"`) {
			t.Fatal("image detail lost", string(wire))
		}
	}
	tooMany := `[{"type":"input_image","image_url":"` + image + `"}` + strings.Repeat(`,{"type":"input_image","image_url":"`+image+`"}`, 128) + `]`
	if _, err := responsesToChatRequest(parse(`[`+call+`,{"type":"function_call_output","call_id":"view","output":`+tooMany+`}]`), nil); err == nil {
		t.Fatal("unbounded media accepted")
	}
	duplicateCall := `[` + call + `,` + call + `,{"type":"function_call_output","call_id":"view","output":"` + image + `"}]`
	if _, err := responsesToChatRequest(parse(duplicateCall), nil); err == nil {
		t.Fatal("ambiguous media call accepted")
	}
}
