package app

import (
	"encoding/json"
	"slices"
	"strconv"
)

const responseStoresKey = "response_vector_stores"
const responseFilesKey = "response_files"
const responseSkillsKey = "response_skills"

// Administrators grant existing provider resources to gateway groups. The grant
// is tied to the credential source, so rotation cannot authorize another resource
// that happens to have the same ID at a different provider.
func parseResponseResourceGrants(raw json.RawMessage) (map[string][]string, error) {
	var grants map[string][]string
	if json.Unmarshal(raw, &grants) != nil || grants == nil || len(grants) > 1000 {
		return nil, bad("resource grants must map group IDs to resource ID lists")
	}
	for group, ids := range grants {
		id, err := strconv.ParseInt(group, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != group || len(ids) > 100 {
			return nil, bad("invalid resource group grant")
		}
		seen := map[string]bool{}
		for _, store := range ids {
			if !validResponseID(store) || seen[store] {
				return nil, bad("invalid or duplicate resource ID")
			}
			seen[store] = true
		}
	}
	return grants, nil
}

func (in *accountInput) bindResponseResourceGrants(u *upstreamAccount) error {
	for _, key := range []string{responseStoresKey, responseFilesKey, responseSkillsKey} {
		if in.Extra[key] == nil {
			continue
		}
		if u.Platform != "openai" || u.protocol() != "responses" && (key != responseFilesKey || u.protocol() != "chat_completions") {
			return bad("resource grants require a compatible OpenAI account")
		}
		in.Extra[key+"_target"], _ = json.Marshal(responseTarget(u))
	}
	return nil
}

func (u *upstreamAccount) allowsResponseResources(key string, groupID int64, ids []string) bool {
	if len(ids) == 0 {
		return true
	}
	if credentialString(u.Extra, key+"_target") != responseTarget(u) {
		return false
	}
	grants, err := parseResponseResourceGrants(u.Extra[key])
	if err != nil {
		return false
	}
	for _, id := range ids {
		if !slices.Contains(grants[strconv.FormatInt(groupID, 10)], id) {
			return false
		}
	}
	return true
}

// Inspect only protocol resource positions, never arbitrary function arguments,
// document text, output citations, or JSON schemas that happen to say file_id.
func requestFileIDs(body map[string]json.RawMessage, tools []map[string]json.RawMessage, protocol string) ([]string, error) {
	var ids []string
	add := func(raw json.RawMessage) error {
		var id string
		if json.Unmarshal(raw, &id) != nil || !validResponseID(id) {
			return bad("invalid input file ID")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		if len(ids) > 100 {
			return bad("too many input file IDs")
		}
		return nil
	}
	part := func(p map[string]json.RawMessage) error {
		raw := p["file_id"]
		if raw == nil || string(raw) == "null" {
			return nil
		}
		for _, alternative := range []string{"file_data", "file_url", "image_url"} {
			if value := p[alternative]; value != nil && string(value) != "null" {
				return bad("use one file input source")
			}
		}
		return add(raw)
	}
	var items []map[string]json.RawMessage
	input := body["input"]
	if protocol == "chat_completions" {
		input = body["messages"]
	}
	if err := json.Unmarshal(input, &items); err != nil {
		var text string
		if protocol == "chat_completions" || input != nil && json.Unmarshal(input, &text) != nil {
			return nil, bad("invalid file input items")
		}
	}
	for _, item := range items {
		if err := part(item); err != nil {
			return nil, err
		}
		for _, field := range []string{"content", "output"} {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(item[field], &parts) != nil {
				var object map[string]json.RawMessage
				if json.Unmarshal(item[field], &object) == nil && object != nil {
					parts = []map[string]json.RawMessage{object}
				}
			}
			for _, p := range parts {
				if err := part(p); err != nil {
					return nil, err
				}
				if protocol == "chat_completions" && credentialString(p, "type") == "file" {
					var file map[string]json.RawMessage
					if json.Unmarshal(p["file"], &file) != nil || file == nil {
						return nil, bad("invalid Chat file input")
					}
					if err := part(file); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	var prompt struct{ Variables map[string]json.RawMessage }
	_ = json.Unmarshal(body["prompt"], &prompt)
	for _, raw := range prompt.Variables {
		var variable map[string]json.RawMessage
		if json.Unmarshal(raw, &variable) == nil {
			if err := part(variable); err != nil {
				return nil, err
			}
		}
	}
	for _, tool := range tools {
		if kind := credentialString(tool, "type"); kind == "code_interpreter" || kind == "shell" {
			field := "container"
			if kind == "shell" {
				field = "environment"
			}
			var container struct {
				Files []json.RawMessage `json:"file_ids"`
			}
			if json.Unmarshal(tool[field], &container) == nil {
				for _, raw := range container.Files {
					if err := add(raw); err != nil {
						return nil, err
					}
				}
			}
		}
		if credentialString(tool, "type") == "image_generation" {
			var mask map[string]json.RawMessage
			_ = json.Unmarshal(tool["input_image_mask"], &mask)
			if err := part(mask); err != nil {
				return nil, err
			}
		}
	}
	slices.Sort(ids)
	return ids, nil
}

func responseFileSearch(tool map[string]json.RawMessage) ([]string, error) {
	var ids []string
	if json.Unmarshal(tool["vector_store_ids"], &ids) != nil || len(ids) == 0 || len(ids) > 100 {
		return nil, bad("file_search requires 1 to 100 vector store IDs")
	}
	for _, id := range ids {
		if !validResponseID(id) {
			return nil, bad("invalid vector store ID")
		}
	}
	for key, raw := range tool {
		switch key {
		case "type", "vector_store_ids":
		case "max_num_results":
			var n int
			if json.Unmarshal(raw, &n) != nil || n < 1 || n > 50 {
				return nil, bad("file search result limit must be 1 to 50")
			}
		case "filters":
			if string(raw) != "null" {
				nodes := 0
				if err := responseFileFilter(raw, 0, &nodes); err != nil {
					return nil, err
				}
			}
		case "ranking_options":
			var options map[string]json.RawMessage
			if json.Unmarshal(raw, &options) != nil || options == nil {
				return nil, bad("invalid file search ranking options")
			}
			for key, raw := range options {
				switch key {
				case "ranker":
					if ranker := credentialString(options, key); ranker != "auto" && ranker != "default-2024-11-15" {
						return nil, bad("unsupported file search ranker")
					}
				case "score_threshold":
					var n *float64
					if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 || *n > 1 {
						return nil, bad("invalid file search score threshold")
					}
				case "hybrid_search":
					var weights map[string]json.RawMessage
					if json.Unmarshal(raw, &weights) != nil || len(weights) != 2 {
						return nil, bad("invalid file search hybrid weights")
					}
					var total float64
					for _, key := range []string{"embedding_weight", "text_weight"} {
						var n *float64
						if json.Unmarshal(weights[key], &n) != nil || n == nil || *n < 0 {
							return nil, bad("invalid file search hybrid weight")
						}
						total += *n
					}
					if total <= 0 {
						return nil, bad("file search hybrid weights must have a positive sum")
					}
				default:
					return nil, bad("unsupported file search ranking option")
				}
			}
		default:
			return nil, bad("unsupported file search option")
		}
	}
	return mergeResponseResources(nil, ids)
}

func responseFileFilter(raw json.RawMessage, depth int, nodes *int) error {
	*nodes++
	var filter map[string]json.RawMessage
	if depth > 8 || *nodes > 256 || json.Unmarshal(raw, &filter) != nil || filter == nil {
		return bad("invalid or excessive file search filter")
	}
	kind := credentialString(filter, "type")
	if kind == "and" || kind == "or" {
		var children []json.RawMessage
		if len(filter) != 2 || json.Unmarshal(filter["filters"], &children) != nil || len(children) == 0 || len(children) > 256 {
			return bad("invalid compound file search filter")
		}
		for _, child := range children {
			if err := responseFileFilter(child, depth+1, nodes); err != nil {
				return err
			}
		}
		return nil
	}
	switch kind {
	case "eq", "ne", "gt", "gte", "lt", "lte", "in", "nin":
	default:
		return bad("invalid file search filter operator")
	}
	if len(filter) != 3 || credentialString(filter, "key") == "" || len(credentialString(filter, "key")) > 256 {
		return bad("invalid file search filter key")
	}
	values := []json.RawMessage{filter["value"]}
	list := kind == "in" || kind == "nin"
	if list && (json.Unmarshal(filter["value"], &values) != nil || len(values) == 0 || len(values) > 256) {
		return bad("invalid file search filter values")
	}
	for _, raw := range values {
		var value any
		if json.Unmarshal(raw, &value) != nil {
			return bad("invalid file search filter value")
		}
		switch value.(type) {
		case string, float64:
		case bool:
			if list {
				return bad("file search filter lists require strings or numbers")
			}
		default:
			return bad("invalid file search filter value")
		}
	}
	return nil
}

func responseFileSearchItem(item map[string]json.RawMessage) error {
	for key := range item {
		if key != "type" && key != "id" && key != "status" && key != "queries" && key != "results" {
			return bad("unsupported file search history field")
		}
	}
	switch credentialString(item, "status") {
	case "in_progress", "searching", "completed", "incomplete", "failed":
	default:
		return bad("invalid file search status")
	}
	var queries []string
	if json.Unmarshal(item["queries"], &queries) != nil || queries == nil || len(queries) > 100 {
		return bad("invalid file search queries")
	}
	if raw := item["results"]; raw != nil && string(raw) != "null" {
		var results []map[string]json.RawMessage
		if json.Unmarshal(raw, &results) != nil || len(results) > 1000 {
			return bad("invalid file search results")
		}
		for _, result := range results {
			if result == nil {
				return bad("invalid file search result")
			}
		}
	}
	return nil
}

func mergeResponseResources(previous, current []string) ([]string, error) {
	ids := append([]string(nil), previous...)
	for _, id := range current {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 100 {
		return nil, bad("too many response resource IDs")
	}
	slices.Sort(ids)
	return ids, nil
}
