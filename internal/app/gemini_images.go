package app

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
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

func (s *gatewaySelection) geminiImagePrice(g gatewayGroup, model, size string) (modelPrice, json.Number, error) {
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

func (s *gatewaySelection) geminiImageCost(g gatewayGroup, model string, u priceUsage, tier, effort string, at time.Time) (priceCost, string, json.Number, error) {
	size := u.ImageSize
	if size == "" {
		size = "2K"
	}
	price, rate, err := s.geminiImagePrice(g, model, size)
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

func countGeminiImages(raw []byte) (int64, error) {
	var body struct {
		Candidates []struct {
			Content struct{ Parts []map[string]json.RawMessage }
		}
	}
	if json.Unmarshal(raw, &body) != nil {
		return 0, &apiError{502, "invalid Gemini image response"}
	}
	var count int64
	for _, candidate := range body.Candidates {
		for _, part := range candidate.Content.Parts {
			value := part["inlineData"]
			if value == nil {
				value = part["inline_data"]
			}
			if value == nil {
				continue
			}
			var inline map[string]json.RawMessage
			if json.Unmarshal(value, &inline) != nil {
				return 0, &apiError{502, "invalid Gemini inline data"}
			}
			kind := credentialString(inline, "mimeType")
			if kind == "" {
				kind = credentialString(inline, "mime_type")
			}
			switch strings.ToLower(strings.TrimSpace(kind)) {
			case "image/png", "image/jpeg", "image/webp", "image/gif":
				data := credentialString(inline, "data")
				if strings.TrimSpace(data) == "" {
					continue
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return 0, &apiError{502, "invalid Gemini image encoding"}
				}
				count++
				if count > 64 {
					return 0, &apiError{502, "too many Gemini images"}
				}
			}
		}
	}
	return count, nil
}
