package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

const socketBridgeHistoryLimit = 8 << 20

// Bridge history is private to the authenticated socket, including store=false.
// ponytail: retain at most 32 snapshots / 8 MiB per connection; evicted chains
// require full input. Shared immutable items avoid copying every prior turn.
func (s *responseSocket) rememberBridge(id string, items []json.RawMessage) {
	if s.bridgeHistory == nil {
		s.bridgeHistory = map[string][]json.RawMessage{}
	}
	if _, exists := s.bridgeHistory[id]; exists {
		return
	}
	size := 0
	for _, item := range items {
		size += len(item)
	}
	if size > 4<<20 {
		return // Deliver and bill the result; refuse an oversized continuation.
	}
	for len(s.bridgeOrder) >= 32 || s.bridgeBytes+size > socketBridgeHistoryLimit {
		old := s.bridgeOrder[0]
		for _, item := range s.bridgeHistory[old] {
			s.bridgeBytes -= len(item)
		}
		delete(s.bridgeHistory, old)
		s.bridgeOrder = s.bridgeOrder[1:]
	}
	s.bridgeHistory[id] = items
	s.bridgeOrder = append(s.bridgeOrder, id)
	s.bridgeBytes += size
}

func (t *responseSocketTurn) bridgeBody(body map[string]json.RawMessage) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		out[k] = v
	}
	var items []json.RawMessage
	if previous := credentialString(body, "previous_response_id"); previous != "" {
		history, ok := t.socket.bridgeHistory[previous]
		if !ok {
			return nil, bad("HTTP bridge continuation is not cached on this connection; resend full input without previous_response_id")
		}
		items = append(items, history...)
	}
	var input []json.RawMessage
	if raw := body["input"]; raw != nil && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			item, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": text})
			input = []json.RawMessage{item}
		} else if json.Unmarshal(raw, &input) != nil {
			return nil, bad("invalid HTTP bridge input")
		}
	}
	for _, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			return nil, bad("invalid HTTP bridge input item")
		}
		kind, id := credentialString(item, "type"), credentialString(item, "id")
		if kind == "item_reference" || kind == "" && id != "" {
			// Ownership/deletion was checked by the common gateway. Only expand
			// content actually observed on this socket, never fetch arbitrary IDs.
			var found json.RawMessage
			for _, history := range t.socket.bridgeHistory {
				for _, candidate := range history {
					var value struct{ ID string }
					if json.Unmarshal(candidate, &value) == nil && value.ID == id {
						found = candidate
					}
				}
			}
			if found == nil {
				return nil, bad("HTTP bridge item is not cached on this connection; resend full input")
			}
			raw = found
		}
		items = append(items, raw)
	}
	if len(items) > 1024 {
		return nil, bad("HTTP bridge history exceeds 1024 items; resend compacted input")
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	out["input"], _ = json.Marshal(items)
	delete(out, "type")
	delete(out, "generate")
	delete(out, "stream_id")
	delete(out, "previous_response_id")
	delete(out, "stream_options")
	out["stream"] = json.RawMessage("true")
	// Obtain opaque reasoning state so later turns can replay store=false output.
	var include []string
	if raw := out["include"]; raw != nil && string(raw) != "null" && json.Unmarshal(raw, &include) != nil {
		return nil, bad("invalid HTTP bridge include")
	}
	found := false
	for _, value := range include {
		found = found || value == "reasoning.encrypted_content"
	}
	if !found {
		include = append(include, "reasoning.encrypted_content")
	}
	out["include"], _ = json.Marshal(include)
	raw, err := json.Marshal(out)
	if len(raw) > 4<<20 {
		return nil, bad("HTTP bridge request exceeds 4 MiB; resend compacted input")
	}
	t.bridgeInput = items
	return raw, err
}

// Observe the sanitized terminal snapshot, but publish history only after the
// shared gateway has validated usage and durably settled the turn.
func (t *responseSocketTurn) observeBridge(raw []byte) {
	if t.socket.mode != "http_bridge" {
		return
	}
	var event struct {
		Type     string
		Response struct{ Output []json.RawMessage }
	}
	if json.Unmarshal(raw, &event) == nil && (event.Type == "response.completed" || event.Type == "response.incomplete") {
		t.bridgeOutput = event.Response.Output
	}
}

func (a *App) socketHTTPUpstream(ctx context.Context, account *upstreamAccount, body map[string]json.RawMessage, headers http.Header, turn *responseSocketTurn) (*http.Response, error) {
	raw, err := turn.bridgeBody(body)
	if err != nil {
		return nil, err
	}
	turn.socket.binding = &responseBinding{AccountID: account.ID, Target: responseTarget(account)}
	if turn.warmup {
		// Prepare local input only: HTTP has no generate=false operation. Do not
		// submit a paid generation or claim that the provider was prewarmed.
		response := map[string]any{"id": "resp_bridge_" + randomToken(18), "object": "response", "model": credentialString(body, "model"), "status": "completed", "output": []any{}}
		var stream bytes.Buffer
		for i, kind := range []string{"response.created", "response.completed"} {
			event, _ := json.Marshal(map[string]any{"type": kind, "sequence_number": i, "response": response})
			stream.WriteString("data: ")
			stream.Write(event)
			stream.WriteString("\n\n")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&stream)}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	upstreamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(15*time.Second, cancel) })
	cleanup := func() { stop(); cancel() }
	resp, err := a.upstreamRequestHeaders(upstreamCtx, account, "POST", "/v1/responses", raw, headers)
	if err != nil {
		cleanup()
		return nil, err
	}
	resp.Body = &socketHTTPBody{ReadCloser: resp.Body, cleanup: cleanup}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
			resp.Body.Close()
			return nil, &apiError{502, "HTTP bridge upstream did not return an event stream"}
		}
		resp.Body = &idleStreamBody{ReadCloser: resp.Body, ctx: upstreamCtx, cancel: cancel, idle: a.streamIdle}
	}
	return resp, nil
}

type socketHTTPBody struct {
	io.ReadCloser
	cleanup func()
}

func (b *socketHTTPBody) Close() error {
	defer b.cleanup()
	return b.ReadCloser.Close()
}
