package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestProxyQualityClassification(t *testing.T) {
	for _, tc := range []struct {
		status     int
		header     http.Header
		body, want string
	}{
		{401, nil, "", "pass"}, {200, nil, "{}", "fail"}, {307, nil, "", "fail"},
		{429, nil, `{}`, "warn"}, {403, nil, `{"error":"denied"}`, "fail"},
		{403, http.Header{"Content-Type": []string{"text/html"}}, "<html>Cloudflare challenge</html>", "challenge"},
		{429, nil, `<script>window._cf_chl_opt={}</script>`, "challenge"},
		{200, http.Header{"Cf-Mitigated": []string{"challenge"}}, "", "challenge"},
	} {
		item := classifyProxyCheck(proxyCheckTargets[1], tc.status, tc.header, []byte(tc.body))
		if item.Status != tc.want {
			t.Fatal("classification", tc, item)
		}
	}
	for _, ray := range []string{"secret=value", strings.Repeat("a", 1000), "1111111111111111-NRT"} {
		item := classifyProxyCheck(proxyCheckTargets[1], 403, http.Header{"Cf-Mitigated": []string{"challenge"}, "Cf-Ray": []string{ray}}, nil)
		if (item.CFRay != "") != (ray == "1111111111111111-NRT") {
			t.Fatal("unsafe diagnostic value", item)
		}
	}
	result := proxyCheckResult{Items: []proxyCheckItem{{Status: "pass"}, {Status: "warn"}, {Status: "fail"}, {Status: "challenge"}, {Status: "pass"}}}
	result.finish()
	if result.Score != 38 || result.Grade != "F" || result.status() != "challenge" || result.Passed != 2 {
		t.Fatal("grade", result)
	}
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Proxy-Authorization") == "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Cookie") != "" {
			t.Error("probe credentials escaped")
		}
		if ip, err := netip.ParseAddr(r.URL.Hostname()); err != nil || !ip.IsLoopback() {
			t.Error("target DNS not pinned")
		}
		switch r.URL.Path {
		case "/trace":
			fmt.Fprint(w, "ip=203.0.113.9\nloc=JP\ncolo=NRT\nsecret=not-for-output\n")
		case "/truncated":
			w.Header().Set("Content-Length", "100")
			fmt.Fprint(w, "short")
		case "/redirect":
			w.Header().Set("Location", "http://127.0.0.1:1/never-follow")
			w.WriteHeader(307)
		case "/huge":
			fmt.Fprint(w, strings.Repeat("x", 9000))
		case "/slow":
			<-r.Context().Done()
		default:
			fmt.Fprint(w, "empty")
		}
	}))
	defer proxy.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(proxy.URL, "http://"))
	n, _ := strconv.Atoi(port)
	p := &proxyTarget{ID: 1, Protocol: "http", Host: host, Port: n, Username: sql.NullString{String: "name", Valid: true}, Password: sql.NullString{String: "secret", Valid: true}}
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}}
	for _, path := range []string{"/trace", "/empty", "/truncated", "/redirect", "/huge"} {
		result := a.runProxyCheck(context.Background(), p, []proxyCheckTarget{{"base_connectivity", "http://localhost:1234" + path, []int{200}}})
		if (result.Passed == 1) != (path == "/trace") {
			t.Fatal("trace validation", path, result)
		}
		if path == "/trace" && (result.ExitIP != "203.0.113.9" || result.CountryCode != "JP" || result.Colo != "NRT") {
			t.Fatal("trace metadata", result)
		}
	}
	if calls.Load() != 5 {
		t.Fatal("redirect or failure retried", calls.Load())
	}
	// Only a bounded prefix is needed for large discovery documents.
	item, raw := a.probeProxyTarget(context.Background(), p, proxyCheckTarget{"gemini", "http://localhost:1234/huge", []int{200}})
	if item.Status != "pass" || len(raw) > 8193 {
		t.Fatal("bounded discovery body", item, len(raw))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	item, _ = a.probeProxyTarget(ctx, p, proxyCheckTarget{"openai", "http://localhost:1234/slow", []int{401}})
	if item.Status != "fail" || ctx.Err() == nil {
		t.Fatal("cancelled probe passed", item)
	}
	before := calls.Load()
	denied := &App{}
	item, _ = denied.probeProxyTarget(context.Background(), p, proxyCheckTargets[0])
	if item.Status != "fail" || calls.Load() != before {
		t.Fatal("private proxy bypassed address policy")
	}
	item, _ = a.probeProxyTarget(context.Background(), p, proxyCheckTarget{"blocked", "http://169.254.169.254/metadata", []int{200}})
	if item.Status != "fail" || calls.Load() != before {
		t.Fatal("private target bypassed address policy")
	}
	// HTTPS proxy certificates are checked even for diagnostic calls.
	tlsProxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("untrusted TLS proxy accepted") }))
	defer tlsProxy.Close()
	p.Host, port, _ = net.SplitHostPort(strings.TrimPrefix(tlsProxy.URL, "https://"))
	p.Port, _ = strconv.Atoi(port)
	p.Protocol = "https"
	item, _ = a.probeProxyTarget(context.Background(), p, proxyCheckTarget{"tls", "http://localhost:1234/trace", []int{200}})
	if item.Status != "fail" {
		t.Fatal("untrusted TLS proxy succeeded")
	}
}

func testProxyQuality(t *testing.T, a *App, admin, ordinary string) {
	t.Helper()
	ctx := context.Background()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Cookie", "private-session")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path string, body any) map[string]any {
		t.Helper()
		w := call(method, path, admin, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	var mode, calls atomic.Int32
	started, release := make(chan struct{}, 1), make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic cHJvYmU6cHJveHktc2VjcmV0" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Api-Key") != "" || !r.URL.IsAbs() {
			t.Error("proxy check leaked credentials or skipped proxy")
		}
		if r.URL.Path == "/trace" {
			if mode.Load() == 3 {
				started <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			if mode.Load() == 2 {
				w.WriteHeader(502)
				fmt.Fprint(w, "proxy-secret")
				return
			}
			fmt.Fprint(w, "ip=203.0.113.19\nloc=JP\ncolo=NRT\nsecret=proxy-secret\n")
			return
		}
		if mode.Load() == 1 {
			switch r.URL.Path {
			case "/anthropic":
				w.Header().Set("Cf-Mitigated", "challenge")
				w.Header().Set("Cf-Ray", "1234567890abcdef-NRT")
				w.WriteHeader(403)
			case "/gemini":
				w.WriteHeader(429)
			case "/grok":
				w.WriteHeader(500)
			default:
				w.WriteHeader(401)
			}
			return
		}
		if r.URL.Path != "/gemini" {
			w.WriteHeader(401)
		}
	}))
	defer server.Close()
	// Only request targets change in this serial integration test; production
	// uses the fixed list. Network, proxy, auth, cache and handlers remain real.
	original := proxyCheckTargets
	proxyCheckTargets = append([]proxyCheckTarget(nil), original...)
	defer func() { proxyCheckTargets = original }()
	for i := range proxyCheckTargets {
		path := proxyCheckTargets[i].Name
		if i == 0 {
			path = "trace"
		}
		proxyCheckTargets[i].URL = "http://localhost:1234/" + path
	}
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	n, _ := strconv.Atoi(port)
	proxy := manage("POST", "/api/v1/admin/proxies", map[string]any{"name": "Quality proxy", "protocol": "http", "host": host, "port": n, "username": "probe", "password": "proxy-secret", "fallback_mode": "direct", "status": "inactive"})
	id := int64(proxy["id"].(float64))
	path := "/api/v1/admin/proxies/" + fmt.Sprint(id)
	for _, suffix := range []string{"/test", "/quality-check"} {
		for _, token := range []string{"", ordinary} {
			if w := call("POST", path+suffix, token, nil); w.Code != 401 && w.Code != 403 {
				t.Fatal("unprivileged probe", w.Code)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached network")
	}
	quality := func() proxyCheckResult {
		t.Helper()
		w := call("POST", path+"/quality-check", admin, map[string]any{"url": "http://169.254.169.254/ignored"})
		var out struct{ Data proxyCheckResult }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal("quality response", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "proxy-secret") || strings.Contains(w.Body.String(), "private-session") {
			t.Fatal("diagnostics exposed secrets")
		}
		return out.Data
	}
	got := quality()
	if got.Score != 100 || got.Grade != "A" || got.Passed != 5 || !got.Cached || calls.Load() != 5 || got.ExitIP != "203.0.113.19" {
		t.Fatal("healthy quality", got)
	}
	if result := manage("GET", path, nil); result["quality_status"] != "healthy" || result["status"] != "inactive" || result["quality_score"] != float64(100) {
		t.Fatal("check changed admin state or missing snapshot", result)
	}
	mode.Store(1)
	got = quality()
	if got.Score != 38 || got.Passed != 2 || got.Challenge != 1 || got.Warn != 1 || got.Failed != 1 || got.Items[2].CFRay == "" {
		t.Fatal("quality classifications", got)
	}
	mode.Store(0)
	if simple := manage("POST", path+"/test", nil); simple["success"] != true || simple["ip_address"] != "203.0.113.19" {
		t.Fatal("basic probe", simple)
	}
	if result := manage("GET", path, nil); result["quality_score"] != float64(38) {
		t.Fatal("basic probe erased quality result", result)
	}
	// A newer failed basic test must win for latency, even within the same second.
	mode.Store(2)
	if simple := manage("POST", path+"/test", nil); simple["success"] != false || simple["error"] == nil {
		t.Fatal("failed basic test response", simple)
	}
	if result := manage("GET", path, nil); result["quality_score"] != float64(38) || result["latency_status"] != "failed" {
		t.Fatal("basic test ordering lost", result)
	}
	mode.Store(0)
	for _, route := range []string{"/api/v1/admin/proxies?search=Quality%20proxy", "/api/v1/admin/proxies/all"} {
		w := call("GET", route, admin, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "proxy-secret") {
			t.Fatal("list result", w.Code, w.Body.String())
		}
		if strings.Contains(route, "?search") && !strings.Contains(w.Body.String(), `"quality_status":"challenge"`) {
			t.Fatal("list lacks cached quality")
		}
	}
	// A changed revision hides previous results without touching account money.
	manage("PUT", path, map[string]any{"name": "Updated quality proxy"})
	if result := manage("GET", path, nil); result["quality_status"] != nil {
		t.Fatal("obsolete check still displayed")
	}
	mode.Store(2)
	before := calls.Load()
	got = quality()
	if got.Failed != 1 || len(got.Items) != 1 || calls.Load() != before+1 {
		t.Fatal("base failure still tested providers", got)
	}
	mode.Store(3)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", path+"/quality-check", admin, nil) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	if w := call("POST", path+"/test", admin, nil); w.Code != 409 {
		t.Error("overlapping probe accepted", w.Code)
	}
	manage("PUT", path, map[string]any{"name": "Changed during check"})
	release <- struct{}{}
	if w := <-done; w.Code != 409 {
		t.Fatal("stale result accepted", w.Code, w.Body.String())
	}
	mode.Store(0)
	if result := manage("GET", path, nil); result["quality_status"] != nil {
		t.Fatal("stale result cached for current proxy")
	}
	for i := 0; i < 4; i++ {
		if !a.takeSlot("proxy-checks", 0, 4) {
			t.Fatal("failed to reserve test slot")
		}
	}
	w := call("POST", path+"/quality-check", admin, nil)
	for i := 0; i < 4; i++ {
		a.releaseSlot("proxy-checks", 0)
	}
	if w.Code != 429 {
		t.Fatal("global diagnostic concurrency limit ignored", w.Code)
	}
	// Expired + direct fallback must still test this exact proxy.
	manage("PUT", path, map[string]any{"status": "active", "expires_at": time.Now().Add(-time.Minute).Unix()})
	if err := a.expireProxies(ctx); err != nil {
		t.Fatal(err)
	}
	before = calls.Load()
	if got = quality(); got.Passed != 5 || calls.Load() != before+5 {
		t.Fatal("expired probe fell back to direct", got)
	}
	// Independent readers recover the Redis snapshot, without an in-memory cache.
	raw, err := jsonRow(a.DB.QueryRow("SELECT "+proxyProjection+" FROM proxies p WHERE id=$1", id))
	if err != nil {
		t.Fatal(err)
	}
	reader := &App{Redis: a.Redis}
	if out := reader.attachProxyChecks(ctx, []json.RawMessage{raw}); !bytes.Contains(out[0], []byte(`"quality_grade":"A"`)) {
		t.Fatal("snapshot did not survive a new reader")
	}
	// Diagnostic Redis failure does not make the proxy appear healthy or block
	// a real probe. Production cache is left untouched for other test workers.
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 20 * time.Millisecond})
	defer broken.Close()
	isolated := &App{DB: a.DB, Redis: broken, privateUpstreams: a.privateUpstreams}
	r := httptest.NewRequest("POST", path+"/quality-check", nil)
	r.SetPathValue("id", fmt.Sprint(id))
	w = httptest.NewRecorder()
	if err = isolated.checkProxyQuality(w, r); err != nil || w.Code != 200 || !strings.Contains(w.Body.String(), `"cached":false`) {
		t.Fatal("cache failure changed diagnostic result", err, w.Body.String())
	}
	if out := isolated.attachProxyChecks(ctx, []json.RawMessage{raw}); !bytes.Equal(out[0], raw) {
		t.Fatal("failed cache changed proxy record")
	}
	manage("DELETE", path, nil)
	if w := call("POST", path+"/quality-check", admin, nil); w.Code != 404 {
		t.Fatal("deleted proxy checked", w.Code)
	}
}
