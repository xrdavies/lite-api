package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

type responseDeletion struct {
	Source responseBinding
	Done   bool
}

func responseDeletionKey(g *gatewayIdentity, id string) string {
	return responseBindingKey(g, id) + ":delete"
}

func (a *App) responseNotDeleted(ctx context.Context, g *gatewayIdentity, id string) error {
	n, err := a.Redis.Exists(ctx, responseDeletionKey(g, id)).Result()
	if err != nil {
		return &apiError{503, "response deletion state unavailable"}
	}
	if n != 0 {
		return missing()
	}
	return nil
}

// Persist intent before contacting the provider. Uncertain outcomes stay denied
// across restarts; a repeated DELETE reconciles the same source, never generation.
func (a *App) deleteResponseResource(w http.ResponseWriter, r *http.Request, g *gatewayIdentity, id string) error {
	ctx := r.Context()
	key := responseDeletionKey(g, id)
	if !a.takeSlot(key, 0, 1) {
		return conflict("response deletion is in progress")
	}
	defer a.releaseSlot(key, 0)
	if err := a.gatewayRPM(ctx, g); err != nil {
		return err
	}
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		return &apiError{429, "user concurrency limit reached"}
	}
	defer a.releaseSlot("user", g.UserID)
	var deletion responseDeletion
	raw, err := a.Redis.Get(ctx, key).Bytes()
	stored := err == nil
	if err != nil && !errors.Is(err, redis.Nil) {
		return &apiError{503, "response deletion state unavailable"}
	}
	if stored && (json.Unmarshal(raw, &deletion) != nil || deletion.Source.AccountID <= 0 || deletion.Source.Target == "" || deletion.Source.History != "") {
		return &apiError{503, "invalid response deletion state"}
	}
	result := map[string]any{"id": id, "object": "response", "deleted": true}
	if stored && deletion.Done {
		return rawReply(w, result)
	}
	taskID, err := a.Redis.Get(ctx, backgroundIndex(g, id)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return &apiError{503, "background storage unavailable"}
	}
	if taskID != "" {
		if !a.takeSlot("background:"+taskID, 0, 1) {
			return conflict("background reconciliation is in progress")
		}
		defer a.releaseSlot("background:"+taskID, 0)
	}
	if !stored {
		if taskID != "" {
			task, err := a.loadBackgroundResponse(ctx, taskID)
			if err != nil {
				return err
			}
			if task.UpstreamID != id || task.Identity.UserID != g.UserID || task.Identity.Key.ID != g.Key.ID || task.Identity.Key.GroupID != g.Key.GroupID {
				return missing()
			}
			if err = a.refreshBackgroundResponse(ctx, task); err != nil {
				return err
			}
			if task.Stage != "terminal" {
				return conflict("cancel or finish the background response before deleting it")
			}
			deletion.Source = responseBinding{AccountID: task.Selection.Account.ID, Target: task.Target, Items: task.Items, MCPTool: task.MCPTool}
		} else {
			binding, err := a.previousResponse(ctx, g, id)
			if err != nil {
				return err
			}
			if binding.History != "" {
				return bad("resource deletion requires a native Responses account")
			}
			deletion.Source = *binding
		}
	}
	if deletion.Source.MCPTool {
		ctx = context.WithValue(ctx, mcpRequestKey{}, true)
	}
	u, err := a.loadAccount(ctx, deletion.Source.AccountID)
	if err != nil {
		return err
	}
	if u.protocol() != "responses" || responseTarget(u) != deletion.Source.Target {
		return conflict("response upstream source changed; restore the original account")
	}
	release, err := a.acquireAccountSlot(ctx, u.ID)
	if err != nil {
		return err
	}
	defer release()
	keys := []string{key}
	for _, item := range deletion.Source.Items {
		keys = append(keys, responseItemKey(g, item)+":delete")
	}
	raw, _ = json.Marshal(deletion)
	if err := a.Redis.Eval(ctx, `
redis.call('SET',KEYS[1],ARGV[1])
for i=2,#KEYS do redis.call('SET',KEYS[i],'1') end
return 1`, keys, string(raw)).Err(); err != nil {
		return &apiError{503, "response deletion could not be saved"}
	}
	resp, err := a.upstreamRequest(ctx, u, "DELETE", "/v1/responses/"+id, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 404 {
		return a.upstreamError(ctx, u, resp.StatusCode, readUpstreamError(resp), &apiError{502, "response deletion is unconfirmed; retry DELETE"})
	}
	if resp.StatusCode == 200 {
		raw, err = io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
		var out struct {
			ID, Object string
			Deleted    bool
			Error      json.RawMessage
		}
		if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &out) != nil || out.ID != id || out.Object != "response" || !out.Deleted || out.Error != nil && string(out.Error) != "null" {
			return &apiError{502, "response deletion is unconfirmed; retry DELETE"}
		}
	}
	deletion.Done = true
	raw, _ = json.Marshal(deletion)
	// Remove the content and source bindings atomically with the acknowledgement.
	// Keep denial markers as long as other response/WS bindings may still exist.
	keys = append(keys, responseBindingKey(g, id), backgroundIndex(g, id), backgroundKey(taskID), backgroundPending)
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := a.Redis.Eval(persist, `
redis.call('SET',KEYS[1],ARGV[1],'EX',2592000)
for i=2,#KEYS-4 do redis.call('EXPIRE',KEYS[i],2592000) end
redis.call('DEL',KEYS[#KEYS-3],KEYS[#KEYS-2])
if ARGV[2]~='' then
 redis.call('DEL',KEYS[#KEYS-1]);redis.call('SREM',KEYS[#KEYS],ARGV[2])
end
return 1`, keys, string(raw), taskID).Err(); err != nil {
		return &apiError{503, "response deletion persistence requires retry"}
	}
	return rawReply(w, result)
}
