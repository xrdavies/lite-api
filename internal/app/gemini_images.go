package app

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"time"
)

type imagePrices struct {
	IndependentImage *bool        `json:"image_rate_independent"`
	ImageRate        *json.Number `json:"image_rate_multiplier"`
	Image1K          *json.Number `json:"image_price_1k"`
	Image2K          *json.Number `json:"image_price_2k"`
	Image4K          *json.Number `json:"image_price_4k"`
}

func (g gatewayGroup) imageRate() json.Number {
	if g.IndependentImage != nil && *g.IndependentImage && g.ImageRate != nil {
		return *g.ImageRate
	}
	return g.Rate
}

func geminiImageModel(model string) bool {
	model = strings.TrimPrefix(strings.ToLower(model), "models/")
	for _, family := range []string{"gemini-3.1-flash-image", "gemini-3-pro-image", "gemini-2.5-flash-image"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return true
		}
	}
	return false
}

// Gemini's existing billing contract uses the request's imageSize, defaulting
// to 2K, independently of the byte dimensions of a returned image.
func geminiImageSize(body map[string]json.RawMessage) (string, string, error) {
	var config struct {
		Image struct {
			Size string `json:"imageSize"`
		} `json:"imageConfig"`
	}
	if value := body["generationConfig"]; value != nil && json.Unmarshal(value, &config) != nil {
		return "", "", bad("invalid generationConfig")
	}
	size := strings.ToUpper(strings.TrimSpace(config.Image.Size))
	if size == "" || size == "AUTO" || size == "512" {
		// 512 is a native provider size without a legacy billing tier.
		return "2K", "default", nil
	}
	if size != "1K" && size != "2K" && size != "4K" {
		return "", "", bad("imageSize must be 512, 1K, 2K or 4K")
	}
	return size, "input", nil
}

// Compatibility endpoints expose only the native image controls here. Keep
// sampling, reasoning and tool conversion in their existing protocol paths.
func geminiOutputOptions(config map[string]any, body map[string]json.RawMessage) error {
	var native map[string]json.RawMessage
	if raw := body["generationConfig"]; raw != nil && (json.Unmarshal(raw, &native) != nil || native == nil) {
		return bad("invalid generationConfig")
	}
	for field := range native {
		if field != "responseModalities" && field != "imageConfig" {
			return bad("converted generationConfig only supports imageConfig and responseModalities")
		}
	}
	raw := native["responseModalities"]
	if modes := body["modalities"]; modes != nil {
		if raw != nil {
			return bad("choose modalities or generationConfig.responseModalities")
		}
		raw = modes
	}
	modes := []string{"TEXT"}
	if geminiImageModel(credentialString(body, "model")) || native["imageConfig"] != nil {
		modes = append(modes, "IMAGE")
	}
	if raw != nil {
		if json.Unmarshal(raw, &modes) != nil || len(modes) < 1 || len(modes) > 2 {
			return bad("Gemini modalities must contain text or image")
		}
		seen := map[string]bool{}
		for i, mode := range modes {
			mode = strings.ToUpper(strings.TrimSpace(mode))
			if mode != "TEXT" && mode != "IMAGE" || seen[mode] {
				return bad("invalid or duplicate Gemini modality")
			}
			seen[mode], modes[i] = true, mode
		}
	}
	config["responseModalities"] = modes
	if raw := native["imageConfig"]; raw != nil {
		var image map[string]json.RawMessage
		if json.Unmarshal(raw, &image) != nil || image == nil {
			return bad("invalid imageConfig")
		}
		if _, _, err := geminiImageSize(body); err != nil {
			return err
		}
		config["imageConfig"] = image
	}
	return nil
}

func geminiImageText(part map[string]json.RawMessage) (string, error) {
	raw := part["inlineData"]
	if raw == nil {
		raw = part["inline_data"]
	}
	var inline map[string]json.RawMessage
	if json.Unmarshal(raw, &inline) != nil || inline == nil {
		return "", &apiError{502, "invalid Gemini inline image"}
	}
	kind := credentialString(inline, "mimeType")
	if kind == "" {
		kind = credentialString(inline, "mime_type")
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return "", &apiError{502, "unsupported Gemini image type"}
	}
	data := credentialString(inline, "data")
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(decoded) == 0 {
		return "", &apiError{502, "invalid Gemini image encoding"}
	}
	// Canonical base64 cannot inject Markdown delimiters or line breaks.
	return "![image](data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(decoded) + ")", nil
}

func (s *gatewaySelection) generatedImagePrice(g gatewayGroup, model, size string) (modelPrice, json.Number, error) {
	if s.Restrict {
		if _, ok := matchPrice(s.Pricing, s.Account.Platform, model); !ok {
			return modelPrice{}, "", denied()
		}
	}
	group, groupSet := groupModelPrice(s.GroupPricing, model)
	channel, channelSet := matchPrice(s.Pricing, s.Account.Platform, model)
	configured, found := channel, channelSet
	if groupSet {
		configured, found = group, true
	}
	if found && (configured.BillingMode == "" || configured.BillingMode == "token") {
		price, err := s.price(model)
		return price, g.Rate, err
	}
	if groupSet {
		group.Platform, group.Models = s.Account.Platform, []string{model}
		return group, g.imageRate(), nil
	}
	price := map[string]*json.Number{"1K": g.Image1K, "2K": g.Image2K, "4K": g.Image4K}[size]
	if price == nil && channelSet {
		return channel, g.imageRate(), nil
	}
	if price == nil && s.Account.Platform == "grok" {
		// Fixed compatibility prices; 4K shares the existing 2K tariff.
		var small, large string
		switch strings.ToLower(strings.TrimSpace(model)) {
		case "grok-imagine", "grok-imagine-image", "grok-imagine-edit":
			small, large = "0.02", "0.02"
		case "grok-imagine-image-quality":
			small, large = "0.05", "0.07"
		case "grok-imagine-image-2.0":
			small, large = "0.06", "0.08"
		}
		if small != "" {
			v := json.Number(large)
			if size == "1K" {
				v = json.Number(small)
			}
			price = &v
		}
	}
	if price == nil {
		// Dated compatibility fallback, not a live provider price feed.
		factor := map[string]string{"1K": "1", "2K": "1.5", "4K": "2"}[size]
		if factor == "" {
			return modelPrice{}, "", bad("invalid image billing size")
		}
		unit := json.Number("0.134")
		if reference, ok := s.Catalog.lookup(s.Account.Platform, model); ok && reference.PerRequest != nil && rat(*reference.PerRequest).Sign() > 0 {
			unit = *reference.PerRequest
		}
		base := json.Number(new(big.Rat).Mul(rat(unit), rat(json.Number(factor))).FloatString(10))
		price = &base
	}
	return modelPrice{Platform: s.Account.Platform, Models: []string{model}, BillingMode: "image", PerRequest: price}, g.imageRate(), nil
}

func (s *gatewaySelection) generatedImageCost(g gatewayGroup, model string, u priceUsage, tier, effort string, at time.Time) (priceCost, string, json.Number, error) {
	size := u.ImageSize
	if size == "" {
		size = "2K"
	}
	price, rate, err := s.generatedImagePrice(g, model, size)
	if err != nil {
		return priceCost{}, "", "", err
	}
	u.Requests = u.ImageCount
	if u.ImageCount == 0 && (price.BillingMode == "image" || price.BillingMode == "per_request") {
		// A rejected image request cannot inherit the generic one-call minimum.
		zero := json.Number("0")
		price.PerRequest, price.Intervals = &zero, nil
	}
	if price.BillingMode != "token" {
		tier = ""
	}
	cost, err := calculatePrice(price, u, rate, tier, effort, u.ImageSize, at, g.LongContext)
	return cost, price.BillingMode, rate, err
}

// Counts per candidate and output identity preserve identical images within a
// payload while deduplicating cumulative SSE frames. Only hashes are retained.
// ponytail: identical delta images across frames are indistinguishable from
// replayed frames; use provider output IDs if those become available.
func geminiImageOutputs(raw []byte, references bool) (map[string]int64, error) {
	var body struct {
		Candidates []struct {
			Index   int
			Content struct{ Parts []map[string]json.RawMessage }
		}
	}
	invalid := func() (map[string]int64, error) { return nil, &apiError{502, "invalid Gemini image response"} }
	if json.Unmarshal(raw, &body) != nil {
		return invalid()
	}
	counts := map[string]int64{}
	total := int64(0)
	for _, candidate := range body.Candidates {
		for _, part := range candidate.Content.Parts {
			value := part["inlineData"]
			if value == nil {
				value = part["inline_data"]
			}
			reference := false
			if value == nil && references {
				value = part["fileData"]
				if value == nil {
					value = part["file_data"]
				}
				reference = value != nil
			}
			if value == nil {
				continue
			}
			var data map[string]json.RawMessage
			if json.Unmarshal(value, &data) != nil || data == nil {
				return invalid()
			}
			kind := credentialString(data, "mimeType")
			if kind == "" {
				kind = credentialString(data, "mime_type")
			}
			kind = strings.ToLower(strings.TrimSpace(kind))
			switch kind {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
			default:
				continue
			}
			identity := ""
			if reference {
				uri := credentialString(data, "fileUri")
				if uri == "" {
					uri = credentialString(data, "file_uri")
				}
				if strings.TrimSpace(uri) == "" {
					return invalid()
				}
				identity = "file:" + digest(uri)
			} else {
				encoded := credentialString(data, "data")
				if strings.TrimSpace(encoded) == "" {
					continue
				}
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(decoded) == 0 {
					return invalid()
				}
				identity = "inline:" + digest(string(decoded))
			}
			total++
			if total > 64 {
				return nil, &apiError{502, "too many Gemini images"}
			}
			counts[strconv.Itoa(candidate.Index)+":"+kind+":"+identity]++
		}
	}
	return counts, nil
}

func countGeminiImages(raw []byte) (int64, error) {
	counts, err := geminiImageOutputs(raw, false)
	var count int64
	for _, n := range counts {
		count += n
	}
	return count, err
}

func (o *textObservation) observeGeminiImages(raw []byte) error {
	counts, err := geminiImageOutputs(raw, true)
	if err != nil {
		return err
	}
	if o.geminiImages == nil {
		o.geminiImages = map[string]int64{}
	}
	for identity, n := range counts {
		if previous := o.geminiImages[identity]; n > previous {
			if o.ImageCount+n-previous > 64 {
				return &apiError{502, "too many Gemini images"}
			}
			o.ImageCount += n - previous
			o.geminiImages[identity] = n
		}
	}
	return nil
}
