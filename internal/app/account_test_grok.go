package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func accountTestAudio(uri string) ([]byte, string, error) {
	if uri == "" {
		// A quarter-second of mono 16 kHz PCM silence tests the endpoint without
		// storing speech or adding a fixture. It does not measure recognition quality.
		audio := make([]byte, 44+8000)
		copy(audio, "RIFF")
		binary.LittleEndian.PutUint32(audio[4:], uint32(len(audio)-8))
		copy(audio[8:], "WAVEfmt ")
		binary.LittleEndian.PutUint32(audio[16:], 16)
		binary.LittleEndian.PutUint16(audio[20:], 1)
		binary.LittleEndian.PutUint16(audio[22:], 1)
		binary.LittleEndian.PutUint32(audio[24:], 16000)
		binary.LittleEndian.PutUint32(audio[28:], 32000)
		binary.LittleEndian.PutUint16(audio[32:], 2)
		binary.LittleEndian.PutUint16(audio[34:], 16)
		copy(audio[36:], "data")
		binary.LittleEndian.PutUint32(audio[40:], 8000)
		return audio, "probe.wav", nil
	}
	invalid := func() ([]byte, string, error) {
		return nil, "", bad("audio_data_url must contain base64 audio up to 8 MiB")
	}
	header, encoded, ok := strings.Cut(uri, ",")
	if !ok || !strings.HasPrefix(header, "data:audio/") || !strings.HasSuffix(header, ";base64") || len(encoded) > base64.StdEncoding.EncodedLen(8<<20) {
		return invalid()
	}
	kind := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	extensions := map[string]string{"audio/wav": "wav", "audio/x-wav": "wav", "audio/wave": "wav", "audio/mpeg": "mp3", "audio/mp3": "mp3", "audio/ogg": "ogg", "audio/opus": "opus", "audio/webm": "webm", "audio/mp4": "m4a", "audio/m4a": "m4a", "audio/x-m4a": "m4a", "audio/aac": "aac", "audio/flac": "flac", "audio/x-flac": "flac"}
	extension, ok := extensions[kind]
	if !ok {
		return invalid()
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > 8<<20 {
		return invalid()
	}
	return raw, "probe." + extension, nil
}

func (a *App) testGrokAccount(ctx context.Context, u *upstreamAccount, model, mode string, in accountTestInput, result *accountTestResult) error {
	// These endpoints always use Bearer, independent of text API compatibility.
	account := *u
	account.Credentials = map[string]json.RawMessage{"api_key": u.Credentials["api_key"], "base_url": u.Credentials["base_url"], "api_protocol": json.RawMessage(`"chat_completions"`)}
	if mode == "realtime" {
		return a.testAccountRealtime(ctx, &account, model, result)
	}
	var raw []byte
	path, contentType := "/v1/"+mode, "application/json"
	search := &grokSearchRequest{Query: strings.TrimSpace(in.Prompt), MaxResults: 3}
	switch mode {
	case "search":
		if search.Query == "" {
			search.Query = "xAI Grok"
		}
		raw = search.upstreamBody("web_search", model)
		path = "/v1/responses"
	case "tts":
		prompt := strings.TrimSpace(in.Prompt)
		if prompt == "" {
			prompt = "Hello from the lite-api connectivity test."
		}
		raw, _ = json.Marshal(map[string]string{"text": prompt, "voice_id": "eve", "language": "en"})
	case "stt":
		audio, filename, err := accountTestAudio(in.Audio)
		if err != nil {
			return err
		}
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		// Native STT parses its options before it consumes the file stream.
		if err = writer.WriteField("language", "en"); err != nil {
			return err
		}
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			return err
		}
		if _, err = part.Write(audio); err != nil {
			return err
		}
		if err = writer.Close(); err != nil {
			return err
		}
		raw, contentType = buffer.Bytes(), writer.FormDataContentType()
	}
	headers := http.Header{"Content-Type": []string{contentType}, "Accept": []string{"application/json"}}
	if mode == "tts" {
		headers.Set("Accept", "audio/*, application/octet-stream")
	}
	resp, err := a.upstreamRequestHeaders(ctx, &account, "POST", path, raw, headers)
	if err != nil {
		return err
	}
	if mode == "tts" {
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &apiError{502, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode)}
		}
		kind, err := audioResponseType("tts", resp.Header.Get("Content-Type"))
		if err != nil {
			return err
		}
		audio, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		if err != nil {
			return err
		}
		if len(audio) == 0 || len(audio) > 4<<20 || json.Valid(audio) || strings.HasPrefix(http.DetectContentType(audio), "text/") {
			return errors.New("invalid TTS audio result")
		}
		detected := http.DetectContentType(audio)
		// MP3 without an ID3 tag starts with an MPEG frame sync word.
		if detected == "application/octet-stream" && len(audio) >= 4 && audio[0] == 0xff && audio[1]&0xe0 == 0xe0 {
			detected = "audio/mpeg"
		}
		if !strings.HasPrefix(detected, "audio/") && detected != "application/ogg" {
			return errors.New("TTS response has no audio signature")
		}
		kind, _, _ = mime.ParseMediaType(kind)
		if kind == "application/octet-stream" {
			kind = detected
		}
		if !strings.HasPrefix(kind, "audio/") && kind != "application/ogg" {
			return errors.New("unrecognized TTS audio type")
		}
		result.Media = []map[string]any{{"type": "audio", "audio_url": "data:" + kind + ";base64," + base64.StdEncoding.EncodeToString(audio), "mime_type": kind}}
		result.Text = fmt.Sprintf("TTS produced %d audio bytes.", len(audio))
		return nil
	}
	defer resp.Body.Close()
	if mode == "stt" {
		if _, err = audioResponseType(mode, resp.Header.Get("Content-Type")); err != nil {
			return err
		}
	}
	data, err := readUpstreamJSON(resp)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	raw, _ = json.Marshal(data)
	if mode == "stt" {
		probe := &audioRequest{}
		if _, err = probe.usage("stt", raw, time.Second); err != nil {
			return err
		}
		result.Text = "STT returned a valid transcription."
		if text := strings.TrimSpace(credentialString(data, "text")); text != "" {
			result.Text += " " + text
		}
		if in.Audio == "" {
			result.Text += " Synthetic silence tests connectivity only."
		}
		return nil
	}
	normalized, _, err := search.response(raw)
	if err != nil {
		return err
	}
	var sources struct{ Results []searchResult }
	_ = json.Unmarshal(normalized, &sources)
	var output []struct{ Type, Status string }
	_ = json.Unmarshal(data["output"], &output)
	calls := 0
	for _, item := range output {
		if item.Type != "web_search_call" {
			continue
		}
		if item.Status != "" && item.Status != "completed" {
			return errors.New("upstream search tool did not complete")
		}
		calls++
	}
	if calls == 0 && len(sources.Results) == 0 {
		return errors.New("upstream search returned no tool or source evidence")
	}
	result.Text = fmt.Sprintf("Web search completed: %d tool call(s), %d source(s).", calls, len(sources.Results))
	return nil
}

func (a *App) testAccountRealtime(ctx context.Context, u *upstreamAccount, model string, result *accountTestResult) error {
	dialCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	conn, resp, err := a.dialUpstreamSocket(dialCtx, u, "/v1/realtime?model="+url.QueryEscape(model), http.Header{"Authorization": []string{"Bearer " + credentialString(u.Credentials, "api_key")}, "User-Agent": []string{"lite-api/1"}})
	cancel()
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &apiError{502, "upstream Realtime handshake failed"}
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
	defer readCancel()
	kind, raw, err := conn.Read(readCtx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
			result.Text = "Realtime WebSocket handshake succeeded; no initial event within 3 seconds. Audio was not exercised."
			return nil
		}
		return errors.New("upstream Realtime closed before an initial event")
	}
	if kind != websocket.MessageText {
		return errors.New("invalid Realtime initial event")
	}
	if _, _, err = realtimeEvent(kind, raw, model, false); err != nil {
		return errors.New("upstream Realtime initial event failed validation")
	}
	var event map[string]json.RawMessage
	_ = json.Unmarshal(raw, &event)
	typ := credentialString(event, "type")
	if typ != "session.created" && typ != "conversation.created" && typ != "session.updated" {
		return errors.New("unexpected Realtime initial event")
	}
	// Only the event type reaches diagnostics; server payload may contain secrets.
	result.Text = "Realtime WebSocket handshake and " + typ + " verified. Audio was not exercised."
	return nil
}
