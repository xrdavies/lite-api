package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type videoPrices struct {
	IndependentVideo *bool                              `json:"video_rate_independent"`
	VideoRate        *json.Number                       `json:"video_rate_multiplier"`
	Video480         *json.Number                       `json:"video_price_480p"`
	Video720         *json.Number                       `json:"video_price_720p"`
	Video1080        *json.Number                       `json:"video_price_1080p"`
	VideoModels      *map[string]map[string]json.Number `json:"video_model_prices"`
}

func videoFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"xai/", "x-ai/", "grok/"} {
		m = strings.TrimPrefix(m, prefix)
	}
	switch m {
	case "grok-video", "grok-video-latest", "grok-imagine-video-preview":
		return "grok-imagine-video"
	case "grok-video-1.5", "grok-imagine-video-1.5-preview":
		return "grok-imagine-video-1.5"
	}
	return m
}

func (p videoPrices) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	for _, f := range []struct {
		name            string
		value           *json.Number
		whole, fraction int
	}{
		{"video_price_480p", p.Video480, 12, 8}, {"video_price_720p", p.Video720, 12, 8}, {"video_price_1080p", p.Video1080, 12, 8}, {"video_rate_multiplier", p.VideoRate, 6, 4},
	} {
		if f.value == nil {
			continue
		}
		v := json.Number(strings.TrimPrefix(f.value.String(), "-"))
		if !validPrice(&v, f.whole, f.fraction) || f.name == "video_rate_multiplier" && rat(*f.value).Sign() < 0 {
			return bad("invalid " + f.name)
		}
		var value any
		if rat(*f.value).Sign() >= 0 {
			value = f.value.String()
		}
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET "+f.name+"=$2 WHERE id=$1", id, value); err != nil {
			return err
		}
	}
	if p.IndependentVideo != nil {
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET video_rate_independent=$2 WHERE id=$1", id, *p.IndependentVideo); err != nil {
			return err
		}
	}
	if p.VideoModels != nil {
		if len(*p.VideoModels) > 100 {
			return bad("too many video model prices")
		}
		normalized := map[string]map[string]json.Number{}
		for model, tiers := range *p.VideoModels {
			family := videoFamily(model)
			if !validNativeModel(family) || len(family) > 100 {
				return bad("invalid video model price")
			}
			if _, exists := normalized[family]; exists {
				return bad("conflicting video model aliases")
			}
			for tier, price := range tiers {
				if tier != "480p" && tier != "720p" && tier != "1080p" || !validPrice(&price, 12, 8) {
					return bad("invalid video model tier or price")
				}
			}
			normalized[family] = tiers
		}
		raw, _ := json.Marshal(normalized)
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET video_model_prices=$2::jsonb WHERE id=$1", id, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func (g gatewayGroup) videoRate() json.Number {
	if g.IndependentVideo != nil && *g.IndependentVideo && g.VideoRate != nil {
		return *g.VideoRate
	}
	return g.Rate
}

func (s *gatewaySelection) videoCost(g gatewayGroup, model string, u priceUsage, at time.Time) (priceCost, error) {
	// Restrictions still use the configured channel model set, even when the
	// actual amount comes from group media prices.
	if s.Restrict {
		if _, ok := matchPrice(s.Pricing, s.Account.Platform, model); !ok {
			return priceCost{}, bad("video model is not in channel pricing")
		}
	}
	if p, ok := groupModelPrice(s.GroupPricing, model); ok && p.BillingMode == "video" {
		return calculatePrice(p, u, g.videoRate(), "", "", u.VideoResolution, at, g.LongContext)
	}
	var price *json.Number
	if g.VideoModels != nil {
		if v, ok := (*g.VideoModels)[videoFamily(model)][u.VideoResolution]; ok {
			price = &v
		}
	}
	if price == nil {
		switch u.VideoResolution {
		case "480p":
			price = g.Video480
		case "720p":
			price = g.Video720
		case "1080p":
			price = g.Video1080
		default:
			return priceCost{}, bad("invalid video resolution")
		}
	}
	if price == nil {
		if p, ok := matchPrice(s.Pricing, s.Account.Platform, model); ok && (p.BillingMode == "video" || p.BillingMode == "per_request" || p.BillingMode == "image") {
			return calculatePrice(p, u, g.videoRate(), "", "", u.VideoResolution, at, g.LongContext)
		}
		// Fixed compatibility baseline, not a live supplier price feed.
		fallback := "0.05"
		if u.VideoResolution != "480p" {
			fallback = "0.07"
		}
		if videoFamily(model) == "grok-imagine-video-1.5" {
			fallback = map[string]string{"480p": "0.08", "720p": "0.14", "1080p": "0.25"}[u.VideoResolution]
		}
		v := json.Number(fallback)
		price = &v
	}
	return calculatePrice(modelPrice{Platform: "grok", Models: []string{model}, BillingMode: "video", PerRequest: price}, u, g.videoRate(), "", "", u.VideoResolution, at, g.LongContext)
}

func parseGrokVideo(raw []byte, operation string) (map[string]json.RawMessage, string, string, int64, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, "", "", 0, bad("video requires a JSON object")
	}
	model := credentialString(body, "model")
	if !validNativeModel(model) || len(model) > 100 {
		return nil, "", "", 0, bad("invalid video model")
	}
	for name := range body {
		if strings.ToLower(name) != name {
			return nil, "", "", 0, bad("video field names must use lowercase")
		}
	}
	for _, name := range []string{"callback_url", "webhook_url", "request_id"} {
		if value := body[name]; value != nil && string(value) != "null" && string(value) != `""` {
			return nil, "", "", 0, bad("unsupported video " + name)
		}
	}
	if value := body["stream"]; value != nil && string(value) != "false" {
		return nil, "", "", 0, bad("video creation is asynchronous")
	}
	if value := body["n"]; value != nil && string(value) != "1" {
		return nil, "", "", 0, bad("one video per task is supported")
	}
	seconds := int64(8)
	if value := body["duration"]; value != nil {
		if json.Unmarshal(value, &seconds) != nil || seconds < 1 || seconds > 15 {
			return nil, "", "", 0, bad("video duration must be 1 to 15 seconds")
		}
	}
	resolution := "480p"
	if value := body["resolution"]; value != nil {
		if json.Unmarshal(value, &resolution) != nil || resolution != "480p" && resolution != "720p" && resolution != "1080p" {
			return nil, "", "", 0, bad("invalid video resolution")
		}
	}
	if operation == "edits" || operation == "extensions" {
		var video struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(body["video"], &video) != nil || !validAudioURL(video.URL) {
			return nil, "", "", 0, bad("video editing and extension require video.url")
		}
	}
	if operation != "generations" || body["image"] == nil && body["reference_images"] == nil && body["last_frame"] == nil && body["keyframes"] == nil {
		if strings.TrimSpace(credentialString(body, "prompt")) == "" {
			return nil, "", "", 0, bad("video prompt is required")
		}
	}
	// Re-encoding normalizes duplicate JSON keys before both admission and dispatch.
	return body, model, resolution, seconds, nil
}

func grokVideoStatus(raw []byte, t *videoTask) (json.RawMessage, string, priceUsage, string, error) {
	var body map[string]json.RawMessage
	u := priceUsage{}
	invalid := func() (json.RawMessage, string, priceUsage, string, error) {
		return nil, "", u, "", &apiError{502, "invalid upstream video result"}
	}
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return invalid()
	}
	if id := credentialString(body, "request_id"); id != "" && id != t.UpstreamID {
		return invalid()
	}
	status := credentialString(body, "status")
	model := credentialString(body, "model")
	public := map[string]any{"request_id": t.ID, "status": status}
	if model != "" {
		public["model"] = model
	}
	switch status {
	case "pending":
	case "expired", "failed":
		public["error"] = map[string]string{"code": "video_generation_failed", "message": "upstream video task " + status}
	case "done":
		var video map[string]json.RawMessage
		if json.Unmarshal(body["video"], &video) != nil || !validAudioURL(credentialString(video, "url")) {
			return invalid()
		}
		seconds := t.Seconds
		if value := video["duration"]; value != nil {
			var n json.Number
			if json.Unmarshal(value, &n) != nil || !validPrice(&n, 4, 8) || rat(n).Sign() <= 0 {
				return invalid()
			}
			// Preserve the existing integer-second, 1..15 billing contract.
			seconds = new(big.Int).Quo(rat(n).Num(), rat(n).Denom()).Int64()
			if seconds <= 0 {
				seconds = t.Seconds
			}
		}
		u = priceUsage{VideoCount: 1, VideoSeconds: min(max(seconds, 1), 15), VideoResolution: t.Resolution}
		v := map[string]json.RawMessage{"url": video["url"]}
		for _, name := range []string{"duration", "respect_moderation"} {
			if value := video[name]; value != nil {
				v[name] = value
			}
		}
		public["video"] = v
		status = "succeeded"
	default:
		return invalid()
	}
	out, err := json.Marshal(public)
	return out, status, u, model, err
}

func (a *App) videoContent(w http.ResponseWriter, r *http.Request, t *videoTask) error {
	var result struct {
		Status string
		Video  struct{ URL string }
	}
	if json.Unmarshal(t.Result, &result) != nil || result.Status != "done" {
		return conflict("video content is not ready")
	}
	link, err := url.Parse(result.Video.URL)
	if err != nil || !validAudioURL(result.Video.URL) || link.Fragment != "" {
		return &apiError{502, "invalid video content URL"}
	}
	u, err := a.videoSource(r.Context(), t)
	if err != nil {
		return err
	}
	release, err := a.acquireAccountSlot(r.Context(), u.ID)
	if err != nil {
		return err
	}
	defer release()
	req, err := http.NewRequestWithContext(r.Context(), "GET", link.String(), nil)
	if err != nil {
		return err
	}
	// Signed content URLs never receive the account API Key or client headers.
	tr, err := a.upstreamTransport(r.Context(), u, req)
	if err != nil {
		return &apiError{502, "video content destination is not allowed"}
	}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return &apiError{502, "video content download failed"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &apiError{502, "video content download rejected"}
	}
	ct, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(ct, "video/") && ct != "application/octet-stream" {
		return &apiError{502, "invalid video content type"}
	}
	// ponytail: bounded buffering avoids returning a successful truncated file;
	// use disk spooling if larger videos are required.
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
	if err != nil || len(data) == 0 || len(data) > 64<<20 {
		return &apiError{502, "video content exceeds limit or is incomplete"}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, err = w.Write(data)
	return err
}
