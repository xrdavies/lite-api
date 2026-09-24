package app

import (
	"encoding/json"
	"slices"
)

// Containers execute at the Responses provider, never in this process. Existing
// IDs require owned response context; uploaded file lifecycle is separate.
func responseCodeTool(tool map[string]json.RawMessage) (string, error) {
	for field, raw := range tool {
		switch field {
		case "type", "container":
		case "allowed_callers":
			var callers []string
			if string(raw) != "null" && (json.Unmarshal(raw, &callers) != nil || len(callers) != 1 || callers[0] != "direct") {
				return "", bad("code interpreter requires direct invocation")
			}
		default:
			return "", bad("unsupported code interpreter option")
		}
	}
	if id := credentialString(tool, "container"); id != "" {
		if !validResponseID(id) {
			return "", bad("invalid code interpreter container")
		}
		return id, nil
	}
	var container map[string]json.RawMessage
	if json.Unmarshal(tool["container"], &container) != nil || credentialString(container, "type") != "auto" {
		return "", bad("code interpreter requires an auto container or an owned container ID")
	}
	for field, raw := range container {
		switch field {
		case "type":
		case "memory_limit":
			size := credentialString(container, field)
			if string(raw) != "null" && size != "1g" && size != "4g" && size != "16g" && size != "64g" {
				return "", bad("invalid code interpreter memory limit")
			}
		case "file_ids":
			var files []string
			if string(raw) != "null" && (json.Unmarshal(raw, &files) != nil || len(files) > 100) {
				return "", bad("invalid code interpreter file list")
			}
		case "network_policy":
			var policy map[string]json.RawMessage
			if json.Unmarshal(raw, &policy) != nil || len(policy) != 1 || credentialString(policy, "type") != "disabled" {
				return "", bad("code interpreter network policy must be disabled")
			}
		default:
			return "", bad("unsupported code interpreter container option")
		}
	}
	return "", nil
}

func responseCodeItem(item map[string]json.RawMessage) (string, string, error) {
	for field := range item {
		switch field {
		case "type", "id", "container_id", "status", "code", "outputs":
		default:
			return "", "", bad("unsupported code interpreter history field")
		}
	}
	id, container := credentialString(item, "id"), credentialString(item, "container_id")
	if !validResponseID(id) || !validResponseID(container) {
		return "", "", bad("code interpreter history requires owned item and container IDs")
	}
	status := credentialString(item, "status")
	if status != "in_progress" && status != "completed" && status != "incomplete" && status != "interpreting" && status != "failed" {
		return "", "", bad("invalid code interpreter status")
	}
	if raw := item["code"]; raw != nil && string(raw) != "null" {
		var code string
		if json.Unmarshal(raw, &code) != nil {
			return "", "", bad("invalid code interpreter code")
		}
	}
	if raw := item["outputs"]; raw != nil && string(raw) != "null" {
		var outputs []map[string]json.RawMessage
		if json.Unmarshal(raw, &outputs) != nil || len(outputs) > 1024 {
			return "", "", bad("invalid code interpreter outputs")
		}
		for _, output := range outputs {
			field := "logs"
			switch credentialString(output, "type") {
			case "logs":
			case "image":
				field = "url"
			default:
				return "", "", bad("invalid code interpreter output type")
			}
			var value *string
			if len(output) != 2 || json.Unmarshal(output[field], &value) != nil || value == nil {
				return "", "", bad("invalid code interpreter output value")
			}
		}
	}
	return id, container, nil
}

func validateResponseContainers(in textRequest, binding *responseBinding) error {
	if in.NativeCode || in.NativeFileSearch {
		if in.Action != "" || in.NativeCompaction {
			return bad("hosted tools require a normal Responses request")
		}
	}
	for _, id := range in.ContainerReferences {
		owned := false
		if binding != nil {
			for _, container := range binding.Containers {
				owned = owned || container == id
			}
		}
		if !owned {
			return missing()
		}
	}
	return nil
}

func mergeResponseContainers(previous, current []string) ([]string, error) {
	ids := append([]string(nil), previous...)
	for _, id := range current {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 1024 {
		return nil, bad("too many code interpreter containers")
	}
	slices.Sort(ids)
	return ids, nil
}

func responseContainerIDs(items []map[string]json.RawMessage) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, item := range items {
		if credentialString(item, "type") != "code_interpreter_call" {
			continue
		}
		_, id, err := responseCodeItem(item)
		if err != nil {
			return nil, &apiError{502, "invalid upstream code interpreter output"}
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
