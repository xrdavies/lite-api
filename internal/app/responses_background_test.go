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
	"sync"
	"testing"
	"time"
)

func TestBackgroundResponseRequests(t *testing.T) {
	for _, raw := range []string{`{"background":true,"store":true}`, `{"background":true,"store":false}`, `{"background":true,"stream":true}`} {
		var body map[string]json.RawMessage
		_ = json.Unmarshal([]byte(raw), &body)
		body["model"], body["input"] = json.RawMessage(`"m"`), json.RawMessage(`"hello"`)
		in, err := parseTextRequest(httptest.NewRequest("POST", "/responses", nil), "responses", body)
		if err != nil || !in.Background || in.Store != (string(body["store"]) == "true") {
			t.Fatal(in, err)
		}
		r := httptest.NewRequest("POST", "/responses/compact", nil)
		r.SetPathValue("action", "compact")
		if _, err = parseTextRequest(r, "responses", body); err == nil {
			t.Fatal("background compact accepted")
		}
		r = httptest.NewRequest("POST", "/responses", nil)
		r = r.WithContext(context.WithValue(r.Context(), socketTurnKey{}, &responseSocketTurn{}))
		if _, err = parseTextRequest(r, "responses", body); err == nil {
			t.Fatal("background WS accepted")
		}
	}
}

func testBackgroundResponses(t *testing.T, a *App, admin string) {
	t.Helper()
	// Stop workers during deterministic failure injection. The final section
	// verifies autonomous reconciliation; the deferred start restores all workers.
	defer pauseTestWorkers(a)()
	call := func(method, path, key string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.RemoteAddr = "192.0.2.163:1234"
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Idempotency-Key", idem)
		r.Header.Set("Cookie", "client-secret")
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	must := func(method, path, key string, body any) map[string]any {
		t.Helper()
		w := call(method, path, key, body, "")
		var out struct{ Data map[string]any }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		return out.Data
	}
	id := func(v map[string]any) int64 { return int64(v["id"].(float64)) }
	prices := func(n string) []any {
		return []any{map[string]any{"models": []string{"bg-model"}, "platform": "openai", "input_price": n, "output_price": "0.002", "cache_read_price": "0.0001", "cache_write_price": "0.003"}}
	}
	gid := id(must("POST", "/api/v1/admin/groups", admin, map[string]any{"name": "Background", "platform": "openai", "model_pricing": prices("0.001"), "free_openai_fast": true, "profit_control_enabled": true}))
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{fastPolicySetting: fastPolicySettings{Rules: []fastPolicyRule{{Tier: "missing", Action: "force_priority", Scope: "apikey", Models: []string{"native-bg"}}}}})
	defer a.DB.Exec("DELETE FROM settings WHERE key=$1", fastPolicySetting)
	uid := id(must("POST", "/api/v1/admin/users", admin, map[string]any{"email": "background@example.test", "password": "background-password", "balance": 100, "concurrency": 5}))
	token := must("POST", "/api/v1/auth/login", "", map[string]any{"email": "background@example.test", "password": "background-password"})["access_token"].(string)
	k := must("POST", "/api/v1/keys", token, map[string]any{"name": "background", "group_id": gid, "quota": 100})
	key, kid := k["key"].(string), id(k)
	other := must("POST", "/api/v1/keys", token, map[string]any{"name": "other-background", "group_id": gid})["key"].(string)
	var mu sync.Mutex
	states := map[string]string{}
	modes := map[string]string{}
	creates, cancels, reads, deletes := 0, 0, 0, 0
	next := ""
	idleDone := make(chan struct{}, 1)
	var resumeQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer background-secret" || r.Header.Get("Cookie") != "" {
			t.Error("background credential isolation")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Background-Trace", "background-"+r.Method)
		native := ""
		stream := r.URL.Query().Get("stream") == "true"
		if r.Method == "POST" && r.URL.Path == "/v1/responses" {
			creates++
			var request map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&request)
			if credentialString(request, "model") != "native-bg" || string(request["background"]) != "true" {
				t.Error("background body mapping")
			}
			if creates == 1 && credentialString(request, "service_tier") != "priority" {
				t.Error("background force fast not applied")
			}
			if next == "reject" || next == "ambiguous" {
				status := 400
				if next == "ambiguous" {
					status = 503
				}
				next = ""
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":"provider-secret"}`)
				return
			}
			native = fmt.Sprintf("resp_bg_%d", creates)
			states[native] = "queued"
			modes[native] = next
			next = ""
			stream = string(request["stream"]) == "true"
		} else {
			native = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/responses/"), "/cancel"), "/input_items")
			if _, ok := states[native]; !ok {
				w.WriteHeader(404)
				return
			}
			if r.Method == "DELETE" {
				deletes++
				delete(states, native)
				fmt.Fprintf(w, `{"id":%q,"object":"response","deleted":true}`, native)
				return
			}
			if r.Method == "POST" {
				cancels++
				if states[native] == "queued" || states[native] == "in_progress" {
					states[native] = "cancelled"
				}
			} else {
				reads++
				resumeQuery = r.URL.RawQuery
			}
			if strings.HasSuffix(r.URL.Path, "/input_items") {
				fmt.Fprint(w, `{"object":"list","data":[{"id":"msg_bg_input","type":"message","role":"user","content":[{"type":"input_text","text":"private background input"}]}],"first_id":"msg_bg_input","last_id":"msg_bg_input","has_more":false}`)
				return
			}
		}
		response := func(status string) map[string]any {
			out := map[string]any{"id": native, "object": "response", "model": "native-bg", "status": status, "background": true, "output": []any{}}
			if status == "cancelled" && modes[native] != "missing-usage" {
				out["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
			}
			if status == "completed" || status == "incomplete" || modes[native] == "failed-usage" {
				out["output"] = []any{map[string]any{"type": "message", "id": "msg_" + native, "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "private generated text"}}}}
				if modes[native] != "missing-usage" {
					var usage map[string]any
					_ = json.Unmarshal([]byte(`{`+responseUsage+`}`), &usage)
					out["usage"] = usage["usage"]
				}
			}
			if status == "failed" {
				out["error"] = map[string]string{"message": "provider-secret"}
			}
			if r.URL.Query().Get("include") == "reasoning.encrypted_content" || r.URL.Query().Get("include[]") == "reasoning.encrypted_content" {
				out["output"] = []any{map[string]any{"type": "reasoning", "id": "rs_" + native, "encrypted_content": "included-reasoning", "native_number": json.Number("9007199254740993")}}
				out["tools"] = []any{map[string]any{"type": "mcp", "server_label": "test", "authorization": "included-secret", "headers": map[string]string{"secret": "included-secret"}}}
			}
			return out
		}
		if !stream {
			json.NewEncoder(w).Encode(response(states[native]))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(kind string, out map[string]any, n int) {
			raw, _ := json.Marshal(map[string]any{"type": kind, "response": out, "sequence_number": n})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			w.(http.Flusher).Flush()
		}
		event("response.created", response("in_progress"), 0)
		if modes[native] == "idle" {
			mu.Unlock()
			<-r.Context().Done()
			mu.Lock()
			modes[native], states[native] = "", "completed"
			idleDone <- struct{}{}
			return
		}
		if modes[native] == "disconnect" {
			modes[native] = ""
			states[native] = "completed"
			return
		}
		states[native] = "completed"
		event("response.completed", response("completed"), 4)
	}))
	defer up.Close()
	aid := id(must("POST", "/api/v1/admin/accounts", admin, map[string]any{"name": "Background", "platform": "openai", "type": "apikey", "extra": map[string]any{upstreamRequestIDHeaderKey: "X-Background-Trace"}, "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "background-secret", "base_url": up.URL, "api_protocol": "responses", "model_mapping": map[string]string{"bg-model": "native-bg"}}}))
	ap := fmt.Sprintf("/api/v1/admin/accounts/%d", aid)
	g := &gatewayIdentity{Key: gatewayKey{ID: kid, GroupID: gid}}
	body := map[string]any{"model": "bg-model", "input": "private request", "background": true, "store": true}
	create := func(idem string) string {
		t.Helper()
		w := call("POST", "/v1/responses", key, body, idem)
		var out struct{ ID, Status string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Status != "queued" {
			t.Fatalf("background create %d %s", w.Code, w.Body.String())
		}
		return out.ID
	}
	load := func(native string) *backgroundResponse {
		t.Helper()
		taskID, e := a.Redis.Get(t.Context(), backgroundIndex(g, native)).Result()
		if e != nil {
			t.Fatal(e)
		}
		task, e := a.loadBackgroundResponse(t.Context(), taskID)
		if e != nil {
			t.Fatal(e)
		}
		return task
	}
	set := func(native, status string) { mu.Lock(); states[native] = status; mu.Unlock() }
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := a.DB.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	first := create("bg-once")
	task := load(first)
	if task.Stage != "pending" || task.Selection.Account.Credentials != nil || task.UpstreamRequestID != "background-POST" {
		t.Fatal("background snapshot", task.Stage)
	}
	envelope, _ := a.Redis.Get(t.Context(), backgroundKey(task.ID)).Result()
	if strings.Contains(envelope, "private request") || strings.Contains(envelope, "background-secret") {
		t.Fatal("plaintext background task")
	}
	if w := call("POST", "/responses", key, body, "bg-once"); w.Code != 200 || w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("background replay", w.Code)
	}
	if w := call("GET", "/responses/"+first, other, nil, ""); w.Code != 404 {
		t.Fatal("foreign background access", w.Code)
	}
	if w := call("POST", "/responses/"+first+"/cancel", other, nil, ""); w.Code != 404 {
		t.Fatal("foreign background cancel", w.Code)
	}
	if w := call("GET", "/responses/"+first+"/input_items", other, nil, ""); w.Code != 404 {
		t.Fatal("foreign background input items", w.Code)
	}
	if w := call("GET", "/responses/"+first+"/input_items", key, nil, ""); w.Code != 409 || strings.Contains(w.Body.String(), "private background input") {
		t.Fatal("pending background input lookup", w.Code)
	}
	if w := call("DELETE", "/responses/"+first, key, nil, ""); w.Code != 409 {
		t.Fatal("pending background deleted", w.Code)
	}
	for _, query := range []string{"?stream=invalid", "?starting_after=0", "?stream=true&starting_after=-1", "?stream=true&stream=false", "?include=unknown", "?stream=%ZZ", "?stream=true;starting_after=0"} {
		if w := call("GET", "/responses/"+first+query, key, nil, ""); w.Code != 400 {
			t.Fatal("invalid background query accepted", query, w.Code)
		}
	}
	mu.Lock()
	beforeReads := reads
	mu.Unlock()
	if w := call("GET", "/responses/"+first+"?stream=true", key, nil, ""); w.Code != 400 {
		t.Fatal("nonstreaming task resumed as stream", w.Code)
	}
	mu.Lock()
	if reads != beforeReads {
		t.Error("invalid stream request reached provider")
	}
	mu.Unlock()
	// A second receipt must not take over an existing provider response ID.
	collision := *task
	collision.ID = "background_collision"
	if err := a.saveBackgroundResponse(t.Context(), &collision); err == nil {
		t.Fatal("provider response ID collision accepted")
	}
	if n, err := a.Redis.Exists(t.Context(), backgroundKey(collision.ID)).Result(); err != nil || n != 0 {
		t.Fatal("collision partially persisted", n, err)
	}
	collision.UpstreamID, collision.Stage, collision.Result = "", "submitting", nil
	if err := a.saveBackgroundResponse(t.Context(), &collision); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"id": first, "object": "response", "status": "queued"})
	if err := a.observeBackgroundResponse(t.Context(), &collision, raw); err == nil || collision.UpstreamID != "" || collision.Stage != "submitting" {
		t.Fatal("failed identity persistence released submission protection", err)
	}
	collision.Stage = "terminal"
	if err := a.saveBackgroundResponse(t.Context(), &collision); err != nil {
		t.Fatal(err)
	}
	if w := call("GET", "/responses/"+first, token, nil, ""); w.Code != 401 {
		t.Fatal("JWT background access", w.Code)
	}
	exec("UPDATE users SET balance=0 WHERE id=$1", uid)
	if w := call("GET", "/responses/"+first, key, nil, ""); w.Code != 200 {
		t.Fatal("zero balance poll", w.Code)
	}
	if w := call("POST", "/responses", key, body, "empty-bg"); w.Code != 402 {
		t.Fatal("zero balance create", w.Code)
	}
	exec("UPDATE users SET balance=100 WHERE id=$1", uid)
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_pricing": prices("0.5"), "force_openai_fast": false, "free_openai_fast": false})
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"profit_min_margin": "0.9"})
	if w := call("POST", "/responses", key, body, "profit-blocked-bg"); w.Code != 503 {
		t.Fatal("new background task bypassed profit gate", w.Code)
	}
	must("PUT", "/api/v1/admin/settings", admin, map[string]any{fastPolicySetting: fastPolicySettings{Rules: []fastPolicyRule{{Tier: "all", Action: "block", Scope: "apikey"}}}})
	exec("ALTER TABLE usage_logs ADD CONSTRAINT test_background_failure CHECK(api_key_id<>" + fmt.Sprint(kid) + ") NOT VALID")
	defer a.DB.Exec("ALTER TABLE usage_logs DROP CONSTRAINT IF EXISTS test_background_failure")
	must("PUT", ap, admin, map[string]any{"extra": map[string]any{upstreamRequestIDHeaderKey: "X-Changed"}})
	set(first, "completed")
	if w := call("GET", "/responses/"+first, key, nil, ""); w.Code != 503 || strings.Contains(w.Body.String(), "private generated") {
		t.Fatal("unsettled output exposed", w.Code, w.Body.String())
	}
	if load(first).Stage != "settling" {
		t.Fatal("missing settlement checkpoint")
	}
	if w := call("GET", "/responses/"+first+"/input_items", key, nil, ""); w.Code != 503 || strings.Contains(w.Body.String(), "private background input") {
		t.Fatal("input lookup bypassed background settlement", w.Code)
	}
	if w := call("DELETE", "/responses/"+first, key, nil, ""); w.Code != 503 {
		t.Fatal("deletion bypassed background settlement", w.Code)
	}
	mu.Lock()
	if deletes != 0 {
		t.Error("unsettled deletion reached provider")
	}
	mu.Unlock()
	exec("ALTER TABLE usage_logs DROP CONSTRAINT test_background_failure")
	fresh := &App{DB: a.DB, Redis: a.Redis, secret: a.secret, instanceLock: a.instanceLock, privateUpstreams: a.privateUpstreams}
	for i := 0; i < 2; i++ {
		if err := fresh.runBackgroundResponses(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	assertUsageRequestID(t, a, task.ID, "background-POST")
	var count int
	var cost, balance, used string
	if err := a.DB.QueryRow("SELECT count(*),sum(actual_cost)::text FROM usage_logs WHERE request_id=$1", task.ID).Scan(&count, &cost); err != nil || count != 1 || cost != "0.0375000000" {
		t.Fatal("background receipt", count, cost, err)
	}
	var tier, total string
	if err := a.DB.QueryRow("SELECT service_tier,total_cost::text FROM usage_logs WHERE request_id=$1", task.ID).Scan(&tier, &total); err != nil || tier != "priority" || total != "0.0750000000" || task.Tier != "priority" || !task.Identity.Group.FreeFast {
		t.Fatal("recovered background fast snapshot", tier, total, task.Tier, err)
	}
	if err := a.DB.QueryRow("SELECT u.balance::text,k.quota_used::text FROM users u JOIN api_keys k ON k.user_id=u.id WHERE k.id=$1", kid).Scan(&balance, &used); err != nil || balance != "99.96250000" || used != "0.03750000" {
		t.Fatal("background funds", balance, used, err)
	}
	for _, prefix := range []string{"/v1/responses/", "/responses/", "/backend-api/codex/responses/"} {
		if w := call("GET", prefix+first, key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "private generated text") {
			t.Fatal("background result alias", w.Code)
		}
		if w := call("GET", prefix+first+"/input_items?limit=1&order=asc", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "private background input") {
			t.Fatal("background input items alias", w.Code, w.Body.String())
		}
		if w := call("GET", prefix+first+"?include=reasoning.encrypted_content", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "included-reasoning") || strings.Contains(w.Body.String(), "included-secret") {
			t.Fatal("background include query", w.Code, w.Body.String())
		}
	}
	set(first, "incomplete")
	if w := call("GET", "/responses/"+first+"?include=reasoning.encrypted_content", key, nil, ""); w.Code != 502 || strings.Contains(w.Body.String(), "included-reasoning") {
		t.Fatal("settled resource changed terminal state", w.Code, w.Body.String())
	}
	set(first, "completed")
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("background resource read charged twice", count, err)
	}
	bound, err := a.previousResponse(t.Context(), g, first)
	if err != nil || bound.AccountID != aid || len(bound.Items) != 1 {
		t.Fatal("background continuation", bound, err)
	}
	if w := call("POST", "/responses/"+first+"/cancel", key, nil, ""); w.Code != 200 {
		t.Fatal("completed cancel", w.Code)
	}
	for _, prefix := range []string{"/v1/responses/", "/responses/", "/backend-api/codex/responses/"} {
		if w := call("DELETE", prefix+first, key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"deleted":true`) {
			t.Fatal("background delete alias", w.Code, w.Body.String())
		}
		if w := call("GET", prefix+first, key, nil, ""); w.Code != 404 {
			t.Fatal("deleted background output visible", w.Code)
		}
	}
	if w := call("POST", "/responses/"+first+"/cancel", key, nil, ""); w.Code != 404 {
		t.Fatal("deleted background cancellation returned output", w.Code)
	}
	if _, err := a.previousResponse(t.Context(), g, first); err == nil {
		t.Fatal("deleted background continuation")
	}
	if err := a.saveBackgroundResponse(t.Context(), task); err == nil {
		t.Fatal("late background write resurrected a deleted task")
	}
	if n, err := a.Redis.Exists(t.Context(), backgroundKey(task.ID), backgroundIndex(g, first)).Result(); err != nil || n != 0 {
		t.Fatal("deleted background content remains", n, err)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", task.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("deletion changed background accounting", count, err)
	}
	mu.Lock()
	if deletes != 1 {
		t.Error("background delete replay dispatched", deletes)
	}
	mu.Unlock()
	must("PUT", fmt.Sprintf("/api/v1/admin/groups/%d", gid), admin, map[string]any{"model_pricing": prices("0.001"), "profit_min_margin": "0"})
	queued := create("cancel-bg")
	for i := 0; i < 2; i++ {
		if w := call("POST", "/responses/"+queued+"/cancel", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "cancelled") {
			t.Fatal("background cancellation", w.Code, w.Body.String())
		}
	}
	cancelTask := load(queued)
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", cancelTask.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("free cancellation billed", count, err)
	}
	// A provider can execute work between our queued observation and cancellation.
	missingCancel := create("cancel-missing-bg")
	mu.Lock()
	modes[missingCancel] = "missing-usage"
	mu.Unlock()
	if w := call("POST", "/responses/"+missingCancel+"/cancel", key, nil, ""); w.Code != 502 || load(missingCancel).Stage != "pending" {
		t.Fatal("unmetered cancellation treated as free", w.Code)
	}
	mu.Lock()
	modes[missingCancel] = "failed-usage"
	mu.Unlock()
	if err := fresh.runBackgroundResponses(t.Context()); err != nil || load(missingCancel).Receipt == nil {
		t.Fatal("cancellation usage not recovered", err)
	}
	failed := create("failed-bg")
	mu.Lock()
	modes[failed] = "failed-usage"
	mu.Unlock()
	set(failed, "failed")
	if w := call("GET", "/responses/"+failed, key, nil, ""); w.Code != 200 || strings.Contains(w.Body.String(), "provider-secret") {
		t.Fatal("failed response usage", w.Code, w.Body.String())
	}
	if load(failed).Receipt == nil {
		t.Fatal("known failed usage discarded")
	}
	for _, item := range []struct{ id, status string }{{queued, "cancelled"}, {failed, "failed"}} {
		task := load(item.id)
		for _, prefix := range []string{"/responses/", "/v1/responses/", "/backend-api/codex/responses/"} {
			w := call("GET", prefix+item.id+"?include[]=reasoning.encrypted_content", key, nil, "")
			var result struct{ Status string }
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != item.status || !strings.Contains(w.Body.String(), "included-reasoning") || !strings.Contains(w.Body.String(), "9007199254740993") || strings.Contains(w.Body.String(), "provider-secret") || strings.Contains(w.Body.String(), "included-secret") {
				t.Fatal("failed/cancelled response include", w.Code, w.Body.String())
			}
			if w := call("GET", prefix+item.id+"?include=reasoning.encrypted_content", other, nil, ""); w.Code != 404 {
				t.Fatal("foreign failed response include", w.Code)
			}
			if w := call("GET", prefix+item.id+"/input_items", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "private background input") {
				t.Fatal("failed/cancelled input items", w.Code, w.Body.String())
			}
		}
		var got int
		want := 0
		if task.Receipt != nil {
			want = 1
		}
		if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", task.ID).Scan(&got); err != nil || got != want || !bytes.Equal(load(item.id).Result, task.Result) {
			t.Fatal("include reads changed settlement or stored result", got, err)
		}
	}
	noUsage := create("failed-no-usage-bg")
	set(noUsage, "failed")
	if w := call("GET", "/responses/"+noUsage, key, nil, ""); w.Code != 502 || load(noUsage).Stage != "pending" {
		t.Fatal("failed task without usage became free", w.Code)
	}
	mu.Lock()
	modes[noUsage] = "failed-usage"
	mu.Unlock()
	if err := fresh.runBackgroundResponses(t.Context()); err != nil {
		t.Fatal(err)
	}
	unknown := create("missing-bg")
	mu.Lock()
	modes[unknown] = "missing-usage"
	mu.Unlock()
	set(unknown, "completed")
	if w := call("GET", "/responses/"+unknown, key, nil, ""); w.Code != 502 || load(unknown).Stage != "pending" {
		t.Fatal("missing background usage accepted", w.Code)
	}
	mu.Lock()
	modes[unknown] = ""
	mu.Unlock()
	if err := fresh.runBackgroundResponses(t.Context()); err != nil {
		t.Fatal(err)
	}
	if load(unknown).Stage != "terminal" {
		t.Fatal("missing usage not recovered")
	}
	rotated := create("rotated-bg")
	must("PUT", ap, admin, map[string]any{"credentials": map[string]string{"api_key": "rotated"}})
	if w := call("GET", "/responses/"+rotated, key, nil, ""); w.Code != 409 {
		t.Fatal("rotated background source accepted", w.Code)
	}
	must("PUT", ap, admin, map[string]any{"credentials": map[string]string{"api_key": "background-secret"}})
	set(rotated, "incomplete")
	if w := call("GET", "/responses/"+rotated, key, nil, ""); w.Code != 200 {
		t.Fatal("restored background source", w.Code)
	}
	// Streaming disconnect preserves the accepted identity for polling/resumption.
	body["stream"] = true
	mu.Lock()
	next = "disconnect"
	mu.Unlock()
	w := call("POST", "/responses", key, body, "stream-bg")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"type":"error"`) || strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal("interrupted background stream", w.Code, w.Body.String())
	}
	mu.Lock()
	streamID := fmt.Sprintf("resp_bg_%d", creates)
	mu.Unlock()
	if load(streamID).Stage != "pending" {
		t.Fatal("stream lost pending task")
	}
	if w = call("GET", "/responses/"+streamID+"?stream=true&starting_after=0", key, nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "event: response.completed\ndata:") {
		t.Fatal("resume background stream", w.Code, w.Body.String())
	}
	if load(streamID).Stage != "terminal" {
		t.Fatal("stream not settled")
	}
	mu.Lock()
	query := resumeQuery
	mu.Unlock()
	if query != "starting_after=0&stream=true" {
		t.Fatal("resume cursor lost", query)
	}
	settledBefore := load(streamID)
	for _, prefix := range []string{"/responses/", "/v1/responses/", "/backend-api/codex/responses/"} {
		w := call("GET", prefix+streamID+"?stream=true&starting_after=0&include=reasoning.encrypted_content&include_obfuscation=false", key, nil, "")
		terminal := strings.Split(w.Body.String(), "event: response.completed\ndata: ")
		if w.Code != 200 || len(terminal) != 2 || !strings.Contains(terminal[1], "included-reasoning") || !strings.Contains(terminal[1], "9007199254740993") || strings.Contains(w.Body.String(), "included-secret") {
			t.Fatal("settled stream lost include fields or leaked secrets", w.Code, w.Body.String())
		}
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE request_id=$1", settledBefore.ID).Scan(&count); err != nil || count != 1 || !bytes.Equal(load(streamID).Result, settledBefore.Result) {
		t.Fatal("stream reread changed settlement or stored result", count, err)
	}
	mu.Lock()
	states[streamID] = "failed"
	mu.Unlock()
	// Replayed provider events cannot change a settled task's terminal state.
	settled := load(streamID)
	badEvent := fmt.Sprintf("data: {\"type\":\"response.failed\",\"response\":{\"id\":%q,\"status\":\"failed\"}}\n\n", streamID)
	w = httptest.NewRecorder()
	err = a.streamBackgroundResponse(w, httptest.NewRequest("GET", "/responses/"+streamID, nil), settled, &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(badEvent))})
	if err == nil || strings.Contains(w.Body.String(), "event: response.failed") || load(streamID).Stage != "terminal" {
		t.Fatal("terminal stream status changed", err, w.Body.String())
	}
	w = call("POST", "/responses", key, body, "stream-complete-bg")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatal("complete background stream", w.Code, w.Body.String())
	}
	if replay := call("POST", "/v1/responses", key, body, "stream-complete-bg"); replay.Header().Get("Idempotency-Replayed") != "true" || replay.Body.String() != w.Body.String() {
		t.Fatal("stream replay")
	}
	oldIdle := a.streamIdle
	a.streamIdle = 30 * time.Millisecond
	defer func() { a.streamIdle = oldIdle }()
	mu.Lock()
	next = "idle"
	mu.Unlock()
	w = call("POST", "/responses", key, body, "stream-idle-bg")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("background idle create", w.Code, w.Body.String())
	}
	select {
	case <-idleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("idle create did not cancel provider read")
	}
	mu.Lock()
	idleID := fmt.Sprintf("resp_bg_%d", creates)
	modes[idleID] = "idle"
	mu.Unlock()
	if load(idleID).Stage != "pending" {
		t.Fatal("idle stream lost task")
	}
	w = call("GET", "/responses/"+idleID+"?stream=true", key, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal("background idle resume", w.Code, w.Body.String())
	}
	select {
	case <-idleDone:
	case <-time.After(5 * time.Second):
		t.Fatal("idle resume did not cancel provider read")
	}
	a.streamIdle = oldIdle
	if err := fresh.runBackgroundResponses(t.Context()); err != nil || load(idleID).Stage != "terminal" {
		t.Fatal("idle background stream did not recover", err)
	}
	delete(body, "stream")
	body["store"] = false
	transient := create("transient-bg")
	set(transient, "completed")
	if w = call("GET", "/responses/"+transient, key, nil, ""); w.Code != 200 {
		t.Fatal("transient completion", w.Code)
	}
	if _, err := a.previousResponse(t.Context(), g, transient); err == nil {
		t.Fatal("nonstored background bound")
	}
	ttl, _ := a.Redis.TTL(t.Context(), backgroundKey(load(transient).ID)).Result()
	if ttl <= 0 || ttl > 10*time.Minute {
		t.Fatal("transient retention", ttl)
	}
	mu.Lock()
	next = "reject"
	mu.Unlock()
	if w = call("POST", "/responses", key, body, "reject-bg"); w.Code != 502 {
		t.Fatal("definite rejection", w.Code)
	}
	if w = call("POST", "/responses", key, body, "reject-bg"); w.Header().Get("Idempotency-Replayed") != "true" {
		t.Fatal("definite rejection replay", w.Code)
	}
	// Unknown acceptance is never resent, and remains visible in the pending set.
	mu.Lock()
	next = "ambiguous"
	before := creates
	mu.Unlock()
	if w = call("POST", "/responses", key, body, "ambiguous-bg"); w.Code != 502 {
		t.Fatal("ambiguous background", w.Code)
	}
	if w = call("POST", "/responses", key, body, "ambiguous-bg"); w.Code != 409 {
		t.Fatal("ambiguous background replay", w.Code)
	}
	mu.Lock()
	after := creates
	cancelCount := cancels
	mu.Unlock()
	if after != before+1 || cancelCount != 2 {
		t.Fatal("duplicate provider operations", before, after, cancelCount)
	}
	var pending []string
	pending, err = a.Redis.SMembers(t.Context(), backgroundPending).Result()
	if err != nil || len(pending) != 1 {
		t.Fatal("unknown acceptance not durable", pending, err)
	}
	ambiguous, err := a.loadBackgroundResponse(t.Context(), pending[0])
	if err != nil || ambiguous.Stage != "submitting" {
		t.Fatal("unknown acceptance state", err)
	}
	must("POST", ap+"/recover-state", admin, map[string]any{})
	autonomous := create("worker-bg")
	set(autonomous, "completed")
	workerCtx, stop := context.WithCancel(context.Background())
	a.startBackgroundResponses(workerCtx)
	deadline := time.Now().Add(5 * time.Second)
	for load(autonomous).Stage != "terminal" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-a.responseWorkerDone
	if load(autonomous).Stage != "terminal" {
		t.Fatal("background worker did not settle")
	}
	// Resolve the intentional unknown-acceptance fixture before restarting workers.
	ids, _ := a.Redis.SMembers(t.Context(), backgroundPending).Result()
	for _, id := range ids {
		task, e := a.loadBackgroundResponse(t.Context(), id)
		if e != nil {
			t.Fatal(e)
		}
		if task.Identity.Key.ID == kid && task.Stage == "submitting" {
			task.Stage = "terminal"
			if e = a.saveBackgroundResponse(t.Context(), task); e != nil {
				t.Fatal(e)
			}
		}
	}
	var reconciled bool
	if err := a.DB.QueryRow("SELECT (100-balance)=(SELECT sum(round(actual_cost,8)) FROM usage_logs WHERE user_id=$1) FROM users WHERE id=$1", uid).Scan(&reconciled); err != nil || !reconciled {
		t.Fatal("background balance reconciliation", err)
	}
}
