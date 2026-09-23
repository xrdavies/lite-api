package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/redis/go-redis/v9"
)

func parseResponsesRequest(r *http.Request, in textRequest, body map[string]json.RawMessage) (textRequest, error) {
	in.Scope, in.Store = "responses", true
	if socketTurn(r.Context()) == nil {
		for _, field := range []string{"stream_id", "generate"} {
			if body[field] != nil {
				return in, bad(field + " requires a WebSocket connection")
			}
		}
	}
	in.Action = r.PathValue("action")
	switch in.Action {
	case "":
	case "compact", "input_tokens":
		in.Scope += "." + in.Action
		in.CountOnly = in.Action == "input_tokens"
		in.Action = "/" + in.Action
		if in.Stream {
			return in, bad("this Responses operation does not stream")
		}
	default:
		return in, missing()
	}
	for _, field := range []string{"background", "store"} {
		if raw := body[field]; raw != nil && string(raw) != "null" {
			var enabled bool
			if json.Unmarshal(raw, &enabled) != nil {
				return in, bad("invalid " + field)
			}
			if field == "background" && enabled {
				return in, bad("background Responses require persistent task support")
			}
			if field == "store" {
				in.Store = enabled
			}
		}
	}
	if raw := body["conversation"]; raw != nil && string(raw) != "null" {
		return in, bad("use previous_response_id for scoped conversations")
	}
	if raw := body["previous_response_id"]; raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &in.Previous) != nil || !validResponseID(in.Previous) {
			return in, bad("invalid previous_response_id")
		}
	}
	if raw := body["reasoning"]; raw != nil && string(raw) != "null" {
		var reasoning struct{ Effort string }
		if json.Unmarshal(raw, &reasoning) != nil || len(reasoning.Effort) > 20 {
			return in, bad("invalid reasoning configuration")
		}
		in.Effort = reasoning.Effort
	}
	if raw := body["input"]; raw != nil && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			var items []map[string]json.RawMessage
			if json.Unmarshal(raw, &items) != nil {
				return in, bad("Responses input must be text or an array of objects")
			}
			for _, item := range items {
				if item == nil || credentialString(item, "type") == "item_reference" {
					return in, bad("supply full input items or a scoped previous_response_id")
				}
				in.NativeCompaction = in.NativeCompaction || credentialString(item, "type") == "compaction_trigger"
			}
			if in.NativeCompaction {
				if !in.Stream || in.Action != "" {
					return in, bad("native compaction requires streaming Responses")
				}
				// The wire protocol requires one trigger as the final input item.
				normalized := items[:0]
				for _, item := range items {
					if credentialString(item, "type") != "compaction_trigger" {
						normalized = append(normalized, item)
					}
				}
				normalized = append(normalized, map[string]json.RawMessage{"type": json.RawMessage(`"compaction_trigger"`)})
				body["input"], _ = json.Marshal(normalized)
				in.Headers.Set("X-Codex-Beta-Features", "remote_compaction_v2")
			}
		}
	} else if in.Previous == "" && body["prompt"] == nil {
		return in, bad("input or previous_response_id is required")
	}
	// Hosted tools have separate charges and task lifecycles; accepting them before
	// those meters exist would silently bill only their surrounding text tokens.
	tools, err := responseClientTools(body)
	if err != nil {
		return in, err
	}
	for _, tool := range tools {
		if err := validateResponseClientTool(tool); err != nil {
			return in, err
		}
	}
	return in, nil
}

func responseNamespaceChildren(tool map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	if name := credentialString(tool, "name"); name == "" || len(name) > 256 {
		return nil, bad("invalid tool namespace")
	}
	raw := tool["tools"]
	if raw != nil && tool["children"] != nil {
		return nil, bad("use tools or children, not both")
	}
	if raw == nil {
		raw = tool["children"]
	}
	var children []map[string]json.RawMessage
	if json.Unmarshal(raw, &children) != nil || len(children) == 0 {
		return nil, bad("namespace requires function tools")
	}
	for _, child := range children {
		if credentialString(child, "type") != "function" || credentialString(child, "name") == "" || len(credentialString(child, "name")) > 256 {
			return nil, bad("namespace supports only named client function tools")
		}
	}
	return children, nil
}

// Additional tools are subject to the same admission and billing rules as tools.
func responseClientTools(body map[string]json.RawMessage) ([]map[string]json.RawMessage, error) {
	var tools []map[string]json.RawMessage
	if raw := body["tools"]; raw != nil && json.Unmarshal(raw, &tools) != nil {
		return nil, bad("invalid tools")
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(body["input"], &items) == nil {
		for _, item := range items {
			if credentialString(item, "type") != "additional_tools" {
				continue
			}
			var extra []map[string]json.RawMessage
			if json.Unmarshal(item["tools"], &extra) != nil {
				return nil, bad("invalid additional tools")
			}
			tools = append(tools, extra...)
		}
	}
	return mergeResponseDiscoveries(tools, items)
}

func validResponseID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func (o *textObservation) observeResponses(data []byte) error {
	var event struct {
		Type, Object, ID, Model, Status string
		Tier                            string `json:"service_tier"`
		Usage, Response, Error          json.RawMessage
		Input                           *int64 `json:"input_tokens"`
	}
	if json.Unmarshal(data, &event) != nil || string(data) == "null" {
		return &apiError{502, "upstream returned invalid Responses JSON"}
	}
	if o.CountOnly {
		if event.Input == nil || !tokenCountsValid(*event.Input) || event.Error != nil && string(event.Error) != "null" {
			return &apiError{502, "upstream token count is invalid"}
		}
		return nil
	}
	if strings.HasPrefix(event.Type, "response.") {
		if event.Response != nil && string(event.Response) != "null" {
			var nested struct{ Type, Status string }
			if json.Unmarshal(event.Response, &nested) != nil || nested.Type != "" {
				return &apiError{502, "upstream response envelope is invalid"}
			}
			if err := o.observeResponses(event.Response); err != nil {
				return err
			}
			if (event.Type == "response.completed" || event.Type == "response.incomplete") && event.Type != "response."+nested.Status {
				return &apiError{502, "upstream terminal response status does not match"}
			}
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			if event.Response == nil || !o.stopped {
				return &apiError{502, "upstream terminal response is missing"}
			}
		case "response.failed":
			return &apiError{502, "upstream response failed"}
		default:
			o.stopped = false
		}
		return nil
	}
	if event.Usage != nil && string(event.Usage) != "null" {
		var values map[string]json.RawMessage
		if json.Unmarshal(event.Usage, &values) != nil || values == nil {
			return &apiError{502, "upstream usage is invalid"}
		}
		values["prompt_tokens"] = values["input_tokens"]
		values["completion_tokens"] = values["output_tokens"]
		values["prompt_tokens_details"] = values["input_tokens_details"]
		values["completion_tokens_details"] = values["output_tokens_details"]
		raw, _ := json.Marshal(values)
		u, err := parseChatUsage(raw)
		if err != nil {
			return err
		}
		o.Usage, o.HasUsage = u, true
	}
	if event.Model != "" {
		o.Model = event.Model
	}
	if event.Tier != "" {
		o.Tier = event.Tier
	}
	// Observe usage before failures so consumed tokens still reach settlement.
	if event.Type == "error" || event.Error != nil && string(event.Error) != "null" || event.Status == "failed" || event.Status == "cancelled" {
		return &apiError{502, "upstream response failed"}
	}
	if event.Object != "response" && !(o.Action == "/compact" && event.Object == "response.compaction") {
		return &apiError{502, "upstream returned an invalid response object"}
	}
	if !validResponseID(event.ID) || o.ResponseID != "" && o.ResponseID != event.ID {
		return &apiError{502, "upstream response ID is invalid or changed"}
	}
	o.ResponseID = event.ID
	o.stopped = event.Status == "completed" || event.Status == "incomplete" || event.Object == "response.compaction"
	return nil
}

// Native responses bind only metadata; protocol conversions include encrypted history.
// Missing/expired bindings refuse continuation across tenants or upstream sources.
type responseBinding struct {
	AccountID int64
	Target    string
	History   string `json:",omitempty"`
}

func responseBindingKey(g *gatewayIdentity, id string) string {
	return fmt.Sprintf("gateway:response:%d:%d:%s", g.Key.ID, g.Key.GroupID, digest(id))
}
func responseTarget(u *upstreamAccount) string {
	base, _ := u.baseURL()
	return digest(u.Platform + "\n" + u.protocol() + "\n" + base + "\n" + credentialString(u.Credentials, "api_key"))
}
func (a *App) previousResponse(ctx context.Context, g *gatewayIdentity, id string) (*responseBinding, error) {
	if id == "" {
		return nil, nil
	}
	if turn := socketTurn(ctx); turn != nil {
		if binding, ok := turn.socket.responses[id]; ok {
			return &binding, nil
		}
	}
	raw, err := a.Redis.Get(ctx, responseBindingKey(g, id)).Bytes()
	if err == redis.Nil {
		return nil, missing()
	}
	var binding responseBinding
	if err != nil || json.Unmarshal(raw, &binding) != nil || binding.AccountID <= 0 || binding.Target == "" {
		return nil, &apiError{503, "response affinity is unavailable"}
	}
	return &binding, nil
}
func (a *App) bindResponse(ctx context.Context, g *gatewayIdentity, u *upstreamAccount, id string) error {
	return a.storeResponseBinding(ctx, g, id, responseBinding{AccountID: u.ID, Target: responseTarget(u)})
}

func (a *App) storeResponseBinding(ctx context.Context, g *gatewayIdentity, id string, binding responseBinding) error {
	raw, _ := json.Marshal(binding)
	key := responseBindingKey(g, id)
	// Atomic collision checking also protects against an upstream reusing IDs.
	ok, err := a.Redis.Eval(ctx, `local old=redis.call('GET',KEYS[1]);if old and old~=ARGV[1] then return 0 end;redis.call('SET',KEYS[1],ARGV[1],'EX',2592000);return 1`, []string{key}, string(raw)).Int()
	if err != nil || ok != 1 {
		return &apiError{503, "response affinity could not be saved"}
	}
	return nil
}
