package app

import "testing"

func TestTextResultValidation(t *testing.T) {
	for _, tt := range []struct {
		name, protocol, raw string
		stream, valid       bool
	}{
		{"missing choices", "chat_completions", `{}`, false, false},
		{"missing message", "chat_completions", `{"choices":[{}]}`, false, false},
		{"text", "chat_completions", `{"choices":[{"message":{"content":"OK"}}]}`, false, true},
		{"empty text", "chat_completions", `{"choices":[{"message":{"content":""}}]}`, false, true},
		{"tool", "chat_completions", `{"choices":[{"message":{"content":null,"tool_calls":[{"type":"function","id":"x","function":{"name":"f","arguments":"{}"}}]}}]}`, false, true},
		{"empty tool", "chat_completions", `{"choices":[{"message":{"tool_calls":[{}]}}]}`, false, false},
		{"refusal", "chat_completions", `{"choices":[{"message":{"refusal":"No"}}]}`, false, true},
		{"filter", "chat_completions", `{"choices":[{"message":{"content":null},"finish_reason":"content_filter"}]}`, false, true},
		{"bad content", "chat_completions", `{"choices":[{"message":{"content":123}}]}`, false, false},
		{"audio", "chat_completions", `{"choices":[{"message":{"audio":{"id":"audio_1"}}}]}`, false, true},
		{"usage tail", "chat_completions", `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`, true, true},
		{"role delta", "chat_completions", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, true, true},
		{"missing content", "anthropic", `{"usage":{"input_tokens":10,"output_tokens":5}}`, false, false},
		{"empty refusal", "anthropic", `{"content":[],"stop_reason":"refusal"}`, false, true},
		{"tool block", "anthropic", `{"content":[{"type":"tool_use","id":"tool_1","name":"f","input":{}}]}`, false, true},
		{"bad block", "anthropic", `{"content":[{}]}`, false, false},
		{"missing vectors", "embeddings", `{"usage":{"prompt_tokens":10}}`, false, false},
		{"numeric vectors", "embeddings", `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`, false, true},
		{"base64 vectors", "embeddings", `{"data":[{"index":0,"embedding":"AAAAAAAAgD8="}]}`, false, true},
		{"bad base64", "embeddings", `{"data":[{"index":0,"embedding":"bad"}]}`, false, false},
		{"null vector", "embeddings", `{"data":[{"index":0,"embedding":[null]}]}`, false, false},
		{"empty vector", "embeddings", `{"data":[{"index":0,"embedding":[]}]}`, false, false},
		{"duplicate index", "embeddings", `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`, false, false},
		{"different dimensions", "embeddings", `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2,3]}]}`, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := textObservation{Protocol: tt.protocol}
			if err := o.observeResult([]byte(tt.raw), tt.stream); (err == nil) != tt.valid {
				t.Fatalf("valid = %v, error = %v", tt.valid, err)
			}
		})
	}
	for _, protocol := range []string{"chat_completions", "anthropic", "embeddings"} {
		o := textObservation{Protocol: protocol}
		raw := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5,"input_tokens":10,"output_tokens":5}}`)
		if err := o.observe(raw); err != nil {
			t.Fatal(err)
		}
		if o.observeResult(raw, false) == nil || !o.HasUsage || o.Usage.Input != 10 {
			t.Fatal("invalid output erased known usage", o)
		}
	}
	for _, protocol := range []string{"chat_completions", "anthropic"} {
		o := textObservation{Protocol: protocol}
		frames := []string{`{"choices":[{"delta":{"content":"OK"}}]}`, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`}
		if protocol == "anthropic" {
			frames = []string{`{"type":"message_start","message":{"content":[]}}`, `{"type":"content_block_start","content_block":{"type":"text","text":""}}`, `{"type":"message_stop"}`}
		}
		for _, raw := range frames {
			if err := o.observeResult([]byte(raw), true); err != nil {
				t.Fatal(err)
			}
		}
		if !o.resultSeen {
			t.Fatal("valid stream result lost", protocol)
		}
	}
	o := textObservation{Protocol: "anthropic"}
	if o.observeResult([]byte(`{"type":"message_stop"}`), true) == nil {
		t.Fatal("empty message stream accepted")
	}
}
