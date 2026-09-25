package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func prepareAccountTest(u *upstreamAccount, in accountTestInput) (string, string, error) {
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	switch mode {
	case "", "default", "text", "image", "video", "search", "tts", "stt", "realtime":
	default:
		return "", "", bad("unsupported account test mode")
	}
	if len(in.Prompt) > 4000 {
		return "", "", bad("test prompt is too long")
	}
	if in.Audio != "" && mode != "stt" {
		return "", "", bad("audio_data_url requires STT mode")
	}
	if mode == "search" || voiceProtocol(mode) {
		if u.Platform != "grok" {
			return "", "", bad("this test mode requires a Grok account")
		}
		if in.Image != "" {
			return "", "", bad("source image requires an image or video test")
		}
		switch mode {
		case "tts", "stt":
			// Standalone audio endpoints do not use the text model mapping.
			if in.Audio != "" {
				if _, _, err := accountTestAudio(in.Audio); err != nil {
					return "", "", err
				}
			}
			return mode, mode, nil
		case "search":
			in.Model = "grok-4.6" // Same configured default as standalone search.
		case "realtime":
			if strings.TrimSpace(in.Model) == "" {
				in.Model = "grok-voice-latest"
			}
		}
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		if mode == "image" {
			switch u.Platform {
			case "openai":
				model = "gpt-image-1"
			case "grok":
				model = "grok-imagine-image"
			case "gemini":
				model = "gemini-2.5-flash-image"
			}
		} else if mode == "video" && u.Platform == "grok" {
			model = "grok-imagine-video"
		} else if mode == "" || mode == "default" || mode == "text" {
			// Fixed compatibility defaults; availability still depends on the
			// configured provider and the account's model mapping/allowlist.
			switch {
			case u.Platform == "gemini":
				model = "gemini-2.0-flash"
			case u.Platform == "grok":
				model = "grok-4.5"
			case u.protocol() == "anthropic" && u.Platform != "openai":
				model = "claude-sonnet-4-5-20250929"
			default:
				model = "gpt-5.4"
			}
		}
	}
	if len(model) > 100 || !validNativeModel(model) {
		return "", "", bad("model_id is required and must fit 100 characters")
	}
	mapped, err := u.mappedModel(model)
	if err != nil {
		return "", "", err
	}
	if len(mapped) > 100 || !validNativeModel(mapped) {
		return "", "", bad("invalid mapped test model")
	}
	if mode == "" || mode == "default" {
		mode = "text"
		m := strings.ToLower(mapped)
		switch {
		case u.Platform == "gemini" && geminiImageModel(m),
			u.Platform == "openai" && (strings.HasPrefix(m, "gpt-image-") || m == "dall-e-2" || m == "dall-e-3"),
			u.Platform == "grok" && (strings.HasPrefix(m, "grok-imagine-image") || strings.HasPrefix(m, "grok-image")):
			mode = "image"
		case u.Platform == "grok" && strings.HasPrefix(videoFamily(m), "grok-imagine-video"),
			u.Platform == "openai" && (strings.HasPrefix(m, "doubao-seedance-") || strings.HasPrefix(m, "seedance-")):
			mode = "video"
		}
	}
	switch mode {
	case "image":
		if u.Platform != "openai" && u.Platform != "grok" && u.Platform != "gemini" {
			return "", "", bad("image tests are unavailable for this platform")
		}
	case "video":
		if u.Platform == "openai" && u.supportsSeedance() {
			mode = "seedance"
		} else if u.Platform != "grok" {
			return "", "", bad("video tests require Grok or an explicit Seedance account")
		}
	case "text":
		if !u.allowsOpenAIProtocol(u.protocol()) {
			return "", "", bad("text test is disabled by account capabilities")
		}
	}
	if in.Image != "" {
		if mode == "text" {
			return "", "", bad("source image requires a media test")
		}
		if _, _, err = accountTestImageData(in.Image); err != nil {
			return "", "", bad("image_data_url must contain a valid image up to 8 MiB")
		}
	}
	return mapped, mode, nil
}

func accountTestImageData(uri string) (string, string, error) {
	header, encoded, ok := strings.Cut(uri, ",")
	if !ok || !strings.HasPrefix(header, "data:image/") || !strings.HasSuffix(header, ";base64") || len(encoded) > base64.StdEncoding.EncodedLen(8<<20) {
		return "", "", errors.New("invalid image data URL")
	}
	kind := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	switch kind {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return "", "", errors.New("unsupported image MIME type")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > 8<<20 || http.DetectContentType(data) != kind {
		return "", "", errors.New("invalid image content")
	}
	return kind, encoded, nil
}

func accountTestImageEvent(uri string) (map[string]any, error) {
	kind := ""
	if strings.HasPrefix(uri, "data:") {
		var err error
		kind, _, err = accountTestImageData(uri)
		if err != nil {
			return nil, err
		}
	} else if !validAudioURL(uri) {
		return nil, errors.New("invalid image result URL")
	}
	return map[string]any{"type": "image", "image_url": uri, "mime_type": kind}, nil
}

// Media stays in memory for the manual SSE response; scheduled results retain
// only the small diagnostic summary, never generated bytes or signed URLs.
func readAccountTestMedia(resp *http.Response) (map[string]json.RawMessage, []byte, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, &apiError{502, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > 16<<20 {
		return nil, nil, errors.New("invalid media test response size")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, nil, errors.New("invalid media test JSON")
	}
	if value := body["error"]; value != nil && string(value) != "null" {
		return nil, nil, errors.New("upstream media test failed")
	}
	return body, raw, nil
}

func (a *App) testAccountMedia(ctx context.Context, u *upstreamAccount, model, mode string, in accountTestInput, result *accountTestResult) error {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = "A red ball on a plain white background."
	}
	// Media endpoints on OpenAI-compatible accounts use Bearer independently of
	// the account's text protocol. Keep the proxy and configured relay root.
	account := *u
	if u.Platform != "gemini" {
		account.Credentials = map[string]json.RawMessage{"api_key": u.Credentials["api_key"], "base_url": u.Credentials["base_url"], "api_protocol": json.RawMessage(`"chat_completions"`)}
	}
	path := "/v1/images/generations"
	body := map[string]any{"model": model, "prompt": prompt, "n": 1}
	switch mode {
	case "image":
		if u.Platform == "grok" {
			body["response_format"] = "b64_json"
		}
		if u.Platform == "gemini" {
			path = "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":generateContent"
			parts := []any{map[string]any{"text": prompt}}
			if in.Image != "" {
				kind, encoded, _ := accountTestImageData(in.Image)
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": kind, "data": encoded}})
			}
			body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}}}
		} else if in.Image != "" {
			path = "/v1/images/edits"
			if u.Platform == "grok" {
				body["image"] = map[string]string{"url": in.Image, "type": "image_url"}
			} else {
				body["images"] = []any{map[string]string{"image_url": in.Image}}
			}
		}
	case "video":
		path = "/v1/videos/generations"
		body = map[string]any{"model": model, "prompt": prompt, "duration": 6, "aspect_ratio": "16:9", "resolution": "480p"}
		if in.Image != "" {
			body["image"] = map[string]string{"url": in.Image, "type": "image_url"}
		}
	case "seedance":
		content := []any{map[string]string{"type": "text", "text": prompt}}
		if in.Image != "" {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{"url": in.Image}, "role": "first_frame"})
		}
		body = map[string]any{"model": model, "content": content, "duration": 5}
	}
	raw, _ := json.Marshal(body)
	var resp *http.Response
	var err error
	if mode == "seedance" {
		resp, err = a.seedanceRequest(ctx, u, "POST", "", raw)
	} else {
		resp, err = a.upstreamRequest(ctx, &account, "POST", path, raw)
	}
	if err != nil {
		return err
	}
	data, raw, err := readAccountTestMedia(resp)
	if err != nil {
		return err
	}
	if mode != "image" {
		return a.pollAccountTestVideo(ctx, &account, mode, data, result)
	}
	if u.Platform == "gemini" {
		count, err := countGeminiImages(raw)
		if err != nil || count < 1 {
			return errors.New("upstream returned no test image")
		}
		var candidates []struct {
			Content struct{ Parts []map[string]json.RawMessage }
		}
		if json.Unmarshal(data["candidates"], &candidates) != nil {
			return errors.New("invalid image candidates")
		}
		for _, c := range candidates {
			for _, part := range c.Content.Parts {
				if part["inlineData"] == nil && part["inline_data"] == nil {
					continue
				}
				value := part["inlineData"]
				if value == nil {
					value = part["inline_data"]
				}
				var inline map[string]json.RawMessage
				if json.Unmarshal(value, &inline) != nil {
					return errors.New("invalid image part")
				}
				kind := credentialString(inline, "mimeType")
				if kind == "" {
					kind = credentialString(inline, "mime_type")
				}
				uri := "data:" + kind + ";base64," + credentialString(inline, "data")
				event, err := accountTestImageEvent(uri)
				if err != nil {
					return err
				}
				result.Media = append(result.Media, event)
			}
		}
	} else {
		var images []map[string]json.RawMessage
		if json.Unmarshal(data["data"], &images) != nil || len(images) == 0 || len(images) > 10 {
			return errors.New("upstream returned no test image")
		}
		for _, item := range images {
			uri := credentialString(item, "url")
			if encoded := credentialString(item, "b64_json"); encoded != "" {
				decoded, err := base64.StdEncoding.DecodeString(encoded)
				if err != nil || len(decoded) == 0 {
					return errors.New("invalid image encoding")
				}
				uri = "data:" + http.DetectContentType(decoded) + ";base64," + encoded
			}
			event, err := accountTestImageEvent(uri)
			if err != nil {
				return err
			}
			result.Media = append(result.Media, event)
		}
	}
	if len(result.Media) == 0 {
		return errors.New("upstream returned no test image")
	}
	result.Text = fmt.Sprintf("Generated %d test image(s).", len(result.Media))
	return nil
}

func (a *App) pollAccountTestVideo(ctx context.Context, u *upstreamAccount, mode string, created map[string]json.RawMessage, result *accountTestResult) error {
	id := credentialString(created, "request_id")
	if mode == "seedance" {
		id = credentialString(created, "id")
	}
	if !validVoiceID(id) {
		return errors.New("invalid video test task ID")
	}
	// Keep the upstream ID if the probe times out: admins can reconcile the
	// possibly billable job. Never resubmit an ambiguous creation in this call.
	result.Text = "Video test task accepted: " + id
	task := &videoTask{ID: id, UpstreamID: id, Seconds: 6, Resolution: "480p"}
	for {
		var resp *http.Response
		var err error
		if mode == "seedance" {
			resp, err = a.seedanceRequest(ctx, u, "GET", id, nil)
		} else {
			resp, err = a.upstreamRequest(ctx, u, "GET", "/v1/videos/"+id, nil)
		}
		if err != nil {
			return err
		}
		data, raw, err := readAccountTestMedia(resp)
		if err != nil {
			return err
		}
		var state, uri string
		if mode == "seedance" {
			_, state, _, _, err = seedanceStatus(raw, task)
			var content map[string]json.RawMessage
			_ = json.Unmarshal(data["content"], &content)
			uri = credentialString(content, "video_url")
		} else {
			_, state, _, _, err = grokVideoStatus(raw, task)
			var video map[string]json.RawMessage
			_ = json.Unmarshal(data["video"], &video)
			uri = credentialString(video, "url")
		}
		if err != nil {
			return err
		}
		switch state {
		case "succeeded":
			if !validAudioURL(uri) {
				return errors.New("video test has no valid output")
			}
			result.Media = []map[string]any{{"type": "video", "video_url": uri, "mime_type": "video/mp4"}}
			result.Text = "Generated test video: " + id
			return nil
		case "failed", "cancelled", "expired":
			return errors.New("video test failed")
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
