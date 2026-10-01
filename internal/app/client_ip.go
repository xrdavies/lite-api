package app

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

type clientIPKey struct{}

func parseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || prefix.Addr().Is4In6() {
			return nil, errors.New("invalid TRUSTED_PROXY_CIDRS; use IPv4 or IPv6 CIDRs")
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil && addr.Zone() == "" {
		return addr.Unmap().String()
	}
	return host
}

// Only a trusted socket peer may supply forwarding headers. Walk from the
// nearest proxy toward the client, stopping before any untrusted hop's claims.
func proxyClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(peer)
	if err != nil || addr.Zone() != "" {
		return peer
	}
	addr = addr.Unmap()
	peer = addr.String()
	isTrusted := func(ip netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
	}
	if !isTrusted(addr) {
		return peer
	}
	values := r.Header.Values("X-Forwarded-For")
	if len(values) == 0 {
		values = r.Header.Values("X-Real-IP")
		if len(values) != 1 || strings.Contains(values[0], ",") {
			return peer
		}
	}
	raw := strings.Join(values, ",")
	if len(raw) > 8192 || strings.Count(raw, ",") >= 32 {
		return peer
	}
	hops := strings.Split(raw, ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || ip.Zone() != "" {
			return peer
		}
		addr = ip.Unmap()
		if !isTrusted(addr) {
			return addr.String()
		}
	}
	return addr.String()
}
