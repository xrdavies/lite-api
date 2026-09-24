package app

import (
	"bytes"
	"encoding/json"
)

func searchCallArguments(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = json.RawMessage(text)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, bad("tool search arguments must be a JSON object")
	}
	return json.Marshal(object)
}

func validateResponseClientTool(tool map[string]json.RawMessage) error {
	switch credentialString(tool, "type") {
	case "namespace":
		_, err := responseNamespaceChildren(tool)
		return err
	case "function", "custom":
		return nil
	case "tool_search":
		if credentialString(tool, "execution") != "client" {
			return bad("hosted tool search requires a native OpenAI Responses account")
		}
		if raw := tool["parameters"]; raw != nil {
			var schema map[string]json.RawMessage
			if json.Unmarshal(raw, &schema) != nil || schema == nil || credentialString(schema, "type") != "object" {
				return bad("tool search parameters must be an object schema")
			}
		}
		return nil
	default:
		return bad("hosted tool billing is not yet available")
	}
}

func hostedToolSearch(tool map[string]json.RawMessage) bool {
	return credentialString(tool, "type") == "tool_search" && (tool["execution"] == nil || string(tool["execution"]) == `"server"`)
}

func validateHostedToolSearch(tool map[string]json.RawMessage) error {
	for field, raw := range tool {
		switch field {
		case "type", "execution":
		case "description", "parameters":
			if string(raw) != "null" {
				return bad("hosted tool search does not accept client search configuration")
			}
		default:
			return bad("unsupported hosted tool search option")
		}
	}
	return nil
}

// Server search only loads client tool definitions. It never grants admission
// to remote MCP servers, shared files, or other independently billed tools.
func serverSearchItem(item map[string]json.RawMessage) error {
	if raw := item["call_id"]; raw != nil && string(raw) != "null" {
		return bad("server tool search history requires a null call ID")
	}
	if raw := item["id"]; raw != nil && string(raw) != "null" && !validResponseID(credentialString(item, "id")) {
		return bad("invalid server tool search item ID")
	}
	if raw := item["status"]; raw != nil && string(raw) != "null" {
		switch credentialString(item, "status") {
		case "in_progress", "completed", "incomplete":
		default:
			return bad("invalid server tool search status")
		}
	}
	if credentialString(item, "type") == "tool_search_call" {
		_, err := searchCallArguments(item["arguments"])
		return err
	}
	if raw := item["tools"]; raw == nil || string(raw) == "null" {
		return bad("server tool search output requires tools")
	}
	_, err := searchDiscoveredTools(item)
	return err
}

func clientSearchItem(item map[string]json.RawMessage) error {
	if raw := item["execution"]; raw != nil && string(raw) != `"client"` {
		return bad("only client tool search history is supported")
	}
	if id := credentialString(item, "call_id"); id == "" || len(id) > 256 {
		return bad("tool search history requires a call ID")
	}
	return nil
}

func searchOutputTools(item map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	if err := clientSearchItem(item); err != nil {
		return nil, err
	}
	return searchDiscoveredTools(item)
}

func searchDiscoveredTools(item map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	var discoveries []map[string]json.RawMessage
	if raw := item["tools"]; raw != nil && json.Unmarshal(raw, &discoveries) != nil {
		return nil, bad("invalid discovered tools")
	}
	for _, tool := range discoveries {
		if err := validateResponseClientTool(tool); err != nil {
			return nil, err
		}
		if kind := credentialString(tool, "type"); kind == "tool_search" || hostedSearchTool(kind) {
			return nil, bad("discovery must return executable tools")
		}
	}
	if raw := item["status"]; raw != nil && string(raw) != `"completed"` {
		return nil, nil
	}
	return discoveries, nil
}

// Canonical JSON compares schema keys without rounding JSON numbers through float64.
func responseToolDefinition(tool map[string]json.RawMessage) string {
	raw, _ := json.Marshal(tool)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&value)
	canonical, _ := json.Marshal(value)
	return string(canonical)
}

// Promote completed client discoveries for function-only upstreams. The native
// request keeps its original history; admission still validates every discovery.
func mergeResponseDiscoveries(tools, items []map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	known := map[string]string{}
	identity := func(tool map[string]json.RawMessage, namespace string) (string, string, error) {
		name := credentialString(tool, "name")
		if name == "" || len(name) > 256 {
			return "", "", bad("invalid discovered tool name")
		}
		key := name
		if namespace != "" {
			key = flattenedToolName(namespace, name)
		}
		return key, namespace + "\x00" + responseToolDefinition(tool), nil
	}
	for _, tool := range tools {
		if kind := credentialString(tool, "type"); kind == "tool_search" || hostedSearchTool(kind) || kind == "image_generation" || responseLocalTool(kind) || kind == "mcp" {
			continue
		}
		ns := ""
		children := []map[string]json.RawMessage{tool}
		if credentialString(tool, "type") == "namespace" {
			var err error
			children, err = responseNamespaceChildren(tool)
			if err != nil {
				return nil, err
			}
			ns = credentialString(tool, "name")
		}
		for _, child := range children {
			key, definition, err := identity(child, ns)
			if err != nil {
				return nil, err
			}
			if old, exists := known[key]; exists && old != definition {
				known[key] = ""
			} else {
				known[key] = definition
			}
		}
	}
	for _, item := range items {
		kind := credentialString(item, "type")
		if (kind == "tool_search_call" || kind == "tool_search_output") && credentialString(item, "execution") == "server" {
			if err := serverSearchItem(item); err != nil {
				return nil, err
			}
			// Preserve native server discoveries in input, without promoting them
			// into declarations or pretending the client executed a function.
			continue
		}
		switch credentialString(item, "type") {
		case "tool_search_call":
			if err := clientSearchItem(item); err != nil {
				return nil, err
			}
			if _, err := searchCallArguments(item["arguments"]); err != nil {
				return nil, err
			}
			continue
		case "tool_search_output":
		default:
			continue
		}
		discoveries, err := searchOutputTools(item)
		if err != nil {
			return nil, err
		}
		for _, tool := range discoveries {
			ns := ""
			children := []map[string]json.RawMessage{tool}
			if credentialString(tool, "type") == "namespace" {
				children, _ = responseNamespaceChildren(tool)
				ns = credentialString(tool, "name")
			}
			added := []map[string]json.RawMessage{}
			for _, child := range children {
				key, definition, err := identity(child, ns)
				if err != nil {
					return nil, err
				}
				if old, exists := known[key]; exists {
					if old != definition {
						return nil, bad("discovered tool conflicts with a declared tool")
					}
					continue
				}
				known[key] = definition
				added = append(added, child)
			}
			if len(added) == 0 {
				continue
			}
			if ns == "" {
				tools = append(tools, tool)
			} else {
				copy := make(map[string]json.RawMessage, len(tool))
				for key, value := range tool {
					copy[key] = value
				}
				delete(copy, "children")
				copy["tools"], _ = json.Marshal(added)
				tools = append(tools, copy)
			}
		}
	}
	return tools, nil
}
