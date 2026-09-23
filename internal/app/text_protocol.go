package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

type textRequest struct {
	Protocol, Scope, Model, Effort, Tier, Action string
	Stream, CountOnly                            bool
	Headers                                      http.Header
}

func parseTextRequest(r *http.Request, protocol string, body map[string]json.RawMessage) (textRequest, error) {
	in := textRequest{Protocol: protocol, Headers: http.Header{}}
	if protocol == "gemini" {
		model, action, ok := strings.Cut(r.PathValue("action"), ":")
		if !ok || !validNativeModel(model) {
			return in, bad("invalid Gemini model action")
		}
		switch action {
		case "generateContent", "streamGenerateContent", "countTokens":
		default:
			return in, missing()
		}
		in.Model, in.Action, in.Scope = model, action, "gemini."+action
		in.Stream, in.CountOnly = action == "streamGenerateContent", action == "countTokens"
		if alt := r.URL.Query().Get("alt"); alt != "" && alt != "sse" {
			return in, bad("only SSE streaming is supported")
		}
		var contents []json.RawMessage
		contentsRaw := body["contents"]
		if raw := body["generateContentRequest"]; raw != nil {
			var nested map[string]json.RawMessage
			if !in.CountOnly || contentsRaw != nil || json.Unmarshal(raw, &nested) != nil || nested == nil {
				return in, bad("countTokens requires either contents or generateContentRequest")
			}
			var nestedModel string
			if raw := nested["model"]; raw != nil && (json.Unmarshal(raw, &nestedModel) != nil || strings.TrimPrefix(nestedModel, "models/") != model) {
				return in, bad("token count model must match the URL model")
			}
			contentsRaw = nested["contents"]
		}
		if json.Unmarshal(contentsRaw, &contents) != nil || len(contents) == 0 {
			return in, bad("contents are required")
		}
		var config struct {
			Thinking struct {
				Level string `json:"thinkingLevel"`
			} `json:"thinkingConfig"`
			Modalities []string `json:"responseModalities"`
		}
		if raw := body["generationConfig"]; raw != nil && json.Unmarshal(raw, &config) != nil {
			return in, bad("invalid generationConfig")
		}
		for _, modality := range config.Modalities {
			if strings.ToUpper(modality) != "TEXT" {
				return in, bad("media generation is not yet available on this endpoint")
			}
		}
		in.Effort = strings.ToLower(config.Thinking.Level)
		if len(in.Effort) > 20 {
			return in, bad("invalid thinkingLevel")
		}
		return in, nil
	}
	in.Scope = "chat"
	if protocol == "anthropic" {
		in.Scope = "messages"
		in.CountOnly = strings.HasSuffix(r.URL.Path, "/count_tokens")
		if in.CountOnly {
			in.Scope += ".count_tokens"
		}
		for _, name := range []string{"Anthropic-Version", "Anthropic-Beta"} {
			value := r.Header.Get(name)
			if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
				return in, bad("invalid protocol header")
			}
			in.Headers.Set(name, value)
		}
		if !in.CountOnly {
			var max int64
			if json.Unmarshal(body["max_tokens"], &max) != nil || max < 1 || max > 2147483647 {
				return in, bad("max_tokens must be a positive integer")
			}
		}
	}
	if json.Unmarshal(body["model"], &in.Model) != nil || !validModelPattern(in.Model) || len(in.Model) > 100 || strings.Contains(in.Model, "*") {
		return in, bad("invalid model")
	}
	if raw := body["stream"]; raw != nil && json.Unmarshal(raw, &in.Stream) != nil {
		return in, bad("invalid stream flag")
	}
	if in.Stream && in.CountOnly {
		return in, bad("token counting does not stream")
	}
	if raw := body["reasoning_effort"]; raw != nil && (json.Unmarshal(raw, &in.Effort) != nil || len(in.Effort) > 20) {
		return in, bad("invalid reasoning_effort")
	}
	if raw := body["service_tier"]; raw != nil && (json.Unmarshal(raw, &in.Tier) != nil || len(in.Tier) > 16) {
		return in, bad("invalid service_tier")
	}
	var messages []json.RawMessage
	if json.Unmarshal(body["messages"], &messages) != nil || len(messages) == 0 {
		return in, bad("messages are required")
	}
	return in, nil
}

func validNativeModel(model string) bool {
	if model == "" || len(model) > 100 || strings.Contains(model, "..") {
		return false
	}
	for _, c := range model {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
			return false
		}
	}
	return true
}

func (in textRequest) upstreamPath(model string) (string, error) {
	switch in.Protocol {
	case "anthropic":
		if in.CountOnly {
			return "/v1/messages/count_tokens", nil
		}
		return "/v1/messages", nil
	case "gemini":
		model = strings.TrimPrefix(model, "models/")
		if !validNativeModel(model) {
			return "", bad("invalid mapped Gemini model")
		}
		path := "/v1beta/models/" + url.PathEscape(model) + ":" + in.Action
		if in.Stream {
			path += "?alt=sse"
		}
		return path, nil
	default:
		return "/v1/chat/completions", nil
	}
}

func (in textRequest) preflightUsage() priceUsage {
	u := priceUsage{Input: 1, Output: 1, CacheRead: 1}
	if in.Protocol == "anthropic" {
		u.CacheWrite = 1
	}
	return u
}

func textErrorBody(protocol string, err error) map[string]any {
	status, message := 500, "internal gateway error"
	var e *apiError
	if errors.As(err, &e) {
		status, message = e.status, e.message
	}
	if protocol == "anthropic" {
		kind := map[int]string{400: "invalid_request_error", 401: "authentication_error", 402: "permission_error", 403: "permission_error", 404: "not_found_error", 429: "rate_limit_error", 503: "overloaded_error"}[status]
		if kind == "" {
			kind = "api_error"
		}
		return map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}}
	}
	if protocol == "gemini" {
		kind := map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 402: "RESOURCE_EXHAUSTED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND", 409: "ABORTED", 429: "RESOURCE_EXHAUSTED", 503: "UNAVAILABLE"}[status]
		if kind == "" {
			kind = "INTERNAL"
		}
		return map[string]any{"error": map[string]any{"code": status, "status": kind, "message": message}}
	}
	return map[string]any{"error": map[string]any{"code": status, "type": "gateway_error", "message": message}}
}
func textGatewayError(w http.ResponseWriter, protocol string, err error) {
	status := 500
	var e *apiError
	if errors.As(err, &e) {
		status = e.status
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(textErrorBody(protocol, err))
}

// Native protocol observers retain cumulative usage without rewriting tool,
// reasoning/signature or content events forwarded to the client.
type textObservation struct {
	Protocol, Model, Tier string
	Usage                 priceUsage
	HasUsage, CountOnly   bool
	started, stopped      bool
	finished              map[int]bool
	blocked               bool
}

func (o *textObservation) complete() bool {
	if o.Protocol == "anthropic" {
		return o.stopped
	}
	if o.blocked {
		return true
	}
	if len(o.finished) == 0 {
		return false
	}
	for _, done := range o.finished {
		if !done {
			return false
		}
	}
	return true
}

func (o *textObservation) observe(data []byte) error {
	var event struct {
		Type, Model, ModelVersion string
		Tier                      string          `json:"service_tier"`
		Usage                     json.RawMessage `json:"usage"`
		Metadata                  json.RawMessage `json:"usageMetadata"`
		Error                     json.RawMessage `json:"error"`
		Message                   json.RawMessage `json:"message"`
		Input                     *int64          `json:"input_tokens"`
		Total                     *int64          `json:"totalTokens"`
		Candidates                []struct {
			Index  int
			Finish string `json:"finishReason"`
		}
		Feedback struct {
			Block string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if json.Unmarshal(data, &event) != nil || string(data) == "null" {
		return &apiError{502, "upstream returned invalid JSON"}
	}
	if event.Error != nil && string(event.Error) != "null" || event.Type == "error" {
		return &apiError{502, "upstream returned an error"}
	}
	if o.CountOnly {
		n := event.Input
		if o.Protocol == "gemini" {
			n = event.Total
		}
		if n == nil || *n < 0 || *n > 2147483647 {
			return &apiError{502, "upstream token count is invalid"}
		}
		return nil
	}
	if event.Model != "" {
		o.Model = event.Model
	}
	if event.Tier != "" {
		o.Tier = event.Tier
	}
	if o.Protocol == "anthropic" {
		switch event.Type {
		case "message_start":
			if o.started || event.Message == nil {
				return &apiError{502, "invalid message stream start"}
			}
			o.started = true
			return o.observe(event.Message)
		case "message_stop":
			if !o.started {
				return &apiError{502, "message stream has no start"}
			}
			o.stopped = true
			return nil
		case "message_delta":
			if !o.started {
				return &apiError{502, "message stream has no start"}
			}
		}
		if event.Usage != nil && string(event.Usage) != "null" {
			return o.anthropicUsage(event.Usage, event.Type == "message_delta")
		}
		return nil
	}
	if o.Protocol == "gemini" {
		if event.ModelVersion != "" {
			o.Model = event.ModelVersion
		}
		if o.finished == nil {
			o.finished = map[int]bool{}
		}
		for _, c := range event.Candidates {
			o.finished[c.Index] = c.Finish != "" || o.finished[c.Index]
		}
		o.blocked = o.blocked || event.Feedback.Block != ""
		if event.Metadata != nil && string(event.Metadata) != "null" {
			var u struct {
				Input    *int64 `json:"promptTokenCount"`
				Output   int64  `json:"candidatesTokenCount"`
				Thoughts int64  `json:"thoughtsTokenCount"`
				Cached   int64  `json:"cachedContentTokenCount"`
			}
			if json.Unmarshal(event.Metadata, &u) != nil || u.Input == nil || !tokenCountsValid(*u.Input, u.Output, u.Thoughts, u.Cached) || u.Cached > *u.Input || u.Output+u.Thoughts > 2147483647 {
				return &apiError{502, "upstream usage is invalid"}
			}
			o.Usage = priceUsage{Input: *u.Input - u.Cached, Output: u.Output + u.Thoughts, CacheRead: u.Cached}
			o.HasUsage = true
		}
		return nil
	}
	if event.Usage != nil && string(event.Usage) != "null" {
		u, err := parseChatUsage(event.Usage)
		if err != nil {
			return err
		}
		o.Usage, o.HasUsage = u, true
	}
	return nil
}

func tokenCountsValid(counts ...int64) bool {
	for _, n := range counts {
		if n < 0 || n > 2147483647 {
			return false
		}
	}
	return true
}
func (o *textObservation) anthropicUsage(raw json.RawMessage, delta bool) error {
	var u struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
		Read   *int64 `json:"cache_read_input_tokens"`
		Write  *int64 `json:"cache_creation_input_tokens"`
		Cache  struct {
			Five *int64 `json:"ephemeral_5m_input_tokens"`
			Hour *int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	}
	if json.Unmarshal(raw, &u) != nil || !delta && (u.Input == nil || u.Output == nil) {
		return &apiError{502, "upstream usage is invalid"}
	}
	next := o.Usage
	for _, p := range []struct {
		value  *int64
		target *int64
	}{{u.Input, &next.Input}, {u.Output, &next.Output}, {u.Read, &next.CacheRead}, {u.Write, &next.CacheWrite}, {u.Cache.Five, &next.CacheWrite5m}, {u.Cache.Hour, &next.CacheWrite1h}} {
		if p.value != nil {
			if !tokenCountsValid(*p.value) {
				return &apiError{502, "upstream usage is invalid"}
			}
			// Delta values are cumulative; zero placeholders do not erase prior metering.
			if !delta || *p.value > 0 {
				*p.target = *p.value
			}
		}
	}
	if next.CacheWrite == 0 {
		next.CacheWrite = next.CacheWrite5m + next.CacheWrite1h
	}
	if !tokenCountsValid(next.CacheWrite) {
		return &apiError{502, "upstream usage is invalid"}
	}
	o.Usage = next
	o.HasUsage = o.HasUsage || u.Input != nil && u.Output != nil
	return nil
}
