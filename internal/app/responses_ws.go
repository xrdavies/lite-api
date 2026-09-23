package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

type responseSocket struct {
	client, upstream       *websocket.Conn
	keyID, groupID, userID int64
	binding                *responseBinding
	proxyTarget            string
	responses              map[string]responseBinding
	responseOrder          []string
}

type responseSocketTurn struct {
	socket   *responseSocket
	warmup   bool
	streamID string
}
type socketTurnKey struct{}

func socketTurn(ctx context.Context) *responseSocketTurn {
	t, _ := ctx.Value(socketTurnKey{}).(*responseSocketTurn)
	return t
}

func (u *upstreamAccount) supportsResponseSocket() bool {
	if u.protocol() != "responses" || u.Platform != "openai" && u.Platform != "grok" {
		return false
	}
	var forced bool
	_ = json.Unmarshal(u.Extra["openai_ws_force_http"], &forced)
	if forced {
		return false
	}
	if raw, ok := u.Extra["openai_apikey_responses_websockets_v2_mode"]; ok {
		var mode string
		return json.Unmarshal(raw, &mode) == nil && mode == "passthrough"
	}
	for _, name := range []string{"openai_apikey_responses_websockets_v2_enabled", "responses_websockets_v2_enabled", "openai_ws_enabled"} {
		if raw, ok := u.Extra[name]; ok {
			var enabled bool
			return json.Unmarshal(raw, &enabled) == nil && enabled
		}
	}
	return false
}
func (s *responseSocket) eligible(u *upstreamAccount) bool {
	return u.supportsResponseSocket() && (s.binding == nil || s.binding.AccountID == u.ID && s.binding.Target == responseTarget(u))
}
func (s *responseSocket) remember(id string, u *upstreamAccount) {
	if id == "" {
		return
	}
	if _, exists := s.responses[id]; !exists {
		s.responseOrder = append(s.responseOrder, id)
	}
	s.responses[id] = responseBinding{AccountID: u.ID, Target: responseTarget(u)}
	// ponytail: keep 1024 connection-local IDs, older stored responses use Redis;
	// nonstored chains older than this bound must resend full context.
	if len(s.responseOrder) > 1024 {
		delete(s.responses, s.responseOrder[0])
		s.responseOrder = s.responseOrder[1:]
	}
}

func socketError(ctx context.Context, conn *websocket.Conn, err error, stream string) error {
	status := 500
	var e *apiError
	if errors.As(err, &e) {
		status = e.status
	}
	payload := map[string]any{"type": "error", "status": status, "error": map[string]any{"type": "gateway_error", "code": "gateway_error", "message": safeGatewayError(err)}}
	if stream != "" {
		payload["stream_id"] = stream
	}
	raw, _ := json.Marshal(payload)
	write, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(write, websocket.MessageText, raw)
}

func (a *App) responsesWebSocket(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		gatewayError(w, &apiError{426, "WebSocket upgrade required"})
		return
	}
	if err := a.checkInstance(r.Context()); err != nil {
		gatewayError(w, err)
		return
	}
	if r.Header.Get("Idempotency-Key") != "" {
		gatewayError(w, bad("Idempotency-Key is not supported for a multi-turn WebSocket"))
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), clientPolicyKey{}, clientPolicy{Protocol: "responses"}))
	g, err := a.gatewayAuth(r, true)
	if err != nil {
		gatewayError(w, err)
		return
	}
	if g.Group.Platform != "openai" && g.Group.Platform != "grok" && g.Group.Platform != "composite" {
		gatewayError(w, bad("Responses WebSocket requires an OpenAI, Grok or composite group"))
		return
	}
	if !a.takeSlot("websocket", g.Key.ID, 4) {
		gatewayError(w, &apiError{429, "too many WebSocket connections for this API key"})
		return
	}
	defer a.releaseSlot("websocket", g.Key.ID)
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	s := &responseSocket{keyID: g.Key.ID, groupID: g.Key.GroupID, userID: g.UserID, responses: map[string]responseBinding{}}
	a.gatewayMu.Lock()
	if a.gatewayStopped || len(a.websockets) >= 128 {
		a.gatewayMu.Unlock()
		gatewayError(w, &apiError{503, "WebSocket capacity is unavailable"})
		return
	}
	if a.websockets == nil {
		a.websockets = map[*responseSocket]context.CancelFunc{}
	}
	a.websockets[s] = cancel
	a.websocketDone.Add(1)
	a.gatewayMu.Unlock()
	defer func() { a.gatewayMu.Lock(); delete(a.websockets, s); a.gatewayMu.Unlock(); a.websocketDone.Done() }()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	s.client = conn
	defer conn.CloseNow()
	defer func() {
		if s.upstream != nil {
			s.upstream.CloseNow()
		}
	}()
	conn.SetReadLimit(4 << 20)
	// Always read control frames and notice disconnects even while a turn runs.
	messages := make(chan []byte, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		for {
			readCtx, readCancel := context.WithTimeout(ctx, 5*time.Minute)
			kind, raw, err := conn.Read(readCtx)
			readCancel()
			if err != nil {
				return
			}
			if kind != websocket.MessageText && kind != websocket.MessageBinary {
				return
			}
			select {
			case messages <- raw:
			case <-ctx.Done():
				return
			default:
				_ = socketError(ctx, conn, &apiError{429, "too many pending WebSocket turns"}, "")
				return
			}
		}
	}()
	defer func() { cancel(); conn.CloseNow(); <-readerDone }()
	for {
		var raw []byte
		select {
		case raw = <-messages:
		case <-ctx.Done():
			return
		}
		body, turn, err := parseSocketTurn(raw, s)
		if err != nil {
			if socketError(ctx, conn, err, "") != nil {
				return
			}
			continue
		}
		// ponytail: turns share one private upstream connection and run serially;
		// add lane multiplexing only with matching per-lane affinity and accounting.
		turnCtx := context.WithValue(ctx, socketTurnKey{}, turn)
		req := r.Clone(turnCtx)
		req.Method = http.MethodPost
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		writer := &socketResponseWriter{header: http.Header{}, ctx: turnCtx, conn: conn, streamID: turn.streamID}
		a.textGateway(writer, req, "responses")
		if writer.err != nil || writer.failed {
			return
		}
	}
}

func parseSocketTurn(raw []byte, s *responseSocket) ([]byte, *responseSocketTurn, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil || credentialString(body, "type") != "response.create" {
		return nil, nil, bad("expected response.create JSON object")
	}
	turn := &responseSocketTurn{socket: s}
	if value, exists := body["stream_id"]; exists {
		if json.Unmarshal(value, &turn.streamID) != nil || len(turn.streamID) == 0 || len(turn.streamID) > 256 {
			return nil, nil, bad("invalid stream_id")
		}
		for _, c := range turn.streamID {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.", c)) {
				return nil, nil, bad("invalid stream_id")
			}
		}
	}
	if value, exists := body["generate"]; exists {
		var generate bool
		if string(value) == "null" || json.Unmarshal(value, &generate) != nil {
			return nil, nil, bad("invalid generate")
		}
		turn.warmup = !generate
	}
	if body["background"] != nil {
		return nil, nil, bad("background is not supported in WebSocket mode")
	}
	delete(body, "type")
	delete(body, "stream_id")
	body["stream"] = json.RawMessage("true")
	result, err := json.Marshal(body)
	return result, turn, err
}

// Adapt native frames to the existing Responses event observer and settlement
// path. A connection stays private to one authenticated client; no shared pool.
func (a *App) socketUpstream(ctx context.Context, account *upstreamAccount, body map[string]json.RawMessage, turn *responseSocketTurn) (*http.Response, error) {
	s := turn.socket
	proxyTarget := "direct"
	if account.ProxyID != nil {
		proxy, err := a.resolveProxy(ctx, *account.ProxyID, map[int64]bool{})
		if err != nil {
			return nil, err
		}
		if proxy != nil {
			proxyTarget = digest(proxy.String())
		}
	}
	if s.upstream != nil && (s.proxyTarget != proxyTarget || !s.eligible(account)) {
		return nil, conflict("upstream configuration changed; reconnect")
	}
	if s.upstream == nil {
		base, err := account.baseURL()
		if err != nil {
			return nil, err
		}
		endpoint, err := upstreamURL(base, "/v1/responses")
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		if err != nil {
			return nil, err
		}
		tr, err := a.upstreamTransport(ctx, account, req)
		if err != nil {
			return nil, err
		}
		defer tr.CloseIdleConnections()
		headers := http.Header{"Authorization": []string{"Bearer " + credentialString(account.Credentials, "api_key")}, "User-Agent": []string{"lite-api/1"}, "OpenAI-Beta": []string{"responses_websockets=2026-02-06"}}
		client := &http.Client{Transport: socketTransport{tr, req.URL.Opaque}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		conn, resp, err := websocket.Dial(ctx, req.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: headers, Host: req.Host, CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			if resp != nil {
				resp.Body = io.NopCloser(strings.NewReader(""))
				return resp, nil
			}
			return nil, &apiError{502, "upstream WebSocket handshake failed"}
		}
		conn.SetReadLimit(2 << 20)
		s.upstream, s.proxyTarget = conn, proxyTarget
		s.binding = &responseBinding{AccountID: account.ID, Target: responseTarget(account)}
	}
	out := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["type"] = json.RawMessage(`"response.create"`)
	delete(out, "stream")
	delete(out, "stream_options")
	raw, _ := json.Marshal(out)
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := s.upstream.Write(writeCtx, websocket.MessageText, raw)
	cancel()
	if err != nil {
		return nil, &apiError{502, "upstream WebSocket write failed; request will not be retried"}
	}
	// A disconnected downstream still owes for tokens already generated. Drain
	// for at most 15 seconds to capture terminal usage, then close the upstream.
	readCtx, readCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(15*time.Second, readCancel) })
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: &socketEventBody{ctx: readCtx, conn: s.upstream, cleanup: func() { stop(); readCancel() }}}, nil
}

type socketTransport struct {
	*http.Transport
	opaque string
}

func (t socketTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.opaque != "" {
		r.URL.Opaque = t.opaque
	}
	return t.Transport.RoundTrip(r)
}

type socketEventBody struct {
	ctx      context.Context
	conn     *websocket.Conn
	buffer   []byte
	terminal bool
	cleanup  func()
}

func (b *socketEventBody) Read(p []byte) (int, error) {
	if len(b.buffer) == 0 {
		if b.terminal {
			return 0, io.EOF
		}
		_, raw, err := b.conn.Read(b.ctx)
		if err != nil {
			return 0, err
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(raw, &event) != nil || event == nil {
			return 0, errors.New("invalid WebSocket event")
		}
		typ := credentialString(event, "type")
		b.terminal = typ == "error" || typ == "response.completed" || typ == "response.incomplete" || typ == "response.failed"
		compact, _ := json.Marshal(event)
		b.buffer = append(append([]byte("data: "), compact...), '\n', '\n')
	}
	n := copy(p, b.buffer)
	b.buffer = b.buffer[n:]
	return n, nil
}
func (b *socketEventBody) Close() error {
	if b.cleanup != nil {
		b.cleanup()
	}
	if !b.terminal {
		return b.conn.CloseNow()
	}
	return nil
}

type socketResponseWriter struct {
	header   http.Header
	ctx      context.Context
	conn     *websocket.Conn
	streamID string
	status   int
	buffer   []byte
	err      error
	failed   bool
}

func (w *socketResponseWriter) Header() http.Header              { return w.header }
func (w *socketResponseWriter) WriteHeader(status int)           { w.status = status }
func (w *socketResponseWriter) Flush()                           {}
func (w *socketResponseWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *socketResponseWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *socketResponseWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return len(p), nil // Drain usage after downstream disconnect.
	}
	if w.status == 0 {
		w.status = 200
	}
	w.buffer = append(w.buffer, p...)
	if len(w.buffer) > 2<<20 {
		w.err = errors.New("WebSocket event exceeds limit")
		return 0, w.err
	}
	if !strings.HasPrefix(w.header.Get("Content-Type"), "text/event-stream") {
		if !json.Valid(w.buffer) {
			return len(p), nil
		}
		var event map[string]json.RawMessage
		_ = json.Unmarshal(w.buffer, &event)
		event["type"] = json.RawMessage(`"error"`)
		event["status"], _ = json.Marshal(w.status)
		w.buffer = nil
		w.failed = true
		w.err = w.send(event)
	} else {
		for {
			end := bytes.Index(w.buffer, []byte("\n\n"))
			if end < 0 {
				break
			}
			frame := w.buffer[:end]
			w.buffer = w.buffer[end+2:]
			var data []string
			for _, line := range strings.Split(string(frame), "\n") {
				if strings.HasPrefix(line, "data:") {
					data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
				}
			}
			if len(data) == 0 {
				continue
			}
			var event map[string]json.RawMessage
			if json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil || event == nil {
				w.err = errors.New("invalid gateway event")
				break
			}
			w.err = w.send(event)
			if w.err != nil {
				break
			}
		}
	}
	if w.err != nil {
		return len(p), nil
	}
	return len(p), nil
}
func (w *socketResponseWriter) send(event map[string]json.RawMessage) error {
	w.failed = w.failed || credentialString(event, "type") == "error"
	if w.streamID != "" {
		event["stream_id"], _ = json.Marshal(w.streamID)
	}
	raw, _ := json.Marshal(event)
	ctx, cancel := context.WithTimeout(w.ctx, 10*time.Second)
	defer cancel()
	return w.conn.Write(ctx, websocket.MessageText, raw)
}
