package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Only documented read parameters reach the original response's upstream.
func responseResourceQuery(r *http.Request) (url.Values, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(r.URL.RawQuery) > 4096 {
		return nil, bad("invalid response query")
	}
	items := strings.HasSuffix(r.URL.Path, "/input_items")
	for name, values := range query {
		if r.Method != "GET" || len(values) != 1 && name != "include" && name != "include[]" {
			return nil, bad("invalid response query parameter")
		}
		switch name {
		case "include", "include[]":
			if len(values) > 8 {
				return nil, bad("too many response include values")
			}
			for _, value := range values {
				switch value {
				case "file_search_call.results", "web_search_call.results", "web_search_call.action.sources", "message.input_image.image_url", "computer_call_output.output.image_url", "code_interpreter_call.outputs", "reasoning.encrypted_content", "message.output_text.logprobs":
				default:
					return nil, bad("invalid response include value")
				}
			}
		case "stream", "include_obfuscation":
			if items || values[0] != "true" && values[0] != "false" {
				return nil, bad("invalid response stream parameter")
			}
		case "starting_after":
			if n, err := strconv.ParseInt(values[0], 10, 64); items || err != nil || n < 0 || query.Get("stream") != "true" {
				return nil, bad("invalid starting_after")
			}
		case "after":
			if !items || !validResponseID(values[0]) {
				return nil, bad("invalid input item cursor")
			}
		case "limit":
			if n, err := strconv.Atoi(values[0]); !items || err != nil || n < 1 || n > 100 {
				return nil, bad("input item limit must be 1 to 100")
			}
		case "order":
			if !items || values[0] != "asc" && values[0] != "desc" {
				return nil, bad("invalid input item order")
			}
		default:
			return nil, bad("invalid response query parameter")
		}
	}
	if query.Has("include") && query.Has("include[]") {
		return nil, bad("use one response include parameter form")
	}
	return query, nil
}

// The caller has authenticated the Key and resolved ownership before this read.
// Retrieval is not a new generation and never creates another billing receipt.
func (a *App) readResponseResource(w http.ResponseWriter, r *http.Request, u *upstreamAccount, id string, query url.Values) error {
	release, err := a.acquireAccountSlot(r.Context(), u.ID)
	if err != nil {
		return err
	}
	defer release()
	path := "/v1/responses/" + id
	items := strings.HasSuffix(r.URL.Path, "/input_items")
	if items {
		path += "/input_items"
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	resp, err := a.upstreamRequest(r.Context(), u, "GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return missing()
	}
	if resp.StatusCode != 200 {
		return a.upstreamError(r.Context(), u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "upstream response resource query rejected"})
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		return &apiError{502, "response resource interrupted or oversized"}
	}
	if err = validateResponseResource(raw, id, items); err != nil {
		return err
	}
	raw, err = sanitizeResponseTools(raw)
	if err != nil {
		return err
	}
	return rawReply(w, json.RawMessage(raw))
}

func validateResponseResource(raw []byte, id string, items bool) error {
	invalid := &apiError{502, "invalid upstream response resource"}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil || result["error"] != nil && string(result["error"]) != "null" {
		return invalid
	}
	if !items {
		if credentialString(result, "id") != id || credentialString(result, "object") != "response" {
			return invalid
		}
		// This path is only for already settled synchronous or background work.
		switch credentialString(result, "status") {
		case "completed", "incomplete":
			return nil
		default:
			return invalid
		}
	}
	var data []map[string]json.RawMessage
	var more *bool
	if credentialString(result, "object") != "list" || json.Unmarshal(result["data"], &data) != nil || data == nil || len(data) > 100 || json.Unmarshal(result["has_more"], &more) != nil || more == nil {
		return invalid
	}
	seen := map[string]bool{}
	for _, item := range data {
		id := credentialString(item, "id")
		if !validResponseID(id) || seen[id] || credentialString(item, "type") == "" {
			return invalid
		}
		seen[id] = true
	}
	if len(data) > 0 {
		if credentialString(result, "first_id") != credentialString(data[0], "id") || credentialString(result, "last_id") != credentialString(data[len(data)-1], "id") {
			return invalid
		}
	} else if *more || credentialString(result, "first_id") != "" || credentialString(result, "last_id") != "" {
		return invalid
	}
	return nil
}

func (a *App) storedResponseLookup(w http.ResponseWriter, r *http.Request, g *gatewayIdentity, id string, query url.Values) error {
	binding, err := a.previousResponse(r.Context(), g, id)
	if binding != nil && (binding.MCPTool || binding.CodeTool || binding.ProgrammaticTool) {
		r = r.WithContext(context.WithValue(r.Context(), responseSecretsKey{}, true))
	}
	if err != nil {
		return err
	}
	if binding.History != "" {
		return bad("response resource lookup requires a native Responses account")
	}
	if query.Get("stream") == "true" {
		return bad("stream retrieval requires a tracked background response")
	}
	u, err := a.loadAccount(r.Context(), binding.AccountID)
	if err != nil {
		return err
	}
	if u.protocol() != "responses" || responseTarget(u) != binding.Target {
		return conflict("response upstream source changed; restore the original account")
	}
	if err = a.gatewayRPM(r.Context(), g); err != nil {
		return err
	}
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		return &apiError{429, "user concurrency limit reached"}
	}
	defer a.releaseSlot("user", g.UserID)
	return a.readResponseResource(w, r, u, id, query)
}
