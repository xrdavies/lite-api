package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Persist the existing settings shape. Only the secret is encrypted; unknown
// fields survive edits, and the read API never returns the stored ciphertext.
type imageStorageConfig struct {
	Enabled          bool   `json:"enabled"`
	ReuseBackup      bool   `json:"reuse_backup_s3"`
	Bucket           string `json:"bucket"`
	Prefix           string `json:"prefix"`
	PublicBaseURL    string `json:"public_base_url"`
	ExpiryHours      int    `json:"presign_expiry_hours"`
	MaxDownloadBytes int64  `json:"max_download_bytes"`
	Endpoint         string `json:"endpoint"`
	Region           string `json:"region"`
	AccessKey        string `json:"access_key_id"`
	Secret           string `json:"secret_access_key,omitempty"`
	PathStyle        bool   `json:"force_path_style"`
}

const imageStorageSetting = "image_storage_config"

func (a *App) imageStorageConfig(ctx context.Context) (imageStorageConfig, error) {
	c := imageStorageConfig{Prefix: "images/", Region: "auto", ExpiryHours: 24, MaxDownloadBytes: 32 << 20}
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", imageStorageSetting).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err == nil {
		err = json.Unmarshal([]byte(raw), &c)
	}
	return c, err
}

func (a *App) storageSecret(value string, encrypt bool) (string, error) {
	cipher := a.chatHistoryCipher()
	aad := []byte("lite-api/image-storage/secret/v1")
	if encrypt {
		nonce := randomBytes(cipher.NonceSize())
		return "enc:v1:" + base64.RawStdEncoding.EncodeToString(cipher.Seal(nonce, nonce, []byte(value), aad)), nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, "enc:v1:"))
	if !strings.HasPrefix(value, "enc:v1:") || err != nil || len(raw) < cipher.NonceSize() {
		return "", &apiError{503, "image storage secret cannot be decrypted"}
	}
	plain, err := cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], aad)
	if err != nil {
		return "", &apiError{503, "image storage secret cannot be decrypted"}
	}
	return string(plain), nil
}

func (a *App) getImageStorage(w http.ResponseWriter, r *http.Request) error {
	c, err := a.imageStorageConfig(r.Context())
	if err != nil {
		return err
	}
	configured := c.Secret != ""
	c.Secret = ""
	return reply(w, map[string]any{"config": c, "secret_configured": configured})
}

func (a *App) saveImageStorage(w http.ResponseWriter, r *http.Request) error {
	var c imageStorageConfig
	if err := decode(w, r, &c); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	if c.Endpoint != "" {
		if err := a.validateUpstream(r.Context(), c.Endpoint); err != nil {
			return err
		}
	}
	if strings.HasSuffix(r.URL.Path, "/test") {
		if c.Secret == "" {
			old, err := a.imageStorageConfig(r.Context())
			if err != nil {
				return err
			}
			c.Secret, err = a.storageSecret(old.Secret, false)
			if err != nil {
				return err
			}
		}
		if !c.complete() {
			return bad("bucket, access_key_id and secret_access_key are required")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		// HEAD checks the real bucket without creating or deleting user objects.
		_, err := a.s3Request(ctx, c, "HEAD", "", "", nil)
		if err != nil {
			return reply(w, map[string]any{"ok": false, "message": "object storage connection or bucket access failed"})
		}
		return reply(w, map[string]any{"ok": true, "message": "connection successful"})
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720036)"); err != nil {
		return err
	}
	old := map[string]json.RawMessage{}
	var raw string
	err = tx.QueryRowContext(r.Context(), "SELECT value FROM settings WHERE key=$1 FOR UPDATE", imageStorageSetting).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (json.Unmarshal([]byte(raw), &old) != nil || old == nil) {
		return errors.New("invalid image storage settings")
	}
	if c.Secret == "" {
		c.Secret = credentialString(old, "secret_access_key")
	} else {
		c.Secret, err = a.storageSecret(c.Secret, true)
		if err != nil {
			return err
		}
	}
	// An incomplete draft can be saved, but is not usable by the task runner.
	encoded, _ := json.Marshal(c)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	for name, value := range fields {
		old[name] = value
	}
	encoded, _ = json.Marshal(old)
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", imageStorageSetting, string(encoded)); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	c.Secret = ""
	return reply(w, c)
}

func (c *imageStorageConfig) validate() error {
	if c.ReuseBackup {
		return bad("reuse_backup_s3 is unsupported; configure independent image storage")
	}
	for _, value := range []*string{&c.Endpoint, &c.Region, &c.Bucket, &c.AccessKey, &c.Secret, &c.Prefix, &c.PublicBaseURL} {
		*value = strings.TrimSpace(*value)
	}
	if c.Region == "" {
		c.Region = "auto"
	}
	if c.Prefix == "" {
		c.Prefix = "images/"
	}
	c.Prefix = strings.TrimSuffix(c.Prefix, "/") + "/"
	if !safeObjectKey(c.Prefix) || len(c.Prefix) > 512 {
		return bad("invalid image storage prefix")
	}
	if c.ExpiryHours == 0 {
		c.ExpiryHours = 24
	}
	if c.ExpiryHours < 1 || c.ExpiryHours > 168 {
		return bad("presign_expiry_hours must be between 1 and 168")
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = 32 << 20
	}
	if c.MaxDownloadBytes < 1 || c.MaxDownloadBytes > 64<<20 {
		return bad("max_download_bytes must be between 1 and 67108864")
	}
	for _, v := range []string{c.Region, c.AccessKey, c.Secret} {
		if len(v) > 1024 || strings.ContainsAny(v, "\r\n\x00") {
			return bad("invalid object storage credential or region")
		}
	}
	if strings.ContainsAny(c.Region, "/\\? #") || strings.ContainsAny(c.AccessKey, "/\\, =") {
		return bad("invalid object storage credential or region")
	}
	if c.Bucket != "" {
		if len(c.Bucket) < 3 || len(c.Bucket) > 63 || c.Bucket[0] == '-' || c.Bucket[len(c.Bucket)-1] == '-' || strings.Contains(c.Bucket, "..") {
			return bad("invalid bucket")
		}
		for _, b := range c.Bucket {
			if b < 'a' || b > 'z' {
				if b < '0' || b > '9' {
					if b != '-' && b != '.' {
						return bad("invalid bucket")
					}
				}
			}
		}
	}
	if c.Endpoint != "" {
		if len(c.Endpoint) > 2048 {
			return bad("invalid object storage endpoint")
		}
		if _, err := parseUpstreamURL(c.Endpoint); err != nil {
			return err
		}
	}
	if c.PublicBaseURL != "" {
		u, err := parseUpstreamURL(c.PublicBaseURL)
		if err != nil || u.Scheme != "https" || len(c.PublicBaseURL) > 2048 {
			return bad("public_base_url must be an HTTPS URL without credentials or query")
		}
		c.PublicBaseURL = strings.TrimRight(c.PublicBaseURL, "/")
	}
	return nil
}

func (c imageStorageConfig) complete() bool {
	return c.Bucket != "" && c.AccessKey != "" && c.Secret != ""
}

func safeObjectKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\r\n\x00") || len(key) > 1024 {
		return false
	}
	for _, part := range strings.Split(strings.TrimSuffix(key, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (c imageStorageConfig) objectURL(key string) (*url.URL, error) {
	if key != "" && !safeObjectKey(key) {
		return nil, bad("invalid object key")
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		if c.Region == "auto" {
			return nil, bad("an endpoint or explicit AWS region is required")
		}
		endpoint = "https://s3." + c.Region + ".amazonaws.com"
	}
	u, err := parseUpstreamURL(endpoint)
	if err != nil {
		return nil, err
	}
	if c.PathStyle {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + c.Bucket
	} else {
		u.Host = c.Bucket + "." + u.Host
	}
	if key != "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + key
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = s3Escape(u.Path, true)
	return u, nil
}

// S3 signing encodes every byte except RFC 3986 unreserved characters; object
// slashes stay literal. Query values encode slashes and spaces as %2F and %20.
func s3Escape(value string, path bool) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-_.~", rune(b)) || path && b == '/' {
			out.WriteByte(b)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[b>>4])
			out.WriteByte(hex[b&15])
		}
	}
	return out.String()
}

func s3Signature(c imageStorageConfig, at time.Time, canonical string) (string, string) {
	mac := func(key []byte, value string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(value))
		return h.Sum(nil)
	}
	scope := at.UTC().Format("20060102") + "/" + c.Region + "/s3/aws4_request"
	key := mac([]byte("AWS4"+c.Secret), at.UTC().Format("20060102"))
	key = mac(key, c.Region)
	key = mac(key, "s3")
	key = mac(key, "aws4_request")
	sum := sha256.Sum256([]byte(canonical))
	return scope, hex.EncodeToString(mac(key, "AWS4-HMAC-SHA256\n"+at.UTC().Format("20060102T150405Z")+"\n"+scope+"\n"+hex.EncodeToString(sum[:])))
}

func signS3Request(req *http.Request, c imageStorageConfig, body []byte, at time.Time) {
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	stamp := at.UTC().Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	headers := "host;x-amz-content-sha256;x-amz-date"
	canonical := req.Method + "\n" + req.URL.EscapedPath() + "\n" + req.URL.RawQuery + "\nhost:" + req.URL.Host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + stamp + "\n\n" + headers + "\n" + payload
	scope, signature := s3Signature(c, at, canonical)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKey+"/"+scope+", SignedHeaders="+headers+", Signature="+signature)
}

func (c imageStorageConfig) downloadURL(key string, at time.Time) (string, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return "", err
	}
	if c.PublicBaseURL != "" {
		return c.PublicBaseURL + "/" + s3Escape(key, true), nil
	}
	scope := at.UTC().Format("20060102") + "/" + c.Region + "/s3/aws4_request"
	q := url.Values{"X-Amz-Algorithm": {"AWS4-HMAC-SHA256"}, "X-Amz-Credential": {c.AccessKey + "/" + scope}, "X-Amz-Date": {at.UTC().Format("20060102T150405Z")}, "X-Amz-Expires": {strconv.Itoa(c.ExpiryHours * 3600)}, "X-Amz-SignedHeaders": {"host"}}
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	_, signature := s3Signature(c, at, "GET\n"+u.EscapedPath()+"\n"+u.RawQuery+"\nhost:"+u.Host+"\n\nhost\nUNSIGNED-PAYLOAD")
	u.RawQuery += "&X-Amz-Signature=" + signature
	return u.String(), nil
}

func (a *App) s3Request(ctx context.Context, c imageStorageConfig, method, key, contentType string, body []byte) ([]byte, error) {
	u, err := c.objectURL(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	signS3Request(req, c, body, time.Now())
	// Sign the logical host before the shared transport pins its resolved IP.
	tr, err := a.upstreamTransport(ctx, &upstreamAccount{}, req)
	if err != nil {
		return nil, err
	}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &apiError{502, "object storage request failed"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &apiError{502, "object storage rejected request"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxDownloadBytes+1))
	if err != nil || int64(len(data)) > c.MaxDownloadBytes {
		return nil, &apiError{502, "object storage response exceeds limit or was interrupted"}
	}
	return data, nil
}

func (a *App) activeImageStorage(ctx context.Context) (imageStorageConfig, error) {
	c, err := a.imageStorageConfig(ctx)
	if err != nil {
		return c, err
	}
	if err = c.validate(); err != nil {
		return c, err
	}
	if !c.Enabled || !c.complete() {
		return c, &apiError{404, "async image storage is not enabled or configured"}
	}
	c.Secret, err = a.storageSecret(c.Secret, false)
	return c, err
}

// Tasks persist compact object URLs, never base64 image blobs. Repeated uploads
// use the same object key so result persistence can retry without regenerating.
func (a *App) storeImageResult(ctx context.Context, c imageStorageConfig, taskID string, raw []byte) ([]byte, error) {
	if !validNativeModel(taskID) {
		return nil, bad("invalid image task ID")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return nil, &apiError{502, "invalid image result"}
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(result["data"], &items) != nil || len(items) == 0 || len(items) > 10 {
		return nil, &apiError{502, "invalid image results"}
	}
	for i, item := range items {
		data, err := a.imageBytes(ctx, c.MaxDownloadBytes, item)
		if err != nil {
			return nil, err
		}
		kind := http.DetectContentType(data)
		ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}[kind]
		if ext == "" {
			return nil, &apiError{502, "unsupported image content"}
		}
		key := c.Prefix + taskID + "-" + strconv.Itoa(i) + ext
		if _, err := a.s3Request(ctx, c, "PUT", key, kind, data); err != nil {
			return nil, err
		}
		link, err := c.downloadURL(key, time.Now())
		if err != nil {
			return nil, err
		}
		item["url"], _ = json.Marshal(link)
		delete(item, "b64_json")
	}
	result["data"], _ = json.Marshal(items)
	return json.Marshal(result)
}

func (a *App) imageBytes(ctx context.Context, limit int64, item map[string]json.RawMessage) ([]byte, error) {
	encoded := strings.TrimSpace(credentialString(item, "b64_json"))
	link := strings.TrimSpace(credentialString(item, "url"))
	if encoded == "" && strings.HasPrefix(link, "data:") {
		header, body, ok := strings.Cut(link, ",")
		if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") {
			return nil, &apiError{502, "invalid image data URL"}
		}
		encoded = body
	}
	var reader io.Reader
	if encoded != "" {
		reader = base64.NewDecoder(base64.StdEncoding, strings.NewReader(encoded))
	} else {
		u, err := url.Parse(link)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Scheme != "https" && u.Scheme != "http" || len(link) > 16384 {
			return nil, &apiError{502, "invalid image URL"}
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return nil, &apiError{502, "invalid image URL"}
		}
		tr, err := a.upstreamTransport(ctx, &upstreamAccount{}, req)
		if err != nil {
			return nil, &apiError{502, "image download destination is not allowed"}
		}
		defer tr.CloseIdleConnections()
		client := http.Client{Transport: tr, Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return nil, &apiError{502, "image download failed"}
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, &apiError{502, "image download rejected"}
		}
		reader = resp.Body
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) > limit || len(data) == 0 {
		return nil, &apiError{502, "image content exceeds limit or is invalid"}
	}
	return data, nil
}

func (a *App) imageStorageRoutes() {
	a.route("GET /api/v1/admin/backups/image-storage", "admin", a.getImageStorage)
	a.route("PUT /api/v1/admin/backups/image-storage", "admin", a.saveImageStorage)
	a.route("POST /api/v1/admin/backups/image-storage/test", "admin", a.saveImageStorage)
}
