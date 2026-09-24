package app

import (
	"encoding/json"
	"strings"
	"time"
)

func hostedSearchTool(kind string) bool {
	switch kind {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11", "x_search":
		return true
	}
	return false
}

// web_search has different options on each platform. Validate after composite
// routing, before selecting an account, rather than interpreting a model prefix.
func validateHostedSearchTools(body map[string]json.RawMessage, platform string) error {
	if platform != "openai" && platform != "grok" {
		return bad("hosted search requires an OpenAI or Grok target")
	}
	tools, err := responseClientTools(body)
	if err != nil {
		return err
	}
	for _, tool := range tools {
		kind := credentialString(tool, "type")
		if !hostedSearchTool(kind) {
			continue
		}
		if platform == "openai" {
			err = validateOpenAIHostedSearch(tool)
		} else if !grokSearchProtocol(kind) {
			err = bad("unsupported Grok search tool")
		} else {
			err = validateGrokHostedSearch(tool)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func validateOpenAIHostedSearch(tool map[string]json.RawMessage) error {
	kind := credentialString(tool, "type")
	invalid := func() error { return bad("invalid OpenAI web search option") }
	if !hostedSearchTool(kind) || kind == "x_search" {
		return invalid()
	}
	preview := strings.HasPrefix(kind, "web_search_preview")
	for name, raw := range tool {
		switch name {
		case "type":
		case "search_context_size":
			value := credentialString(tool, name)
			if value != "low" && value != "medium" && value != "high" {
				return invalid()
			}
		case "external_web_access":
			if preview || string(raw) != "true" && string(raw) != "false" {
				return invalid()
			}
		case "return_token_budget":
			value := credentialString(tool, name)
			if preview || value != "default" && value != "unlimited" {
				return invalid()
			}
		case "filters":
			if preview {
				return invalid()
			}
			if string(raw) == "null" {
				continue
			}
			var filters map[string]json.RawMessage
			if json.Unmarshal(raw, &filters) != nil || filters == nil {
				return invalid()
			}
			for key, value := range filters {
				if key != "allowed_domains" && key != "blocked_domains" {
					return invalid()
				}
				if string(value) == "null" {
					continue
				}
				var domains []string
				if json.Unmarshal(value, &domains) != nil || len(domains) > 100 {
					return invalid()
				}
				for _, domain := range domains {
					if !validDomainName(domain) {
						return invalid()
					}
				}
			}
		case "user_location":
			if string(raw) == "null" {
				continue
			}
			var location map[string]json.RawMessage
			if json.Unmarshal(raw, &location) != nil || location == nil {
				return invalid()
			}
			for key, raw := range location {
				if key != "type" && key != "country" && key != "city" && key != "region" && key != "timezone" {
					return invalid()
				}
				if key != "type" && string(raw) == "null" {
					continue
				}
				var value string
				if json.Unmarshal(raw, &value) != nil || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n") {
					return invalid()
				}
				if key == "type" && value != "approximate" {
					return invalid()
				}
				if key == "country" && (len(value) != 2 || value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z') {
					return invalid()
				}
				if key == "timezone" {
					if _, err := time.LoadLocation(value); err != nil || value == "" || value == "Local" {
						return invalid()
					}
				}
			}
		default:
			return invalid()
		}
	}
	return nil
}

func validDomainName(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
