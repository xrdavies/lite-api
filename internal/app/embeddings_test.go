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
)

func TestEmbeddingInputAndUsage(t *testing.T) {
	for _, raw := range []string{`"hello"`, `["one","two"]`, `[1,2,3]`, `[[1,2],[3]]`} {
		if !validEmbeddingInput(json.RawMessage(raw)) {
			t.Fatal("valid embedding input rejected", raw)
		}
	}
	for _, raw := range []string{`null`, `""`, `[]`, `[null]`, `[-1]`, `[1.5]`, `[[]]`, `[[null]]`, `[["1"]]`, `{"text":"hello"}`} {
		if validEmbeddingInput(json.RawMessage(raw)) {
			t.Fatal("invalid embedding input accepted", raw)
		}
	}
	for _, usage := range []string{`{"prompt_tokens":12,"total_tokens":12}`, `{"input_tokens":12}`, `{"total_tokens":12}`} {
		o := textObservation{Protocol: "embeddings"}
		if err := o.observe([]byte(`{"usage":` + usage + `}`)); err != nil || !o.HasUsage || o.Usage.Input != 12 || o.Usage.Output != 0 {
			t.Fatal("embedding usage", o, err)
		}
	}
}

func testEmbeddings(t *testing.T, a *App, admin string) {
	t.Helper()
	call := func(path, token string, body any, idem string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", idem)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		return w
	}
	manage := func(path, token string, body any) map[string]any {
		t.Helper()
		w := call(path, token, body, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var envelope struct{ Data map[string]any }
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope.Data
	}
	var count atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"upstream-embedding"}]}`)
			return
		}
		count.Add(1)
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer embedding-upstream" || r.Header.Get("X-Api-Key") != "" || r.Header.Get("Anthropic-Version") != "" || r.Header.Get("Cookie") != "" {
			t.Error("embedding path or upstream credential")
		}
		var body struct {
			Model      string
			Input      []string
			Format     string `json:"encoding_format"`
			Dimensions int
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "upstream-embedding" || len(body.Input) != 2 || body.Format != "base64" || body.Dimensions != 2 {
			t.Error("embedding body changed", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"object":"list","model":"upstream-embedding","data":[{"object":"embedding","index":0,"embedding":"AAAAAAAAgD8="},{"object":"embedding","index":1,"embedding":"AACAPwAAAAA="}],"usage":{"prompt_tokens":12,"total_tokens":12}}`)
	}))
	defer upstream.Close()
	user := manage("/api/v1/admin/users", admin, map[string]any{"email": "embedding@example.test", "password": "embedding-password", "balance": 1})
	uid := int64(user["id"].(float64))
	token := manage("/api/v1/auth/login", "", map[string]any{"email": "embedding@example.test", "password": "embedding-password"})["access_token"].(string)
	gid := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "Embeddings", "platform": "openai"})["id"].(float64))
	// An input-only price must not require an output or cache price for text embeddings.
	manage("/api/v1/admin/channels", admin, map[string]any{"name": "Embeddings", "group_ids": []int64{gid}, "model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"client-embedding"}, "input_price": json.Number("0.0000001")}}})
	manage("/api/v1/admin/accounts", admin, map[string]any{"name": "Embeddings", "platform": "openai", "type": "apikey", "group_ids": []int64{gid}, "credentials": map[string]any{"api_key": "embedding-upstream", "base_url": upstream.URL + "/v1", "api_protocol": "responses", "model_mapping": map[string]string{"client-embedding": "upstream-embedding"}}})
	key := manage("/api/v1/keys", token, map[string]any{"name": "Embeddings", "group_id": gid})["key"].(string)
	body := map[string]any{"model": "client-embedding", "input": []string{"one", "two"}, "encoding_format": "base64", "dimensions": 2}
	first := call("/v1/embeddings", key, body, "embedding-request")
	second := call("/embeddings", key, body, "embedding-request")
	if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" || count.Load() != 1 {
		t.Fatal("embedding forwarding or alias replay", first.Code, first.Body.String(), second.Code)
	}
	if !strings.Contains(first.Body.String(), "AAAAAAAAgD8=") {
		t.Fatal("base64 vector changed")
	}
	var input, output, logged int
	var cost, balance, endpoint string
	if err := a.DB.QueryRow("SELECT l.input_tokens,l.output_tokens,l.actual_cost::text,u.balance::text,l.upstream_endpoint FROM usage_logs l JOIN users u ON u.id=l.user_id WHERE u.id=$1", uid).Scan(&input, &output, &cost, &balance, &endpoint); err != nil || input != 12 || output != 0 || cost != "0.0000012000" || balance != "0.99999880" || endpoint != "/v1/embeddings" {
		t.Fatal("embedding settlement", input, output, cost, balance, endpoint, err)
	}
	body["stream"] = true
	if w := call("/embeddings", key, body, ""); w.Code != 400 || count.Load() != 1 {
		t.Fatal("streaming embeddings accepted", w.Code)
	}
	delete(body, "stream")
	body["input"] = []any{nil}
	if w := call("/embeddings", key, body, ""); w.Code != 400 || count.Load() != 1 {
		t.Fatal("invalid embedding input dispatched", w.Code)
	}
	if err := a.DB.QueryRow("SELECT count(*) FROM usage_logs WHERE user_id=$1", uid).Scan(&logged); err != nil || logged != 1 {
		t.Fatal("duplicate embedding billing", logged, err)
	}
	otherGroup := int64(manage("/api/v1/admin/groups", admin, map[string]any{"name": "No embedding", "platform": "grok"})["id"].(float64))
	otherKey := manage("/api/v1/keys", token, map[string]any{"name": "No embedding", "group_id": otherGroup})["key"].(string)
	body["input"] = "hello"
	if w := call("/embeddings", otherKey, body, ""); w.Code != 404 || count.Load() != 1 {
		t.Fatal("embedding platform restriction bypass", w.Code)
	}
	// Text protocol does not change the embedding endpoint or its Bearer auth.
	// Exercise both ordinary and endpoint-specific composite routing.
	body["input"] = []string{"one", "two"}
	for _, platform := range []string{"openai", "composite"} {
		group := manage("/api/v1/admin/groups", admin, map[string]any{"name": "Embedding Messages " + platform, "platform": platform,
			"model_pricing": []any{map[string]any{"platform": "openai", "models": []string{"client-embedding"}, "input_price": "0.0000001"}}})
		groupID := int64(group["id"].(float64))
		if platform == "composite" {
			w := call(fmt.Sprintf("/api/v1/admin/groups/%d/composite-routes", groupID), admin, map[string]any{
				"public_model": "client-embedding", "target_platform": "openai", "upstream_model": "client-embedding", "endpoint": "embeddings"}, "")
			if w.Code != 201 {
				t.Fatal("embedding composite route", w.Code, w.Body.String())
			}
		}
		account := manage("/api/v1/admin/accounts", admin, map[string]any{"name": "Embedding Messages " + platform, "platform": "openai", "type": "apikey", "group_ids": []int64{groupID},
			"credentials": map[string]any{"api_key": "embedding-upstream", "base_url": upstream.URL + "/v1", "api_protocol": "anthropic", "openai_capabilities": []string{"embeddings"}, "model_mapping": map[string]string{"client-embedding": "upstream-embedding"}}})
		accountID := int64(account["id"].(float64))
		key := manage("/api/v1/keys", token, map[string]any{"name": "Embedding Messages", "group_id": groupID})["key"].(string)
		before := count.Load()
		first := call("/v1/embeddings", key, body, "messages-embedding")
		second := call("/embeddings", key, body, "messages-embedding")
		if first.Code != 200 || second.Code != 200 || first.Body.String() != second.Body.String() || second.Header().Get("Idempotency-Replayed") != "true" || count.Load() != before+1 {
			t.Fatal("Messages account embedding", platform, first.Code, first.Body.String(), second.Code)
		}
		var gotAccount, gotGroup int64
		if err := a.DB.QueryRow("SELECT account_id,group_id,actual_cost::text,upstream_endpoint FROM usage_logs WHERE request_id=$1", first.Header().Get("X-Request-ID")).Scan(&gotAccount, &gotGroup, &cost, &endpoint); err != nil || gotAccount != accountID || gotGroup != groupID || cost != "0.0000012000" || endpoint != "/v1/embeddings" {
			t.Fatal("Messages embedding receipt", gotAccount, gotGroup, cost, endpoint, err)
		}
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"client-embedding"`) {
			t.Fatal("callable embedding absent from catalog", platform, w.Code, w.Body.String())
		}
		// Capability restrictions still govern an otherwise compatible account.
		if _, err := a.DB.Exec(`UPDATE accounts SET credentials=jsonb_set(credentials,'{openai_capabilities}','["chat_completions"]'),updated_at=clock_timestamp() WHERE id=$1`, accountID); err != nil {
			t.Fatal(err)
		}
		if w := call("/embeddings", key, body, ""); w.Code != 503 || count.Load() != before+1 {
			t.Fatal("embedding capability bypass", platform, w.Code, w.Body.String())
		}
	}
}
