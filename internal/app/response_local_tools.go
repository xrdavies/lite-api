package app

import "encoding/json"

func responseLocalTool(kind string) bool {
	return kind == "apply_patch" || kind == "local_shell" || kind == "shell" || kind == "computer" || kind == "computer_use_preview"
}

func responseLocalItem(kind string) bool {
	switch kind {
	case "apply_patch_call", "apply_patch_call_output", "local_shell_call", "local_shell_call_output", "shell_call", "shell_call_output", "computer_call", "computer_call_output":
		return true
	}
	return false
}

// Local skills are client paths. Hosted resource references are validated by
// responseShellTool instead; the request must identify its execution environment.
func validateLocalEnvironment(raw json.RawMessage) error {
	var env map[string]json.RawMessage
	if json.Unmarshal(raw, &env) != nil || credentialString(env, "type") != "local" {
		return bad("shell requires environment.type=local")
	}
	for field, value := range env {
		switch field {
		case "type":
		case "skills":
			var skills []map[string]json.RawMessage
			if string(value) == "null" || json.Unmarshal(value, &skills) != nil || len(skills) > 128 {
				return bad("invalid local skills")
			}
			for _, skill := range skills {
				if len(skill) != 3 || credentialString(skill, "name") == "" || credentialString(skill, "path") == "" {
					return bad("local skills require name, path and description")
				}
				var description string
				if string(skill["description"]) == "null" || json.Unmarshal(skill["description"], &description) != nil {
					return bad("invalid local skill description")
				}
			}
		default:
			return bad("unsupported local environment option")
		}
	}
	return nil
}

func validateResponseLocalTool(tool map[string]json.RawMessage) error {
	kind := credentialString(tool, "type")
	if kind == "computer" || kind == "computer_use_preview" {
		return validateComputerTool(tool)
	}
	if kind == "shell" {
		_, _, err := responseShellTool(tool)
		return err
	}
	for field, raw := range tool {
		switch field {
		case "type":
		case "environment":
			if kind != "shell" {
				return bad("this client tool does not accept an environment")
			}
		case "allowed_callers":
			if kind == "local_shell" {
				return bad("this client tool does not accept allowed_callers")
			}
			if err := validateAllowedCallers(raw); err != nil {
				return err
			}
		default:
			return bad("unsupported native client tool option")
		}
	}
	return nil
}

// Commands and paths are client data, never opened or executed by the gateway.
// Validate the envelope. Hosted shell references also require owned response
// context, checked by the shared container admission before forwarding.
func validateResponseLocalItem(item map[string]json.RawMessage) error {
	kind := credentialString(item, "type")
	identity := "call_id"
	if kind == "local_shell_call_output" {
		identity = "id" // The legacy output identifies the call with id, not call_id.
	}
	if !validResponseID(credentialString(item, identity)) {
		return bad("native client tool history requires a call ID")
	}
	if raw := item["id"]; raw != nil && string(raw) != "null" && !validResponseID(credentialString(item, "id")) {
		return bad("invalid native client tool item ID")
	}
	if raw := item["status"]; raw != nil && string(raw) != "null" && kind != "apply_patch_call_output" {
		status := credentialString(item, "status")
		if status != "in_progress" && status != "completed" && (status != "incomplete" || kind == "apply_patch_call") {
			return bad("invalid native client tool status")
		}
	}
	if raw := item["caller"]; raw != nil && string(raw) != "null" {
		program, err := hasProgrammaticCaller(raw)
		if err != nil {
			return err
		}
		if program && kind != "shell_call" && kind != "shell_call_output" && kind != "apply_patch_call" && kind != "apply_patch_call_output" {
			return bad("programmatic caller is not valid for this client tool")
		}
	}
	if raw := item["environment"]; raw != nil && string(raw) != "null" {
		if kind != "shell_call" {
			return bad("unexpected client tool environment")
		}
		var err error
		if hostedShellItem(item) {
			_, err = responseShellContainer(raw)
		} else {
			err = validateLocalEnvironment(raw)
		}
		if err != nil {
			return err
		}
	}
	for _, field := range []string{"container_id", "file_ids", "sandbox_id"} {
		if item[field] != nil {
			return bad("hosted resources are not available in client tool history")
		}
	}
	var object map[string]json.RawMessage
	var text *string
	switch kind {
	case "computer_call", "computer_call_output":
		return validateComputerItem(item)
	case "apply_patch_call":
		if json.Unmarshal(item["operation"], &object) != nil || object == nil || credentialString(object, "path") == "" {
			return bad("invalid patch operation")
		}
		switch credentialString(object, "type") {
		case "create_file", "update_file":
			if json.Unmarshal(object["diff"], &text) != nil || text == nil {
				return bad("patch operation requires a diff")
			}
		case "delete_file":
		default:
			return bad("invalid patch operation type")
		}
	case "shell_call", "local_shell_call":
		if json.Unmarshal(item["action"], &object) != nil || object == nil {
			return bad("invalid client shell action")
		}
		field := "commands"
		if kind == "local_shell_call" {
			field = "command"
			if credentialString(object, "type") != "exec" {
				return bad("invalid local_shell action type")
			}
		}
		var commands []string
		if json.Unmarshal(object[field], &commands) != nil || len(commands) == 0 {
			return bad("client shell action requires commands")
		}
	case "apply_patch_call_output":
		if status := credentialString(item, "status"); status != "completed" && status != "failed" {
			return bad("invalid patch output status")
		}
		if raw := item["output"]; raw != nil && string(raw) != "null" && json.Unmarshal(raw, &text) != nil {
			return bad("invalid patch output")
		}
	case "local_shell_call_output":
		if json.Unmarshal(item["output"], &text) != nil || text == nil {
			return bad("invalid local_shell output")
		}
	case "shell_call_output":
		var chunks []struct {
			Stdout, Stderr *string
			Outcome        struct {
				Type     string
				ExitCode *int64 `json:"exit_code"`
			}
		}
		if string(item["output"]) == "null" || json.Unmarshal(item["output"], &chunks) != nil {
			return bad("invalid shell output")
		}
		for _, chunk := range chunks {
			if chunk.Stdout == nil || chunk.Stderr == nil || chunk.Outcome.Type != "timeout" && (chunk.Outcome.Type != "exit" || chunk.Outcome.ExitCode == nil) {
				return bad("invalid shell output chunk")
			}
		}
	}
	return nil
}
