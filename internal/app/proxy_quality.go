package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

type proxyCheckTarget struct {
	Name, URL string
	Allowed   []int
}

// These are unauthenticated connectivity targets, not model capability tests.
var proxyCheckTargets = []proxyCheckTarget{
	{"base_connectivity", "https://www.cloudflare.com/cdn-cgi/trace", []int{200}},
	{"openai", "https://api.openai.com/v1/models", []int{401}},
	{"anthropic", "https://api.anthropic.com/v1/messages", []int{400, 401, 404, 405}},
	{"gemini", "https://generativelanguage.googleapis.com/$discovery/rest?version=v1beta", []int{200}},
	{"grok", "https://api.x.ai/v1/models", []int{401}},
}

type proxyCheckItem struct {
	Target  string `json:"target"`
	Status  string `json:"status"`
	HTTP    int    `json:"http_status,omitempty"`
	Latency int64  `json:"latency_ms,omitempty"`
	Message string `json:"message,omitempty"`
	CFRay   string `json:"cf_ray,omitempty"`
}
type proxyCheckResult struct {
	ID          int64            `json:"proxy_id"`
	Score       int              `json:"score"`
	Grade       string           `json:"grade"`
	Summary     string           `json:"summary"`
	ExitIP      string           `json:"exit_ip,omitempty"`
	Country     string           `json:"country,omitempty"`
	CountryCode string           `json:"country_code,omitempty"`
	Colo        string           `json:"colo,omitempty"`
	Latency     int64            `json:"base_latency_ms,omitempty"`
	Passed      int              `json:"passed_count"`
	Warn        int              `json:"warn_count"`
	Failed      int              `json:"failed_count"`
	Challenge   int              `json:"challenge_count"`
	Checked     int64            `json:"checked_at"`
	Observed    time.Time        `json:"observed_at"`
	Items       []proxyCheckItem `json:"items"`
	Cached      bool             `json:"cached"`
}

var proxyRay = regexp.MustCompile(`(?i)^[0-9a-f]{16,32}(?:-[a-z]{3,5})?$`)

func proxyChallenge(status int, header http.Header, body []byte) bool {
	if strings.EqualFold(strings.TrimSpace(header.Get("cf-mitigated")), "challenge") {
		return true
	}
	if status != 403 && status != 429 {
		return false
	}
	text := strings.ToLower(string(body[:min(len(body), 4096)]))
	for _, marker := range []string{"window._cf_chl_opt", "just a moment", "enable javascript and cookies to continue", "__cf_chl_", "challenge-platform"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/html") &&
		(strings.Contains(text, "<html") || strings.Contains(text, "<!doctype html")) &&
		(strings.Contains(text, "cloudflare") || strings.Contains(text, "challenge"))
}

func classifyProxyCheck(target proxyCheckTarget, status int, header http.Header, body []byte) proxyCheckItem {
	item := proxyCheckItem{Target: target.Name, HTTP: status, Status: "fail", Message: "unexpected HTTP status"}
	if proxyChallenge(status, header, body) {
		item.Status, item.Message = "challenge", "target returned a challenge page"
		if ray := strings.TrimSpace(header.Get("Cf-Ray")); proxyRay.MatchString(ray) {
			item.CFRay = ray
		}
		return item
	}
	for _, allowed := range target.Allowed {
		if status == allowed {
			item.Status, item.Message = "pass", "target is reachable"
			return item
		}
	}
	if status == 429 {
		item.Status, item.Message = "warn", "target returned a rate limit"
	}
	return item
}

func (r *proxyCheckResult) finish() {
	for _, item := range r.Items {
		switch item.Status {
		case "pass":
			r.Passed++
		case "warn":
			r.Warn++
		case "challenge":
			r.Challenge++
		default:
			r.Failed++
		}
	}
	r.Score = max(0, 100-r.Warn*10-r.Failed*22-r.Challenge*30)
	switch {
	case r.Score >= 90:
		r.Grade = "A"
	case r.Score >= 75:
		r.Grade = "B"
	case r.Score >= 60:
		r.Grade = "C"
	case r.Score >= 40:
		r.Grade = "D"
	default:
		r.Grade = "F"
	}
	r.Summary = fmt.Sprintf("passed %d, warnings %d, failures %d, challenges %d", r.Passed, r.Warn, r.Failed, r.Challenge)
}

func (r proxyCheckResult) status() string {
	switch {
	case r.Challenge > 0:
		return "challenge"
	case r.Failed > 0:
		return "failed"
	case r.Warn > 0:
		return "warn"
	case r.Passed > 0:
		return "healthy"
	default:
		return "failed"
	}
}

func (a *App) runProxyCheck(ctx context.Context, p *proxyTarget, targets []proxyCheckTarget) proxyCheckResult {
	result := proxyCheckResult{ID: p.ID, Checked: time.Now().Unix(), Items: []proxyCheckItem{}}
	for _, target := range targets {
		item, raw := a.probeProxyTarget(ctx, p, target)
		if target.Name == "base_connectivity" {
			result.Latency = item.Latency
			if item.Status == "pass" {
				for _, line := range strings.Split(string(raw), "\n") {
					key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
					switch key {
					case "ip":
						if ip, err := netip.ParseAddr(value); err == nil && ip.Zone() == "" {
							result.ExitIP = ip.String()
						}
					case "loc":
						if len(value) == 2 && value[0] >= 'A' && value[0] <= 'Z' && value[1] >= 'A' && value[1] <= 'Z' {
							result.CountryCode = value
						}
					case "colo":
						if len(value) == 3 && strings.Trim(value, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") == "" {
							result.Colo = value
						}
					}
				}
				if result.ExitIP == "" {
					item.Status, item.Message = "fail", "trace endpoint returned no valid exit IP"
				}
			}
		}
		result.Items = append(result.Items, item)
		if ctx.Err() != nil || target.Name == "base_connectivity" && item.Status != "pass" {
			break
		}
	}
	result.finish()
	result.Observed = time.Now().UTC()
	return result
}

func (a *App) probeProxyTarget(ctx context.Context, p *proxyTarget, target proxyCheckTarget) (proxyCheckItem, []byte) {
	started := time.Now()
	fail := func(message string) (proxyCheckItem, []byte) {
		return proxyCheckItem{Target: target.Name, Status: "fail", Message: message, Latency: time.Since(started).Milliseconds()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	proxy, err := a.proxyURL(ctx, p) // Exact proxy, never the account fallback resolver.
	if err != nil {
		return fail("proxy destination is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target.URL, nil)
	if err != nil {
		return fail("invalid check target")
	}
	tr, err := a.transportViaProxy(ctx, req, proxy)
	if err != nil {
		return fail("check target is unavailable")
	}
	defer tr.CloseIdleConnections()
	tr.ResponseHeaderTimeout = 10 * time.Second
	tr.MaxResponseHeaderBytes = 64 << 10
	req.Header.Set("Accept", "application/json,text/html,*/*")
	req.Header.Set("User-Agent", "lite-api/1")
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fail("proxy connectivity request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (8<<10)+1))
	if err != nil {
		return fail("check response was interrupted")
	}
	// Target documents may be larger than the classification prefix. A trace
	// must be complete so an error page cannot masquerade as a valid exit IP.
	if target.Name == "base_connectivity" && len(raw) > 8<<10 {
		return fail("trace response exceeds limit")
	}
	item := classifyProxyCheck(target, resp.StatusCode, resp.Header, raw[:min(len(raw), 8<<10)])
	item.Latency = time.Since(started).Milliseconds()
	return item, raw
}

func proxyCheckKey(id int64, revision time.Time, quality bool) string {
	return fmt.Sprintf("proxy:check:%d:%s:%t", id, revision.UTC().Format(time.RFC3339Nano), quality)
}

func (a *App) checkProxyQuality(w http.ResponseWriter, r *http.Request) error {
	return a.handleProxyCheck(w, r, true)
}
func (a *App) testProxy(w http.ResponseWriter, r *http.Request) error {
	return a.handleProxyCheck(w, r, false)
}
func (a *App) handleProxyCheck(w http.ResponseWriter, r *http.Request, quality bool) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if !a.takeSlot("proxy-check", id, 1) {
		return conflict("a check for this proxy is already running")
	}
	defer a.releaseSlot("proxy-check", id)
	if !a.takeSlot("proxy-checks", 0, 4) {
		return &apiError{429, "proxy check concurrency limit reached"}
	}
	defer a.releaseSlot("proxy-checks", 0)
	ctx, cancel := context.WithTimeout(r.Context(), 80*time.Second)
	defer cancel()
	p, err := loadProxyTarget(ctx, a.DB, id)
	if err != nil {
		return err
	}
	targets := proxyCheckTargets
	if !quality {
		targets = targets[:1]
	}
	result := a.runProxyCheck(ctx, p, targets)
	if err = ctx.Err(); err != nil {
		return err
	}
	current, err := loadProxyTarget(ctx, a.DB, id)
	if err != nil {
		return err
	}
	if !current.UpdatedAt.Equal(p.UpdatedAt) {
		return conflict("proxy changed during check; run it again")
	}
	// Keys include the database revision: late writes never replace a newer
	// proxy's check. Separate results keep a basic test from erasing quality.
	cacheCtx, cacheCancel := context.WithTimeout(ctx, 2*time.Second)
	defer cacheCancel()
	result.Cached = true
	raw, _ := json.Marshal(result)
	if err = a.Redis.Set(cacheCtx, proxyCheckKey(id, p.UpdatedAt, quality), raw, 24*time.Hour).Err(); err != nil {
		result.Cached = false
		slog.Warn("proxy check result was not cached", "proxy_id", id)
	}
	if quality {
		return reply(w, result)
	}
	out := map[string]any{"success": result.Passed == 1, "latency_ms": result.Latency, "ip": result.ExitIP,
		"ip_address": result.ExitIP, "loc": result.CountryCode, "country_code": result.CountryCode, "colo": result.Colo,
		"message": result.Items[0].Message, "cached": result.Cached}
	if result.Passed != 1 {
		out["error"] = result.Items[0].Message
	}
	return reply(w, out)
}

func (a *App) attachProxyChecks(ctx context.Context, items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return items
	}
	keys := make([]string, 0, 2*len(items))
	for _, raw := range items {
		var p struct {
			ID      int64
			Updated time.Time `json:"updated_at"`
		}
		if json.Unmarshal(raw, &p) != nil {
			return items
		}
		keys = append(keys, proxyCheckKey(p.ID, p.Updated, false), proxyCheckKey(p.ID, p.Updated, true))
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	values, err := a.Redis.MGet(ctx, keys...).Result()
	if err != nil {
		return items // Diagnostic cache availability does not gate management reads.
	}
	for i, raw := range items {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		var last time.Time
		set := func(name string, value any) { fields[name], _ = json.Marshal(value) }
		for j := 0; j < 2; j++ {
			value, _ := values[2*i+j].(string)
			var result proxyCheckResult
			if json.Unmarshal([]byte(value), &result) != nil || len(result.Items) == 0 {
				continue
			}
			if !result.Observed.Before(last) {
				last = result.Observed
				status := "failed"
				if result.Items[0].Status == "pass" {
					status = "success"
				}
				set("latency_ms", result.Latency)
				set("latency_status", status)
				set("latency_message", result.Items[0].Message)
				set("ip_address", result.ExitIP)
				set("country_code", result.CountryCode)
			}
			if j == 1 {
				set("quality_status", result.status())
				set("quality_score", result.Score)
				set("quality_grade", result.Grade)
				set("quality_summary", result.Summary)
				set("quality_checked_at", result.Checked)
			}
		}
		items[i], _ = json.Marshal(fields)
	}
	return items
}
