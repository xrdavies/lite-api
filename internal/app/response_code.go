package app

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Code Interpreter and hosted Shell containers execute at the provider, never
// in this process. Both use the same owned response context and file grants.
func responseCodeTool(tool map[string]json.RawMessage) (string, error) {
	for field, raw := range tool {
		switch field {
		case "type", "container":
		case "allowed_callers":
			if err := validateAllowedCallers(raw); err != nil {
				return "", err
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
		case "skills":
			if credentialString(container, "type") != "container_auto" {
				return bad("skills require a hosted shell environment")
			}
			if _, err := responseSkills(raw); err != nil {
				return err
			}
		default:
			return bad("unsupported container option")
		}
	}
	return nil
}

// Inline bundles stay opaque: only the upstream mounts or executes them.
// Referenced skills use the same source-bound group grants as files and stores.
func responseSkills(raw json.RawMessage) ([]string, error) {
	var skills []map[string]json.RawMessage
	if json.Unmarshal(raw, &skills) != nil || skills == nil || len(skills) > 100 {
		return nil, bad("skills must contain at most 100 entries")
	}
	var ids []string
	for _, skill := range skills {
		switch credentialString(skill, "type") {
		case "skill_reference":
			id := credentialString(skill, "skill_id")
			if !validResponseID(id) || len(id) > 64 {
				return nil, bad("invalid hosted skill ID")
			}
			for key := range skill {
				if key != "type" && key != "skill_id" && key != "version" {
					return nil, bad("unsupported hosted skill reference option")
				}
			}
			if skill["version"] != nil {
				version := credentialString(skill, "version")
				if version != "latest" {
					n, err := strconv.ParseUint(version, 10, 64)
					if err != nil || n == 0 || strconv.FormatUint(n, 10) != version {
						return nil, bad("skill version must be latest or a positive integer string")
					}
				}
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		case "inline":
			name, description := credentialString(skill, "name"), credentialString(skill, "description")
			if len(skill) != 4 || strings.TrimSpace(name) == "" || len(name) > 256 || strings.TrimSpace(description) == "" || len(description) > 10000 {
				return nil, bad("inline skills require name, description and source")
			}
			var source map[string]json.RawMessage
			if json.Unmarshal(skill["source"], &source) != nil || len(source) != 3 || credentialString(source, "type") != "base64" || credentialString(source, "media_type") != "application/zip" {
				return nil, bad("inline skill source must be a base64 ZIP bundle")
			}
			data := credentialString(source, "data")
			if data == "" || len(data) > base64.StdEncoding.EncodedLen(8<<20) {
				return nil, bad("inline skill exceeds 8 MiB")
			}
			size, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
			if err != nil || size == 0 || size > 8<<20 {
				return nil, bad("invalid inline skill encoding")
			}
		default:
			return nil, bad("unsupported hosted skill type")
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func requestSkillIDs(tools []map[string]json.RawMessage) ([]string, error) {
	var ids []string
	for _, tool := range tools {
		if credentialString(tool, "type") != "shell" {
			continue
		}
		var env map[string]json.RawMessage
		if json.Unmarshal(tool["environment"], &env) != nil || credentialString(env, "type") != "container_auto" || env["skills"] == nil {
			continue
		}
		current, err := responseSkills(env["skills"])
		if err != nil {
			return nil, err
		}
		ids, err = mergeResponseResources(ids, current)
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
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
			if err := validateAllowedCallers(raw); err != nil {
				return "", false, err
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
	if in.NativeCode || in.NativeFileSearch || in.NativeProgrammatic {
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
