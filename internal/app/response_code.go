package app

import (
	"encoding/json"
	"slices"
	"strings"
)

// Code Interpreter and hosted Shell containers execute at the provider, never
// in this process. Both use the same owned response context and file grants.
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
	return "", validateResponseAutoContainer(container)
}

func validateResponseAutoContainer(container map[string]json.RawMessage) error {
	for field, raw := range container {
		switch field {
		case "type":
		case "memory_limit":
			size := credentialString(container, field)
			if string(raw) != "null" && size != "1g" && size != "4g" && size != "16g" && size != "64g" {
				return bad("invalid container memory limit")
			}
		case "file_ids":
			var files []string
			if string(raw) != "null" && (json.Unmarshal(raw, &files) != nil || len(files) > 100) {
				return bad("invalid container file list")
			}
		case "network_policy":
			if err := validateContainerNetwork(raw); err != nil {
				return err
			}
		default:
			return bad("unsupported container option")
		}
	}
	return nil
}

func validateContainerNetwork(raw json.RawMessage) error {
	var policy map[string]json.RawMessage
	if json.Unmarshal(raw, &policy) != nil || policy == nil {
		return bad("invalid container network policy")
	}
	if credentialString(policy, "type") == "disabled" && len(policy) == 1 {
		return nil
	}
	if credentialString(policy, "type") != "allowlist" {
		return bad("invalid container network policy type")
	}
	for key := range policy {
		if key != "type" && key != "allowed_domains" && key != "domain_secrets" {
			return bad("unsupported container network option")
		}
	}
	var domains []string
	if json.Unmarshal(policy["allowed_domains"], &domains) != nil || domains == nil || len(domains) > 100 {
		return bad("invalid container domain allowlist")
	}
	allowed := map[string]bool{}
	for _, domain := range domains {
		if !validDomainName(domain) || allowed[strings.ToLower(domain)] {
			return bad("invalid or duplicate container domain")
		}
		allowed[strings.ToLower(domain)] = true
	}
	if raw := policy["domain_secrets"]; raw != nil {
		var secrets []map[string]json.RawMessage
		if json.Unmarshal(raw, &secrets) != nil || secrets == nil || len(secrets) > 100 {
			return bad("invalid container domain secrets")
		}
		seen := map[string]bool{}
		for _, secret := range secrets {
			domain, name, value := credentialString(secret, "domain"), credentialString(secret, "name"), credentialString(secret, "value")
			key := strings.ToLower(domain) + "\x00" + name
			if len(secret) != 3 || !allowed[strings.ToLower(domain)] || !validResponseID(name) || value == "" || len(value) > 64<<10 || strings.ContainsAny(value, "\r\n\x00") || seen[key] {
				return bad("domain secret requires a unique name, an allowed domain and a valid value")
			}
			seen[key] = true
		}
	}
	return nil
}

func responseShellTool(tool map[string]json.RawMessage) (string, bool, error) {
	for field, raw := range tool {
		switch field {
		case "type", "environment":
		case "allowed_callers":
			var callers []string
			if string(raw) != "null" && (json.Unmarshal(raw, &callers) != nil || len(callers) != 1 || callers[0] != "direct") {
				return "", false, bad("shell requires direct invocation")
			}
		default:
			return "", false, bad("unsupported shell option")
		}
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(tool["environment"], &env) != nil || env == nil {
		return "", false, bad("shell requires an explicit environment")
	}
	switch credentialString(env, "type") {
	case "local":
		return "", false, validateLocalEnvironment(tool["environment"])
	case "container_auto":
		return "", true, validateResponseAutoContainer(env)
	case "container_reference":
		id, err := responseShellContainer(tool["environment"])
		return id, true, err
	default:
		return "", false, bad("unsupported shell environment")
	}
}

func responseShellContainer(raw json.RawMessage) (string, error) {
	var env map[string]json.RawMessage
	if json.Unmarshal(raw, &env) != nil || len(env) != 2 || credentialString(env, "type") != "container_reference" || !validResponseID(credentialString(env, "container_id")) {
		return "", bad("shell requires a valid container reference")
	}
	return credentialString(env, "container_id"), nil
}

func hostedShellItem(item map[string]json.RawMessage) bool {
	if credentialString(item, "type") != "shell_call" {
		return false
	}
	var env map[string]json.RawMessage
	_ = json.Unmarshal(item["environment"], &env)
	return credentialString(env, "type") == "container_reference"
}

func responseShellItem(item map[string]json.RawMessage) (string, string, error) {
	id := credentialString(item, "id")
	if !validResponseID(id) {
		return "", "", bad("hosted shell history requires an owned item ID")
	}
	container, err := responseShellContainer(item["environment"])
	if err == nil {
		err = validateResponseLocalItem(item)
	}
	return id, container, err
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
		if credentialString(item, "type") != "code_interpreter_call" && !hostedShellItem(item) {
			continue
		}
		var id string
		var err error
		if hostedShellItem(item) {
			_, id, err = responseShellItem(item)
		} else {
			_, id, err = responseCodeItem(item)
		}
		if err != nil {
			return nil, &apiError{502, "invalid upstream container tool output"}
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}
