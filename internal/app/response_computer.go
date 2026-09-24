package app

import "encoding/json"

// Computer tools run in the client's browser/desktop. The gateway forwards
// actions and screenshots without opening a browser, fetching images or acting.
func validateComputerTool(tool map[string]json.RawMessage) error {
	if credentialString(tool, "type") == "computer" {
		if len(tool) != 1 {
			return bad("computer accepts only type")
		}
		return nil
	}
	if len(tool) != 4 {
		return bad("computer_use_preview requires display dimensions and environment")
	}
	for _, field := range []string{"display_width", "display_height"} {
		var dimension float64
		if json.Unmarshal(tool[field], &dimension) != nil || dimension <= 0 {
			return bad("invalid computer display dimension")
		}
	}
	switch credentialString(tool, "environment") {
	case "windows", "mac", "linux", "ubuntu", "browser":
		return nil
	}
	return bad("invalid computer environment")
}

func validateComputerItem(item map[string]json.RawMessage) error {
	checkField := "pending_safety_checks"
	if credentialString(item, "type") == "computer_call_output" {
		checkField = "acknowledged_safety_checks"
		var output map[string]json.RawMessage
		if json.Unmarshal(item["output"], &output) != nil || credentialString(output, "type") != "computer_screenshot" {
			return bad("computer output requires a screenshot")
		}
		for field, raw := range output {
			switch field {
			case "type", "image_url":
			case "file_id":
				if string(raw) != "null" {
					return bad("computer screenshots require an image URL or data URL, not a shared file ID")
				}
			case "detail":
				switch credentialString(output, field) {
				case "auto", "low", "high", "original":
				default:
					return bad("invalid screenshot detail")
				}
			default:
				return bad("unsupported screenshot option")
			}
		}
		if _, err := anthropicMediaSource(credentialString(output, "image_url"), true); err != nil {
			return err
		}
	} else {
		var actions []map[string]json.RawMessage
		if raw := item["actions"]; raw != nil && string(raw) != "null" {
			if json.Unmarshal(raw, &actions) != nil || len(actions) == 0 {
				return bad("computer actions must be a nonempty array")
			}
		}
		if raw := item["action"]; raw != nil && string(raw) != "null" {
			var action map[string]json.RawMessage
			if len(actions) != 0 || json.Unmarshal(raw, &action) != nil || action == nil {
				return bad("use one computer action or batched actions")
			}
			actions = append(actions, action)
		}
		if len(actions) == 0 {
			return bad("computer call requires actions")
		}
		for _, action := range actions {
			if err := validateComputerAction(action); err != nil {
				return err
			}
		}
	}
	// Safety acknowledgements are supplied by the client, never synthesized.
	if raw := item[checkField]; raw != nil && string(raw) != "null" {
		var checks []struct {
			ID            string
			Code, Message *string
		}
		if json.Unmarshal(raw, &checks) != nil {
			return bad("invalid computer safety checks")
		}
		for _, check := range checks {
			if !validResponseID(check.ID) {
				return bad("computer safety check requires an ID")
			}
		}
	}
	return nil
}

func validateComputerAction(action map[string]json.RawMessage) error {
	numbers := []string{}
	kind := credentialString(action, "type")
	switch kind {
	case "click", "double_click", "move", "scroll":
		numbers = []string{"x", "y"}
		if kind == "scroll" {
			numbers = append(numbers, "scroll_x", "scroll_y")
		}
		if kind == "click" {
			switch credentialString(action, "button") {
			case "left", "right", "wheel", "back", "forward":
			default:
				return bad("invalid computer mouse button")
			}
		}
	case "drag":
		var path []struct{ X, Y *float64 }
		if json.Unmarshal(action["path"], &path) != nil || len(path) == 0 {
			return bad("computer drag requires a path")
		}
		for _, point := range path {
			if point.X == nil || point.Y == nil {
				return bad("invalid computer drag coordinates")
			}
		}
	case "type":
		var text *string
		if json.Unmarshal(action["text"], &text) != nil || text == nil {
			return bad("computer typing requires text")
		}
	case "keypress", "wait", "screenshot":
	default:
		return bad("unsupported computer action")
	}
	for _, field := range numbers {
		var value *float64
		if json.Unmarshal(action[field], &value) != nil || value == nil {
			return bad("invalid computer coordinates")
		}
	}
	if raw := action["keys"]; kind == "keypress" || raw != nil && string(raw) != "null" {
		var keys []string
		if json.Unmarshal(raw, &keys) != nil || kind == "keypress" && len(keys) == 0 {
			return bad("invalid computer keys")
		}
	}
	return nil
}
