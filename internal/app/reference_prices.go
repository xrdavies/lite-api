package app

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed reference_prices.json
var bundledPrices []byte

var priceDateSuffix = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

// A catalog is immutable after publication, including nested price cards. Requests
// keep its pointer until settlement even if a later reload publishes new prices.
type priceCatalog struct {
	AsOf   string           `json:"as_of"`
	Source string           `json:"source"`
	Prices []referencePrice `json:"prices"`
	hash   string
	index  map[string]modelPrice
}

type referencePrice struct {
	modelPrice
	FastRatio string `json:"fast_ratio,omitempty"`
}

func parsePriceCatalog(raw []byte) (*priceCatalog, error) {
	if len(raw) > 8<<20 {
		return nil, errors.New("price catalog exceeds 8 MiB")
	}
	c := &priceCatalog{}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(c); err != nil {
		return nil, errors.New("invalid price catalog JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("price catalog must be one JSON object")
	}
	if _, err := time.Parse("2006-01-02", c.AsOf); err != nil || len(c.Source) == 0 || len(c.Source) > 2000 || len(c.Prices) == 0 || len(c.Prices) > 10000 {
		return nil, errors.New("invalid price catalog metadata or size")
	}
	c.index = map[string]modelPrice{}
	for i := range c.Prices {
		p := &c.Prices[i].modelPrice
		if !supportedPlatform(p.Platform) {
			return nil, errors.New("unsupported price catalog platform")
		}
		card := []modelPrice{*p}
		if err := validateModelPrices(card, false); err != nil {
			return nil, fmt.Errorf("price catalog card %d: %w", i, err)
		}
		*p = card[0]
		// Reference context ladders scale overridden channel flat prices too.
		// Keep them as multipliers so a catalog cannot silently reintroduce
		// absolute provider prices over an operator's custom tariff.
		if p.BillingMode == "token" {
			for _, iv := range p.Intervals {
				if iv.Input != nil || iv.Output != nil || iv.CacheWrite != nil || iv.CacheWrite1h != nil || iv.CacheRead != nil || iv.PerRequest != nil {
					return nil, errors.New("reference token intervals must use multipliers")
				}
			}
		}
		if ratio := c.Prices[i].FastRatio; ratio != "" {
			if len(ratio) > 50 || strings.ContainsAny(ratio, "eE") {
				return nil, errors.New("invalid reference priority ratio")
			}
			value, ok := new(big.Rat).SetString(ratio)
			if !ok || value.Sign() <= 0 || value.Cmp(big.NewRat(1000000, 1)) >= 0 {
				return nil, errors.New("invalid reference priority ratio")
			}
			p.fastRatio = value
		}
		if p.Input == nil && p.Output == nil && p.ImageInput == nil && p.ImageOutput == nil && p.PerRequest == nil && len(p.Intervals) == 0 {
			return nil, errors.New("empty reference price card")
		}
		for _, name := range p.Models {
			if !concreteModel(name) {
				return nil, errors.New("price catalog requires concrete model names")
			}
			key := p.Platform + "\x00" + pricingName(name)
			if _, exists := c.index[key]; exists {
				return nil, errors.New("duplicate price catalog model")
			}
			c.index[key] = *p
		}
	}
	c.hash = digest(string(raw))
	return c, nil
}

func (a *App) reloadPrices() error {
	a.priceMu.Lock()
	defer a.priceMu.Unlock()
	raw := bundledPrices
	if a.priceFile != "" {
		f, err := os.Open(a.priceFile)
		if err != nil {
			return errors.New("cannot open PRICING_FILE")
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return errors.New("PRICING_FILE must be a regular file")
		}
		raw, err = io.ReadAll(io.LimitReader(f, (8<<20)+1))
		f.Close()
		if err != nil {
			return errors.New("cannot read PRICING_FILE")
		}
	}
	if old := a.prices.Load(); old != nil && old.hash == digest(string(raw)) {
		return nil
	}
	c, err := parsePriceCatalog(raw)
	if err != nil {
		return err
	}
	a.prices.Store(c)
	return nil
}

func priceNames(name string) []string {
	name = pricingName(strings.TrimPrefix(strings.ToLower(name), "models/"))
	candidates := []string{name, priceDateSuffix.ReplaceAllString(name, "")}
	if strings.HasPrefix(name, "gpt-") {
		for _, effort := range []string{"-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-none"} {
			if strings.HasSuffix(name, effort) {
				candidates = append(candidates, strings.TrimSuffix(name, effort))
			}
		}
		if name == "gpt-5.6" {
			candidates = append(candidates, "gpt-5.6-sol")
		}
		if name == "gpt-6" {
			candidates = append(candidates, "gpt-6-astra")
		}
	}
	return candidates
}

func (c *priceCatalog) lookup(platform, name string) (modelPrice, bool) {
	if c == nil {
		return modelPrice{}, false
	}
	for _, candidate := range priceNames(name) {
		if platform != "" {
			if p, ok := c.index[platform+"\x00"+candidate]; ok {
				return p, true
			}
			continue
		}
		var found modelPrice
		matches := 0
		for _, provider := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
			if p, ok := c.index[provider+"\x00"+candidate]; ok {
				found = p
				matches++
			}
		}
		if matches == 1 {
			return found, true
		}
		if matches > 1 {
			return modelPrice{}, false
		}
	}
	return modelPrice{}, false
}

func matchPrice(prices []modelPrice, platform, model string) (modelPrice, bool) {
	// Literal variants win over canonical fallback, including explicit free rates.
	for _, name := range priceNames(model) {
		for _, p := range prices {
			if p.Platform == platform {
				for _, pattern := range p.Models {
					if patternMatches(pricingName(pattern), name) {
						return p, true
					}
				}
			}
		}
	}
	return modelPrice{}, false
}

func resolvedModelPrice(c *priceCatalog, configured []modelPrice, platform, model string, restrict bool) (modelPrice, error) {
	custom, found := matchPrice(configured, platform, model)
	if restrict && !found {
		return modelPrice{}, denied()
	}
	base, known := c.lookup(platform, model)
	if !known {
		// A relay protocol may expose a model from another provider. Only an
		// unambiguous catalog identity can supply its reference price.
		base, known = c.lookup("", model)
	}
	base.Platform = platform
	if !found {
		if known {
			return base, nil
		}
		return modelPrice{}, &apiError{503, "model price is not configured"}
	}
	if custom.BillingMode != "token" && custom.BillingMode != "" || !known || base.BillingMode != "token" {
		return custom, nil
	}
	custom.fastRatio = base.fastRatio
	writeOverride := custom.CacheWrite
	// Omitted text components inherit reference rates; explicit zero stays free.
	// Image rates remain channel-only. A single cache-write override applies to
	// both TTLs. Channel intervals replace the reference long-context ladder.
	for _, pair := range []struct {
		dst      **json.Number
		fallback *json.Number
	}{
		{&custom.Input, base.Input}, {&custom.Output, base.Output}, {&custom.CacheWrite, base.CacheWrite}, {&custom.CacheRead, base.CacheRead}, {&custom.Fast, base.Fast},
	} {
		if *pair.dst == nil {
			*pair.dst = pair.fallback
		}
	}
	if custom.CacheWrite1h == nil {
		if writeOverride != nil {
			custom.CacheWrite1h = writeOverride
		} else {
			custom.CacheWrite1h = base.CacheWrite1h
		}
	}
	if len(custom.Intervals) == 0 {
		// Catalog tiers contain multipliers, so channel flat prices keep their
		// own values when the reference context surcharge applies.
		custom.Intervals = base.Intervals
	}
	return custom, nil
}

// Group cards match the billing model name, independently of their platform
// label. Exact names beat the first matching wildcard. They replace channel
// cards; omitted components inherit reference prices, not channel prices.
func effectiveModelPrice(c *priceCatalog, group, channel []modelPrice, platform, model string, restrict bool) (modelPrice, error) {
	if restrict {
		if _, found := matchPrice(channel, platform, model); !found {
			return modelPrice{}, denied()
		}
	}
	var matched *modelPrice
search:
	for i := range group {
		for _, pattern := range group[i].Models {
			if pricingName(pattern) == pricingName(model) {
				matched = &group[i]
				break search
			}
			if matched == nil && patternMatches(pricingName(pattern), pricingName(model)) {
				matched = &group[i]
			}
		}
	}
	if matched == nil {
		return resolvedModelPrice(c, channel, platform, model, false)
	}
	card := *matched
	card.Platform, card.Models = platform, []string{model}
	if card.BillingMode == "token" || card.BillingMode == "" {
		// Group token intervals are stored for compatibility but only the
		// reference long-context ladder applies to these flat overrides.
		card.Intervals = nil
	}
	return resolvedModelPrice(c, []modelPrice{card}, platform, model, false)
}

func (a *App) referencePricing(w http.ResponseWriter, r *http.Request) error {
	name := strings.TrimSpace(r.URL.Query().Get("model"))
	if !concreteModel(name) {
		return bad("a valid model parameter is required")
	}
	platform := strings.ToLower(r.URL.Query().Get("platform"))
	if platform != "" && !supportedPlatform(platform) {
		return bad("unsupported platform")
	}
	c := a.prices.Load()
	p, found := c.lookup(platform, name)
	out := map[string]any{"found": found, "as_of": c.AsOf, "checksum": c.hash}
	if found {
		for key, value := range publicPricing(plazaPrice(p, true)) {
			out[key] = value
		}
	}
	return reply(w, out)
}

func (a *App) referenceModels(w http.ResponseWriter, r *http.Request) error {
	platform := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform")))
	if !supportedPlatform(platform) {
		return bad("a supported platform parameter is required")
	}
	c := a.prices.Load()
	models := []string{}
	for _, p := range c.Prices {
		if p.Platform == platform {
			models = append(models, p.Models...)
		}
	}
	sort.Strings(models)
	return reply(w, map[string]any{"models": models, "as_of": c.AsOf, "checksum": c.hash})
}
