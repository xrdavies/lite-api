package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const responseExtensionsSetting = "responses_extension_paths"

type responseExtensionsKey struct{}

// Registered paths use the ordinary Responses request, result and usage contract.
// Only an explicit placeholder grants access to a previously owned response.
func validateResponseExtensions(paths []string) error {
	if len(paths) > 64 {
		return bad("too many Responses extension paths")
	}
	for i, path := range paths {
		parts := strings.Split(path, "/")
		if !safeResponsesAction(strings.ReplaceAll(path, "{response_id}", "r")) {
			return bad("invalid Responses extension path")
		}
		for _, part := range parts {
			if part == "compact" || part == "input_tokens" || part == "input_items" || part == "cancel" {
				return bad("built-in Responses operations cannot be registered as extensions")
			}
			if strings.ContainsAny(part, "{}") && part != "{response_id}" {
				return bad("invalid Responses resource placeholder")
			}
		}
		for _, previous := range paths[:i] {
			other := strings.Split(previous, "/")
			if len(other) != len(parts) {
				continue
			}
			overlaps := true
			for j := range parts {
				if parts[j] != other[j] && parts[j] != "{response_id}" && other[j] != "{response_id}" {
					overlaps = false
				}
			}
			if overlaps {
				return bad("overlapping Responses extension paths")
			}
		}
	}
	return nil
}

func safeResponsesAction(action string) bool {
	if action == "" || len("/backend-api/codex/responses/")+len(action) > 128 {
		return false
	}
	parts := strings.Split(action, "/")
	if len(parts) > 8 {
		return false
	}
	for _, part := range parts {
		if len(part) > 128 || strings.Trim(part, ".") == "" {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
				return false
			}
		}
	}
	return true
}

func (a *App) loadResponseExtensions(ctx context.Context) ([]string, error) {
	paths := []string{}
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", responseExtensionsSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return paths, nil
	}
	if err != nil || json.Unmarshal([]byte(raw), &paths) != nil || paths == nil || validateResponseExtensions(paths) != nil {
		return nil, &apiError{503, "Responses extension configuration unavailable"}
	}
	return paths, nil
}

func matchResponseExtension(ctx context.Context, action string) ([]string, bool) {
	paths, _ := ctx.Value(responseExtensionsKey{}).([]string)
	parts := strings.Split(action, "/")
	for _, path := range paths {
		pattern := strings.Split(path, "/")
		if len(pattern) != len(parts) {
			continue
		}
		var resources []string
		matched := true
		for i, part := range pattern {
			if part == "{response_id}" && validResponseID(parts[i]) {
				resources = append(resources, parts[i])
			} else if part != parts[i] {
				matched = false
				break
			}
		}
		if matched {
			return resources, true
		}
	}
	return nil, false
}
