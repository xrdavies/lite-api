package app

import (
	"encoding/json"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prices are USD per token (not per million). Keep decimal text until calculation.
type modelPrice struct {
	Platform     string                 `json:"platform"`
	Models       []string               `json:"models"`
	BillingMode  string                 `json:"billing_mode"`
	Input        *json.Number           `json:"input_price"`
	Output       *json.Number           `json:"output_price"`
	CacheWrite   *json.Number           `json:"cache_write_price"`
	CacheWrite1h *json.Number           `json:"cache_write_1h_price"`
	CacheRead    *json.Number           `json:"cache_read_price"`
	ImageInput   *json.Number           `json:"image_input_price"`
	ImageOutput  *json.Number           `json:"image_output_price"`
	PerRequest   *json.Number           `json:"per_request_price"`
	Fast         *json.Number           `json:"fast_multiplier"`
	Flex         *json.Number           `json:"flex_multiplier"`
	Reasoning    map[string]json.Number `json:"reasoning_effort_multipliers"`
	Intervals    []priceInterval        `json:"intervals"`
	TimePricing  *timePrice             `json:"time_pricing"`
}
type priceInterval struct {
	Min              int64        `json:"min_tokens"`
	Max              *int64       `json:"max_tokens"`
	Label            string       `json:"tier_label"`
	Input            *json.Number `json:"input_price"`
	Output           *json.Number `json:"output_price"`
	CacheWrite       *json.Number `json:"cache_write_price"`
	CacheWrite1h     *json.Number `json:"cache_write_1h_price"`
	CacheRead        *json.Number `json:"cache_read_price"`
	PerRequest       *json.Number `json:"per_request_price"`
	InputMultiplier  *json.Number `json:"input_multiplier"`
	OutputMultiplier *json.Number `json:"output_multiplier"`
	WriteMultiplier  *json.Number `json:"cache_write_multiplier"`
	ReadMultiplier   *json.Number `json:"cache_read_multiplier"`
	SortOrder        int          `json:"sort_order"`
}
type timePrice struct {
	Timezone     string            `json:"timezone"`
	WeekdaysOnly bool              `json:"weekdays_only"`
	Periods      []timePricePeriod `json:"periods"`
}
type timePricePeriod struct {
	Start      string      `json:"start_time"`
	End        string      `json:"end_time"`
	Multiplier json.Number `json:"multiplier"`
}

func positiveDecimal(n json.Number, integer, scale int) bool {
	return validPrice(&n, integer, scale) && rat(n).Sign() > 0
}
func validPrice(n *json.Number, integer, scale int) bool {
	if n == nil {
		return true
	}
	raw := n.String()
	if len(raw) > 100 || !json.Valid([]byte(raw)) {
		return false
	}
	if i := strings.IndexAny(raw, "eE"); i >= 0 {
		exponent, err := strconv.Atoi(raw[i+1:])
		if err != nil || exponent < -100 || exponent > 100 {
			return false
		}
	}
	v, ok := new(big.Rat).SetString(raw)
	if !ok || v.Sign() < 0 {
		return false
	}
	rounded := v.FloatString(scale)
	if len(strings.SplitN(rounded, ".", 2)[0]) > integer {
		return false
	}
	return v.Cmp(rat(json.Number(rounded))) == 0
}
func validMultiplier(n *json.Number) bool {
	return n == nil || positiveDecimal(*n, 6, 6)
}
func pricingName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.HasPrefix(s, "claude-") {
		s = strings.ReplaceAll(s, ".", "-")
	}
	return s
}
func validModelPattern(s string) bool {
	return len(s) > 0 && len(s) <= 200 && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\r\n\x00") && !strings.Contains(strings.TrimSuffix(s, "*"), "*")
}
func patternsOverlap(a, b string) bool {
	return a == b || strings.HasSuffix(a, "*") && strings.HasPrefix(strings.TrimSuffix(b, "*"), strings.TrimSuffix(a, "*")) || strings.HasSuffix(b, "*") && strings.HasPrefix(strings.TrimSuffix(a, "*"), strings.TrimSuffix(b, "*"))
}
func clockSeconds(s string, end bool) (int, error) {
	if end && (s == "00:00" || s == "00:00:00") {
		return 86400, nil
	}
	layout := "15:04:05"
	if len(s) == 5 {
		layout = "15:04"
	}
	v, err := time.Parse(layout, s)
	if err != nil || v.Format(layout) != s {
		return 0, bad("time must use HH:mm or HH:mm:ss")
	}
	return v.Hour()*3600 + v.Minute()*60 + v.Second(), nil
}
func (p *timePrice) validate() error {
	if p == nil || len(p.Periods) == 0 {
		return nil
	}
	if p.Timezone == "" || p.Timezone == "Local" {
		return bad("an explicit timezone is required")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return bad("invalid timezone")
	}
	if len(p.Periods) > 100 {
		return bad("too many pricing periods")
	}
	spans := make([][2]int, 0, len(p.Periods))
	for _, period := range p.Periods {
		start, e1 := clockSeconds(period.Start, false)
		end, e2 := clockSeconds(period.End, true)
		if e1 != nil || e2 != nil || start >= end || period.Start == period.End {
			return bad("invalid pricing time interval")
		}
		if !positiveDecimal(period.Multiplier, 6, 2) {
			return bad("time multiplier must be positive with at most two decimal places")
		}
		spans = append(spans, [2]int{start, end})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	for i := 1; i < len(spans); i++ {
		if spans[i][0] < spans[i-1][1] {
			return bad("pricing periods overlap")
		}
	}
	return nil
}
func validateModelPrices(prices []modelPrice, accountStats bool) error {
	if len(prices) > 1000 {
		return bad("too many pricing entries")
	}
	patterns := map[string][]string{}
	totalModels := 0
	for i := range prices {
		p := &prices[i]
		totalModels += len(p.Models)
		if totalModels > 2000 {
			return bad("too many pricing model patterns")
		}
		if p.Platform == "" {
			p.Platform = "anthropic"
		}
		if !supportedPlatform(p.Platform) {
			return bad("unsupported pricing platform")
		}
		if p.BillingMode == "" {
			p.BillingMode = "token"
		}
		if p.BillingMode != "token" && p.BillingMode != "per_request" && p.BillingMode != "image" {
			return bad("invalid billing mode")
		}
		if len(p.Models) == 0 || len(p.Models) > 100 {
			return bad("pricing requires 1 to 100 models")
		}
		if p.Intervals == nil {
			p.Intervals = []priceInterval{}
		}
		if p.Reasoning == nil {
			p.Reasoning = map[string]json.Number{}
		}
		for _, model := range p.Models {
			if !validModelPattern(model) {
				return bad("invalid model pattern")
			}
			n := pricingName(model)
			// ponytail: bounded configuration scan; use a prefix tree if large catalogs need frequent edits.
			for _, prev := range patterns[p.Platform] {
				if patternsOverlap(n, prev) {
					return bad("overlapping model pricing patterns")
				}
			}
			patterns[p.Platform] = append(patterns[p.Platform], n)
		}
		integer, scale := 8, 12
		if accountStats {
			integer, scale = 10, 10
		}
		for _, n := range []*json.Number{p.Input, p.Output, p.CacheWrite, p.CacheRead} {
			if !validPrice(n, integer, scale) {
				return bad("invalid token price")
			}
		}
		imageInteger, imageScale := 12, 8
		if accountStats {
			imageInteger, imageScale = 10, 10
		}
		if !validPrice(p.CacheWrite1h, 8, 12) || !validPrice(p.ImageInput, 8, 12) || !validPrice(p.ImageOutput, imageInteger, imageScale) || !validPrice(p.PerRequest, 10, 10) || !validMultiplier(p.Fast) || !validMultiplier(p.Flex) {
			return bad("invalid price or multiplier")
		}
		for effort, n := range p.Reasoning {
			switch effort {
			case "none", "minimal", "low", "medium", "high", "xhigh", "max":
			default:
				return bad("invalid reasoning effort")
			}
			if !positiveDecimal(n, 6, 6) {
				return bad("invalid reasoning multiplier")
			}
		}
		if p.BillingMode != "token" && p.PerRequest == nil && len(p.Intervals) == 0 {
			return bad("per-request and image billing require a price")
		}
		if p.TimePricing != nil && len(p.TimePricing.Periods) == 0 {
			p.TimePricing = nil
		}
		if p.TimePricing != nil && (p.BillingMode != "token" || accountStats) {
			return bad("time pricing only applies to channel token prices")
		}
		if err := p.TimePricing.validate(); err != nil {
			return err
		}
		if accountStats && (p.Fast != nil || p.Flex != nil || p.ImageInput != nil) {
			return bad("account stats pricing does not support service tier or image input overrides")
		}
		if len(p.Intervals) > 100 {
			return bad("too many pricing intervals")
		}
		sorted := append([]priceInterval(nil), p.Intervals...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Min < sorted[j].Min })
		labels := map[string]bool{}
		for j, iv := range sorted {
			if iv.Min < 0 || iv.Min > 2147483647 || iv.Max != nil && (*iv.Max <= iv.Min || *iv.Max > 2147483647) || len([]rune(iv.Label)) > 50 || iv.SortOrder < 0 || iv.SortOrder > 1000000 {
				return bad("invalid pricing interval")
			}
			hasPrice := false
			for _, n := range []*json.Number{iv.Input, iv.Output, iv.CacheWrite, iv.CacheWrite1h, iv.CacheRead, iv.PerRequest} {
				if !validPrice(n, 8, 12) {
					return bad("invalid interval price")
				}
				hasPrice = hasPrice || n != nil
			}
			for _, n := range []*json.Number{iv.InputMultiplier, iv.OutputMultiplier, iv.WriteMultiplier, iv.ReadMultiplier} {
				if !validMultiplier(n) || accountStats && n != nil {
					return bad("invalid interval multiplier")
				}
				hasPrice = hasPrice || n != nil
			}
			if !hasPrice {
				return bad("interval requires a price or multiplier")
			}
			if p.BillingMode == "token" {
				if j > 0 && (sorted[j-1].Max == nil || *sorted[j-1].Max > iv.Min) {
					return bad("pricing intervals overlap")
				}
			} else {
				label := strings.ToLower(iv.Label)
				if label != "" {
					if labels[label] {
						return bad("duplicate pricing tier label")
					}
					labels[label] = true
				}
			}
		}
	}
	return nil
}

func rat(n json.Number) *big.Rat { v, _ := new(big.Rat).SetString(n.String()); return v }
func decimalOr(n *json.Number, fallback string) *big.Rat {
	if n == nil {
		return rat(json.Number(fallback))
	}
	return rat(*n)
}
func (p *timePrice) multiplierAt(at time.Time) *big.Rat {
	one := big.NewRat(1, 1)
	if p == nil || len(p.Periods) == 0 || at.IsZero() {
		return one
	}
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return one
	}
	local := at.In(loc)
	if p.WeekdaysOnly && (local.Weekday() == time.Saturday || local.Weekday() == time.Sunday) {
		return one
	}
	sec := local.Hour()*3600 + local.Minute()*60 + local.Second()
	for _, period := range p.Periods {
		start, _ := clockSeconds(period.Start, false)
		end, _ := clockSeconds(period.End, true)
		if sec >= start && sec < end {
			return rat(period.Multiplier)
		}
	}
	return one
}

// Context includes cached input. Output does not move a request into a higher tier.
type priceUsage struct {
	Input, Output, CacheWrite, CacheWrite5m, CacheWrite1h, CacheRead int64
	ImageInput, ImageOutput                                          int64
	Requests                                                         int64
}
type priceCost struct {
	Input, Output, CacheWrite, CacheRead, ImageInput, ImageOutput string
	Total, Actual, Debit                                          string
	totalValue                                                    *big.Rat
}

func (p *modelPrice) interval(context int64) *priceInterval {
	for i := range p.Intervals {
		iv := &p.Intervals[i]
		if context > iv.Min && (iv.Max == nil || context <= *iv.Max) {
			return iv
		}
	}
	return nil
}
func resolvedPrice(base, override, multiplier *json.Number) *big.Rat {
	if override != nil {
		return rat(*override)
	}
	if base == nil {
		return nil
	}
	return new(big.Rat).Mul(rat(*base), decimalOr(multiplier, "1"))
}

// Both billing and the price directory resolve the same mutually exclusive tiers.
func tokenPrices(p modelPrice, iv *priceInterval) [5]*big.Rat {
	if iv == nil {
		iv = &priceInterval{}
	}
	write1h := p.CacheWrite1h
	if write1h == nil {
		write1h = p.CacheWrite
	}
	hour := resolvedPrice(write1h, iv.CacheWrite1h, iv.WriteMultiplier)
	if iv.CacheWrite != nil && iv.CacheWrite1h == nil {
		hour = rat(*iv.CacheWrite)
	}
	return [5]*big.Rat{resolvedPrice(p.Input, iv.Input, iv.InputMultiplier), resolvedPrice(p.Output, iv.Output, iv.OutputMultiplier), resolvedPrice(p.CacheWrite, iv.CacheWrite, iv.WriteMultiplier), hour, resolvedPrice(p.CacheRead, iv.CacheRead, iv.ReadMultiplier)}
}

// calculatePrice requires a resolved price card. Missing prices for consumed units
// are errors; an explicit zero remains free. Catalog fallback belongs to resolution.
func calculatePrice(p modelPrice, u priceUsage, rate json.Number, serviceTier, effort, label string, at time.Time, longContext bool) (priceCost, error) {
	empty := priceCost{}
	if err := validateModelPrices([]modelPrice{p}, false); err != nil {
		return empty, err
	}
	if !validPrice(&rate, 6, 4) {
		return empty, bad("invalid billing rate")
	}
	for _, n := range []int64{u.Input, u.Output, u.CacheWrite, u.CacheWrite5m, u.CacheWrite1h, u.CacheRead, u.ImageInput, u.ImageOutput, u.Requests} {
		if n < 0 || n > 2147483647 {
			return empty, bad("invalid usage count")
		}
	}
	contextTokens := u.Input + u.CacheWrite + u.CacheRead
	var parts [6]*big.Rat
	for i := range parts {
		parts[i] = new(big.Rat)
	}
	total := new(big.Rat)
	multiplier := big.NewRat(1, 1)
	if n, ok := p.Reasoning[effort]; ok {
		multiplier.Mul(multiplier, rat(n))
	}
	if p.BillingMode == "per_request" || p.BillingMode == "image" {
		selected := p.PerRequest
		matchedLabel := false
		if label != "" {
			for _, iv := range p.Intervals {
				if iv.Label == label && iv.PerRequest != nil && rat(*iv.PerRequest).Sign() > 0 {
					selected = iv.PerRequest
					matchedLabel = true
					break
				}
			}
		}
		if !matchedLabel {
			if iv := p.interval(contextTokens); iv != nil && iv.PerRequest != nil && rat(*iv.PerRequest).Sign() > 0 {
				selected = iv.PerRequest
			}
		}
		if selected == nil {
			return empty, bad("no price for request tier")
		}
		count := u.Requests
		if count == 0 {
			count = 1
		}
		total.Mul(rat(*selected), big.NewRat(count, 1))
		total.Mul(total, multiplier)
	} else {
		if !longContext {
			contextTokens = 1
		}
		unitPrices := tokenPrices(p, p.interval(contextTokens))
		prices := [6]*big.Rat{unitPrices[0], unitPrices[1], unitPrices[2], unitPrices[4], nil, nil}
		hourPrice := unitPrices[3]
		if p.ImageInput != nil && rat(*p.ImageInput).Sign() > 0 {
			prices[4] = rat(*p.ImageInput)
		} else {
			prices[4] = prices[0]
		}
		// Channel image output is an explicit override: omission means zero.
		prices[5] = decimalOr(p.ImageOutput, "0")
		imageInput := min(u.ImageInput, u.Input)
		counts := [6]int64{u.Input - imageInput, max(u.Output-u.ImageOutput, 0), 0, u.CacheRead, imageInput, u.ImageOutput}
		switch strings.ToLower(strings.TrimSpace(serviceTier)) {
		case "priority", "fast":
			multiplier.Mul(multiplier, decimalOr(p.Fast, "2"))
		case "ultrafast":
			multiplier.Mul(multiplier, big.NewRat(2, 1))
		case "flex":
			multiplier.Mul(multiplier, decimalOr(p.Flex, "0.5"))
		}
		multiplier.Mul(multiplier, p.TimePricing.multiplierAt(at))
		component := func(count int64, price *big.Rat) (*big.Rat, error) {
			if count == 0 {
				return new(big.Rat), nil
			}
			if price == nil {
				return nil, bad("price unavailable for consumed units")
			}
			return new(big.Rat).Mul(big.NewRat(count, 1), price), nil
		}
		for i, count := range counts {
			v, err := component(count, prices[i])
			if err != nil {
				return empty, err
			}
			parts[i] = v
		}
		five, hour := u.CacheWrite5m, u.CacheWrite1h
		if u.CacheWrite > 0 && five+hour > u.CacheWrite {
			// Positive values: integer half-up rounding preserves the aggregate exactly.
			five = (u.CacheWrite*five*2 + (five + hour)) / (2 * (five + hour))
			hour = u.CacheWrite - five
		}
		split := p.CacheWrite1h != nil
		for _, configured := range p.Intervals {
			split = split || configured.CacheWrite1h != nil
		}
		if split && (five > 0 || hour > 0) {
			c5, err := component(five, prices[2])
			if err != nil {
				return empty, err
			}
			c1, err := component(hour, hourPrice)
			if err != nil {
				return empty, err
			}
			parts[2].Add(c5, c1)
		} else {
			v, err := component(u.CacheWrite, prices[2])
			if err != nil {
				return empty, err
			}
			parts[2] = v
		}
		for _, part := range parts {
			part.Mul(part, multiplier)
			total.Add(total, part)
		}
	}
	actual := new(big.Rat).Mul(total, rat(rate))
	// Log columns retain 10 decimals; balance and quota deltas round once to 8.
	return priceCost{parts[0].FloatString(10), parts[1].FloatString(10), parts[2].FloatString(10), parts[3].FloatString(10), parts[4].FloatString(10), parts[5].FloatString(10), total.FloatString(10), actual.FloatString(10), actual.FloatString(8), total}, nil
}
