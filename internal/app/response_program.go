package app

import (
	"encoding/json"
	"slices"
)

func programmaticTool(kind string) bool { return kind == "programmatic_tool_calling" }

func validateProgrammaticTool(tool map[string]json.RawMessage) error {
	if len(tool) != 1 || !programmaticTool(credentialString(tool, "type")) {
		return bad("unsupported programmatic tool option")
	}
	return nil
}

// Programmatic tool calls are provider-executed JavaScript plus client-owned
// function calls. The gateway relays the native Responses items and never
// executes the program.
func validateAllowedCallers(raw json.RawMessage) error {
	if raw == nil || string(raw) == "null" {
		return nil
	}
	var callers []string
	if json.Unmarshal(raw, &callers) != nil || len(callers) == 0 || len(callers) > 2 {
		return bad("invalid allowed_callers")
	}
	seen := map[string]bool{}
	for _, caller := range callers {
		if caller != "direct" && caller != "programmatic" || seen[caller] {
			return bad("invalid allowed_callers")
		}
		seen[caller] = true
	}
	return nil
}

func hasProgrammaticCaller(raw json.RawMessage) (bool, error) {
	if raw == nil || string(raw) == "null" {
		return false, nil
	}
	var caller map[string]json.RawMessage
	if json.Unmarshal(raw, &caller) != nil || caller == nil {
		return false, bad("invalid tool caller")
	}
	switch credentialString(caller, "type") {
	case "direct":
		if len(caller) != 1 {
			return false, bad("direct caller has unsupported fields")
		}
		return false, nil
	case "program":
		if len(caller) != 2 || !validProgramCallID(credentialString(caller, "caller_id")) {
			return false, bad("program caller requires a valid caller ID")
		}
		return true, nil
	default:
		return false, bad("invalid tool caller type")
	}
}

func validateResponseProgramItem(item map[string]json.RawMessage) (bool, error) {
	kind := credentialString(item, "type")
	switch kind {
	case "program":
		for field := range item {
			switch field {
			case "type", "id", "call_id", "code", "fingerprint":
			default:
				return false, bad("unsupported program item field")
			}
		}
		if !validResponseID(credentialString(item, "id")) || !validProgramCallID(credentialString(item, "call_id")) {
			return false, bad("program item requires valid IDs")
		}
		for _, field := range []string{"code", "fingerprint"} {
			var value *string
			if json.Unmarshal(item[field], &value) != nil || value == nil || len(*value) > 10<<20 {
				return false, bad("program item code or fingerprint is invalid")
			}
		}
		return true, nil
	case "program_output":
		for field := range item {
			switch field {
			case "type", "id", "call_id", "result", "status":
			default:
				return false, bad("unsupported program output field")
			}
		}
		if !validResponseID(credentialString(item, "id")) || !validProgramCallID(credentialString(item, "call_id")) {
			return false, bad("program output requires valid IDs")
		}
		if raw := item["result"]; raw == nil {
			return false, bad("program output requires a result")
		} else {
			var result *string
			if json.Unmarshal(raw, &result) != nil || result == nil || len(*result) > 10<<20 {
				return false, bad("program output result is invalid")
			}
		}
		status := credentialString(item, "status")
		if status != "completed" && status != "incomplete" {
			return false, bad("invalid program output status")
		}
		return true, nil
	case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output", "shell_call", "shell_call_output", "apply_patch_call", "apply_patch_call_output":
		program, err := hasProgrammaticCaller(item["caller"])
		if err != nil {
			return false, err
		}
		return program, nil
	default:
		return false, nil
	}
}

// Call IDs have a separate namespace from output item IDs. Reuse scoped item
// bindings for program callers; ':' cannot occur in a client item_reference ID.
func programCallReference(id string) string { return "program:" + id }

// Provider-authored programs and outputs must be replayed unchanged. Persist a
// digest alongside item ownership, never the source code or replay fingerprint.
func programItemReference(item map[string]json.RawMessage) string {
	return "program-item:" + digest(responseToolDefinition(item))
}

func validProgramCallID(id string) bool { return len(id) <= 64 && validResponseID(id) }

func validateProgrammaticDeclarations(tools []map[string]json.RawMessage) (bool, error) {
	programmatic := false
	for _, tool := range tools {
		if programmaticTool(credentialString(tool, "type")) {
			if err := validateProgrammaticTool(tool); err != nil {
				return false, err
			}
			programmatic = true
			continue
		}
		if raw := tool["allowed_callers"]; raw != nil {
			switch credentialString(tool, "type") {
			case "function", "custom", "apply_patch", "shell", "code_interpreter", "mcp":
			default:
				return false, bad("this tool does not accept allowed_callers")
			}
			if err := validateAllowedCallers(raw); err != nil {
				return false, err
			}
			var callers []string
			_ = json.Unmarshal(raw, &callers)
			programmatic = programmatic || slices.Contains(callers, "programmatic")
		}
		if credentialString(tool, "type") == "namespace" {
			children, err := responseNamespaceChildren(tool)
			if err != nil {
				return false, err
			}
			childProgrammatic, err := validateProgrammaticDeclarations(children)
			if err != nil {
				return false, err
			}
			programmatic = programmatic || childProgrammatic
		}
	}
	return programmatic, nil
}
