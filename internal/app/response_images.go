package app

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// This is billing metadata only; neither prompts nor image bytes are persisted.
type responseImageConfig struct {
	Model, Size string
}

func parseResponseImageTool(tool map[string]json.RawMessage) (*responseImageConfig, error) {
	c := &responseImageConfig{Model: "gpt-image-2"} // Existing billing fallback, not the provider's default model.
	invalid := func() (*responseImageConfig, error) { return nil, bad("invalid image_generation option") }
	for name, raw := range tool {
		value := credentialString(tool, name)
		allowed := ""
		switch name {
		case "type":
			continue
		case "model":
			if !validNativeModel(value) || len(value) > 100 {
				return invalid()
			}
			c.Model = value
			continue
		case "size":
			if value != "auto" && imageSizeTier(value) == "" || len(value) > 32 {
				return invalid()
			}
			c.Size = value
			continue
		case "action":
			allowed = "|auto|generate|edit|"
		case "background":
			allowed = "|auto|opaque|transparent|"
		case "quality":
			allowed = "|auto|low|medium|high|xhigh|max|"
		case "output_format":
			allowed = "|png|jpeg|webp|"
		case "moderation":
			allowed = "|auto|low|"
		case "input_fidelity":
			if string(raw) == "null" {
				continue
			}
			allowed = "|low|high|"
		case "output_compression", "partial_images":
			limit := 100
			if name == "partial_images" {
				limit = 3
			}
			var n int
			if string(raw) == "null" || json.Unmarshal(raw, &n) != nil || n < 0 || n > limit {
				return invalid()
			}
			continue
		case "input_image_mask":
			var mask map[string]json.RawMessage
			if json.Unmarshal(raw, &mask) != nil || len(mask) != 1 || (!validImageSource(credentialString(mask, "image_url")) && !validResponseID(credentialString(mask, "file_id"))) {
				return nil, bad("image mask requires an image URL or file ID")
			}
			continue
		default:
			return invalid()
		}
		if value == "" || strings.Contains(value, "|") || !strings.Contains(allowed, "|"+value+"|") {
			return invalid()
		}
	}
	return c, nil
}

func imageSizeTier(size string) string {
	size = strings.ToLower(strings.TrimSpace(size))
	switch size {
	case "1k", "2k", "4k":
		return strings.ToUpper(size)
	}
	w, h, ok := strings.Cut(size, "x")
	width, e1 := strconv.Atoi(w)
	height, e2 := strconv.Atoi(h)
	if !ok || e1 != nil || e2 != nil || width <= 0 || height <= 0 {
		return ""
	}
	if max(width, height) <= 1024 {
		return "1K"
	}
	if max(width, height) <= 2048 {
		return "2K"
	}
	return "4K"
}

type responseImageMeter struct {
	// IDs deduplicate item.done and terminal output. Hashes cover relay outputs
	// without IDs; partial images never count as additional completed images.
	seen  map[string]string
	order []string
}

func (m *responseImageMeter) observe(raw []byte) error {
	var event struct {
		Type, Status   string
		Response, Item json.RawMessage
		Output         []json.RawMessage
	}
	invalid := func() error { return &apiError{502, "invalid upstream image generation result"} }
	if json.Unmarshal(raw, &event) != nil {
		return invalid()
	}
	if event.Response != nil && string(event.Response) != "null" {
		var nested map[string]json.RawMessage
		if json.Unmarshal(event.Response, &nested) != nil || nested["response"] != nil {
			return invalid()
		}
		return m.observe(event.Response)
	}
	items := event.Output
	if event.Type == "response.output_item.done" {
		items = []json.RawMessage{event.Item}
	} else if event.Type != "" || event.Status != "completed" && event.Status != "incomplete" && event.Status != "failed" && event.Status != "cancelled" {
		return nil
	}
	for _, raw := range items {
		var kind struct{ Type string }
		if json.Unmarshal(raw, &kind) != nil {
			return invalid()
		}
		if kind.Type != "image_generation_call" {
			continue
		}
		var item struct{ Type, ID, Result, Size, Status string }
		if json.Unmarshal(raw, &item) != nil {
			return invalid()
		}
		if item.Result == "" || item.Status == "failed" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(item.Result)
		if err != nil || len(data) == 0 || len(item.Size) > 32 || len(item.ID) > 256 {
			return invalid()
		}
		id := item.ID
		if id == "" {
			id = digest(item.Result)
		}
		if m.seen == nil {
			m.seen = map[string]string{}
		}
		if old, exists := m.seen[id]; exists {
			if old == "" {
				m.seen[id] = item.Size
			}
			continue
		}
		if len(m.seen) >= 1024 {
			return invalid()
		}
		m.seen[id] = item.Size
		m.order = append(m.order, id)
	}
	return nil
}

func (m *responseImageMeter) apply(u *priceUsage, config *responseImageConfig) {
	u.ImageSizes, u.ImageOutputSize = [3]int64{}, ""
	u.ImageCount = int64(len(m.seen))
	if u.ImageCount == 0 {
		return
	}
	u.Requests = u.ImageCount
	sizes := make([]string, 0, len(m.order))
	for _, id := range m.order {
		sizes = append(sizes, m.seen[id])
	}
	input := ""
	if config != nil {
		input = config.Size
	}
	applyImageSizes(u, input, sizes)
}

func applyImageSizes(u *priceUsage, input string, sizes []string) {
	u.ImageSizes, u.ImageOutputSize = [3]int64{}, ""
	u.ImageSize, u.ImageSizeSource, u.ImageInputSize = "2K", "default", input
	if size := imageSizeTier(input); size != "" {
		u.ImageSize, u.ImageSizeSource = size, "input"
	}
	for _, size := range sizes {
		if u.ImageOutputSize == "" {
			u.ImageOutputSize = size
		}
		if tier := imageSizeTier(size); tier != "" {
			index := map[string]int{"1K": 0, "2K": 1, "4K": 2}[tier]
			u.ImageSizes[index]++
			if u.ImageSizeSource != "output" || tier > u.ImageSize {
				u.ImageSize = tier
			}
			u.ImageSizeSource = "output"
		}
	}
}

// Direct image responses share the size contract with hosted image output, but
// each data entry is one image even when two entries contain the same bytes.
func (o *textObservation) observeImages(raw []byte) error {
	var result struct {
		Data         []map[string]json.RawMessage
		Model, Size  string
		Usage, Error json.RawMessage
	}
	invalid := func() error { return &apiError{502, "upstream image response is invalid"} }
	if json.Unmarshal(raw, &result) != nil || len(result.Data) == 0 || len(result.Data) > 10 || len(result.Size) > 32 || result.Error != nil && string(result.Error) != "null" {
		return invalid()
	}
	u := priceUsage{ImageRequest: true}
	sizes := []string{}
	for _, fields := range result.Data {
		if fields == nil {
			return invalid()
		}
		if strings.TrimSpace(credentialString(fields, "url")) == "" && strings.TrimSpace(credentialString(fields, "b64_json")) == "" {
			continue
		}
		size := credentialString(fields, "size")
		if size == "" {
			size = result.Size
		}
		if len(size) > 32 {
			return invalid()
		}
		sizes = append(sizes, size)
		u.ImageCount++
	}
	u.Requests = u.ImageCount
	applyImageSizes(&u, o.Usage.ImageInputSize, sizes)
	o.Usage, o.Model = u, result.Model
	if result.Usage != nil && string(result.Usage) != "null" {
		var values map[string]json.RawMessage
		if json.Unmarshal(result.Usage, &values) != nil || values == nil {
			return invalid()
		}
		for _, pair := range [][2]string{{"input_tokens", "prompt_tokens"}, {"output_tokens", "completion_tokens"}, {"input_tokens_details", "prompt_tokens_details"}, {"output_tokens_details", "completion_tokens_details"}} {
			if value := values[pair[0]]; value != nil {
				values[pair[1]] = value
			}
		}
		encoded, _ := json.Marshal(values)
		tokens, err := parseChatUsage(encoded)
		if err != nil {
			return err
		}
		u.Input, u.Output, u.CacheRead, u.CacheWrite = tokens.Input, tokens.Output, tokens.CacheRead, tokens.CacheWrite
		u.ImageInput, u.ImageOutput = tokens.ImageInput, tokens.ImageOutput
		o.Usage, o.HasUsage = u, true
	}
	if u.ImageCount == 0 {
		return invalid()
	}
	return nil
}

func (o *textObservation) observeImageStream(raw []byte) error {
	var event map[string]json.RawMessage
	invalid := func() error { return &apiError{502, "upstream image stream is invalid"} }
	if json.Unmarshal(raw, &event) != nil || event == nil || event["error"] != nil && string(event["error"]) != "null" {
		return invalid()
	}
	kind := credentialString(event, "type")
	if kind != "image_generation.partial_image" && kind != "image_edit.partial_image" && kind != "image_generation.completed" && kind != "image_edit.completed" {
		return invalid()
	}
	image := credentialString(event, "b64_json")
	link := credentialString(event, "url")
	if image == "" && !validAudioURL(link) {
		return invalid()
	}
	if image != "" {
		if data, err := base64.StdEncoding.DecodeString(image); err != nil || len(data) == 0 {
			return invalid()
		}
	}
	if strings.HasSuffix(kind, ".partial_image") {
		var index int
		if value := event["partial_image_index"]; value == nil || string(value) == "null" || json.Unmarshal(value, &index) != nil || index < 0 || index > 2 {
			return invalid()
		}
		return nil
	}
	size := credentialString(event, "size")
	id := credentialString(event, "id")
	if len(size) > 32 || len(id) > 256 {
		return invalid()
	}
	if id == "" {
		id = digest(image + "\n" + link)
	}
	meter := o.ImageStream
	if meter.seen == nil {
		meter.seen = map[string]string{}
	}
	if previous, exists := meter.seen[id]; exists {
		if previous == "" {
			meter.seen[id] = size
		}
	} else {
		if len(meter.seen) >= 10 {
			return invalid()
		}
		meter.seen[id] = size
		meter.order = append(meter.order, id)
	}
	// Observe completed output before parsing usage: a malformed later frame must
	// not erase images already produced. Stream usage snapshots are cumulative.
	input := o.Usage.ImageInputSize
	meter.apply(&o.Usage, &responseImageConfig{Size: input})
	o.Usage.ImageRequest, o.stopped = true, true
	if model := credentialString(event, "model"); model != "" {
		o.Model = model
	}
	if usage := event["usage"]; usage != nil && string(usage) != "null" {
		normalized, _ := json.Marshal(map[string]any{"data": []map[string]json.RawMessage{event}, "usage": usage})
		next := textObservation{Protocol: "images"}
		if err := next.observeImages(normalized); err != nil {
			return err
		}
		o.Usage.Input, o.Usage.Output = next.Usage.Input, next.Usage.Output
		o.Usage.CacheRead, o.Usage.CacheWrite = next.Usage.CacheRead, next.Usage.CacheWrite
		o.Usage.ImageInput, o.Usage.ImageOutput = next.Usage.ImageInput, next.Usage.ImageOutput
		o.HasUsage = next.HasUsage
	}
	return nil
}

func imageSizeBreakdown(sizes [3]int64) any {
	values := map[string]int64{}
	for i, size := range []string{"1K", "2K", "4K"} {
		if sizes[i] > 0 {
			values[size] = sizes[i]
		}
	}
	if len(values) == 0 {
		return nil
	}
	raw, _ := json.Marshal(values)
	return string(raw)
}

func (s *gatewaySelection) responseImageModel(requested, response string) string {
	model := "gpt-image-2"
	if s.ResponseImage != nil {
		model = s.ResponseImage.Model
	}
	switch s.BillingSource {
	case "requested":
		return requested
	case "upstream":
		return s.UpstreamModel
	case "response_model":
		if response != "" {
			return response
		}
	case "channel_mapped":
		if s.ChannelModel != requested {
			return s.ChannelModel
		}
	}
	return model
}
