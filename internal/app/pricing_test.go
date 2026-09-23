package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func number(s string) *json.Number { n := json.Number(s); return &n }
func TestPricingContracts(t *testing.T) {
	base := modelPrice{Platform: "openai", Models: []string{"test-model"}, BillingMode: "token", Input: number("0.000001"), Output: number("0.000003"), CacheWrite: number("0.000002"), CacheWrite1h: number("0.000004"), CacheRead: number("0.0000001"), Reasoning: map[string]json.Number{"high": "1.5"}}
	maxTokens := int64(100)
	base.Intervals = []priceInterval{{Min: 0, Max: &maxTokens, InputMultiplier: number("1")}, {Min: 100, InputMultiplier: number("2"), OutputMultiplier: number("2"), WriteMultiplier: number("2"), ReadMultiplier: number("2")}}
	base.TimePricing = &timePrice{Timezone: "UTC", WeekdaysOnly: true, Periods: []timePricePeriod{{Start: "09:00", End: "10:00", Multiplier: "0.5"}}}
	at := time.Date(2026, 9, 23, 9, 30, 0, 0, time.UTC)
	cost, err := calculatePrice(base, priceUsage{Input: 60, Output: 10, CacheWrite: 30, CacheWrite5m: 20, CacheWrite1h: 10, CacheRead: 10}, "0.75", "priority", "high", "", at, true)
	if err != nil || cost.Input != "0.0000900000" || cost.Output != "0.0000450000" || cost.CacheWrite != "0.0001200000" || cost.CacheRead != "0.0000015000" || cost.Total != "0.0002565000" || cost.Actual != "0.0001923750" || cost.Debit != "0.00019238" {
		t.Fatalf("exact tier boundary cost: %+v %v", cost, err)
	}
	higher, err := calculatePrice(base, priceUsage{Input: 61, Output: 10, CacheWrite: 30, CacheWrite5m: 20, CacheWrite1h: 10, CacheRead: 10}, "0.75", "priority", "high", "", at, true)
	if err != nil || higher.Total != "0.0005160000" {
		t.Fatalf("higher tier: %+v %v", higher, err)
	}
	low, err := calculatePrice(base, priceUsage{Input: 61, Output: 10, CacheWrite: 30, CacheWrite5m: 20, CacheWrite1h: 10, CacheRead: 10}, "0.75", "priority", "high", "", at, false)
	if err != nil || low.Total != "0.0002580000" {
		t.Fatalf("disabled long context: %+v %v", low, err)
	}
	if base.Input.String() != "0.000001" {
		t.Fatal("price snapshot mutated")
	}
	for _, tc := range []struct {
		at   time.Time
		want string
	}{{at, "0.5"}, {at.Add(30 * time.Minute), "1.0"}, {at.AddDate(0, 0, 3), "1.0"}} {
		if got := base.TimePricing.multiplierAt(tc.at).FloatString(1); got != tc.want {
			t.Errorf("time multiplier: %s", got)
		}
	}
	if base.interval(0) != nil || base.interval(100) != &base.Intervals[0] || base.interval(101) != &base.Intervals[1] {
		t.Fatal("interval boundary differs from (min,max]")
	}
	// A missing price is not a zero price; explicit zeros and tiny prices remain exact.
	blank := modelPrice{Platform: "openai", Models: []string{"blank"}}
	if _, err = calculatePrice(blank, priceUsage{Input: 1}, "1", "", "", "", at, true); err == nil {
		t.Fatal("missing price charged as free")
	}
	blank.Input = number("0")
	if free, err := calculatePrice(blank, priceUsage{Input: 1}, "1", "", "", "", at, true); err != nil || free.Debit != "0.00000000" {
		t.Fatal(free, err)
	}
	blank.Input = number("5e-9")
	if tiny, err := calculatePrice(blank, priceUsage{Input: 1}, "1", "", "", "", at, true); err != nil || tiny.Debit != "0.00000001" {
		t.Fatal(tiny, err)
	}
	if _, err = calculatePrice(blank, priceUsage{Input: -1}, "1", "", "", "", at, true); err == nil {
		t.Fatal("negative usage accepted")
	}
	for _, raw := range []string{"1e1000", "-1", "NaN", "0.0000000000001", "99999999999999999999"} {
		if validPrice(number(raw), 8, 12) {
			t.Errorf("invalid price accepted: %s", raw)
		}
	}
	split, err := calculatePrice(base, priceUsage{CacheWrite: 3, CacheWrite5m: 5, CacheWrite1h: 5}, "1", "", "", "", time.Time{}, true)
	if err != nil || split.CacheWrite != "0.0000080000" {
		t.Fatal("cache details not bounded", split, err)
	}
	base.Intervals[0].Input = number("0.000010")
	base.Intervals[0].InputMultiplier = number("99")
	override, err := calculatePrice(base, priceUsage{Input: 1}, "1", "", "", "", time.Time{}, true)
	if err != nil || override.Input != "0.0000100000" {
		t.Fatal("explicit price must win over interval multiplier", override, err)
	}
}
func TestPriceConfigurationValidation(t *testing.T) {
	good := `{"platform":"openai","models":["gpt-*"],"input_price":1e-6,"output_price":0.000002,"intervals":[{"min_tokens":0,"max_tokens":100,"input_multiplier":1},{"min_tokens":100,"input_multiplier":2}]}`
	var p modelPrice
	if err := json.Unmarshal([]byte(good), &p); err != nil {
		t.Fatal(err)
	}
	if err := validateModelPrices([]modelPrice{p}, false); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"models":[],"input_price":1}`,
		`{"platform":"bedrock","models":["m"]}`,
		`{"models":["claude-4.5","Claude-4-5"]}`,
		`{"models":["gpt-*","gpt-5"]}`,
		`{"models":["m"],"fast_multiplier":0}`,
		`{"models":["m"],"reasoning_effort_multipliers":{"unknown":2}}`,
		`{"models":["m"],"intervals":[{"min_tokens":0,"max_tokens":100,"input_price":1},{"min_tokens":99,"input_price":2}]}`,
		`{"models":["m"],"billing_mode":"image"}`,
		`{"models":["m"],"time_pricing":{"timezone":"Local","periods":[{"start_time":"09:00","end_time":"10:00","multiplier":1}]}}`,
		`{"models":["m"],"time_pricing":{"timezone":"UTC","periods":[{"start_time":"09:00","end_time":"10:00","multiplier":1},{"start_time":"09:59","end_time":"11:00","multiplier":1}]}}`,
	} {
		var p modelPrice
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		if err := validateModelPrices([]modelPrice{p}, false); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := validateModelPrices([]modelPrice{p}, true); err == nil {
		t.Fatal("stats interval multiplier accepted")
	}
	midnight := timePrice{Timezone: "Asia/Tokyo", Periods: []timePricePeriod{{Start: "22:00", End: "00:00", Multiplier: "0.25"}}}
	if err := midnight.validate(); err != nil {
		t.Fatal(err)
	}
	if got := midnight.multiplierAt(time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)).FloatString(2); got != "0.25" {
		t.Fatal(got)
	}
}
func TestSupportedModels(t *testing.T) {
	prices := []modelPrice{{Platform: "openai", Models: []string{"Priced", "Other", "hidden-*"}, Input: number("1")}, {Platform: "anthropic", Models: []string{"Priced"}, Input: number("2")}}
	models := supportedModels(prices, map[string]map[string]string{"openai": {"alias": "priced", "unpriced": "missing", "wild-*": "*"}})
	if len(models) != 5 {
		t.Fatalf("unexpected models: %+v", models)
	}
	for _, m := range models {
		if strings.Contains(m.Name, "*") {
			t.Fatal("wildcard leaked into concrete model list")
		}
		if m.Name == "alias" && (m.Pricing == nil || m.Pricing.Input.String() != "1") {
			t.Fatal("alias did not use target price")
		}
		if m.Name == "unpriced" && m.Pricing != nil {
			t.Fatal("invented unconfigured price")
		}
	}
}

func TestRequestAndImagePricing(t *testing.T) {
	p := modelPrice{Platform: "openai", Models: []string{"image"}, BillingMode: "image", PerRequest: number("0.02"), Reasoning: map[string]json.Number{"high": "1.5"}, Intervals: []priceInterval{{Label: "HD", PerRequest: number("0.03")}}}
	got, err := calculatePrice(p, priceUsage{Requests: 3}, "0.75", "priority", "high", "HD", time.Time{}, true)
	if err != nil || got.Total != "0.1350000000" || got.Actual != "0.1012500000" {
		t.Fatal(got, err)
	}
	p.BillingMode = "token"
	p.Input = number("0.000001")
	p.Output = number("0.000002")
	p.ImageInput = number("0.000003")
	p.ImageOutput = number("0.000004")
	p.Intervals = nil
	got, err = calculatePrice(p, priceUsage{Input: 10, ImageInput: 3, Output: 8, ImageOutput: 2}, "1", "", "", "", time.Time{}, true)
	if err != nil || got.Input != "0.0000070000" || got.ImageInput != "0.0000090000" || got.Output != "0.0000120000" || got.ImageOutput != "0.0000080000" || got.Total != "0.0000360000" {
		t.Fatal(got, err)
	}
	p.ImageInput = number("0")
	p.ImageOutput = nil
	got, err = calculatePrice(p, priceUsage{Input: 10, ImageInput: 30, Output: 8, ImageOutput: 2}, "1", "", "", "", time.Time{}, true)
	if err != nil || got.Input != "0.0000000000" || got.ImageInput != "0.0000100000" || got.ImageOutput != "0.0000000000" {
		t.Fatal("image fallback semantics", got, err)
	}
}

func TestRequestContextTier(t *testing.T) {
	p := modelPrice{Platform: "openai", Models: []string{"request"}, BillingMode: "per_request", PerRequest: number("0.1"), Intervals: []priceInterval{{Min: 100, PerRequest: number("0.2")}}}
	for _, tc := range []struct {
		input int64
		want  string
	}{{100, "0.1000000000"}, {101, "0.2000000000"}} {
		got, err := calculatePrice(p, priceUsage{Input: tc.input}, "1", "", "", "", time.Time{}, true)
		if err != nil || got.Total != tc.want {
			t.Fatal(got, err)
		}
	}
}
