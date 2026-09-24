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
			if field == "background" {
				in.Background = enabled
			}
			if field == "store" {
				in.Store = enabled
			}
		}
	}
	if in.Background {
		if in.Action != "" || socketTurn(r.Context()) != nil {
			return in, bad("background Responses require the HTTP create endpoint")
		}
		// Background retention is opt-in; the provider only keeps omitted/false
		// store responses for its short polling window.
		in.Store = string(body["store"]) == "true"
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
				if item == nil {
					return in, bad("supply full input items or a scoped previous_response_id")
				}
				kind := credentialString(item, "type")
				if responseLocalItem(kind) {
					if err := validateResponseLocalItem(item); err != nil {
						return in, err
					}
					in.NativeClientTools = true
				}
				if (kind == "tool_search_call" || kind == "tool_search_output") && credentialString(item, "execution") == "server" {
					in.HostedToolSearch = true
				}
				if kind == "image_generation_call" && item["id"] != nil {
					id := credentialString(item, "id")
					if !validResponseID(id) || len(in.ItemReferences) >= 1024 {
						return in, bad("invalid image generation item reference")
					}
					in.ItemReferences = append(in.ItemReferences, id)
				}
				if kind == "item_reference" || kind == "" && item["id"] != nil {
					id := credentialString(item, "id")
					if !validResponseID(id) {
						return in, bad("item_reference requires a valid item ID")
					}
					for field := range item {
						if field != "id" && field != "type" {
							return in, bad("item_reference accepts only id and type")
						}
					}
					if raw := item["type"]; raw != nil && string(raw) != "null" && kind != "item_reference" {
						return in, bad("invalid item_reference type")
					}
					in.ItemReferences = append(in.ItemReferences, id)
					if len(in.ItemReferences) > 1024 {
						return in, bad("too many item references")
					}
					item["type"] = json.RawMessage(`"item_reference"`)
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
			} else if len(in.ItemReferences) > 0 {
				body["input"], _ = json.Marshal(items)
			}
		}
	} else if in.Previous == "" && body["prompt"] == nil {
		return in, bad("input or previous_response_id is required")
	}
	// Hosted tools require their own meters and platform admission.
	tools, err := responseClientTools(body)
	if err != nil {
		return in, err
	}
	for _, tool := range tools {
		if responseLocalTool(credentialString(tool, "type")) {
			if err := validateResponseLocalTool(tool); err != nil {
				return in, err
			}
			in.NativeClientTools = true
			continue
		}
		if hostedToolSearch(tool) {
			if err := validateHostedToolSearch(tool); err != nil {
				return in, err
			}
			in.HostedToolSearch = true
			continue
		}
		if credentialString(tool, "type") == "image_generation" {
			if in.ResponseImage != nil {
				return in, bad("only one image_generation tool is supported")
			}
			in.ResponseImage, err = parseResponseImageTool(tool)
			if err != nil {
				return in, err
			}
			continue
		}
		if hostedSearchTool(credentialString(tool, "type")) {
			in.HostedSearch = true
			continue
		}
		if err := validateResponseClientTool(tool); err != nil {
			return in, err
		}
	}
	if (in.HostedSearch || in.HostedToolSearch || in.ResponseImage != nil) && (in.Action != "" || in.NativeCompaction) {
		return in, bad("hosted tools require a normal Responses request")
	}
	if in.ResponseImage != nil {
		var items []map[string]json.RawMessage
		_ = json.Unmarshal(body["input"], &items)
		for _, item := range items {
			var content []map[string]json.RawMessage
			_ = json.Unmarshal(item["content"], &content)
			for _, part := range content {
				if credentialString(part, "type") == "input_image" && part["file_id"] != nil && string(part["file_id"]) != "null" {
					return in, bad("image inputs require URLs or data URLs; unscoped file IDs are not supported")
				}
			}
		}
	}
	if in.Background && in.NativeCompaction {
		return in, bad("native compaction cannot run in the background")
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
		Usage, Response, Error, Output  json.RawMessage
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
	if o.stopped && event.Output != nil && string(event.Output) != "null" {
		var items []map[string]json.RawMessage
		if json.Unmarshal(event.Output, &items) != nil || len(items) > 1024 {
			return &apiError{502, "upstream response output is invalid or exceeds limit"}
		}
		o.ResponseItems = nil
		for _, item := range items {
			if id := credentialString(item, "id"); id != "" {
				if !validResponseID(id) {
					return &apiError{502, "upstream response item ID is invalid"}
				}
				o.ResponseItems = append(o.ResponseItems, id)
			}
		}
	}
	return nil
}

// Native responses bind only metadata; protocol conversions include encrypted history.
// Missing/expired bindings refuse continuation across tenants or upstream sources.
type responseBinding struct {
	AccountID int64
	Target    string
	ImageTool bool     `json:",omitempty"`
	History   string   `json:",omitempty"`
	Items     []string `json:",omitempty"`
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
	if err := a.responseNotDeleted(ctx, g, id); err != nil {
		return nil, err
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
func (a *App) bindResponse(ctx context.Context, g *gatewayIdentity, u *upstreamAccount, id string, items ...string) error {
	return a.storeResponseBinding(ctx, g, id, responseBinding{AccountID: u.ID, Target: responseTarget(u), Items: items})
}

func (a *App) storeResponseBinding(ctx context.Context, g *gatewayIdentity, id string, binding responseBinding) error {
	raw, _ := json.Marshal(binding)
	keys := []string{responseBindingKey(g, id)}
	for _, item := range binding.Items {
		keys = append(keys, responseItemKey(g, item))
	}
	source, _ := json.Marshal(responseBinding{AccountID: binding.AccountID, Target: binding.Target, ImageTool: binding.ImageTool})
	for _, key := range keys {
		keys = append(keys, key+":delete")
	}
	// Validate every collision before writing: a failed item binding must not
	// publish a response whose items resolve to a different upstream source.
	ok, err := a.Redis.Eval(ctx, `
local count=#KEYS/2
for i=1,count do
 local key=KEYS[i]
 if redis.call('EXISTS',KEYS[count+i])~=0 then return 0 end
 local value=ARGV[2];if i==1 then value=ARGV[1] end
 local old=redis.call('GET',key);if old and old~=value then return 0 end
end
for i=1,count do
 local key=KEYS[i]
 local value=ARGV[2];if i==1 then value=ARGV[1] end
 redis.call('SET',key,value,'EX',2592000)
end
return 1`, keys, string(raw), string(source)).Int()
	if err != nil || ok != 1 {
		return &apiError{503, "response affinity could not be saved"}
	}
	return nil
}

func responseItemKey(g *gatewayIdentity, id string) string {
	return fmt.Sprintf("gateway:response-item:%d:%d:%s", g.Key.ID, g.Key.GroupID, digest(id))
}

func (a *App) responseItemSource(ctx context.Context, g *gatewayIdentity, ids []string, binding *responseBinding) (*responseBinding, error) {
	if len(ids) == 0 {
		return binding, nil
	}
	deletedKeys := make([]string, 0, len(ids))
	for _, id := range ids {
		deletedKeys = append(deletedKeys, responseItemKey(g, id)+":delete")
	}
	deleted, err := a.Redis.MGet(ctx, deletedKeys...).Result()
	if err != nil {
		return nil, &apiError{503, "response item deletion state unavailable"}
	}
	for _, value := range deleted {
		if value != nil {
			return nil, missing()
		}
	}
	if binding != nil && binding.History != "" {
		return nil, bad("item_reference requires a native Responses account")
	}
	local := map[string]responseBinding{}
	if turn := socketTurn(ctx); turn != nil {
		for _, response := range turn.socket.responses {
			for _, item := range response.Items {
				local[item] = response
			}
		}
	}
	var keys []string
	for _, id := range ids {
		if _, ok := local[id]; !ok {
			keys = append(keys, responseItemKey(g, id))
		}
	}
	var stored []any
	if len(keys) > 0 {
		var err error
		stored, err = a.Redis.MGet(ctx, keys...).Result()
		if err != nil {
			return nil, &apiError{503, "response item affinity is unavailable"}
		}
	}
	for _, id := range ids {
		source, ok := local[id]
		if !ok {
			value := stored[0]
			stored = stored[1:]
			if value == nil {
				return nil, missing()
			}
			raw, ok := value.(string)
			if !ok || json.Unmarshal([]byte(raw), &source) != nil || source.AccountID <= 0 || source.Target == "" || source.History != "" {
				return nil, &apiError{503, "response item affinity is invalid"}
			}
		}
		if binding != nil && (binding.AccountID != source.AccountID || binding.Target != source.Target) {
			return nil, bad("response items must share the previous response upstream source")
		}
		imageTool := source.ImageTool || binding != nil && binding.ImageTool
		binding = &responseBinding{AccountID: source.AccountID, Target: source.Target, ImageTool: imageTool}
	}
	return binding, nil
}
