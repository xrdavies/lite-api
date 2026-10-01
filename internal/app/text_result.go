package app

import (
	"encoding/base64"
	"encoding/json"
	"math"
)

// Validate results separately from metering: malformed output must not erase
// already observed consumption. Relay responses may omit optional metadata.
func (o *textObservation) observeResult(raw []byte, stream bool) error {
	if o.CountOnly {
		return nil
	}
	invalid := func() error { return &apiError{502, "upstream returned no valid " + o.Protocol + " result"} }
	switch o.Protocol {
	case "chat_completions":
		var result struct {
			Choices []struct {
				Message, Delta map[string]json.RawMessage
				Finish         string `json:"finish_reason"`
			}
		}
		if json.Unmarshal(raw, &result) != nil || !stream && len(result.Choices) == 0 {
			return invalid()
		}
		for _, choice := range result.Choices {
			message := choice.Message
			if stream {
				message = choice.Delta
			}
			if message == nil {
				return invalid()
			}
			valid := false
			for _, field := range []string{"content", "refusal", "reasoning_content", "reasoning"} {
				if value := message[field]; value != nil && string(value) != "null" {
					var text string
					if json.Unmarshal(value, &text) != nil {
						return invalid()
					}
					valid = valid || text != "" || !stream
				}
			}
			if value := message["tool_calls"]; value != nil && string(value) != "null" {
				var calls []map[string]json.RawMessage
				if json.Unmarshal(value, &calls) != nil {
					return invalid()
				}
				for _, call := range calls {
					if len(call) == 0 {
						return invalid()
					}
				}
				valid = valid || len(calls) > 0
			}
			for _, field := range []string{"function_call", "audio"} {
				if value := message[field]; value != nil && string(value) != "null" {
					var object map[string]json.RawMessage
					if json.Unmarshal(value, &object) != nil || len(object) == 0 {
						return invalid()
					}
					valid = true
				}
			}
			valid = valid || choice.Finish == "content_filter"
			if !stream && !valid {
				return invalid()
			}
			o.resultSeen = o.resultSeen || valid
		}
	case "anthropic":
		var result struct {
			Type    string
			Content []map[string]json.RawMessage
			Block   map[string]json.RawMessage `json:"content_block"`
			Delta   map[string]json.RawMessage
			Message json.RawMessage
		}
		if json.Unmarshal(raw, &result) != nil {
			return invalid()
		}
		if !stream {
			if result.Content == nil {
				return invalid()
			}
			for _, block := range result.Content {
				if credentialString(block, "type") == "" {
					return invalid()
				}
			}
			o.resultSeen = true // An explicit empty content list can be a refusal.
		} else {
			switch result.Type {
			case "message_start":
				var message struct{ Content []map[string]json.RawMessage }
				if json.Unmarshal(result.Message, &message) != nil {
					return invalid()
				}
				o.resultSeen = len(message.Content) > 0
			case "content_block_start":
				if credentialString(result.Block, "type") == "" {
					return invalid()
				}
				o.resultSeen = true
			case "content_block_delta":
				if credentialString(result.Delta, "type") == "" {
					return invalid()
				}
				o.resultSeen = true
			case "message_delta":
				o.resultSeen = o.resultSeen || credentialString(result.Delta, "stop_reason") == "refusal"
			case "message_stop":
				if !o.resultSeen {
					return invalid()
				}
			}
		}
	case "embeddings":
		var result struct {
			Data []struct {
				Index     int
				Embedding json.RawMessage
			}
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Data) == 0 {
			return invalid()
		}
		seen := map[int]bool{}
		dimension := 0
		for _, item := range result.Data {
			if item.Index < 0 || item.Index >= len(result.Data) || seen[item.Index] {
				return invalid()
			}
			seen[item.Index] = true
			var encoded string
			n := 0
			if json.Unmarshal(item.Embedding, &encoded) == nil {
				data, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(data)%4 != 0 {
					return invalid()
				}
				n = len(data) / 4
			} else {
				var vector []*float64
				if json.Unmarshal(item.Embedding, &vector) != nil {
					return invalid()
				}
				for _, value := range vector {
					if value == nil || math.IsInf(*value, 0) || math.IsNaN(*value) {
						return invalid()
					}
				}
				n = len(vector)
			}
			if n == 0 || dimension != 0 && dimension != n {
				return invalid()
			}
			dimension = n
		}
	}
	return nil
}
