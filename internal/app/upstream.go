package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
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
)

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
func (a *App) upstreamRequestHeaders(ctx context.Context, account *upstreamAccount, method, path string, body []byte, headers http.Header) (*http.Response, error) {
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
	key := credentialString(account.Credentials, "api_key")
	if key != "" {
		switch account.protocol() {
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
	return resp, nil
}

// Shared by HTTP and WebSocket handshakes, including proxy and DNS pinning.
func (a *App) upstreamTransport(ctx context.Context, account *upstreamAccount, req *http.Request) (*http.Transport, error) {
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
	if account.ProxyID != nil {
		proxy, err := a.resolveProxy(ctx, *account.ProxyID, map[int64]bool{})
		if err != nil {
			return nil, err
		}
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
	u := &upstreamAccount{}
	var credentials, extra []byte
	err := a.DB.QueryRowContext(ctx, `SELECT id,name,platform,type,status,credentials,extra,proxy_id,schedulable,updated_at FROM accounts WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&u.ID, &u.Name, &u.Platform, &u.Type, &u.Status, &credentials, &extra, &u.ProxyID, &u.Schedulable, &u.UpdatedAt)
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
	var mapping map[string]string
	if raw := u.Credentials["model_mapping"]; raw != nil {
		if err := json.Unmarshal(raw, &mapping); err != nil {
			return "", bad("invalid model mapping")
		}
	}
	if len(mapping) == 0 {
		return model, nil
	}
	if value, ok := mapping[model]; ok {
		if value == "" {
			return model, nil
		}
		return value, nil
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
		if strings.HasPrefix(model, strings.TrimSuffix(pattern, "*")) {
			value := mapping[pattern]
			if value == "" || value == "*" {
				return model, nil
			}
			return value, nil
		}
	}
	return "", bad("model is not allowed by this account")
}
func (a *App) resolveProxy(ctx context.Context, id int64, seen map[int64]bool) (*url.URL, error) {
	if seen[id] || len(seen) > 8 {
		return nil, bad("proxy fallback cycle")
	}
	seen[id] = true
	var protocol, host, status, mode string
	var username, password sql.NullString
	var port int
	var expires sql.NullTime
	var backup sql.NullInt64
	err := a.DB.QueryRowContext(ctx, `SELECT protocol,host,port,username,password,status,expires_at,fallback_mode,backup_proxy_id FROM proxies WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&protocol, &host, &port, &username, &password, &status, &expires, &mode, &backup)
	if err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, bad("proxy is inactive")
	}
	if expires.Valid && time.Now().After(expires.Time) {
		switch mode {
		case "direct":
			return nil, nil
		case "proxy":
			if backup.Valid {
				return a.resolveProxy(ctx, backup.Int64, seen)
			}
		}
		return nil, bad("proxy has expired")
	}
	if protocol != "http" && protocol != "https" && protocol != "socks5" && protocol != "socks5h" {
		return nil, bad("unsupported proxy protocol")
	}
	// The transport dials the proxy using this pinned IP; HTTPS proxies retain a
	// hostname for TLS through the guarded DialContext in the caller.
	addr, err := a.resolveUpstream(ctx, host)
	if err != nil {
		return nil, err
	}
	pinnedHost := addr.String()
	if protocol == "https" {
		pinnedHost = host
	}
	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(pinnedHost, strconv.Itoa(port))}
	if username.Valid && username.String != "" {
		u.User = url.UserPassword(username.String, password.String)
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
	if result["error"] != nil {
		return nil, errors.New("upstream returned an error")
	}
	return result, nil
}
