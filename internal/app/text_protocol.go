package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
)

type textRequest struct {
	Protocol, Scope, Model, Effort, Tier, Action string
	Previous                                     string
	Store                                        bool
	NativeCompaction                             bool
	Stream, CountOnly                            bool
	ImageGeneration                              bool
	ImageSize, ImageSizeSource, ImageInputSize   string
	Headers                                      http.Header
	Search                                       *grokSearchRequest
}

func (in textRequest) compositeEndpoint() string {
	if in.Protocol == "seedance" || in.Protocol == "videos" {
		return "any"
	}
	if in.Protocol == "alpha_search" {
		return "responses"
	}
	if in.Protocol == "anthropic" {
		if in.CountOnly {
			return "count_tokens"
		}
		return "messages"
	}
	if in.Protocol == "images" {
		return "images"
	}
	return in.Protocol
}

func parseTextRequest(r *http.Request, protocol string, body map[string]json.RawMessage) (textRequest, error) {
	in := textRequest{Protocol: protocol, Headers: http.Header{}}
	if audioProtocol(protocol) {
		in.Model, in.Scope = protocol, protocol
		return in, nil
	}
	if protocol == "images" {
		if strings.HasSuffix(r.URL.Path, "/generations") {
			in.Action, in.Scope = "generations", "images.generations"
		} else if strings.HasSuffix(r.URL.Path, "/edits") {
			in.Action, in.Scope = "edits", "images.edits"
		} else {
			return in, missing()
		}
		if json.Unmarshal(body["model"], &in.Model) != nil || !validModelPattern(in.Model) || strings.Contains(in.Model, "*") {
			return in, bad("invalid model")
		}
		var prompt string
		if json.Unmarshal(body["prompt"], &prompt) != nil || strings.TrimSpace(prompt) == "" || len([]rune(prompt)) > 10000 {
			return in, bad("prompt is required and must be at most 10000 characters")
		}
		if raw := body["n"]; raw != nil {
			var n int
			if json.Unmarshal(raw, &n) != nil || n < 1 || n > 10 {
				return in, bad("n must be between 1 and 10")
			}
		}
		if raw := body["response_format"]; raw != nil {
			var format string
			if json.Unmarshal(raw, &format) != nil || format != "b64_json" && format != "url" {
				return in, bad("invalid image response_format")
			}
		}
		if raw := body["stream"]; raw != nil && string(raw) != "false" {
			return in, bad("image generation does not stream")
		}
		if in.Action == "edits" {
			var images []json.RawMessage
			if raw := body["images"]; raw != nil {
				if json.Unmarshal(raw, &images) != nil || len(images) == 0 || len(images) > 10 {
					return in, bad("image edits require between 1 and 10 images")
				}
			} else if raw := body["image"]; raw != nil {
				// xAI's image endpoint uses one image object instead of the
				// OpenAI-compatible images array. Keep the original object on
				// the wire; this branch only validates the shared request shape.
				images = []json.RawMessage{raw}
			} else {
				return in, bad("image edits require an image source")
			}
			for _, raw := range images {
				var image struct {
					URL      string `json:"url"`
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
					FileID string `json:"file_id"`
				}
				if json.Unmarshal(raw, &image) != nil || image.FileID != "" {
					return in, bad("image edits require image URLs or data URLs")
				}
				imageURL := image.URL
				if imageURL == "" {
					imageURL = image.ImageURL.URL
				}
				if !validImageSource(imageURL) {
					return in, bad("invalid image edit source")
				}
			}
		}
		return in, nil
	}
	if grokSearchProtocol(protocol) {
		var err error
		in.Search, err = parseGrokSearch(body)
		in.Model, in.Scope = "grok-4.6", protocol
		return in, err
	}
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
		configBody := body
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
			configBody = nested
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
		if raw := configBody["generationConfig"]; raw != nil && json.Unmarshal(raw, &config) != nil {
			return in, bad("invalid generationConfig")
		}
		for _, modality := range config.Modalities {
			switch strings.ToUpper(strings.TrimSpace(modality)) {
			case "TEXT":
			case "IMAGE":
				in.ImageGeneration = true
			default:
				return in, bad("media generation is not yet available on this endpoint")
			}
		}
		var err error
		in.ImageSize, in.ImageSizeSource, err = geminiImageSize(configBody)
		if err != nil {
			return in, err
		}
		var imageConfig struct {
			Image struct {
				Size string `json:"imageSize"`
			} `json:"imageConfig"`
		}
		_ = json.Unmarshal(configBody["generationConfig"], &imageConfig)
		in.ImageInputSize = strings.TrimSpace(imageConfig.Image.Size)
		in.ImageGeneration = in.ImageGeneration || geminiImageModel(in.Model)
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
	if protocol == "alpha_search" {
		in.Scope = protocol
		if raw := body["stream"]; raw != nil && (string(raw) != "false") {
			return in, bad("alpha search does not stream")
		}
		// This is an independent search command, not a Responses request.
		for _, field := range []string{"prompt_cache_key", "prompt_cache_retention", "store"} {
			delete(body, field)
		}
		return in, nil
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
	if protocol == "anthropic" {
		var err error
		in.Effort, err = requestEffort(body, protocol)
		if err != nil {
			return in, err
		}
	}
	if raw := body["service_tier"]; raw != nil && (json.Unmarshal(raw, &in.Tier) != nil || len(in.Tier) > 16) {
		return in, bad("invalid service_tier")
	}
	if protocol == "responses" {
		return parseResponsesRequest(r, in, body)
	}
	if protocol == "embeddings" {
		in.Scope = "embeddings"
		if in.Stream || !validEmbeddingInput(body["input"]) {
			return in, bad("embeddings require nonempty input and do not stream")
		}
		if raw := body["encoding_format"]; raw != nil {
			var format string
			if json.Unmarshal(raw, &format) != nil || format != "float" && format != "base64" {
				return in, bad("invalid embedding encoding_format")
			}
		}
		if raw := body["dimensions"]; raw != nil {
			var n int64
			if json.Unmarshal(raw, &n) != nil || n < 1 || n > 2147483647 {
				return in, bad("invalid embedding dimensions")
			}
		}
		return in, nil
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
	case "web_search", "x_search":
		return "/v1/responses", nil
	case "alpha_search":
		return "/v1/alpha/search", nil
	case "responses":
		return "/v1/responses" + in.Action, nil
	case "embeddings":
		return "/v1/embeddings", nil
	case "tts", "stt":
		return "/v1/" + in.Protocol, nil
	case "images":
		return "/v1/images/" + in.Action, nil
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

func validImageSource(raw string) bool {
	if len(raw) == 0 || len(raw) > 16<<20 || strings.ContainsAny(raw, "\r\n\x00") {
		return false
	}
	if strings.HasPrefix(raw, "data:image/") {
		return strings.Contains(raw, ";base64,")
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != ""
}

// parseImageMultipart converts the OpenAI edit form into the same JSON shape as
// the JSON endpoint. The upstream only receives normalized data URLs.
func parseImageMultipart(body []byte, contentType string) (map[string]json.RawMessage, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, bad("invalid image edit multipart form")
	}
	form := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	out := map[string]json.RawMessage{}
	var images []map[string]any
	for {
		part, err := form.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad("invalid image edit multipart form")
		}
		data, readErr := io.ReadAll(io.LimitReader(part, 8<<20+1))
		_ = part.Close()
		if readErr != nil || len(data) > 8<<20 {
			return nil, bad("image edit file is too large")
		}
		name := part.FormName()
		if part.FileName() != "" && (name == "image" || strings.HasPrefix(name, "image[")) {
			content := part.Header.Get("Content-Type")
			if !strings.HasPrefix(content, "image/") {
				return nil, bad("image edit file must be an image")
			}
			images = append(images, map[string]any{"url": "data:" + content + ";base64," + base64.StdEncoding.EncodeToString(data)})
			continue
		}
		value := strings.TrimSpace(string(data))
		switch name {
		case "model", "prompt", "response_format":
			out[name] = json.RawMessage(fmt.Sprintf("%q", value))
		case "n":
			out[name] = json.RawMessage(value)
		case "image":
			if value != "" && validImageSource(value) {
				images = append(images, map[string]any{"url": value})
			}
		}
	}
	if len(images) == 0 {
		return nil, bad("image edits require an image file")
	}
	out["images"], _ = json.Marshal(images)
	return out, nil
}

// imagesToGrok keeps the public OpenAI image contract while emitting the JSON
// object shape used by the Grok image endpoint. Generation requests already
// share the wire format; edits are the only incompatible part.
func imagesToGrok(body map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if body["images"] == nil {
		return body, nil
	}
	var sources []json.RawMessage
	if json.Unmarshal(body["images"], &sources) != nil || len(sources) == 0 || len(sources) > 10 {
		return nil, bad("invalid image edit sources")
	}
	objects := make([]map[string]any, 0, len(sources))
	for _, raw := range sources {
		var image struct {
			URL      string `json:"url"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}
		if json.Unmarshal(raw, &image) != nil {
			return nil, bad("invalid image edit source")
		}
		if image.URL == "" {
			image.URL = image.ImageURL.URL
		}
		if !validImageSource(image.URL) {
			return nil, bad("invalid image edit source")
		}
		objects = append(objects, map[string]any{"url": image.URL, "type": "image_url"})
	}
	delete(body, "images")
	if len(objects) == 1 {
		body["image"], _ = json.Marshal(objects[0])
	} else {
		body["images"], _ = json.Marshal(objects)
	}
	return body, nil
}

func (in textRequest) preflightUsage() priceUsage {
	if in.Protocol == "images" {
		return priceUsage{Requests: 1}
	}
	if in.Protocol == "embeddings" {
		return priceUsage{Input: 1}
	}
	u := priceUsage{Input: 1, Output: 1, CacheRead: 1}
	if in.Protocol == "anthropic" {
		u.CacheWrite = 1
	}
	if in.ImageGeneration {
		u.Output = 2
		u.ImageOutput = 1
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
	ResponseID, Action    string
	Usage                 priceUsage
	HasUsage, CountOnly   bool
	started, stopped      bool
	finished              map[int]bool
	blocked               bool
	ImageRejected         bool
	ImageCount            int64
}

func (o *textObservation) complete() bool {
	if o.Protocol == "anthropic" || o.Protocol == "responses" {
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
	if o.Protocol == "images" {
		var result struct {
			Data  []json.RawMessage `json:"data"`
			Error json.RawMessage   `json:"error"`
		}
		if json.Unmarshal(data, &result) != nil || len(result.Data) == 0 || len(result.Data) > 10 || result.Error != nil && string(result.Error) != "null" {
			return &apiError{502, "upstream image response is invalid"}
		}
		for _, item := range result.Data {
			var fields map[string]json.RawMessage
			if json.Unmarshal(item, &fields) != nil || fields == nil {
				return &apiError{502, "upstream image result is invalid"}
			}
		}
		o.Usage = priceUsage{Requests: int64(len(result.Data))}
		o.HasUsage = true
		return nil
	}
	if o.Protocol == "alpha_search" {
		var result map[string]json.RawMessage
		if json.Unmarshal(data, &result) != nil || result == nil || (result["error"] != nil && string(result["error"]) != "null") {
			return &apiError{502, "upstream search response is invalid"}
		}
		// A successful standalone search is one billable call, with no token usage.
		o.Usage, o.HasUsage = priceUsage{Requests: 1}, true
		return nil
	}
	if o.Protocol == "responses" {
		return o.observeResponses(data)
	}
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
			if c.Finish != "" && c.Finish != "STOP" && c.Finish != "MAX_TOKENS" {
				o.ImageRejected = true
			}
		}
		o.blocked = o.blocked || event.Feedback.Block != ""
		count, err := countGeminiImages(data)
		if err != nil {
			return err
		}
		// ponytail: preserve cumulative relay payload semantics with the maximum
		// count in one frame; delta-only multi-image streams may undercount.
		o.ImageCount = max(o.ImageCount, count)
		if event.Metadata != nil && string(event.Metadata) != "null" {
			var u struct {
				Input         *int64 `json:"promptTokenCount"`
				Output        int64  `json:"candidatesTokenCount"`
				Thoughts      int64  `json:"thoughtsTokenCount"`
				Cached        int64  `json:"cachedContentTokenCount"`
				OutputDetails []struct {
					Modality string `json:"modality"`
					Tokens   int64  `json:"tokenCount"`
				} `json:"candidatesTokensDetails"`
			}
			if json.Unmarshal(event.Metadata, &u) != nil || u.Input == nil || !tokenCountsValid(*u.Input, u.Output, u.Thoughts, u.Cached) || u.Cached > *u.Input || u.Output+u.Thoughts > 2147483647 {
				return &apiError{502, "upstream usage is invalid"}
			}
			imageOutput := int64(0)
			for _, detail := range u.OutputDetails {
				if detail.Tokens < 0 || detail.Tokens > 2147483647 {
					return &apiError{502, "upstream image usage is invalid"}
				}
				if strings.EqualFold(detail.Modality, "IMAGE") {
					imageOutput += detail.Tokens
				}
			}
			if imageOutput > u.Output {
				return &apiError{502, "upstream image usage is invalid"}
			}
			o.Usage = priceUsage{Input: *u.Input - u.Cached, Output: u.Output + u.Thoughts, CacheRead: u.Cached, ImageOutput: imageOutput}
			o.HasUsage = true
		}
		return nil
	}
	if event.Usage != nil && string(event.Usage) != "null" {
		usage := event.Usage
		if o.Protocol == "embeddings" {
			var values map[string]json.RawMessage
			if json.Unmarshal(usage, &values) != nil || values == nil {
				return &apiError{502, "upstream usage is invalid"}
			}
			if values["prompt_tokens"] == nil {
				values["prompt_tokens"] = values["input_tokens"]
				if values["prompt_tokens"] == nil {
					values["prompt_tokens"] = values["total_tokens"]
				}
			}
			if values["completion_tokens"] == nil {
				values["completion_tokens"] = json.RawMessage("0")
				if values["output_tokens"] != nil {
					values["completion_tokens"] = values["output_tokens"]
				}
			}
			if values["prompt_tokens_details"] == nil {
				values["prompt_tokens_details"] = values["input_tokens_details"]
			}
			usage, _ = json.Marshal(values)
		}
		u, err := parseChatUsage(usage)
		if err != nil {
			return err
		}
		o.Usage, o.HasUsage = u, true
	}
	return nil
}

func validEmbeddingInput(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text != ""
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil || len(list) == 0 {
		return false
	}
	// One token sequence, a batch of token sequences, or a batch of strings.
	if validTokenSequence(raw) {
		return true
	}
	if len(list) > 2048 {
		return false
	}
	var texts []string
	if json.Unmarshal(raw, &texts) == nil {
		for _, text := range texts {
			if text == "" {
				return false
			}
		}
		return true
	}
	for _, tokens := range list {
		if !validTokenSequence(tokens) {
			return false
		}
	}
	return true
}

func validTokenSequence(raw json.RawMessage) bool {
	var tokens []json.RawMessage
	if json.Unmarshal(raw, &tokens) != nil || len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		n, err := json.Number(string(token)).Int64()
		if err != nil || !tokenCountsValid(n) {
			return false
		}
	}
	return true
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
