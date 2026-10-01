package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const upstreamRequestIDHeaderKey = "upstream_request_id_header"

func applyAccountHeaderOverrides(headers http.Header, account *upstreamAccount) {
	if account == nil || headers == nil || !headerOverrideEligible(account.Platform) || account.Type != "apikey" || !credentialBool(account.Credentials, "header_override_enabled") {
		return
	}
	var overrides map[string]string
	if json.Unmarshal(account.Credentials["header_overrides"], &overrides) != nil || len(overrides) > maxHeaderOverrides {
		return
	}
	for name, value := range overrides {
		name, value, err := normalizeHeaderOverride(name, value)
		if err != nil || name == "" || value == "" {
			continue
		}
		for existing := range headers {
			if strings.EqualFold(existing, name) {
				delete(headers, existing)
			}
		}
		headers.Set(name, value)
	}
}

func headerOverrideEligible(platform string) bool {
	switch platform {
	case "openai", "anthropic", "grok", "kimi", "zhipu", "deepseek", "minimax":
		return true
	default:
		return false
	}
}

func validRequestIDHeader(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	return true
}

func cleanUpstreamRequestID(id string) string {
	id = strings.TrimSpace(id)
	if !utf8.ValidString(id) || strings.ContainsAny(id, "\x00\r\n") {
		return ""
	}
	if len(id) > 128 {
		id = id[:128]
		for !utf8.ValidString(id) {
			id = id[:len(id)-1]
		}
	}
	return id
}

// Only an explicitly configured HTTP response header identifies the request.
// Provider resource IDs and WebSocket handshake IDs have different meanings.
func upstreamRequestID(account *upstreamAccount, headers http.Header) string {
	if account == nil {
		return ""
	}
	name := strings.TrimSpace(credentialString(account.Extra, upstreamRequestIDHeaderKey))
	if !validRequestIDHeader(name) {
		return ""
	}
	return cleanUpstreamRequestID(headers.Get(name))
}

// URL and DNS validation run at configuration time and again on each connection.
// Private destinations require deployment-level CIDR authorization, never a request flag.
func parseUpstreamURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, bad("invalid upstream URL")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, bad("invalid upstream port")
		}
	}
	if strings.Contains(u.Path, "..") || strings.ContainsAny(raw, "\r\n\\") {
		return nil, bad("invalid upstream path")
	}
	return u, nil
}
func (a *App) allowedAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range a.privateUpstreams {
		if prefix.Contains(addr) {
			return true
		}
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, raw := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96"} {
		if netip.MustParsePrefix(raw).Contains(addr) {
			return false
		}
	}
	return true
}
func (a *App) resolveUpstream(ctx context.Context, host string) (netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !a.allowedAddress(addr) {
			return netip.Addr{}, bad("upstream destination is not allowed")
		}
		return addr.Unmap(), nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		return netip.Addr{}, bad("upstream hostname cannot be resolved")
	}
	for _, addr := range addresses {
		if !a.allowedAddress(addr) {
			return netip.Addr{}, bad("upstream destination is not allowed")
		}
	}
	return addresses[0].Unmap(), nil
}
func (a *App) validateUpstream(ctx context.Context, raw string) error {
	u, err := parseUpstreamURL(raw)
	if err != nil {
		return err
	}
	addr, err := a.resolveUpstream(ctx, u.Hostname())
	if err != nil {
		return err
	}
	if u.Scheme == "http" && !addr.IsPrivate() && !addr.IsLoopback() {
		return bad("public upstreams require HTTPS")
	}
	return nil
}
func upstreamURL(base, path string) (string, error) {
	u, err := parseUpstreamURL(base)
	if err != nil {
		return "", err
	}
	prefix := strings.TrimRight(u.Path, "/")
	// Explicit versioned API roots (including /api/paas/v4) keep their own version.
	if strings.HasPrefix(path, "/v1/") && (strings.HasSuffix(prefix, "/v1") || strings.HasSuffix(prefix, "/v4")) {
		path = strings.TrimPrefix(path, "/v1")
	}
	if strings.HasPrefix(path, "/v1beta/") && strings.HasSuffix(prefix, "/v1beta") {
		path = strings.TrimPrefix(path, "/v1beta")
	}
	path, query, _ := strings.Cut(path, "?")
	u.RawQuery = query
	u.Path = prefix + path
	u.RawPath = ""
	return u.String(), nil
}

// A small connection pool per request keeps proxy changes immediately effective.
// ponytail: per-request transports; cache by immutable account transport revision if handshake cost dominates.
func (a *App) upstreamRequest(ctx context.Context, account *upstreamAccount, method, path string, body []byte) (*http.Response, error) {
	return a.upstreamRequestHeaders(ctx, account, method, path, body, nil)
}
func (a *App) sendUpstreamRequest(ctx context.Context, account *upstreamAccount, method, path string, body []byte, headers http.Header) (*http.Response, error) {
	// Normalize the original protocol path before provider-specific URL joining
	// strips /v1 for DeepSeek's native Responses endpoint.
	if method == http.MethodPost && (path == "/v1/responses" || strings.HasPrefix(path, "/v1/responses/")) {
		body = normalizeNativeCNResponsesBody(account, body)
	}
	path = nativeCNResponsesPath(account, path)
	base, err := account.baseURL()
	if err != nil {
		return nil, err
	}
	endpoint, err := upstreamURL(base, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tr, err := a.upstreamTransport(ctx, account, req)
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Codex-Beta-Features"} {
		if value := headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", "lite-api/1")
	for _, name := range []string{"Content-Type", "Accept"} {
		if value := headers.Get(name); value != "" {
			req.Header.Set(name, value)
		}
	}
	key := credentialString(account.Credentials, "api_key")
	if key != "" {
		protocol := account.protocol()
		// Embeddings and Responses counting use Bearer independently of text protocol.
		// Keep its configured origin and proxy; never send a relay key elsewhere.
		if account.Platform == "openai" && (path == "/v1/embeddings" || path == "/v1/responses/input_tokens") {
			protocol = "chat_completions"
		}
		switch protocol {
		case "anthropic":
			req.Header.Set("x-api-key", key)
			if req.Header.Get("Anthropic-Version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		case "gemini":
			req.Header.Set("x-goog-api-key", key)
		default:
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	applyAccountHeaderOverrides(req.Header, account)
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		tr.CloseIdleConnections()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &apiError{502, "upstream connection failed"}
	}
	resp.Body = &transportBody{ReadCloser: resp.Body, transport: tr}
	a.recordGrokQuota(ctx, account, resp)
	return resp, nil
}

func nativeCNResponsesPath(account *upstreamAccount, path string) string {
	if account == nil || account.Platform != "deepseek" || account.protocol() != "responses" || path != "/v1/responses" && !strings.HasPrefix(path, "/v1/responses/") {
		return path
	}
	return strings.TrimPrefix(path, "/v1")
}

// Shared by HTTP and WebSocket handshakes, including proxy and DNS pinning.
func (a *App) upstreamTransport(ctx context.Context, account *upstreamAccount, req *http.Request) (*http.Transport, error) {
	var proxy *url.URL
	if account.ProxyID != nil {
		var err error
		proxy, err = a.resolveProxy(ctx, *account.ProxyID)
		if err != nil {
			return nil, err
		}
	}
	return a.transportViaProxy(ctx, req, proxy)
}

// A nil proxy explicitly means direct, never the process proxy environment.
func (a *App) transportViaProxy(ctx context.Context, req *http.Request, proxy *url.URL) (*http.Transport, error) {
	addr, err := a.resolveUpstream(ctx, req.URL.Hostname())
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme == "http" && !addr.IsPrivate() && !addr.IsLoopback() {
		return nil, bad("public upstreams require HTTPS")
	}
	originalHost, hostname := req.URL.Host, req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
		if req.URL.Scheme == "http" {
			port = "80"
		}
	}
	// Pin the checked IP even through a proxy; SNI and Host retain the requested name.
	req.URL.Host = net.JoinHostPort(addr.String(), port)
	req.Host = originalHost
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname}
	tr.ResponseHeaderTimeout = 30 * time.Second
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
		if req.URL.Scheme == "http" && (proxy.Scheme == "http" || proxy.Scheme == "https") {
			// Pin the absolute proxy target too; Request.Host would otherwise
			// cause the proxy to resolve the hostname again after validation.
			req.URL.Opaque = "//" + req.URL.Host + req.URL.EscapedPath()
		}
		if proxy.Scheme == "https" {
			proxyAddr, err := a.resolveUpstream(ctx, proxy.Hostname())
			if err != nil {
				return nil, err
			}
			tr.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(proxyAddr.String(), proxy.Port()))
				if err != nil {
					return nil, err
				}
				secure := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: proxy.Hostname()})
				if err = secure.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				return secure, nil
			}
		}
	}
	return tr, nil
}

type transportBody struct {
	io.ReadCloser
	transport *http.Transport
}

func (b *transportBody) Close() error {
	err := b.ReadCloser.Close()
	b.transport.CloseIdleConnections()
	return err
}

type upstreamAccount struct {
	ID                           int64
	Name, Platform, Type, Status string
	Credentials                  map[string]json.RawMessage
	Extra                        map[string]json.RawMessage
	ProxyID                      *int64
	Schedulable                  bool
	UpdatedAt                    time.Time
}

func credentialString(m map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

func apiKeyRequestCredentials(source map[string]json.RawMessage, protocol string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{
		"api_key": source["api_key"], "base_url": source["base_url"],
	}
	out["api_protocol"], _ = json.Marshal(protocol)
	for _, key := range []string{"header_override_enabled", "header_overrides"} {
		if value := source[key]; value != nil {
			out[key] = value
		}
	}
	return out
}
func (u *upstreamAccount) protocol() string {
	if u.Platform == "gemini" {
		return "gemini"
	}
	if u.Platform == "anthropic" {
		return "anthropic"
	}
	protocol := credentialString(u.Credentials, "api_protocol")
	if protocol == "anthropic" || protocol == "responses" {
		return protocol
	}
	return "chat_completions"
}
func (u *upstreamAccount) baseURL() (string, error) {
	if base := credentialString(u.Credentials, "base_url"); base != "" {
		return base, nil
	}
	// These are protocol roots; custom relay roots use the same joining rules.
	switch u.Platform {
	case "openai":
		return "https://api.openai.com", nil
	case "anthropic":
		return "https://api.anthropic.com", nil
	case "gemini":
		return "https://generativelanguage.googleapis.com", nil
	case "grok":
		return "https://api.x.ai", nil
	case "kimi":
		if u.protocol() == "anthropic" {
			return "https://api.moonshot.cn/anthropic", nil
		}
		return "https://api.moonshot.cn", nil
	case "zhipu":
		if u.protocol() == "anthropic" {
			return "https://open.bigmodel.cn/api/anthropic", nil
		}
		return "https://open.bigmodel.cn/api/paas/v4", nil
	case "deepseek":
		if u.protocol() == "anthropic" {
			return "https://api.deepseek.com/anthropic", nil
		}
		return "https://api.deepseek.com", nil
	case "minimax":
		if u.protocol() == "anthropic" {
			return "https://api.minimaxi.com/anthropic", nil
		}
		return "https://api.minimaxi.com", nil
	}
	return "", bad("unsupported platform")
}
func (a *App) loadAccount(ctx context.Context, id int64) (*upstreamAccount, error) {
	return loadAccountFrom(ctx, a.DB, id)
}

func loadAccountFrom(ctx context.Context, q queryer, id int64) (*upstreamAccount, error) {
	u := &upstreamAccount{}
	var credentials, extra []byte
	err := q.QueryRowContext(ctx, `SELECT id,name,platform,type,status,credentials,extra,proxy_id,schedulable,updated_at FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&u.ID, &u.Name, &u.Platform, &u.Type, &u.Status, &credentials, &extra, &u.ProxyID, &u.Schedulable, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if !supportedPlatform(u.Platform) || u.Type != "apikey" {
		return nil, bad("account type or platform is unsupported")
	}
	if err = json.Unmarshal(credentials, &u.Credentials); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(extra, &u.Extra); err != nil {
		return nil, err
	}
	mode := credentialString(u.Credentials, "account_mode")
	if mode != "" && mode != "payg" {
		return nil, bad("only pay-as-you-go accounts are supported")
	}
	if credentialString(u.Credentials, "api_key") == "" {
		return nil, bad("account has no API key")
	}
	return u, nil
}
func (u *upstreamAccount) mappedModel(model string) (string, error) {
	mapped, _, err := u.resolveModelMapping(model)
	return mapped, err
}

func (u *upstreamAccount) resolveModelMapping(model string) (string, bool, error) {
	return u.resolveModelMappingFor(model, false)
}

func (u *upstreamAccount) resolveModelMappingFor(model string, compact bool) (string, bool, error) {
	resolve := func(raw json.RawMessage, name string, strict bool) (string, bool, error) {
		var mapping map[string]string
		if raw != nil {
			if err := json.Unmarshal(raw, &mapping); err != nil {
				return "", false, bad("invalid model mapping")
			}
		}
		if len(mapping) == 0 {
			return name, false, nil
		}
		if value, ok := mapping[name]; ok {
			if value == "" {
				return name, true, nil
			}
			return value, true, nil
		}
		patterns := make([]string, 0, len(mapping))
		for pattern := range mapping {
			if strings.HasSuffix(pattern, "*") {
				patterns = append(patterns, pattern)
			}
		}
		sort.Slice(patterns, func(i, j int) bool {
			if len(patterns[i]) != len(patterns[j]) {
				return len(patterns[i]) > len(patterns[j])
			}
			return patterns[i] < patterns[j]
		})
		for _, pattern := range patterns {
			if strings.HasPrefix(name, strings.TrimSuffix(pattern, "*")) {
				value := mapping[pattern]
				if value == "" || value == "*" {
					return name, true, nil
				}
				return value, true, nil
			}
		}
		if strict {
			return "", false, bad("model is not allowed by this account")
		}
		return name, false, nil
	}
	if compact {
		if mapped, matched, err := resolve(u.Credentials["compact_model_mapping"], model, false); err != nil || matched {
			return mapped, matched, err
		}
	}
	mapped, matched, err := resolve(u.Credentials["model_mapping"], model, true)
	if err != nil || !compact {
		return mapped, matched, err
	}
	if compactModel, compactMatched, compactErr := resolve(u.Credentials["compact_model_mapping"], mapped, false); compactErr != nil || compactMatched {
		return compactModel, true, compactErr
	}
	return mapped, matched, nil
}
func (a *App) resolveProxy(ctx context.Context, id int64) (*url.URL, error) {
	p, err := resolveProxyTarget(ctx, a.DB, id, time.Now())
	if err != nil || p == nil {
		return nil, err
	}
	return a.proxyURL(ctx, p)
}

func (a *App) proxyURL(ctx context.Context, p *proxyTarget) (*url.URL, error) {
	if p.Protocol != "http" && p.Protocol != "https" && p.Protocol != "socks5" && p.Protocol != "socks5h" {
		return nil, bad("unsupported proxy protocol")
	}
	// Pin proxy DNS; HTTPS retains its hostname for certificate validation.
	addr, err := a.resolveUpstream(ctx, p.Host)
	if err != nil {
		return nil, err
	}
	host := addr.String()
	if p.Protocol == "https" {
		host = p.Host
	}
	u := &url.URL{Scheme: p.Protocol, Host: net.JoinHostPort(host, strconv.Itoa(p.Port))}
	if p.Username.Valid && p.Username.String != "" {
		u.User = url.UserPassword(p.Username.String, p.Password.String)
	}
	return u, nil
}

func readUpstreamJSON(resp *http.Response) (map[string]json.RawMessage, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{502, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, errors.New("upstream response interrupted")
	}
	if len(b) > 4<<20 {
		return nil, errors.New("upstream response exceeds limit")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(b, &result) != nil || result == nil {
		return nil, errors.New("upstream returned invalid JSON")
	}
	if value := result["error"]; value != nil && string(value) != "null" {
		return nil, errors.New("upstream returned an error")
	}
	return result, nil
}

func (a *App) acquireAccountSlot(ctx context.Context, id int64) (func(), error) {
	var limit int
	if err := a.DB.QueryRowContext(ctx, "SELECT concurrency FROM accounts WHERE id=$1 AND deleted_at IS NULL", id).Scan(&limit); err != nil {
		return nil, err
	}
	if !a.takeSlot("account", id, limit) {
		return nil, &apiError{429, "upstream account concurrency limit reached"}
	}
	return func() { a.releaseSlot("account", id) }, nil
}
