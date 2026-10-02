package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestIndependentReasoningUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		output      int64
	}{
		{"separate", `"prompt_tokens":210,"completion_tokens":1,"total_tokens":436,"completion_tokens_details":{"reasoning_tokens":225}`, 226},
		{"smaller", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":35,"completion_tokens_details":{"reasoning_tokens":5}`, 25},
		{"included", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":30,"completion_tokens_details":{"reasoning_tokens":5}`, 20},
		{"no total", `"prompt_tokens":10,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":5}`, 20},
		{"partial gap", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":33,"completion_tokens_details":{"reasoning_tokens":5}`, 23},
		{"bound by reasoning", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":9223372036854775807,"completion_tokens_details":{"reasoning_tokens":5}`, 25},
		{"cache", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":35,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":5}`, 25},
		{"negative total", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":-1`, -1},
		{"negative reasoning", `"prompt_tokens":10,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":-1}`, -1},
		{"unproven reasoning", `"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"completion_tokens_details":{"reasoning_tokens":5}`, -1},
		{"overflow", `"prompt_tokens":10,"completion_tokens":2147483647,"total_tokens":2147483658,"completion_tokens_details":{"reasoning_tokens":1}`, -1},
		{"cache overflow", `"prompt_tokens":10,"completion_tokens":20,"total_tokens":35,"prompt_tokens_details":{"cached_tokens":9,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":5}`, -1},
		{"image boundary", `"prompt_tokens":10,"completion_tokens":1,"total_tokens":16,"completion_tokens_details":{"reasoning_tokens":5,"image_tokens":2}`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, protocol := range []string{"chat_completions", "responses"} {
				usage := "{" + tc.usage + "}"
				raw := `{"usage":` + usage + `}`
				if protocol == "responses" {
					usage = strings.NewReplacer("prompt_tokens", "input_tokens", "completion_tokens", "output_tokens").Replace(usage)
					raw = `{"id":"resp_reasoning","object":"response","status":"completed","output":[],"usage":` + usage + `}`
				}
				for _, envelope := range []bool{false, true} {
					data := raw
					if envelope && protocol == "responses" {
						data = `{"type":"response.completed","response":` + raw + `}`
					}
					o := textObservation{Protocol: protocol}
					err := o.observe([]byte(data))
					if tc.output < 0 {
						if err == nil || o.HasUsage {
							t.Fatalf("%s accepted invalid usage: %+v", protocol, o)
						}
					} else if err != nil || !o.HasUsage || o.Usage.Output != tc.output {
						t.Fatalf("%s output=%d want=%d err=%v", protocol, o.Usage.Output, tc.output, err)
					}
					if tc.name == "cache" && (o.Usage.Input != 5 || o.Usage.CacheRead != 3 || o.Usage.CacheWrite != 2) {
						t.Fatal("cache split changed", o.Usage)
					}
				}
			}
		})
	}
}

func TestMessagesSnapshotWithoutItemID(t *testing.T) {
	message := `{"type":"message","content":[{"type":"output_text","text":"OK"}]}`
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"text", message, true},
		{"multiple", message + "," + message, true},
		{"tool identity", `{"type":"function_call","call_id":"call_a","name":"lookup","arguments":"{}"}`, false},
		{"reasoning identity", `{"type":"reasoning","encrypted_content":"opaque"}`, false},
		{"tool call identity", `{"id":"fc_a","type":"function_call","name":"lookup","arguments":"{}"}`, false},
		{"duplicate supplied identity", strings.Replace(message, `"type"`, `"id":"msg_a","type"`, 1) + "," + strings.Replace(message, `"type"`, `"id":"msg_a","type"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newResponsesMessagesStream("public", nil)
			raw, err := s.response([]byte(fmt.Sprintf(`{"id":"resp_a","status":"completed","output":[%s]}`, tc.output)), priceUsage{Input: 10, Output: 2})
			if (err == nil) != tc.valid {
				t.Fatal(string(raw), err)
			}
			if tc.valid && (!strings.Contains(string(raw), `"text":"OK"`) || strings.Count(string(raw), "msg_lite_") != 1) {
				t.Fatal("lost text or leaked synthetic item identity", string(raw))
			}
		})
	}
	s := newResponsesMessagesStream("public", nil)
	_, _ = s.event([]byte(`{"type":"response.created","response":{"id":"resp_a"}}`), priceUsage{})
	if _, err := s.event([]byte(`{"type":"response.output_item.added","item":`+message+`}`), priceUsage{}); err == nil {
		t.Fatal("stream accepted missing identity")
	}
	// The complete snapshot exception cannot relax streaming terminal checks.
	s = newResponsesMessagesStream("public", nil)
	_, _ = s.event([]byte(`{"type":"response.created","response":{"id":"resp_a"}}`), priceUsage{})
	var response messagesResponseBody
	_ = json.Unmarshal([]byte(`{"status":"completed","output":[`+message+`]}`), &response)
	if _, _, err := s.finish(response, priceUsage{}); err == nil {
		t.Fatal("stream terminal accepted missing identity")
	}
}
