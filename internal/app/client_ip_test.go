package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrustedClientIP(t *testing.T) {
	trusted, err := parseTrustedProxies("127.0.0.1/32, 10.0.0.0/8, ::1/128")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"127.0.0.1", "bad/32", "::ffff:127.0.0.1/128"} {
		if _, err := parseTrustedProxies(raw); err == nil {
			t.Fatalf("invalid proxy configuration accepted: %s", raw)
		}
	}
	for _, tt := range []struct{ name, peer, forwarded, real, want string }{
		{"untrusted", "192.0.2.1:1234", "198.51.100.42", "", "192.0.2.1"},
		{"trusted", "127.0.0.1:1234", "198.51.100.42", "", "198.51.100.42"},
		{"spoofed prefix", "127.0.0.1:1234", "203.0.113.1, 198.51.100.42, 10.0.0.5", "", "198.51.100.42"},
		{"real fallback", "127.0.0.1:1234", "", "198.51.100.42", "198.51.100.42"},
		{"invalid chain", "127.0.0.1:1234", "198.51.100.42, invalid", "203.0.113.1", "127.0.0.1"},
		{"invalid real", "127.0.0.1:1234", "", "198.51.100.42, 203.0.113.1", "127.0.0.1"},
		{"no header", "127.0.0.1:1234", "", "", "127.0.0.1"},
		{"ipv6", "[::1]:1234", "2001:db8::1", "", "2001:db8::1"},
		{"mapped", "[::ffff:127.0.0.1]:1234", "::ffff:198.51.100.42", "", "198.51.100.42"},
		{"zone", "[::1]:1234", "fe80::1%eth0", "", "::1"},
		{"bounded hops", "127.0.0.1:1234", strings.Repeat("10.0.0.1,", 32) + "198.51.100.42", "", "127.0.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.peer
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if tt.real != "" {
				r.Header.Set("X-Real-IP", tt.real)
			}
			a := &App{mux: http.NewServeMux(), trustedProxies: trusted}
			a.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				if got := clientIP(r); got != tt.want {
					t.Errorf("client IP = %s, want %s", got, tt.want)
				}
			})
			a.Handler().ServeHTTP(httptest.NewRecorder(), r)
		})
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Add("X-Forwarded-For", "203.0.113.5")
	r.Header.Add("X-Forwarded-For", "198.51.100.42, 10.0.0.5")
	if got := proxyClientIP(r, trusted); got != "198.51.100.42" {
		t.Fatal("multiple forwarding headers", got)
	}
	if got := proxyClientIP(r, nil); got != "127.0.0.1" {
		t.Fatal("empty config trusts headers", got)
	}
}
