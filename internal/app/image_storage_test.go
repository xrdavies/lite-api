package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testS3Storage(t *testing.T, a *App) imageStorageConfig {
	t.Helper()
	endpoint := os.Getenv("TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Fatal("run scripts/test-integration.py for isolated S3 storage")
	}
	c := imageStorageConfig{Enabled: true, Bucket: fmt.Sprintf("lite-api-%x", randomBytes(8)), Prefix: "generated/", Endpoint: endpoint,
		Region: "us-east-1", AccessKey: os.Getenv("TEST_S3_ACCESS_KEY"), Secret: os.Getenv("TEST_S3_SECRET_KEY"), PathStyle: true}
	if err := c.validate(); err != nil || !c.complete() {
		t.Fatal("invalid S3 test configuration", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.s3Request(ctx, c, "PUT", "", "", nil); err != nil {
		t.Fatal("create S3 test bucket", err)
	}
	return c // The integration runner removes the entire disposable storage container.
}

func TestImageStorageS3(t *testing.T) {
	if os.Getenv("TEST_S3_ENDPOINT") == "" {
		t.Skip("run scripts/test-integration.py for isolated S3 storage")
	}
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("::1/128")}}
	c := testS3Storage(t, a)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.s3Request(ctx, c, "HEAD", "", "", nil); err != nil {
		t.Fatal("signed bucket HEAD", err)
	}
	// A mapped loopback address makes the signed Host differ from the pinned
	// IPv4 destination, without relying on a host's localhost IPv6 preference.
	endpoint, _ := url.Parse(c.Endpoint)
	endpoint.Host = "[::ffff:127.0.0.1]:" + endpoint.Port()
	c.Endpoint = endpoint.String()
	png, _ := base64.StdEncoding.DecodeString(testImagePNG)
	for _, key := range []string{"generated/simple.png", "generated/空 格+%?#&=/image.png"} {
		if _, err := a.s3Request(ctx, c, "PUT", key, "image/png", png); err != nil {
			t.Fatal("signed upload", key, err)
		}
		link, err := c.downloadURL(key, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.imageBytes(ctx, c.MaxDownloadBytes, map[string]json.RawMessage{"url": json.RawMessage(fmt.Sprintf("%q", link))})
		if err != nil || !bytes.Equal(got, png) {
			t.Fatal("presigned download changed bytes", key, err)
		}
		parsed, _ := url.Parse(link)
		query := parsed.Query()
		query.Set("X-Amz-Expires", "60")
		parsed.RawQuery = query.Encode()
		if _, err := a.imageBytes(ctx, c.MaxDownloadBytes, map[string]json.RawMessage{"url": json.RawMessage(fmt.Sprintf("%q", parsed.String()))}); err == nil || err.Error() != "image download rejected" {
			t.Fatal("tampered presigned URL was not rejected by storage", err)
		}
		expired, err := c.downloadURL(key, time.Now().Add(-25*time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.imageBytes(ctx, c.MaxDownloadBytes, map[string]json.RawMessage{"url": json.RawMessage(fmt.Sprintf("%q", expired))}); err == nil || err.Error() != "image download rejected" {
			t.Fatal("expired presigned URL was not rejected by storage", err)
		}
	}
	wrong := c
	wrong.Secret += "wrong"
	if _, err := a.s3Request(ctx, wrong, "HEAD", "", "", nil); err == nil || err.Error() != "object storage rejected request" {
		t.Fatal("incorrect signing secret was not rejected by storage", err)
	}
	wrong = c
	wrong.Bucket = "missing-bucket"
	if _, err := a.s3Request(ctx, wrong, "HEAD", "", "", nil); err == nil || err.Error() != "object storage rejected request" {
		t.Fatal("missing bucket was not rejected by storage", err)
	}
	// Multi-image conversion exercises actual uploads and both supported source
	// representations; no blob may survive in the compact persisted result.
	raw := []byte(`{"created":1,"data":[{"b64_json":"` + testImagePNG + `"},{"url":"data:image/png;base64,` + testImagePNG + `"}]}`)
	result, err := a.storeImageResult(ctx, c, "imgtask_s3", raw)
	if err != nil || bytes.Contains(result, []byte("base64")) || bytes.Contains(result, []byte("b64_json")) {
		t.Fatal("image result storage", err)
	}
	var out struct{ Data []map[string]json.RawMessage }
	if json.Unmarshal(result, &out) != nil || len(out.Data) != 2 {
		t.Fatal("image results lost")
	}
	for _, item := range out.Data {
		got, err := a.imageBytes(ctx, c.MaxDownloadBytes, item)
		if err != nil || !bytes.Equal(got, png) {
			t.Fatal("stored image download", err)
		}
	}
}

func TestImageStorageTransportFailures(t *testing.T) {
	a := &App{privateUpstreams: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	var leaked atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/redirect":
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
		case "/images/large":
			_, _ = w.Write(make([]byte, 33))
		case "/images/interrupted":
			w.Header().Set("Content-Length", "32")
			_, _ = io.WriteString(w, "short")
		case "/images/cancel":
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			w.WriteHeader(403)
			_, _ = io.WriteString(w, "private-storage-error")
		}
	}))
	defer server.Close()
	c := imageStorageConfig{Endpoint: server.URL, Bucket: "images", PathStyle: true, Region: "auto", AccessKey: "access", Secret: "secret", MaxDownloadBytes: 32}
	for _, key := range []string{"redirect", "large", "interrupted", "denied", "cancel"} {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := a.s3Request(ctx, c, "GET", key, "", nil)
		cancel()
		if err == nil || strings.Contains(err.Error(), "private-storage-error") {
			t.Fatal("unsafe storage response accepted", key, err)
		}
	}
	if leaked.Load() != 0 {
		t.Fatal("storage credentials followed a redirect")
	}
	if _, err := (&App{}).s3Request(context.Background(), c, "HEAD", "", "", nil); err == nil {
		t.Fatal("private storage destination accepted without deployment authorization")
	}
}

func TestImageStorageConfigAndSigning(t *testing.T) {
	a := &App{secret: []byte(strings.Repeat("s", 32))}
	c := imageStorageConfig{
		Enabled:          true,
		Bucket:           "images",
		Prefix:           "generated/",
		Endpoint:         "https://objects.example.test",
		Region:           "auto",
		AccessKey:        "access",
		Secret:           "secret",
		PathStyle:        true,
		ExpiryHours:      12,
		MaxDownloadBytes: 1 << 20,
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if !c.complete() {
		t.Fatal("complete configuration was rejected")
	}
	encrypted, err := a.storageSecret(c.Secret, true)
	if err != nil || encrypted == c.Secret || !strings.HasPrefix(encrypted, "enc:v1:") {
		t.Fatalf("secret was not encrypted: %q %v", encrypted, err)
	}
	plain, err := a.storageSecret(encrypted, false)
	if err != nil || plain != c.Secret {
		t.Fatalf("secret round trip failed: %q %v", plain, err)
	}
	u, err := c.objectURL("generated/job 1/0.png")
	if err != nil || u.String() != "https://objects.example.test/images/generated/job%201/0.png" {
		t.Fatalf("object URL: %v %v", u, err)
	}
	if got, err := c.downloadURL("generated/job 1/0.png", time.Unix(1710000000, 0)); err != nil || !strings.Contains(got, "X-Amz-Signature=") || !strings.Contains(got, "X-Amz-Expires=43200") {
		t.Fatalf("presigned URL: %s %v", got, err)
	}
	virtual := c
	virtual.PathStyle = false
	virtual.Endpoint = "https://objects.example.test:9443/root%20path"
	if got, err := virtual.objectURL("generated/图片+%.png"); err != nil || got.String() != "https://images.objects.example.test:9443/root%20path/generated/%E5%9B%BE%E7%89%87%2B%25.png" {
		t.Fatal("virtual hosted object URL", got, err)
	}
	virtual.PublicBaseURL = "https://cdn.example.test/images"
	if got, err := virtual.downloadURL("generated/图片+%.png", time.Now()); err != nil || got != "https://cdn.example.test/images/generated/%E5%9B%BE%E7%89%87%2B%25.png" {
		t.Fatal("public object URL", got, err)
	}
	for _, invalid := range []imageStorageConfig{
		{Bucket: "bad..bucket", Endpoint: c.Endpoint},
		{Bucket: "images", Endpoint: "http://objects.example.test", PublicBaseURL: "https://public.example.test/?token=x"},
		{Bucket: "images", Endpoint: c.Endpoint, Prefix: "../secrets"},
	} {
		if err := invalid.validate(); err == nil {
			t.Fatalf("invalid storage configuration accepted: %#v", invalid)
		}
	}
}
