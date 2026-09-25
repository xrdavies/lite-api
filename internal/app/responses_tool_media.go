package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

const toolMediaMarker = "[Tool output media moved to the following user message]"

// Chat tool messages accept text. Only recognized image nodes are lifted;
// arbitrary result objects and their numeric values remain tool output text.
func convertedToolOutput(raw json.RawMessage) (string, []any, error) {
	text := string(raw)
	if len(raw) == 0 || text == "null" {
		return "", nil, nil
	}
	if json.Unmarshal(raw, &text) == nil {
		if strings.HasPrefix(text, "data:image/") {
			raw, _ = json.Marshal(map[string]string{"type": "input_image", "image_url": text})
		} else if json.Valid([]byte(text)) {
			raw = json.RawMessage(text)
		} else {
			return text, nil, nil
		}
	}
	var media []any
	var rewrite func(json.RawMessage, int) (json.RawMessage, bool, error)
	rewrite = func(value json.RawMessage, depth int) (json.RawMessage, bool, error) {
		if depth > 64 {
			return nil, false, bad("tool output nesting exceeds limit")
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(value, &object) == nil && object != nil {
			kind := credentialString(object, "type")
			if kind == "input_file" || (kind == "input_image" || kind == "image_url") && object["file_id"] != nil {
				return nil, false, bad("tool file references require a native Responses account")
			}
			if kind == "input_image" || kind == "image_url" {
				uri := credentialString(object, "image_url")
				detail := credentialString(object, "detail")
				if uri == "" {
					var image map[string]json.RawMessage
					_ = json.Unmarshal(object["image_url"], &image)
					uri = credentialString(image, "url")
					if detail == "" {
						detail = credentialString(image, "detail")
					}
				}
				if len(media) >= 128 {
					return nil, false, bad("too many tool output images")
				}
				if _, err := anthropicMediaSource(uri, true); err != nil {
					return nil, false, err
				}
				image := map[string]string{"url": uri}
				if detail != "" {
					if detail != "auto" && detail != "low" && detail != "high" && detail != "original" {
						return nil, false, bad("invalid tool output image detail")
					}
					image["detail"] = detail
				}
				media = append(media, map[string]any{"type": "image_url", "image_url": image})
				marker, _ := json.Marshal(map[string]string{"type": "input_text", "text": toolMediaMarker})
				return marker, true, nil
			}
			if content := object["content"]; content != nil {
				updated, changed, err := rewrite(content, depth+1)
				if err != nil || !changed {
					return value, false, err
				}
				object["content"] = updated
				encoded, err := json.Marshal(object)
				return encoded, true, err
			}
			return value, false, nil
		}
		var items []json.RawMessage
		if json.Unmarshal(value, &items) != nil {
			return value, false, nil
		}
		changed := false
		for i, item := range items {
			updated, didChange, err := rewrite(item, depth+1)
			if err != nil {
				return nil, false, err
			}
			items[i] = updated
			changed = changed || didChange
		}
		if !changed {
			return value, false, nil
		}
		encoded, err := json.Marshal(items)
		return encoded, true, err
	}
	updated, changed, err := rewrite(raw, 0)
	if err != nil {
		return "", nil, err
	}
	if changed {
		text = string(updated)
	}
	return text, media, nil
}

// Reorder only batches with image results. History keeps the original identities
// and media so a later continuation can rebuild the same attributed user turn.
func convertedToolMediaMessages(messages []convertedChatMessage) ([]convertedChatMessage, error) {
	replies := map[string]convertedChatMessage{}
	withMedia := map[string]bool{}
	for _, message := range messages {
		if message.Role == "tool" {
			replies[message.CallID] = message
			withMedia[message.CallID] = withMedia[message.CallID] || len(message.ToolMedia) > 0
		}
	}
	consumed := map[string]bool{}
	batches := map[int]bool{}
	for i, message := range messages {
		for _, call := range message.Calls {
			if withMedia[call.ID] {
				batches[i] = true
			}
		}
		if batches[i] {
			for _, call := range message.Calls {
				if consumed[call.ID] {
					return nil, bad("duplicate tool call identity in media history")
				}
				consumed[call.ID] = true
			}
		}
	}
	for id, reply := range replies {
		if len(reply.ToolMedia) > 0 && !consumed[id] {
			return nil, bad("tool output image requires a matching call")
		}
	}
	out := make([]convertedChatMessage, 0, len(messages))
	for i, message := range messages {
		if message.Role == "tool" && consumed[message.CallID] {
			continue
		}
		if !batches[i] {
			out = append(out, message)
			continue
		}
		calls := message.Calls
		message.Calls = nil
		for _, call := range calls {
			if _, ok := replies[call.ID]; ok {
				message.Calls = append(message.Calls, call)
			}
		}
		out = append(out, message)
		media := []any{}
		for _, call := range message.Calls {
			reply := replies[call.ID]
			if len(reply.ToolMedia) > 0 {
				media = append(media, map[string]string{"type": "text", "text": fmt.Sprintf("[Tool output media for call %s]", call.ID)})
				media = append(media, reply.ToolMedia...)
			}
			reply.ToolMedia = nil
			out = append(out, reply)
		}
		if len(media) > 0 {
			out = append(out, convertedChatMessage{Role: "user", Content: media})
		}
	}
	return out, nil
}
