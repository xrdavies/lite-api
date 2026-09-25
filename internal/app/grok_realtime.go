package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const realtimeBilling = "gateway:realtime-billing"

// Native JSON stays intact after validation. Binary audio is supported by the
// provider; it is never decoded as JSON or mistaken for a transcript.
func realtimeEvent(kind websocket.MessageType, raw []byte, model string, client bool) ([]byte, bool, error) {
	if kind == websocket.MessageBinary {
		if len(raw) == 0 {
			return nil, false, bad("empty realtime audio")
		}
		return raw, true, nil
	}
	var event map[string]json.RawMessage
	if kind != websocket.MessageText || json.Unmarshal(raw, &event) != nil || event == nil {
		return nil, false, bad("realtime requires JSON events or binary audio")
	}
	typ := credentialString(event, "type")
	if typ == "" {
		return nil, false, bad("realtime event type is required")
	}
	if !client && typ == "error" {
		return nil, false, &apiError{502, "upstream realtime returned an error"}
	}
	if client {
		for _, name := range []string{"", "session", "response"} {
			scope := event[name]
			if name == "" {
				scope, _ = json.Marshal(event)
			}
			if len(scope) == 0 {
				continue
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(scope, &fields) != nil || fields == nil {
				return nil, false, bad("invalid realtime session or response")
			}
			for key, value := range fields {
				lower := strings.ToLower(key)
				if (lower == "type" || lower == "model" || lower == "session" || lower == "response" || lower == "resumption" || lower == "conversation_id") && key != lower {
					return nil, false, bad("invalid realtime field casing")
				}
				if key == "model" && credentialString(fields, key) != model {
					return nil, false, bad("reconnect to change realtime model")
				}
				if key == "conversation_id" {
					return nil, false, bad("external realtime conversation resumption is unsupported")
				}
				if key == "resumption" && string(value) != "null" {
					var resume map[string]json.RawMessage
					if json.Unmarshal(value, &resume) != nil || len(resume) != 1 || string(resume["enabled"]) != "false" {
						return nil, false, bad("realtime resumption is unsupported")
					}
					fields[key], _ = json.Marshal(resume)
				}
			}
			if name != "" {
				event[name], _ = json.Marshal(fields)
			} else {
				event = fields
			}
		}
	}
	audio := false
	if strings.Contains(typ, "audio") && !strings.Contains(typ, "transcript") {
		for _, field := range []string{"audio", "delta", "data"} {
			value := credentialString(event, field)
			if value == "" {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil || len(decoded) == 0 {
				return nil, false, bad("invalid realtime audio encoding")
			}
			audio = true
		}
	}
	// Canonicalize repeated JSON keys before either side interprets them.
	normalized, _ := json.Marshal(event)
	return normalized, audio, nil
}

func (a *App) grokRealtime(w http.ResponseWriter, r *http.Request) {
	id, started := randomToken(24), time.Now()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Request-ID", id)
	var g *gatewayIdentity
	var selected *gatewaySelection
	in := textRequest{Protocol: "realtime", Stream: true}
	fail := func(err error) { a.recordGatewayError(id, g, selected, r, in, err, started); gatewayError(w, err) }
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		fail(&apiError{426, "WebSocket upgrade required"})
		return
	}
	if err := a.checkInstance(r.Context()); err != nil {
		fail(err)
		return
	}
	if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("Sec-WebSocket-Protocol") != "" {
		fail(bad("realtime uses API key headers without idempotency or token subprotocols"))
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(bad("invalid realtime query"))
		return
	}
	for key, values := range query {
		if key != "model" || len(values) != 1 {
			fail(bad("only one realtime model query is supported"))
			return
		}
	}
	model := query.Get("model")
	if model == "" {
		model = "grok-voice-latest"
	}
	if !validNativeModel(model) {
		fail(bad("invalid realtime model"))
		return
	}
	in.Model = model
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	r = r.WithContext(context.WithValue(ctx, clientPolicyKey{}, clientPolicy{Protocol: "realtime"}))
	g, err = a.gatewayAuth(r, true)
	if err != nil {
		fail(err)
		return
	}
	if g.Group.Platform != "grok" {
		fail(&apiError{404, "realtime requires a Grok group"})
		return
	}
	if !g.Group.allows(model) {
		fail(denied())
		return
	}
	if !a.takeSlot("websocket", g.Key.ID, 4) {
		fail(&apiError{429, "too many WebSocket connections for this API key"})
		return
	}
	defer a.releaseSlot("websocket", g.Key.ID)
	socket := &responseSocket{realtimeID: id}
	a.gatewayMu.Lock()
	if a.gatewayStopped || len(a.websockets) >= 128 {
		a.gatewayMu.Unlock()
		fail(&apiError{503, "WebSocket capacity is unavailable"})
		return
	}
	if a.websockets == nil {
		a.websockets = map[*responseSocket]context.CancelFunc{}
	}
	a.websockets[socket] = cancel
	a.websocketDone.Add(1)
	a.gatewayMu.Unlock()
	defer func() { a.gatewayMu.Lock(); delete(a.websockets, socket); a.gatewayMu.Unlock(); a.websocketDone.Done() }()
	g, err = a.acquireGatewayUser(r, g, model, nil)
	if err != nil {
		fail(err)
		return
	}
	defer a.releaseSlot("user", g.UserID)
	defer a.trackKeySlot(g.Key.ID)()
	snapshot := g.Group
	binding, err := a.voiceLibraryBinding(ctx, g)
	if err != nil {
		fail(err)
		return
	}
	excluded := map[int64]bool{}
	var upstream *websocket.Conn
	var lastRejection *passthroughError
	var lastRejectedAccount *gatewaySelection
	for attempt := 0; attempt < 4; attempt++ {
		selected, err = a.chooseAccount(ctx, g, model, in, excluded, binding, nil, a.prices.Load())
		var busy *accountBusy
		if errors.As(err, &busy) {
			_, err = a.waitAdmission(ctx, "account", busy.ID, gatewayQueueTimeout, func(waitCtx context.Context) (bool, error) {
				if err := a.revalidateQueuedRequest(r.WithContext(waitCtx), g, snapshot, in, model); err != nil {
					return false, err
				}
				var e error
				selected, e = a.chooseAccount(waitCtx, g, model, in, excluded, binding, nil, a.prices.Load())
				if selected != nil && waitCtx.Err() != nil {
					selected.Release()
					selected = nil
					return false, waitCtx.Err()
				}
				if errors.As(e, &busy) {
					return false, nil
				}
				return selected != nil, e
			}, nil)
		}
		if err != nil {
			if errors.Is(err, errNoUpstream) && lastRejection != nil {
				selected = lastRejectedAccount
				err = lastRejection
			}
			fail(err)
			return
		}
		billingModel := selected.ChannelModel
		if selected.BillingSource == "requested" || selected.BillingSource == "upstream" {
			billingModel = model
		}
		if selected.Restrict {
			if _, ok := matchPrice(selected.Pricing, "grok", billingModel); !ok {
				selected.Release()
				fail(denied())
				return
			}
		}
		if _, err = selected.audioCost(g.Group, billingModel, "1", started); err != nil {
			selected.Release()
			fail(err)
			return
		}
		if attempt == 0 {
			if err = a.gatewayRPM(ctx, g); err != nil {
				selected.Release()
				fail(err)
				return
			}
		}
		dialCtx, dialCancel := context.WithTimeout(ctx, 12*time.Second)
		var resp *http.Response
		upstream, resp, err = a.dialUpstreamSocket(dialCtx, selected.Account, "/v1/realtime?model="+url.QueryEscape(model), http.Header{"Authorization": []string{"Bearer " + credentialString(selected.Account.Credentials, "api_key")}, "User-Agent": []string{"lite-api/1"}})
		dialCancel()
		if err == nil {
			break
		}
		status, retry := 502, ""
		if resp != nil {
			status = resp.StatusCode
			retry = resp.Header.Get("Retry-After")
		}
		var failure error = &apiError{503, "realtime upstream attempts exhausted"}
		if resp != nil {
			failure = a.upstreamError(ctx, selected.Account, status, readUpstreamError(resp), failure)
			if resp.Body != nil {
				resp.Body.Close()
			}
		}
		lastRejection = nil
		_ = errors.As(failure, &lastRejection)
		lastRejectedAccount = selected
		if !skipErrorMonitoring(failure) {
			a.recordUpstreamFailure(id, g, selected, r, in, "/v1/realtime", status, started)
		}
		a.markGatewayFailure(ctx, selected, status, retry, nil)
		selected.Release()
		excluded[selected.Account.ID] = true
	}
	if upstream == nil {
		if lastRejection != nil {
			fail(lastRejection)
			return
		}
		fail(&apiError{503, "realtime upstream attempts exhausted"})
		return
	}
	defer selected.Release()
	defer upstream.CloseNow()
	upstream.SetReadLimit(4 << 20)
	// Revocations during a slow upstream handshake must still reject the upgrade.
	if err = a.realtimeAuthorized(r, g, selected, model); err != nil {
		fail(err)
		return
	}
	client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer client.CloseNow()
	client.SetReadLimit(4 << 20)
	connected := time.Now()
	if err = a.relayRealtime(ctx, cancel, client, upstream, r, id, model, connected, g, selected); err != nil {
		a.recordGatewayError(id, g, selected, r, in, err, started)
	}
}

func (a *App) realtimeAuthorized(r *http.Request, g *gatewayIdentity, s *gatewaySelection, model string) error {
	if err := a.checkInstance(r.Context()); err != nil {
		return err
	}
	fresh, err := a.gatewayAuth(r, true)
	if err != nil {
		return err
	}
	if fresh.Key.ID != g.Key.ID || fresh.Key.GroupID != g.Key.GroupID || fresh.UserID != g.UserID || fresh.Group.Platform != "grok" || !fresh.Group.allows(model) {
		return denied()
	}
	u, err := a.loadAccount(r.Context(), s.Account.ID)
	if err != nil || u.Status != "active" || !u.Schedulable || responseTarget(u) != responseTarget(s.Account) || !reflect.DeepEqual(u.ProxyID, s.Account.ProxyID) {
		return &apiError{403, "realtime upstream account changed; reconnect"}
	}
	if !accountQuotaAvailable(u.Extra, time.Now()) {
		return &apiError{429, "upstream account quota exhausted"}
	}
	var active bool
	err = a.DB.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE a.id=$1 AND ag.group_id=$2 AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=now()) AND (a.overload_until IS NULL OR a.overload_until<=now()) AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=now()))`, u.ID, g.Key.GroupID).Scan(&active)
	if err != nil {
		return err
	}
	if !active {
		return denied()
	}
	return nil
}

type realtimeFrame struct {
	kind   websocket.MessageType
	raw    []byte
	client bool
	err    error
}

func (a *App) relayRealtime(ctx context.Context, cancel context.CancelFunc, client, upstream *websocket.Conn, r *http.Request, id, model string, started time.Time, g *gatewayIdentity, selected *gatewaySelection) (result error) {
	frames := make(chan realtimeFrame, 2)
	readDone := make(chan struct{}, 2)
	read := func(conn *websocket.Conn, fromClient bool) {
		defer func() { readDone <- struct{}{} }()
		for {
			kind, raw, err := conn.Read(ctx)
			select {
			case frames <- realtimeFrame{kind, raw, fromClient, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}
	go read(client, true)
	go read(upstream, false)
	defer func() {
		// Report policy/settlement errors before cancelling readers, which closes
		// their connections and would otherwise discard the error event.
		if result != nil {
			writeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			_ = socketError(writeCtx, client, result, "")
			stop()
		} else {
			_ = client.Close(websocket.StatusNormalClosure, "")
		}
		cancel()
		client.CloseNow()
		upstream.CloseNow()
		<-readDone
		<-readDone
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	audioSeen := false
	lastActivity, lastCheckpoint := started, started
	checkpoint := func(at time.Time) error {
		if !audioSeen {
			return nil
		}
		u := priceUsage{AudioUnits: big.NewRat(max(at.Sub(started).Nanoseconds(), 1), int64(time.Minute)).RatString()}
		receipt, err := a.makeReceipt(id, g, selected, model, "", "", "", u, true, at.Sub(started), 0, started, digest("realtime\n"+model), clientIP(r), r.UserAgent(), r.URL.Path, "")
		if err != nil {
			return err
		}
		receipt.WebSocket = true
		receipt.Upstream = "/v1/realtime"
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		persistCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		if err = a.Redis.HSet(persistCtx, realtimeBilling, id, raw).Err(); err != nil {
			return &apiError{503, "realtime billing checkpoint unavailable"}
		}
		lastCheckpoint = at
		return nil
	}
loop:
	for {
		select {
		case <-ctx.Done():
			result = &apiError{503, "realtime session stopped"}
			break loop
		case now := <-tick.C:
			if now.Sub(lastActivity) > 5*time.Minute {
				result = &apiError{408, "realtime idle timeout"}
				break loop
			}
			if err := a.realtimeAuthorized(r, g, selected, model); err != nil {
				result = err
				break loop
			}
			if err := checkpoint(now); err != nil {
				result = err
				break loop
			}
		case frame := <-frames:
			if frame.err != nil {
				status := websocket.CloseStatus(frame.err)
				if status != websocket.StatusNormalClosure && status != websocket.StatusGoingAway {
					result = &apiError{502, "realtime connection interrupted"}
				}
				break loop
			}
			raw, audio, err := realtimeEvent(frame.kind, frame.raw, model, frame.client)
			if err != nil {
				result = err
				break loop
			}
			if frame.kind == websocket.MessageText {
				raw, err = a.realtimeVoices(ctx, g, selected.Account, raw, frame.client)
				if err != nil {
					result = err
					break loop
				}
			}
			if frame.client && (!audio || !audioSeen) {
				if err = a.realtimeAuthorized(r, g, selected, model); err != nil {
					result = err
					break loop
				}
			}
			now := time.Now()
			lastActivity = now
			if audio && !audioSeen {
				audioSeen = true
				if err = checkpoint(now); err != nil {
					result = err
					break loop
				}
			}
			if audioSeen && now.Sub(lastCheckpoint) >= time.Second {
				if err = checkpoint(now); err != nil {
					result = err
					break loop
				}
			}
			target := client
			if frame.client {
				target = upstream
			}
			writeCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			err = target.Write(writeCtx, frame.kind, raw)
			stop()
			if err != nil {
				result = &apiError{502, "realtime connection interrupted"}
				break loop
			}
		}
	}
	if audioSeen {
		// Freeze the final receipt before settlement; retries always apply this
		// exact snapshot, never a new duration with the same deduplication ID.
		if err := checkpoint(time.Now()); err != nil {
			return err
		}
		settleCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := a.settleRealtime(settleCtx, id); err != nil {
			return &apiError{503, "realtime settlement pending recovery"}
		}
	}
	return result
}

func (a *App) settleRealtime(ctx context.Context, id string) error {
	raw, err := a.Redis.HGet(ctx, realtimeBilling, id).Bytes()
	if err != nil {
		return err
	}
	var receipt usageReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.RequestID != id || receipt.Fingerprint != receipt.fingerprint() {
		return errors.New("invalid realtime billing checkpoint")
	}
	if err = a.saveReceipt(ctx, &receipt); err != nil {
		return err
	}
	return a.Redis.HDel(ctx, realtimeBilling, id).Err()
}

func (a *App) recoverRealtime(ctx context.Context) error {
	var cursor uint64
	for {
		ids, next, err := a.Redis.HScan(ctx, realtimeBilling, cursor, "*", 100).Result()
		if err != nil {
			return err
		}
		for i := 0; i+1 < len(ids); i += 2 {
			id := ids[i]
			live := false
			a.gatewayMu.Lock()
			for socket := range a.websockets {
				if socket.realtimeID == id {
					live = true
					break
				}
			}
			a.gatewayMu.Unlock()
			if !live {
				if err = a.settleRealtime(ctx, id); err != nil {
					return fmt.Errorf("recover realtime billing: %w", err)
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}
