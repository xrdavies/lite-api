package app

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamRequestID(t *testing.T) {
	for _, name := range []string{"X-Request-ID", "trace_1", "!#$%&'*+-.^_`|~", strings.Repeat("x", 64)} {
		if !validRequestIDHeader(name) {
			t.Fatal("valid header rejected", name)
		}
	}
	for _, name := range []string{"", "X Request-ID", "X-ID:", "x\r\ny", "请求", strings.Repeat("x", 65)} {
		if validRequestIDHeader(name) {
			t.Fatal("invalid header accepted", name)
		}
	}
	account := &upstreamAccount{Extra: map[string]json.RawMessage{upstreamRequestIDHeaderKey: json.RawMessage(`" x-custom-trace "`)}}
	headers := http.Header{"X-Request-Id": []string{"do-not-guess"}, "X-Custom-Trace": []string{"  custom  "}}
	if upstreamRequestID(nil, headers) != "" || upstreamRequestID(&upstreamAccount{}, headers) != "" || upstreamRequestID(account, nil) != "" || upstreamRequestID(account, headers) != "custom" {
		t.Fatal("request ID source selection")
	}
	for _, tc := range []struct{ raw, want string }{
		{" \tvalue\t ", "value"}, {strings.Repeat("x", 129), strings.Repeat("x", 128)},
		{strings.Repeat("界", 50), strings.Repeat("界", 42)}, {"a\x00b", ""}, {"a\xffb", ""}, {"a\nb", ""},
	} {
		if got := cleanUpstreamRequestID(tc.raw); got != tc.want {
			t.Fatalf("request ID normalization: %q want %q", got, tc.want)
		}
	}
}

func assertUsageRequestID(t *testing.T, a *App, id, want string) {
	t.Helper()
	var value sql.NullString
	if err := a.DB.QueryRow("SELECT upstream_request_id FROM usage_logs WHERE request_id=$1", id).Scan(&value); err != nil || value.Valid != (want != "") || value.String != want {
		t.Fatalf("upstream request ID: got=%v want=%q error=%v", value, want, err)
	}
}

func testUpstreamRequestIDs(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.208:1234"
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(method, path, token string, body any) map[string]any {
		t.Helper()
		w := call(method, path, token, body)
		var result struct{ Data map[string]any }
		if w.Code != 200 && w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return result.Data
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "standard-id")
		w.Header().Set("X-Custom-Trace", "custom-id")
		w.Header().Set("X-Long-Trace", strings.Repeat("界", 50))
		var body struct{ Stream bool }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		}
	}))
	defer up.Close()
	gid := int64(manage("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Request ID group", "platform": "openai", "is_exclusive": true})["id"].(float64))
	manage("POST", "/api/v1/admin/channels", admin, map[string]any{"name": "Request ID price", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"trace-model"}, "input_price": 0, "output_price": 0, "cache_read_price": 0, "cache_write_price": 0}}})
	input := map[string]any{"name": "Request ID account", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"base_url": up.URL, "api_key": "trace-provider"}}
	for _, invalid := range []any{1, true, []string{"X-ID"}, "X ID", "X-ID:", strings.Repeat("x", 65), "X\r\nID"} {
		input["extra"] = map[string]any{upstreamRequestIDHeaderKey: invalid}
		if w := call("POST", "/api/v1/admin/accounts", admin, input); w.Code != 400 {
			t.Fatal("invalid request ID header accepted on create", invalid, w.Code)
		}
	}
	delete(input, "extra")
	aid := int64(manage("POST", "/api/v1/admin/accounts", admin, input)["id"].(float64))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	manage("POST", "/api/v1/admin/users", admin, map[string]any{"email": "request-id@example.test", "password": "trace-password", "balance": 1, "allowed_groups": []int64{gid}})
	user := manage("POST", "/api/v1/auth/login", "", map[string]any{"email": "request-id@example.test", "password": "trace-password"})["access_token"].(string)
	key := manage("POST", "/api/v1/keys", user, map[string]any{"name": "Request ID key", "group_id": gid})["key"].(string)
	for _, tc := range []struct {
		header any
		want   string
	}{
		{nil, ""}, {" x-custom-trace ", "custom-id"}, {"X-Absent", ""}, {"X-Long-Trace", strings.Repeat("界", 42)}, {"X-Request-ID", "standard-id"}, {" ", ""},
	} {
		manage("PUT", ap, admin, map[string]any{"extra": map[string]any{upstreamRequestIDHeaderKey: tc.header}})
		for _, stream := range []bool{false, true} {
			w := call("POST", "/v1/chat/completions", key, map[string]any{"model": "trace-model", "messages": []any{map[string]string{"role": "user", "content": "ok"}}, "stream": stream})
			if w.Code != 200 || strings.Contains(w.Body.String(), "error") {
				t.Fatal("request ID forwarding", w.Code, w.Body.String())
			}
			assertUsageRequestID(t, a, w.Header().Get("X-Request-ID"), tc.want)
		}
	}
	var has bool
	if err := a.DB.QueryRow("SELECT extra ? $2 FROM accounts WHERE id=$1", aid, upstreamRequestIDHeaderKey).Scan(&has); err != nil || has {
		t.Fatal("cleared header remained configured", has, err)
	}
	manage("PUT", ap, admin, map[string]any{"extra": map[string]any{upstreamRequestIDHeaderKey: "X-Request-ID"}})
	manage("PUT", ap, admin, map[string]any{"extra": map[string]any{"quota_limit": 1}})
	if w := call("PUT", ap, admin, map[string]any{"name": "must-not-save", "extra": map[string]any{upstreamRequestIDHeaderKey: false}}); w.Code != 400 {
		t.Fatal("invalid update accepted", w.Code)
	}
	stored := manage("GET", ap, admin, nil)
	if stored["name"] != input["name"] || stored["extra"].(map[string]any)[upstreamRequestIDHeaderKey] != "X-Request-ID" {
		t.Fatal("invalid update changed account", stored)
	}
	manage("PUT", ap, admin, map[string]any{"extra": map[string]any{upstreamRequestIDHeaderKey: nil}})
	if err := a.DB.QueryRow("SELECT extra ? $2 FROM accounts WHERE id=$1", aid, upstreamRequestIDHeaderKey).Scan(&has); err != nil || has {
		t.Fatal("null did not clear configured header", has, err)
	}
}
