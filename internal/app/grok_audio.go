package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

type audioPrices struct {
	Realtime *json.Number `json:"audio_realtime_price_per_min"`
	TTS      *json.Number `json:"audio_tts_price_per_million_chars"`
	STT      *json.Number `json:"audio_stt_price_per_hour"`
}

func (p audioPrices) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	for _, f := range []struct {
		name  string
		price *json.Number
	}{{"audio_tts_price_per_million_chars", p.TTS}, {"audio_stt_price_per_hour", p.STT}, {"audio_realtime_price_per_min", p.Realtime}} {
		if f.price == nil {
			continue
		}
		v := json.Number(strings.TrimPrefix(f.price.String(), "-"))
		if !validPrice(&v, 12, 8) {
			return bad("invalid " + f.name)
		}
		var value any
		if rat(*f.price).Sign() >= 0 {
			value = f.price.String()
		}
		if _, err := tx.ExecContext(ctx, "UPDATE groups SET "+f.name+"=$2 WHERE id=$1", id, value); err != nil {
			return err
		}
	}
	return nil
}

func audioProtocol(protocol string) bool { return protocol == "tts" || protocol == "stt" }

func voiceProtocol(protocol string) bool { return audioProtocol(protocol) || protocol == "realtime" }

type audioRequest struct {
	Body        []byte
	ContentType string
	Characters  int64
	Model       string
}

func parseAudioRequest(protocol, contentType string, raw []byte) (*audioRequest, error) {
	in := &audioRequest{Body: raw, ContentType: contentType}
	if contentType == "" {
		in.ContentType = "application/json"
	}
	kind, params, err := mime.ParseMediaType(in.ContentType)
	if err != nil {
		return nil, bad("invalid audio content type")
	}
	if protocol == "tts" {
		var body map[string]json.RawMessage
		if kind != "application/json" || !utf8.Valid(raw) || json.Unmarshal(raw, &body) != nil || body == nil {
			return nil, bad("TTS requires a JSON object")
		}
		for name := range body {
			lower := strings.ToLower(name)
			if (lower == "input" || lower == "text" || lower == "prompt" || lower == "model") && name != lower {
				return nil, bad("audio field names must use lowercase")
			}
		}
		if body["model"] != nil {
			if json.Unmarshal(body["model"], &in.Model) != nil || !validNativeModel(in.Model) {
				return nil, bad("invalid audio model")
			}
		}
		if stream := body["stream"]; stream != nil && string(stream) != "false" {
			return nil, bad("TTS HTTP streaming is unsupported")
		}
		spoken := ""
		for _, name := range []string{"input", "text", "prompt"} {
			if body[name] == nil {
				continue
			}
			var text string
			if json.Unmarshal(body[name], &text) != nil {
				return nil, bad("invalid TTS text")
			}
			text = strings.TrimSpace(text)
			if text != "" {
				if spoken != "" && spoken != text {
					return nil, bad("conflicting TTS text fields")
				}
				spoken = text
				in.Characters = int64(utf8.RuneCountInString(text))
			}
		}
		if in.Characters == 0 || in.Characters > 60000 {
			return nil, bad("TTS text must contain 1 to 60000 characters")
		}
		// Resolve duplicate JSON keys before dispatch so billing and the
		// provider cannot interpret different values from the same request.
		in.Body, _ = json.Marshal(body)
		return in, nil
	}
	if kind == "application/json" {
		var body map[string]json.RawMessage
		if json.Unmarshal(raw, &body) != nil || body == nil || !validAudioURL(credentialString(body, "url")) {
			return nil, bad("STT JSON requires an audio URL")
		}
		if body["model"] != nil {
			if json.Unmarshal(body["model"], &in.Model) != nil || !validNativeModel(in.Model) {
				return nil, bad("invalid audio model")
			}
		}
		if stream := body["stream"]; stream != nil && string(stream) != "false" {
			return nil, bad("STT HTTP streaming is unsupported")
		}
		for name := range body {
			if strings.EqualFold(name, "model") && name != "model" {
				return nil, bad("audio field names must use lowercase")
			}
		}
		in.Body, _ = json.Marshal(body)
		return in, nil
	}
	if kind != "multipart/form-data" || params["boundary"] == "" {
		return nil, bad("STT requires multipart audio or a JSON URL")
	}
	reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	files, urls, fields := 0, 0, 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, bad("invalid audio multipart body")
		}
		fields++
		if fields > 128 {
			return nil, bad("too many audio form fields")
		}
		if part.FileName() != "" {
			if part.FormName() != "file" {
				return nil, bad("unexpected audio upload field")
			}
			n, err := io.Copy(io.Discard, part)
			if err != nil || n == 0 {
				return nil, bad("empty or invalid audio upload")
			}
			files++
		} else {
			if files > 0 {
				return nil, bad("STT options must precede the audio file")
			}
			value, err := io.ReadAll(io.LimitReader(part, 8193))
			if err != nil || len(value) > 8192 {
				return nil, bad("audio form field exceeds limit")
			}
			if part.FormName() == "url" {
				if !validAudioURL(string(value)) {
					return nil, bad("invalid audio URL")
				}
				urls++
			}
			if part.FormName() == "stream" && string(value) != "false" {
				return nil, bad("STT HTTP streaming is unsupported")
			}
			if strings.EqualFold(part.FormName(), "model") {
				if !validNativeModel(string(value)) || in.Model != "" && in.Model != string(value) {
					return nil, bad("invalid or conflicting audio model")
				}
				in.Model = string(value)
			}
		}
		_ = part.Close()
	}
	if files+urls != 1 {
		return nil, bad("STT requires exactly one audio file or URL")
	}
	return in, nil
}

func validAudioURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 8192 && u.Hostname() != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http")
}

func audioResponseType(protocol, contentType string) (string, error) {
	kind, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", &apiError{502, "invalid upstream audio content type"}
	}
	if protocol == "tts" && (strings.HasPrefix(kind, "audio/") || kind == "application/octet-stream") {
		return contentType, nil
	}
	if protocol == "stt" && kind == "application/json" {
		return contentType, nil
	}
	return "", &apiError{502, "unexpected upstream audio content type"}
}

func (in *audioRequest) usage(protocol string, raw []byte, elapsed time.Duration) (priceUsage, error) {
	if len(raw) == 0 {
		return priceUsage{}, &apiError{502, "empty upstream audio response"}
	}
	if protocol == "tts" {
		return priceUsage{AudioUnits: big.NewRat(in.Characters, 1000000).RatString()}, nil
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil || response == nil || response["error"] != nil && string(response["error"]) != "null" {
		return priceUsage{}, &apiError{502, "invalid upstream transcription"}
	}
	var transcript string
	var channels []json.RawMessage
	if (len(response["text"]) == 0 || response["text"][0] != '"' || json.Unmarshal(response["text"], &transcript) != nil) && (json.Unmarshal(response["channels"], &channels) != nil || len(channels) == 0) {
		return priceUsage{}, &apiError{502, "upstream transcription result is missing"}
	}
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(response["usage"], &usage)
	var seconds *big.Rat
	for _, value := range []json.RawMessage{response["duration"], response["duration_seconds"], response["audio_duration"], usage["seconds"]} {
		if len(value) == 0 || value[0] == '"' {
			continue
		}
		n := json.Number(value)
		if validPrice(&n, 10, 8) && rat(n).Sign() > 0 {
			seconds = rat(n)
			break
		}
	}
	if seconds == nil {
		// ponytail: retain the compatibility estimate when the provider omits
		// duration; replace with trusted media metadata when providers expose it.
		seconds = big.NewRat(max(elapsed.Nanoseconds(), 1), int64(time.Second))
	}
	return priceUsage{AudioUnits: seconds.Quo(seconds, big.NewRat(3600, 1)).RatString()}, nil
}

func (g gatewayGroup) audioCost(protocol, units string) (priceCost, error) {
	price, fallback := g.TTS, "15"
	if protocol == "stt" {
		price, fallback = g.STT, "0.10"
	}
	if protocol == "realtime" {
		price, fallback = g.Realtime, "0.05"
	}
	u, ok := new(big.Rat).SetString(units)
	if !voiceProtocol(protocol) || !ok || u.Sign() < 0 || !validPrice(price, 12, 8) || !validPrice(&g.Rate, 6, 4) {
		return priceCost{}, fmt.Errorf("invalid audio billing units or price")
	}
	total := new(big.Rat).Mul(decimalOr(price, fallback), u)
	actual := new(big.Rat).Mul(total, rat(g.Rate))
	return priceCost{Input: "0", Output: "0", CacheWrite: "0", CacheRead: "0", ImageInput: "0", ImageOutput: "0", Total: total.FloatString(10), Actual: actual.FloatString(10), Debit: actual.FloatString(8), totalValue: total}, nil
}

func (s *gatewaySelection) audioCost(g gatewayGroup, model, units string, at time.Time) (priceCost, error) {
	if p, err := s.price(model); err == nil && p.BillingMode == "per_request" {
		return calculatePrice(p, priceUsage{AudioUnits: units}, g.Rate, "", "", s.Audio, at, g.LongContext)
	}
	return g.audioCost(s.Audio, units)
}

// Headers originate from the validated request parser, never from the client
// header map; upstream authentication still comes from the selected account.
func (in *audioRequest) headers() http.Header {
	return http.Header{"Content-Type": []string{in.ContentType}, "Accept": []string{"application/json, audio/*"}}
}
