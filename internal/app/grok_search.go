package app

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func grokSearchProtocol(protocol string) bool {
	return protocol == "web_search" || protocol == "x_search"
}

type grokSearchRequest struct {
	Query                    string   `json:"query"`
	Input                    string   `json:"input"`
	MaxResults               int      `json:"max_results"`
	AllowedXHandles          []string `json:"allowed_x_handles"`
	ExcludedXHandles         []string `json:"excluded_x_handles"`
	FromDate                 string   `json:"from_date"`
	ToDate                   string   `json:"to_date"`
	EnableImageUnderstanding *bool    `json:"enable_image_understanding"`
	EnableVideoUnderstanding *bool    `json:"enable_video_understanding"`
}

func parseGrokSearch(body map[string]json.RawMessage) (*grokSearchRequest, error) {
	raw, _ := json.Marshal(body)
	var in grokSearchRequest
	if json.Unmarshal(raw, &in) != nil {
		return nil, bad("invalid search request")
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		in.Query = strings.TrimSpace(in.Input)
	}
	if in.Query == "" {
		return nil, bad("query is required")
	}
	if raw := body["stream"]; raw != nil && string(raw) != "false" {
		return nil, bad("standalone search does not stream")
	}
	if in.MaxResults <= 0 {
		in.MaxResults = 5
	}
	if in.MaxResults > 20 {
		in.MaxResults = 20
	}
	return &in, nil
}

func (in *grokSearchRequest) upstreamBody(protocol, model string) []byte {
	tool := map[string]any{"type": protocol}
	body := map[string]any{
		"model": model, "tools": []any{tool}, "include": []string{protocol + "_call.action.sources"},
		"store": false, "stream": false,
	}
	location := "the web"
	if protocol == "x_search" {
		location = "X"
		body["tool_choice"] = "required"
		if len(in.AllowedXHandles) > 0 {
			tool["allowed_x_handles"] = in.AllowedXHandles
		}
		if len(in.ExcludedXHandles) > 0 {
			tool["excluded_x_handles"] = in.ExcludedXHandles
		}
		if strings.TrimSpace(in.FromDate) != "" {
			tool["from_date"] = strings.TrimSpace(in.FromDate)
		}
		if strings.TrimSpace(in.ToDate) != "" {
			tool["to_date"] = strings.TrimSpace(in.ToDate)
		}
		if in.EnableImageUnderstanding != nil {
			tool["enable_image_understanding"] = *in.EnableImageUnderstanding
		}
		if in.EnableVideoUnderstanding != nil {
			tool["enable_video_understanding"] = *in.EnableVideoUnderstanding
		}
	}
	body["input"] = fmt.Sprintf("Search %s for the query below. Return only JSON shaped as {\"results\":[{\"url\":\"https://...\",\"title\":\"title\",\"snippet\":\"summary\"}]}, with at most %d unique results. Every URL must be an actual %s source. Include a nonempty title and factual snippet. Do not use markdown.\n\nUser query:\n%s", location, in.MaxResults, protocol, in.Query)
	raw, _ := json.Marshal(body)
	return raw
}

type searchResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

func searchURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

func searchTitle(title, link string) string {
	title = strings.TrimSpace(title)
	if title == link {
		return ""
	}
	if _, err := strconv.Atoi(title); err == nil {
		return ""
	}
	return title
}

func (in *grokSearchRequest) response(raw []byte) ([]byte, string, error) {
	var response struct {
		Model, Status string
		Error         json.RawMessage
		Output        []struct {
			Type    string
			Action  struct{ Sources []searchResult }
			Content []struct {
				Type, Text  string
				Annotations []struct {
					Type string
					searchResult
				}
			}
		}
	}
	if json.Unmarshal(raw, &response) != nil || response.Output == nil ||
		(response.Error != nil && string(response.Error) != "null") ||
		(response.Status != "" && response.Status != "completed") {
		return nil, "", &apiError{502, "upstream search response is invalid or incomplete"}
	}
	sources := map[string]searchResult{}
	order := []string{}
	add := func(result searchResult) {
		key := searchURL(result.URL)
		if key == "" {
			return
		}
		previous, exists := sources[key]
		if !exists {
			previous.URL = strings.TrimSpace(result.URL)
			order = append(order, key)
		}
		if previous.Title == "" {
			previous.Title = searchTitle(result.Title, previous.URL)
		}
		if previous.Snippet == "" {
			previous.Snippet = strings.TrimSpace(result.Snippet)
		}
		sources[key] = previous
	}
	structured := []searchResult{}
	for _, item := range response.Output {
		if item.Type == "web_search_call" || item.Type == "x_search_call" {
			for _, result := range item.Action.Sources {
				add(result)
			}
		}
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type != "output_text" {
				continue
			}
			for _, ann := range part.Annotations {
				if ann.Type == "url_citation" || ann.Type == "web" {
					add(ann.searchResult)
				}
			}
			start, end := strings.IndexByte(part.Text, '{'), strings.LastIndexByte(part.Text, '}')
			var parsed struct{ Results []searchResult }
			if start >= 0 && end >= start && json.Unmarshal([]byte(part.Text[start:end+1]), &parsed) == nil {
				structured = append(structured, parsed.Results...)
			}
		}
	}
	results := []searchResult{}
	seen := map[string]bool{}
	// Model text may enrich a cited source, but cannot invent a search result.
	for _, result := range structured {
		key := searchURL(result.URL)
		source, exists := sources[key]
		if !exists || seen[key] || len(results) >= in.MaxResults {
			continue
		}
		if title := searchTitle(result.Title, result.URL); title != "" {
			source.Title = title
		}
		if snippet := strings.TrimSpace(result.Snippet); snippet != "" {
			source.Snippet = snippet
		}
		results, seen[key] = append(results, source), true
	}
	for _, key := range order {
		if !seen[key] && len(results) < in.MaxResults {
			results = append(results, sources[key])
		}
	}
	for i := range results {
		if results[i].Title == "" {
			u, _ := url.Parse(results[i].URL)
			results[i].Title = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		}
	}
	body, err := json.Marshal(map[string]any{"query": in.Query, "results": results, "provider": "grok-native", "max_results": in.MaxResults})
	return body, response.Model, err
}
