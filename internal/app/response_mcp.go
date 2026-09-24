package app

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

type mcpRequestKey struct{}

// Remote API Key MCP execution belongs to the selected Responses provider.
// No connector OAuth, tunnels, or local MCP transport is started by the gateway.
func validateResponseMCPTool(tool map[string]json.RawMessage) error {
	if !validResponseID(credentialString(tool, "server_label")) {
		return bad("MCP requires a server label")
	}
	u, err := url.Parse(credentialString(tool, "server_url"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return bad("MCP requires an HTTPS server URL without user information or fragment")
	}
	for field, raw := range tool {
		switch field {
		case "type", "server_label", "server_url":
		case "server_description":
			var value string
			if json.Unmarshal(raw, &value) != nil || len(value) > 10000 {
				return bad("invalid MCP description")
			}
		case "headers":
			var headers map[string]string
			if json.Unmarshal(raw, &headers) != nil || len(headers) > 64 {
				return bad("invalid MCP headers")
			}
			for name, value := range headers {
				if name == "" || len(name) > 256 || len(value) > 8192 || strings.ContainsAny(value, "\r\n\x00") {
					return bad("invalid MCP header")
				}
				for _, c := range name {
					if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
						return bad("invalid MCP header name")
					}
				}
			}
		case "allowed_tools":
			if err := validateMCPFilter(raw, true); err != nil {
				return err
			}
		case "require_approval":
			if string(raw) == "null" || string(raw) == `"always"` || string(raw) == `"never"` {
				continue
			}
			var filters map[string]json.RawMessage
			if json.Unmarshal(raw, &filters) != nil || filters == nil {
				return bad("invalid MCP approval policy")
			}
			for name, filter := range filters {
				if name != "always" && name != "never" {
					return bad("invalid MCP approval policy")
				}
				if err := validateMCPFilter(filter, false); err != nil {
					return err
				}
			}
		case "defer_loading":
			var enabled *bool
			if json.Unmarshal(raw, &enabled) != nil || enabled == nil {
				return bad("invalid MCP defer_loading")
			}
		case "allowed_callers":
			var callers []string
			if string(raw) != "null" && (json.Unmarshal(raw, &callers) != nil || len(callers) != 1 || callers[0] != "direct") {
				return bad("MCP requires direct invocation")
			}
		default:
			return bad("unsupported MCP option; use a remote server URL and API Key headers")
		}
	}
	return nil
}

func validateMCPFilter(raw json.RawMessage, namesAllowed bool) error {
	if namesAllowed && string(raw) == "null" {
		return nil
	}
	var names []string
	if namesAllowed && json.Unmarshal(raw, &names) == nil {
		for _, name := range names {
			if name == "" || len(name) > 256 {
				return bad("invalid MCP tool name")
			}
		}
		return nil
	}
	var filter map[string]json.RawMessage
	if json.Unmarshal(raw, &filter) != nil || filter == nil {
		return bad("invalid MCP tool filter")
	}
	for field, value := range filter {
		switch field {
		case "read_only":
			var flag *bool
			if json.Unmarshal(value, &flag) != nil || flag == nil {
				return bad("invalid MCP read_only filter")
			}
		case "tool_names":
			if json.Unmarshal(value, &names) != nil || names == nil {
				return bad("invalid MCP tool names")
			}
			for _, name := range names {
				if name == "" || len(name) > 256 {
					return bad("invalid MCP tool name")
				}
			}
		default:
			return bad("unsupported MCP tool filter")
		}
	}
	return nil
}

func responseMCPItem(kind string) bool {
	return kind == "mcp_call" || kind == "mcp_list_tools" || kind == "mcp_approval_request" || kind == "mcp_approval_response"
}

// Even a full history item must be one observed for this Key and upstream.
// In particular, approving a foreign request must never reach a shared account.
func validateResponseMCPItem(item map[string]json.RawMessage) (string, error) {
	id := credentialString(item, "id")
	if raw := item["approval_request_id"]; raw != nil && string(raw) != "null" && credentialString(item, "type") != "mcp_approval_response" {
		if !validResponseID(credentialString(item, "approval_request_id")) {
			return "", bad("invalid MCP approval request ID")
		}
	}
	if credentialString(item, "type") == "mcp_approval_response" {
		id = credentialString(item, "approval_request_id")
		var approve *bool
		if json.Unmarshal(item["approve"], &approve) != nil || approve == nil {
			return "", bad("MCP approval requires an explicit boolean")
		}
		if raw := item["reason"]; raw != nil && string(raw) != "null" {
			var reason string
			if json.Unmarshal(raw, &reason) != nil {
				return "", bad("invalid MCP approval reason")
			}
		}
	} else if !validResponseID(credentialString(item, "server_label")) {
		return "", bad("MCP history requires a server label")
	}
	if !validResponseID(id) {
		return "", bad("MCP history requires a scoped item ID")
	}
	return id, nil
}

// Strip credential fields from Responses envelopes before returning or caching
// them. RawMessage preserves numbers and leaves unrelated payloads untouched.
func sanitizeResponseMCP(raw []byte) ([]byte, error) {
	if !bytes.ContainsAny(raw, "\\") && !bytes.Contains(raw, []byte("headers")) && !bytes.Contains(raw, []byte("authorization")) {
		return raw, nil
	}
	var clean func(json.RawMessage, int) (json.RawMessage, bool, error)
	clean = func(raw json.RawMessage, depth int) (json.RawMessage, bool, error) {
		if depth > 32 {
			return nil, false, &apiError{502, "upstream response envelope exceeds nesting limit"}
		}
		value := bytes.TrimSpace(raw)
		if len(value) == 0 {
			return raw, false, nil
		}
		changed := false
		if value[0] == '[' {
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) != nil {
				return nil, false, &apiError{502, "invalid response array"}
			}
			for i, item := range items {
				result, modified, err := clean(item, depth+1)
				if err != nil {
					return nil, false, err
				}
				items[i], changed = result, changed || modified
			}
			if changed {
				out, err := json.Marshal(items)
				return out, true, err
			}
		} else if value[0] == '{' {
			var object map[string]json.RawMessage
			if json.Unmarshal(raw, &object) != nil {
				return nil, false, &apiError{502, "invalid response envelope"}
			}
			if credentialString(object, "type") == "mcp" {
				for _, field := range []string{"headers", "authorization"} {
					if object[field] != nil {
						delete(object, field)
						changed = true
					}
				}
			} else {
				for _, field := range []string{"response", "item", "tools", "input", "output", "data"} {
					if child := object[field]; child != nil {
						result, modified, err := clean(child, depth+1)
						if err != nil {
							return nil, false, err
						}
						object[field], changed = result, changed || modified
					}
				}
			}
			if changed {
				out, err := json.Marshal(object)
				return out, true, err
			}
		}
		return raw, false, nil
	}
	result, _, err := clean(raw, 0)
	return result, err
}
