package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSettledResponseResources(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			settled := json.RawMessage(fmt.Sprintf(`{"id":"resp_owned","status":%q,"error":{"message":"safe failure"}}`, status))
			raw := []byte(fmt.Sprintf(`{"id":"resp_owned","object":"response","status":%q,"output":[{"type":"reasoning","encrypted_content":"include","n":9007199254740993}]}`, status))
			if status == "failed" || status == "cancelled" {
				raw = bytes.Replace(raw, []byte(`"output":`), []byte(`"error":{"message":"upstream-secret"},"output":`), 1)
			}
			clean, err := prepareResponseResource(raw, "resp_owned", false, settled)
			if err != nil || bytes.Contains(clean, []byte("upstream-secret")) || !bytes.Contains(clean, []byte("9007199254740993")) || !bytes.Contains(clean, []byte("include")) {
				t.Fatal("settled resource projection", string(clean), err)
			}
			for _, bad := range [][]byte{
				bytes.Replace(raw, []byte("resp_owned"), []byte("resp_foreign"), 1),
				bytes.Replace(raw, []byte(`"object":"response"`), []byte(`"object":"list"`), 1),
				bytes.Replace(raw, []byte(status), []byte("in_progress"), 1),
			} {
				if _, err := prepareResponseResource(bad, "resp_owned", false, settled); err == nil {
					t.Fatal("invalid settled response accepted", string(bad))
				}
			}
			if _, err := prepareResponseResource(raw, "resp_owned", false, nil); (err != nil) != (status == "failed" || status == "cancelled") {
				t.Fatal("unproven failure accepted", err)
			}
			// A terminal stream reread must use this response's include projection,
			// not replace it with the less detailed durable result or bill it again.
			task := &backgroundResponse{videoTask: videoTask{UpstreamID: "resp_owned", Stage: "terminal", Result: settled}}
			wire := fmt.Sprintf("data: {\"type\":\"response.%s\",\"response\":%s}\n\n", status, raw)
			response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
			w := httptest.NewRecorder()
			if err := (&App{}).streamBackgroundResponse(w, httptest.NewRequest("GET", "/responses/resp_owned", nil), task, response); err != nil || !strings.Contains(w.Body.String(), "include") || !strings.Contains(w.Body.String(), "9007199254740993") || strings.Contains(w.Body.String(), "upstream-secret") || !bytes.Equal(task.Result, settled) {
				t.Fatal("terminal stream projection", err, w.Body.String())
			}
		})
	}
}

func testResponseResources(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(method, path, key string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Cookie", "client-cookie")
		r.Header.Set("Anthropic-Beta", "client-beta")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, key string, body any) map[string]any {
		t.Helper()
		w := call(method, path, key, body)
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	uid := int64(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "response-resource@example.test", "password": "resource-password", "balance": 1, "concurrency": 1})["id"].(float64))
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "response-resource@example.test", "password": "resource-password"})["access_token"].(string)
	gid := int64(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Response resources", "platform": "openai", "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"resource-model"}, "input_price": "0.001", "output_price": "0.001", "cache_read_price": "0.001", "cache_write_price": "0.001"}}})["id"].(float64))
	k := must("POST", "/api/v1/keys", token, map[string]any{"name": "response resources", "group_id": gid, "quota": 1})
	key, kid := k["key"].(string), int64(k["id"].(float64))
	other := must("POST", "/api/v1/keys", token, map[string]any{"name": "other response resources", "group_id": gid})["key"].(string)
	var creates, reads, deletes, mode atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer resource-upstream" || r.Header.Get("Cookie") != "" || r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("response resource credential isolation")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" && r.URL.Path == "/v1/responses" {
			n := creates.Add(1)
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || credentialString(body, "model") != "resource-model" {
				t.Error("invalid resource create")
			}
			response := fmt.Sprintf(`{"id":"resp_resource_%d","object":"response","status":"completed","model":"resource-model","output":[{"id":"msg_resource_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":"private output"}]}],%s}`, n, n, responseUsage)
			if string(body["stream"]) == "true" {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
			} else {
				fmt.Fprint(w, response)
			}
			return
		}
		if r.Method == "DELETE" {
			deletes.Add(1)
			id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
			switch mode.Load() {
			case 1:
				id = "resp_foreign"
			case 2:
				w.WriteHeader(404)
				return
			case 3:
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"resource-upstream"}}`)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"object":"response","deleted":true}`, id)
			return
		}
		reads.Add(1)
		if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/v1/responses/resp_resource_") {
			t.Error("incorrect resource request", r.Method, r.URL.Path)
		}
		switch mode.Load() {
		case 1:
			fmt.Fprint(w, `{"id":"resp_foreign","object":"response","status":"completed"}`)
			return
		case 2:
			w.WriteHeader(404)
			return
		case 3:
			w.WriteHeader(401)
			fmt.Fprint(w, `{"error":{"message":"resource-upstream"}}`)
			return
		case 4:
			fmt.Fprint(w, `{"object":"list","data":[{"id":"msg_input","type":"message"}],"has_more":true,"first_id":"foreign","last_id":"msg_input"}`)
			return
		case 5:
			fmt.Fprint(w, `{"object":"list","data":[],"has_more":true,"first_id":null,"last_id":null}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/input_items") {
			if r.URL.Query().Get("after") != "msg_prior" || r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("order") != "asc" || r.URL.Query().Get("include[]") != "message.input_image.image_url" {
				t.Error("input item pagination changed", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"object":"list","data":[{"id":"msg_input","type":"message","role":"user","content":[{"type":"input_text","text":"private input"}]}],"first_id":"msg_input","last_id":"msg_input","has_more":true}`)
			return
		}
		if r.URL.Query().Get("include") != "reasoning.encrypted_content" {
			t.Error("response include query missing", r.URL.RawQuery)
		}
		fmt.Fprintf(w, `{"id":%q,"object":"response","status":"completed","output":[{"id":"msg_output","type":"reasoning","encrypted_content":"opaque output"}]}`, strings.TrimPrefix(r.URL.Path, "/v1/responses/"))
	}))
	defer up.Close()
	aid := int64(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Resource origin", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "resource-upstream", "base_url": up.URL, "api_protocol": "responses"}})["id"].(float64))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	body := map[string]any{"model": "resource-model", "input": "private input"}
	for _, stream := range []bool{false, true} {
		body["stream"] = stream
		if w := call("POST", "/responses", key, body); w.Code != 200 || !strings.Contains(w.Body.String(), "private output") {
			t.Fatal("resource create", w.Code, w.Body.String())
		}
	}
	body["store"], body["stream"] = false, false
	if w := call("POST", "/responses", key, body); w.Code != 200 {
		t.Fatal("nonstored create", w.Code)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := a.DB.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Retrieval of accepted work needs no spending balance or fresh scheduling.
	exec("UPDATE users SET balance=0 WHERE id=$1", uid)
	must("POST", ap+"/schedulable", admin, map[string]any{"schedulable": false})
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, id := range []string{"resp_resource_1", "resp_resource_2"} {
			w := call("GET", prefix+"/"+id+"?include=reasoning.encrypted_content", key, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "opaque output") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("native retrieval alias", prefix, w.Code, w.Body.String())
			}
			w = call("GET", prefix+"/"+id+"/input_items?after=msg_prior&limit=1&order=asc&include[]=message.input_image.image_url", key, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), "private input") {
				t.Fatal("input item alias", prefix, w.Code, w.Body.String())
			}
		}
	}
	before := reads.Load()
	for _, credential := range []string{other, token, ""} {
		for _, suffix := range []string{"", "/input_items"} {
			w := call("GET", "/responses/resp_resource_1"+suffix, credential, nil)
			want := 401
			if credential == other {
				want = 404
			}
			if w.Code != want {
				t.Fatal("foreign response lookup", w.Code, want)
			}
		}
	}
	for _, id := range []string{"resp_resource_3", "resp_unknown"} {
		if w := call("GET", "/responses/"+id, key, nil); w.Code != 404 {
			t.Fatal("unknown or nonstored response", w.Code)
		}
	}
	for _, suffix := range []string{"?stream=true", "?include=unknown", "?limit=1", "?include=reasoning.encrypted_content&include[]=reasoning.encrypted_content", "/input_items?limit=101", "/input_items?limit=0", "/input_items?limit=1&limit=2", "/input_items?order=sideways", "/input_items?after=../foreign", "/input_items?stream=true", "/input_items?after=%ZZ"} {
		if w := call("GET", "/responses/resp_resource_1"+suffix, key, nil); w.Code != 400 {
			t.Fatal("invalid resource query", suffix, w.Code)
		}
	}
	if !a.takeSlot("user", uid, 1) {
		t.Fatal("acquire user slot")
	}
	w := call("GET", "/responses/resp_resource_1", key, nil)
	a.releaseSlot("user", uid)
	if w.Code != 429 {
		t.Fatal("resource concurrency bypass", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-resource-secret"}})
	if w := call("GET", "/responses/resp_resource_1", key, nil); w.Code != 409 {
		t.Fatal("resource source rotation bypass", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "resource-upstream"}})
	identity := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	u, err := a.loadAccount(t.Context(), aid)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.bindChatResponse(t.Context(), identity, u, "resp_converted_resource", nil); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/responses/resp_converted_resource", key, nil); w.Code != 400 {
		t.Fatal("converted history sent to native resource", w.Code)
	}
	if reads.Load() != before {
		t.Fatal("rejected resource request reached upstream")
	}
	for _, tc := range []struct{ mode, status int32 }{{1, 502}, {2, 404}, {3, 502}, {4, 502}, {5, 502}} {
		mode.Store(tc.mode)
		path := "/responses/resp_resource_1"
		if tc.mode >= 4 {
			path += "/input_items"
		}
		w := call("GET", path, key, nil)
		if w.Code != int(tc.status) || strings.Contains(w.Body.String(), "resource-upstream") || strings.Contains(w.Body.String(), "resp_foreign") {
			t.Fatal("invalid resource output exposed", tc.mode, w.Code, w.Body.String())
		}
	}
	mode.Store(0)
	// Resource deletion uses the same user/account limits and a per-response lock.
	must("PUT", ap, admin, map[string]any{"concurrency": 1})
	for _, slot := range []struct {
		name   string
		id     int64
		status int
	}{{"user", uid, 429}, {"account", aid, 429}, {responseDeletionKey(identity, "resp_resource_1"), 0, 409}} {
		if !a.takeSlot(slot.name, slot.id, 1) {
			t.Fatal("acquire deletion test slot")
		}
		w := call("DELETE", "/responses/resp_resource_1", key, nil)
		a.releaseSlot(slot.name, slot.id)
		if w.Code != slot.status || deletes.Load() != 0 {
			t.Fatal("deletion concurrency bypass", slot.name, w.Code, deletes.Load())
		}
	}
	for _, tc := range []struct {
		id, key string
		status  int
	}{{"resp_resource_1", other, 404}, {"resp_resource_1", token, 401}, {"resp_resource_1", "", 401}, {"resp_resource_3", key, 404}, {"resp_unknown", key, 404}, {"resp_converted_resource", key, 400}, {"resp_resource_1?stream=false", key, 400}} {
		if w := call("DELETE", "/responses/"+tc.id, tc.key, nil); w.Code != tc.status || deletes.Load() != 0 {
			t.Fatal("invalid deletion reached upstream", w.Code, tc.status, deletes.Load())
		}
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "rotated-resource-secret"}})
	if w := call("DELETE", "/responses/resp_resource_1", key, nil); w.Code != 409 || deletes.Load() != 0 {
		t.Fatal("deletion changed upstream source", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]any{"api_key": "resource-upstream"}})
	// Unknown deletion outcomes block all uses, including connection-local WS
	// affinity, without erasing the source needed for reconciliation after restart.
	mode.Store(1)
	if w := call("DELETE", "/responses/resp_resource_1", key, nil); w.Code != 502 {
		t.Fatal("invalid deletion acknowledgement accepted", w.Code)
	}
	if ttl, err := a.Redis.TTL(t.Context(), responseDeletionKey(identity, "resp_resource_1")).Result(); err != nil || ttl >= 0 {
		t.Fatal("unconfirmed deletion expired", ttl, err)
	}
	for _, suffix := range []string{"", "/input_items"} {
		if w := call("GET", "/responses/resp_resource_1"+suffix, key, nil); w.Code != 404 {
			t.Fatal("uncertain deletion remained readable", w.Code)
		}
	}
	socket := &responseSocket{responses: map[string]responseBinding{}}
	socket.remember("resp_resource_1", u, "msg_resource_1")
	ctx := context.WithValue(t.Context(), socketTurnKey{}, &responseSocketTurn{socket: socket})
	if _, err := a.previousResponse(ctx, identity, "resp_resource_1"); err == nil {
		t.Fatal("WS cached response bypassed deletion")
	}
	if _, err := a.responseItemSource(ctx, identity, []string{"msg_resource_1"}, nil); err == nil {
		t.Fatal("WS cached item bypassed deletion")
	}
	if err := a.bindResponse(t.Context(), identity, u, "resp_resource_1", "msg_resource_1"); err == nil {
		t.Fatal("late settlement resurrected deleted response")
	}
	if err := a.bindResponse(t.Context(), identity, u, "resp_resurrected", "msg_resource_1"); err == nil {
		t.Fatal("late settlement resurrected deleted item")
	}
	mode.Store(3)
	if w := call("DELETE", "/responses/resp_resource_1", key, nil); w.Code != 502 || strings.Contains(w.Body.String(), "resource-upstream") {
		t.Fatal("delete upstream error", w.Code)
	}
	// A fresh app shares only durable state; provider 404 confirms absence.
	mode.Store(2)
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	req := httptest.NewRequest("DELETE", "/responses/resp_resource_1", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.SetPathValue("response_id", "resp_resource_1")
	w = httptest.NewRecorder()
	fresh.backgroundResponseLookup(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":true`) {
		t.Fatal("delete recovery", w.Code, w.Body.String())
	}
	beforeDeletes := deletes.Load()
	for _, prefix := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		if w := call("DELETE", prefix+"/resp_resource_1", key, nil); w.Code != 200 || deletes.Load() != beforeDeletes {
			t.Fatal("delete alias replay", prefix, w.Code, deletes.Load())
		}
	}
	if n, err := a.Redis.Exists(t.Context(), responseBindingKey(identity, "resp_resource_1")).Result(); err != nil || n != 0 {
		t.Fatal("deleted response binding retained", n, err)
	}
	for _, k := range []string{responseDeletionKey(identity, "resp_resource_1"), responseItemKey(identity, "msg_resource_1") + ":delete"} {
		if ttl, err := a.Redis.TTL(t.Context(), k).Result(); err != nil || ttl <= 29*24*time.Hour || ttl > 30*24*time.Hour {
			t.Fatal("confirmed deletion retention", ttl, err)
		}
	}
	mode.Store(0)
	if w := call("DELETE", "/v1/responses/resp_resource_2", key, nil); w.Code != 200 || deletes.Load() != beforeDeletes+1 {
		t.Fatal("confirmed delete", w.Code, w.Body.String())
	}
	exec("UPDATE api_keys SET status='inactive' WHERE id=$1", kid)
	if w := call("GET", "/responses/resp_resource_1", key, nil); w.Code != 401 {
		t.Fatal("disabled key retrieval", w.Code)
	}
	if w := call("DELETE", "/responses/resp_resource_1", key, nil); w.Code != 401 {
		t.Fatal("disabled key deletion replay", w.Code)
	}
	exec("UPDATE api_keys SET status='active',group_id=NULL WHERE id=$1", kid)
	if w := call("GET", "/responses/resp_resource_1", key, nil); w.Code != 403 {
		t.Fatal("detached key retrieval", w.Code)
	}
	if w := call("DELETE", "/responses/resp_resource_1", key, nil); w.Code != 403 {
		t.Fatal("detached key deletion replay", w.Code)
	}
	exec("UPDATE api_keys SET group_id=$2 WHERE id=$1", kid, gid)
	if err := a.bindResponse(t.Context(), identity, u, "resp_expired_resource"); err != nil {
		t.Fatal(err)
	}
	if err := a.Redis.Del(t.Context(), responseBindingKey(identity, "resp_expired_resource")).Err(); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/responses/resp_expired_resource", key, nil); w.Code != 404 {
		t.Fatal("expired binding lookup", w.Code)
	}
	var count int
	var used, balance string
	if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE api_key_id=$1", kid).Scan(&count, &used); err != nil || count != 3 || used != "0.0840000000" {
		t.Fatal("resource reads changed usage", count, used, err)
	}
	if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1", kid).Scan(&balance, &used); err != nil || balance != "0.00000000" || used != "0.08400000" || creates.Load() != 3 {
		t.Fatal("resource reads charged or regenerated", balance, used, creates.Load(), err)
	}
}
