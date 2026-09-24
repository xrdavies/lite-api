package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const webSearchSetting = "web_search_emulation_config"
const webSearchFeature = "web_search_emulation"

type webSearchConfig struct {
	Enabled   bool                `json:"enabled"`
	Providers []webSearchProvider `json:"providers"`
}
type webSearchProvider struct {
	Type         string `json:"type"`
	APIKey       string `json:"api_key,omitempty"`
	Configured   bool   `json:"api_key_configured"`
	QuotaLimit   *int64 `json:"quota_limit"`
	SubscribedAt *int64 `json:"subscribed_at,omitempty"`
	QuotaUsed    int64  `json:"quota_used,omitempty"`
	ProxyID      *int64 `json:"proxy_id"`
	ExpiresAt    *int64 `json:"expires_at,omitempty"`
}
type webSearchResult struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	PageAge string `json:"page_age,omitempty"`
}
type webSearchResponse struct {
	Provider string            `json:"provider"`
	Results  []webSearchResult `json:"results"`
	Query    string            `json:"query"`
}

// Fixed endpoints keep provider credentials out of administrator-supplied URLs.
var webSearchEndpoints = map[string]string{
	"brave":  "https://api.search.brave.com/res/v1/web/search",
	"tavily": "https://api.tavily.com/search",
}

func loadWebSearch(ctx context.Context, q queryer) (webSearchConfig, error) {
	cfg := webSearchConfig{Providers: []webSearchProvider{}}
	var raw string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", webSearchSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") || json.Unmarshal([]byte(raw), &cfg) != nil {
		return cfg, &apiError{503, "search configuration is invalid"}
	}
	if cfg.Providers == nil {
		cfg.Providers = []webSearchProvider{}
	}
	return cfg, nil
}

func validWebSearchType(kind string) bool { return kind == "brave" || kind == "tavily" }

func (cfg *webSearchConfig) validate() error {
	if len(cfg.Providers) > 10 {
		return bad("too many search providers")
	}
	seen := map[string]bool{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !validWebSearchType(p.Type) || seen[p.Type] {
			return bad("search providers must have unique brave or tavily types")
		}
		seen[p.Type] = true
		if len(p.APIKey) > 4096 || strings.ContainsAny(p.APIKey, " \t\r\n\x00") || cfg.Enabled && p.APIKey == "" {
			return bad("invalid or missing search API key")
		}
		if p.QuotaLimit != nil && (*p.QuotaLimit < 0 || *p.QuotaLimit > 1_000_000_000_000) || p.ProxyID != nil && *p.ProxyID <= 0 {
			return bad("invalid search quota or proxy")
		}
		for _, ts := range []*int64{p.SubscribedAt, p.ExpiresAt} {
			if ts != nil && (*ts < 0 || *ts > 253402300799) {
				return bad("invalid search timestamp")
			}
		}
		p.Configured, p.QuotaUsed = false, 0 // Output fields never set counters.
	}
	if cfg.Providers == nil {
		cfg.Providers = []webSearchProvider{}
	}
	return nil
}

func (a *App) webSearchConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "PUT" {
		var in *webSearchConfig
		if err := decode(w, r, &in); err != nil {
			return err
		}
		if in == nil {
			return bad("search configuration must be an object")
		}
		tx, err := a.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// Share the proxy graph lock with proxy removal and concurrent config saves.
		if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720034)"); err != nil {
			return err
		}
		old, err := loadWebSearch(r.Context(), tx)
		if err != nil {
			return err
		}
		for i := range in.Providers {
			p := &in.Providers[i]
			for _, prev := range old.Providers {
				if p.Type == prev.Type && p.APIKey == "" {
					p.APIKey = prev.APIKey
				}
			}
			if p.ProxyID != nil {
				if _, err = loadProxyTarget(r.Context(), tx, *p.ProxyID); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return bad("search proxy does not exist")
					}
					return err
				}
			}
		}
		if err = in.validate(); err != nil {
			return err
		}
		raw, _ := json.Marshal(in)
		if _, err = tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", webSearchSetting, string(raw)); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	cfg, err := loadWebSearch(r.Context(), a.DB)
	if err != nil {
		return err
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		p.Configured, p.APIKey = p.APIKey != "", ""
		p.QuotaUsed, err = a.webSearchUsage(r.Context(), p.Type)
		if err != nil {
			return &apiError{503, "search quota storage unavailable"}
		}
	}
	return reply(w, cfg)
}

func webSearchQuotaKey(kind string) string { return "lite:websearch:quota:" + kind }

func (a *App) webSearchUsage(ctx context.Context, kind string) (int64, error) {
	n, err := a.Redis.HGet(ctx, webSearchQuotaKey(kind), "used").Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return n, err
}

// Monthly anchors use UTC midnight and clamp the day to the target month.
func webSearchQuotaTTL(anchor *int64, now time.Time) time.Duration {
	if anchor == nil || *anchor == 0 {
		return 32 * 24 * time.Hour
	}
	start, now := time.Unix(*anchor, 0).UTC(), now.UTC()
	months := max(0, (now.Year()-start.Year())*12+int(now.Month()-start.Month()))
	for {
		month := time.Date(start.Year(), start.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
		last := month.AddDate(0, 1, -1).Day()
		next := month.AddDate(0, 0, min(start.Day(), last)-1)
		if next.After(now) {
			return next.Sub(now)
		}
		months++
	}
}

var webSearchReserve = redis.NewScript(`
local used = tonumber(redis.call('HGET', KEYS[1], 'used') or '0')
if used >= tonumber(ARGV[1]) then return '' end
if redis.call('EXISTS', KEYS[1]) == 0 then
 redis.call('HSET', KEYS[1], 'epoch', ARGV[3])
 redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
redis.call('HINCRBY', KEYS[1], 'used', 1)
return redis.call('HGET', KEYS[1], 'epoch')
`)
var webSearchRollback = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'epoch') == ARGV[1] and tonumber(redis.call('HGET', KEYS[1], 'used') or '0') > 0 then
 return redis.call('HINCRBY', KEYS[1], 'used', -1)
end
return 0
`)

func (a *App) runWebSearch(ctx context.Context, cfg webSearchConfig, query string, accountProxy *int64, test bool) (*webSearchResponse, error) {
	if strings.TrimSpace(query) == "" || len(query) > 4096 || strings.ContainsRune(query, 0) {
		return nil, bad("search query must contain 1 to 4096 bytes")
	}
	cfg.Providers = append([]webSearchProvider(nil), cfg.Providers...)
	if err := cfg.validate(); err != nil {
		return nil, &apiError{503, "search configuration is invalid"}
	}
	ctx, cancel := context.WithTimeout(ctx, 63*time.Second)
	defer cancel()
	type candidate struct {
		provider webSearchProvider
		weight   float64
	}
	var candidates []candidate
	for _, p := range cfg.Providers {
		if p.APIKey == "" || p.ExpiresAt != nil && *p.ExpiresAt <= time.Now().Unix() {
			continue
		}
		weight := float64(0)
		if !test && p.QuotaLimit != nil && *p.QuotaLimit > 0 {
			used, err := a.webSearchUsage(ctx, p.Type)
			if err != nil {
				return nil, &apiError{503, "search quota storage unavailable"}
			}
			if used >= *p.QuotaLimit {
				continue
			}
			weight = float64(*p.QuotaLimit-used) * (0.5 + rand.Float64())
		}
		candidates = append(candidates, candidate{p, weight})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].weight > candidates[j].weight })
	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := c.provider
		var epoch string
		if !test && p.QuotaLimit != nil && *p.QuotaLimit > 0 {
			var err error
			epoch, err = webSearchReserve.Run(ctx, a.Redis, []string{webSearchQuotaKey(p.Type)}, *p.QuotaLimit, max(1, webSearchQuotaTTL(p.SubscribedAt, time.Now()).Milliseconds()), randomToken(18)).Text()
			if err != nil {
				return nil, &apiError{503, "search quota storage unavailable"}
			}
			if epoch == "" {
				continue
			}
		}
		proxy := p.ProxyID
		if accountProxy != nil {
			proxy = accountProxy
		}
		results, err := a.searchProvider(ctx, p, query, proxy)
		if err == nil {
			return &webSearchResponse{p.Type, results, query}, nil
		}
		if epoch != "" {
			cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
			rollbackErr := webSearchRollback.Run(cleanup, a.Redis, []string{webSearchQuotaKey(p.Type)}, epoch).Err()
			stop()
			if rollbackErr != nil {
				return nil, &apiError{503, "search quota rollback failed"}
			}
		}
	}
	return nil, &apiError{503, "no available search provider (exhausted, expired or failed)"}
}

func (a *App) searchProvider(ctx context.Context, p webSearchProvider, query string, proxy *int64) ([]webSearchResult, error) {
	endpoint := webSearchEndpoints[p.Type]
	method, body := "GET", []byte(nil)
	if p.Type == "brave" {
		endpoint += "?" + url.Values{"q": {query}, "count": {"5"}}.Encode()
	} else {
		method = "POST"
		body, _ = json.Marshal(map[string]any{"query": query, "max_results": 5, "search_depth": "basic"})
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if p.Type == "brave" {
		req.Header.Set("X-Subscription-Token", p.APIKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		req.Header.Set("Content-Type", "application/json")
	}
	tr, err := a.upstreamTransport(ctx, &upstreamAccount{ProxyID: proxy}, req)
	if err != nil {
		return nil, err
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, &apiError{502, "search provider rejected request"}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, &apiError{502, "search response interrupted or oversized"}
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil || envelope["error"] != nil {
		return nil, &apiError{502, "invalid search response"}
	}
	if p.Type == "brave" {
		if json.Unmarshal(envelope["web"], &envelope) != nil || envelope == nil {
			return nil, &apiError{502, "missing search results"}
		}
	}
	var items []struct{ URL, Title, Description, Content, Age string }
	if json.Unmarshal(envelope["results"], &items) != nil || items == nil {
		return nil, &apiError{502, "invalid search results"}
	}
	out := make([]webSearchResult, 0, min(5, len(items)))
	for _, item := range items[:min(5, len(items))] {
		u, err := url.Parse(item.URL)
		if err != nil || u.Host == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" || len(item.URL) > 8192 {
			return nil, &apiError{502, "invalid search result URL"}
		}
		snippet := item.Description
		if p.Type == "tavily" {
			snippet = item.Content
		}
		out = append(out, webSearchResult{item.URL, truncate(item.Title, 1024), truncate(snippet, 16384), truncate(item.Age, 128)})
	}
	return out, nil
}

func (a *App) testWebSearch(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Query string `json:"query"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.Query) == "" {
		in.Query = "搜索今年世界大事件"
	}
	cfg, err := loadWebSearch(r.Context(), a.DB)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result, err := a.runWebSearch(ctx, cfg, in.Query, nil, true)
	if err != nil {
		return err
	}
	return reply(w, result)
}

func (a *App) resetWebSearch(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Type string `json:"provider_type"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if !validWebSearchType(in.Type) {
		return bad("provider_type must be brave or tavily")
	}
	if err := a.Redis.Del(r.Context(), webSearchQuotaKey(in.Type)).Err(); err != nil {
		return &apiError{503, "search quota storage unavailable"}
	}
	return reply(w, nil)
}

func (a *App) webSearchRoutes() {
	const path = "/api/v1/admin/settings/web-search-emulation"
	a.route("GET "+path, "admin", a.webSearchConfig)
	a.route("PUT "+path, "admin", a.webSearchConfig)
	a.route("POST "+path+"/test", "admin", a.testWebSearch)
	a.route("POST "+path+"/reset-usage", "admin", a.resetWebSearch)
}
