package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessagesDispatch(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" gpt-5.4-high ", "gpt-5.4"}, {"openai/GPT-5.3-codex-spark-extraHigh", "gpt-5.3-codex-spark"},
		{"gpt-5.6-high", "gpt-5.6-sol"}, {"gpt-5.6-luna-low", "gpt-5.6-luna"},
		{"gpt-5.6-new-high", "gpt-5.6-new-high"}, {"gpt-5.6-max", "gpt-5.6-max"},
		{"custom-high", "custom-high"}, {"gpt-4o-high", "gpt-4o-high"}, {"openai/gpt-5.5", "openai/gpt-5.5"},
	} {
		if got := normalizeDispatchModel(tc.input); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	c := messagesDispatchConfig{Opus: " gpt-5.4-high ", Exact: map[string]string{" claude-sonnet-latest ": " gpt-5.4-mini-medium ", "": "discard", "empty": " "}}
	if err := c.normalize(); err != nil || c.Opus != "gpt-5.4" || len(c.Exact) != 1 {
		t.Fatal(c, err)
	}
	for _, tc := range []struct{ model, want string }{
		{"claude-sonnet-latest", "gpt-5.4-mini"}, {"claude-sonnet-other", "gpt-5.3-codex"},
		{"CLAUDE-OPUS-4-6", "gpt-5.4"}, {"claude-haiku-latest", "gpt-5.4-mini"},
		{"alias-opus", ""}, {"gpt-custom", ""}, {"claude-fable", ""},
	} {
		if got := c.resolve(tc.model); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	for _, config := range []messagesDispatchConfig{
		{Opus: "*"}, {Haiku: strings.Repeat("a", 101)}, {Exact: map[string]string{"alias": "a", " alias ": "b"}},
	} {
		if err := config.normalize(); err == nil {
			t.Fatal("accepted invalid dispatch config", config)
		}
	}
	for _, target := range []string{"openai", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		g := &gatewayIdentity{Group: gatewayGroup{Platform: target}}
		for _, protocol := range []string{"anthropic", "responses", "chat_completions"} {
			_, err := g.messagesDispatch(textRequest{Protocol: protocol, Model: "claude-opus"})
			if (err != nil) != (target == "openai" && protocol == "anthropic") {
				t.Fatal("dispatch gate", target, protocol, err)
			}
		}
	}
}

func testMessagesDispatch(t *testing.T, a *App, admin string) {
	t.Helper()
	defer pauseTestWorkers(a)()
	call := func(method, path, token string, body any, idem string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewReader(mustJSON(body)))
		r.RemoteAddr = "192.0.2.209:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(m map[string]any) int64 { return int64(m["id"].(float64)) }
	must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "dispatch@example.test", "password": "dispatch-password", "balance": 100})
	user := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "dispatch@example.test", "password": "dispatch-password"})["access_token"].(string)
	var calls atomic.Int64
	var seen atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		model := credentialString(body, "model")
		seen.Store(model)
		if r.Header.Get("X-Api-Key") != "dispatch-secret" || r.Header.Get("Authorization") != "" {
			t.Error("native dispatch credentials")
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			fmt.Fprint(w, `{"input_tokens":3}`)
			return
		}
		message := fmt.Sprintf(`{"type":"message","id":"msg_dispatch","model":%q,"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`, model)
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":"+message+"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		} else {
			fmt.Fprint(w, message)
		}
	}))
	defer up.Close()
	group := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Dispatch", "platform": "openai", "default_mapped_model": "legacy-unused", "messages_dispatch_model_config": map[string]any{"opus_mapped_model": " gpt-5.4-high ", "exact_model_mappings": map[string]string{" claude-fable ": "dispatch-target"}}})
	gid := id(group)
	gp := fmt.Sprintf("/api/v1/admin/groups/%d", gid)
	account := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Dispatch", "platform": "openai", "type": "apikey", "concurrency": 1, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "dispatch-secret", "base_url": up.URL, "api_protocol": "anthropic"}})
	aid := id(account)
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	prices := []modelPrice{{Platform: "openai", Models: []string{"*"}, Input: number("0.001"), Output: number("0.002"), CacheRead: number("0"), CacheWrite: number("0"), CacheWrite1h: number("0")}}
	channel := must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Dispatch", "group_ids": []int64{gid}, "billing_model_source": "requested", "model_pricing": prices})
	cp := fmt.Sprintf("/api/v1/admin/channels/%d", id(channel))
	key := must("POST", "/api/v1/keys", user, map[string]any{"name": "Dispatch", "group_id": gid})["key"].(string)
	body := map[string]any{"model": "claude-fable", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/messages/count_tokens"} {
		w := call("POST", path, key, body, "")
		if w.Code != 403 || calls.Load() != 0 {
			t.Fatal("disabled dispatch called upstream", path, w.Code, w.Body.String())
		}
	}
	must("PUT", gp, admin, map[string]any{"allow_messages_dispatch": true})
	for _, patch := range []any{map[string]any{"description": "preserve"}, map[string]any{"allow_messages_dispatch": nil, "default_mapped_model": nil, "messages_dispatch_model_config": nil}} {
		got := must("PUT", gp, admin, patch)
		if got["allow_messages_dispatch"] != true || got["default_mapped_model"] != "legacy-unused" || got["messages_dispatch_model_config"].(map[string]any)["opus_mapped_model"] != "gpt-5.4" {
			t.Fatal("omission/null changed policy", got)
		}
	}
	check := func(model, target string, stream bool) *httptest.ResponseRecorder {
		t.Helper()
		body["model"], body["stream"] = model, stream
		w := call("POST", "/v1/messages", key, body, "")
		if w.Code != 200 || seen.Load() != target || strings.Contains(w.Body.String(), `"type":"error"`) {
			t.Fatal("dispatch", model, target, seen.Load(), w.Code, w.Body.String())
		}
		var requested, upstream, actual string
		if err := a.DB.QueryRow("SELECT requested_model,upstream_model,actual_cost::text FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&requested, &upstream, &actual); err != nil || requested != model || upstream != target || actual != "0.0140000000" {
			t.Fatal("dispatch billing", requested, upstream, actual, err)
		}
		return w
	}
	for _, tc := range []struct{ model, want string }{{"claude-fable", "dispatch-target"}, {"claude-opus-latest", "gpt-5.4"}, {"claude-sonnet-latest", "gpt-5.3-codex"}, {"claude-haiku-latest", "gpt-5.4-mini"}, {"ordinary-model", "ordinary-model"}} {
		for _, stream := range []bool{false, true} {
			check(tc.model, tc.want, stream)
		}
	}
	body["model"], body["stream"] = "claude-fable", false
	w := call("POST", "/messages/count_tokens", key, body, "")
	var count int
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", w.Header().Get("X-Request-ID")).Scan(&count); err != nil || count != 0 || w.Code != 200 || seen.Load() != "dispatch-target" {
		t.Fatal("dispatch token count", w.Code, w.Body.String(), count, err)
	}
	// Dispatch models drive account admission; their mapping values are not a
	// second forwarding rewrite. Account mappings of the channel model win.
	mapping := map[string]string{"dispatch-target": "not-forwarded", "channel-name": "account-wins"}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"model_mapping": mapping}})
	check("claude-fable", "dispatch-target", false)
	must("PUT", cp, admin, map[string]any{"model_mapping": map[string]any{"openai": map[string]string{"claude-fable": "channel-name"}}})
	check("claude-fable", "account-wins", true)
	mapping["channel-name"] = ""
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"model_mapping": mapping}})
	check("claude-fable", "channel-name", false)
	delete(mapping, "dispatch-target")
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"model_mapping": mapping}})
	before := calls.Load()
	if w := call("POST", "/v1/messages", key, body, ""); w.Code != 503 || calls.Load() != before {
		t.Fatal("account admission bypassed", w.Code, w.Body.String())
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"model_mapping": map[string]string{}}})
	must("PUT", cp, admin, map[string]any{"model_mapping": map[string]any{}})
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": true, "models": []string{"dispatch-target"}}})
	if w := call("POST", "/v1/messages", key, body, ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("mapping bypassed client allowlist", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"model_allowlist": map[string]any{"enabled": false}})
	// Model-priority routing uses the dispatch target, while billing still uses
	// the public request name. An explicit channel/account mapping wins on wire.
	preferred := must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Dispatch preferred", "platform": "openai", "type": "apikey", "priority": 100, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "dispatch-secret", "base_url": up.URL, "api_protocol": "anthropic"}})
	preferredID := id(preferred)
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": true, "model_routing": map[string]any{"dispatch-target": []int64{preferredID}}})
	routed := check("claude-fable", "dispatch-target", false)
	var selected int64
	if err := a.DB.QueryRow("SELECT account_id FROM usage_logs WHERE request_id=$1", routed.Header().Get("X-Request-ID")).Scan(&selected); err != nil || selected != preferredID {
		t.Fatal("dispatch target did not select preferred pool", selected, err)
	}
	must("PUT", fmt.Sprintf("/api/v1/admin/accounts/%d", preferredID), admin, map[string]any{"status": "inactive"})
	must("PUT", gp, admin, map[string]any{"model_routing_enabled": false})
	body["stream"] = false
	first := call("POST", "/v1/messages", key, body, "dispatch-replay")
	must("PUT", gp, admin, map[string]any{"allow_messages_dispatch": false})
	before = calls.Load()
	replay := call("POST", "/v1/messages", key, body, "dispatch-replay")
	if first.Code != 200 || replay.Code != 200 || first.Body.String() != replay.Body.String() || calls.Load() != before || replay.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("completed request was resent after policy change", replay.Code)
	}
	if w := call("POST", "/v1/messages", key, body, ""); w.Code != 403 || calls.Load() != before {
		t.Fatal("new request ignored disabled gate", w.Code)
	}
	must("PUT", gp, admin, map[string]any{"allow_messages_dispatch": true})
	// Reject stale policy while waiting for an account, before any upstream I/O.
	if !a.takeSlot("account", aid, 1) {
		t.Fatal("cannot occupy account")
	}
	defer a.releaseSlot("account", aid)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("POST", "/v1/messages", key, body, "") }()
	waitForQueue(t, a, "account", aid, 1)
	must("PUT", gp, admin, map[string]any{"allow_messages_dispatch": false})
	select {
	case w := <-done:
		if w.Code != 409 || calls.Load() != before {
			t.Fatal("queued dispatch ignored policy edit", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued policy did not refresh")
	}
	// Invalid settings roll back all fields in the same administrator write.
	if w := call("PUT", gp, admin, map[string]any{"allow_messages_dispatch": true, "messages_dispatch_model_config": map[string]any{"opus_mapped_model": "*"}}, ""); w.Code != 400 {
		t.Fatal("invalid config accepted", w.Code)
	}
	got := must("GET", gp, admin, nil)
	if got["allow_messages_dispatch"] != false {
		t.Fatal("invalid config partially committed")
	}
	for _, platform := range []string{"composite", "anthropic", "gemini", "grok", "kimi", "zhipu", "deepseek", "minimax"} {
		got := must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Dispatch " + platform, "platform": platform, "allow_messages_dispatch": true, "default_mapped_model": "unused", "messages_dispatch_model_config": map[string]string{"opus_mapped_model": "unused"}})
		if got["allow_messages_dispatch"] != (platform == "composite") || got["default_mapped_model"] != "" || len(got["messages_dispatch_model_config"].(map[string]any)) != 0 {
			t.Fatal("platform policy normalization", got)
		}
		if platform == "composite" {
			cgid := id(got)
			cgp := fmt.Sprintf("/api/v1/admin/groups/%d", cgid)
			ckey := must("POST", "/api/v1/keys", user, map[string]any{"name": "Composite dispatch", "group_id": cgid})["key"].(string)
			must("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Composite dispatch", "group_ids": []int64{cgid}, "model_pricing": prices})
			must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Composite dispatch", "platform": "openai", "type": "apikey", "group_ids": []int64{cgid}, "credentials": map[string]any{"api_key": "dispatch-secret", "base_url": up.URL, "api_protocol": "anthropic"}})
			must("POST", cgp+"/composite-routes", admin, map[string]any{"public_model": "claude-opus", "upstream_model": "route-model", "target_platform": "openai"})
			body["model"] = "claude-opus"
			for _, enabled := range []bool{false, true} {
				must("PUT", cgp, admin, map[string]any{"allow_messages_dispatch": enabled})
				for _, endpoint := range []string{"/v1/messages", "/messages/count_tokens"} {
					before := calls.Load()
					w := call("POST", endpoint, ckey, body, "")
					if !enabled && (w.Code != 403 || calls.Load() != before) || enabled && (w.Code != 200 || seen.Load() != "gpt-5.4") {
						t.Fatal("composite OpenAI gate/default", enabled, w.Code, w.Body.String(), seen.Load())
					}
				}
			}
			must("PUT", cgp, admin, map[string]any{"allow_messages_dispatch": false})
			must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Composite native", "platform": "anthropic", "type": "apikey", "group_ids": []int64{cgid}, "credentials": map[string]any{"api_key": "dispatch-secret", "base_url": up.URL}})
			must("POST", cgp+"/composite-routes", admin, map[string]any{"public_model": "claude-haiku", "upstream_model": "native-haiku", "target_platform": "anthropic"})
			body["model"] = "claude-haiku"
			if w := call("POST", "/messages/count_tokens", ckey, body, ""); w.Code != 200 || seen.Load() != "native-haiku" {
				t.Fatal("composite native target incorrectly gated or remapped", w.Code, w.Body.String(), seen.Load())
			}
		}
	}
	must("PUT", gp, admin, map[string]any{"messages_dispatch_model_config": map[string]any{}, "default_mapped_model": ""})
	got = must("GET", gp, admin, nil)
	if len(got["messages_dispatch_model_config"].(map[string]any)) != 0 || got["default_mapped_model"] != "" {
		t.Fatal("explicit clear failed")
	}
}
