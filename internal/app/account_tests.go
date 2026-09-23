package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type accountTestResult struct {
	Status   string    `json:"status"`
	Text     string    `json:"response_text"`
	Error    string    `json:"error_message"`
	Latency  int64     `json:"latency_ms"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at"`
}

func (a *App) runAccountTest(ctx context.Context, u *upstreamAccount, model, prompt string) accountTestResult {
	result := accountTestResult{Status: "failed", Started: time.Now().UTC()}
	err := func() error {
		if model == "" || len(model) > 100 {
			return bad("model_id is required and must fit 100 characters")
		}
		mapped, err := u.mappedModel(model)
		if err != nil {
			return err
		}
		if prompt == "" {
			prompt = "Reply with OK."
		}
		var path string
		var body any
		switch u.protocol() {
		case "anthropic":
			path = "/v1/messages"
			body = map[string]any{"model": mapped, "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": prompt}}}
		case "gemini":
			path = "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(mapped, "models/")) + ":generateContent"
			body = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}}, "generationConfig": map[string]any{"maxOutputTokens": 64}}
		case "responses":
			path = "/v1/responses"
			body = map[string]any{"model": mapped, "input": prompt, "max_output_tokens": 64}
		default:
			path = "/v1/chat/completions"
			body = map[string]any{"model": mapped, "messages": []any{map[string]any{"role": "user", "content": prompt}}, "max_completion_tokens": 64}
		}
		raw, _ := json.Marshal(body)
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		response, err := a.upstreamRequest(ctx, u, http.MethodPost, path, raw)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		data, err := readUpstreamJSON(response)
		if err != nil {
			return err
		}
		switch u.protocol() {
		case "anthropic":
			var blocks []struct{ Type, Text string }
			if json.Unmarshal(data["content"], &blocks) != nil {
				return errors.New("missing Anthropic content")
			}
			for _, b := range blocks {
				if b.Type == "text" {
					result.Text += b.Text
				}
			}
		case "gemini":
			var candidates []struct {
				Content struct{ Parts []struct{ Text string } }
			}
			if json.Unmarshal(data["candidates"], &candidates) != nil || len(candidates) == 0 {
				return errors.New("missing Gemini candidates")
			}
			for _, p := range candidates[0].Content.Parts {
				result.Text += p.Text
			}
		case "responses":
			var output []struct{ Content []struct{ Type, Text string } }
			if json.Unmarshal(data["output"], &output) != nil {
				return errors.New("missing Responses output")
			}
			for _, o := range output {
				for _, p := range o.Content {
					if p.Type == "output_text" {
						result.Text += p.Text
					}
				}
			}
		default:
			var choices []struct{ Message struct{ Content string } }
			if json.Unmarshal(data["choices"], &choices) != nil || len(choices) == 0 {
				return errors.New("missing completion choices")
			}
			result.Text = choices[0].Message.Content
		}
		if strings.TrimSpace(result.Text) == "" {
			return errors.New("upstream returned no test text")
		}
		return nil
	}()
	if err == nil {
		result.Status = "success"
	} else {
		// Do not persist provider bodies, URLs or credentials in diagnostic failures.
		result.Error = "upstream test failed"
		var e *apiError
		if errors.As(err, &e) {
			result.Error = e.message
		}
		if errors.Is(err, context.DeadlineExceeded) {
			result.Error = "upstream test timed out"
		}
		if errors.Is(err, context.Canceled) {
			result.Error = "upstream test canceled"
		}
	}
	result.Text = strings.ReplaceAll(result.Text, credentialString(u.Credentials, "api_key"), "[REDACTED]")
	if len([]rune(result.Text)) > 4096 {
		result.Text = string([]rune(result.Text)[:4096])
	}
	result.Finished = time.Now().UTC()
	result.Latency = result.Finished.Sub(result.Started).Milliseconds()
	return result
}
func (a *App) recoverTestAccount(ctx context.Context, u *upstreamAccount) error {
	_, err := a.DB.ExecContext(ctx, recoverTestAccountSQL, u.ID, u.UpdatedAt)
	return err
}

const recoverTestAccountSQL = `UPDATE accounts SET status=CASE WHEN status='error' THEN 'active' ELSE status END,error_message=NULL,rate_limited_at=NULL,rate_limit_reset_at=NULL,overload_until=NULL,temp_unschedulable_until=NULL,temp_unschedulable_reason=NULL,extra=extra - 'model_rate_limits',updated_at=now() WHERE id=$1 AND updated_at=$2 AND deleted_at IS NULL AND status<>'inactive' AND schedulable AND (NOT auto_pause_on_expired OR expires_at IS NULL OR expires_at>now())`

func (a *App) testAccount(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Model  string `json:"model_id"`
		Prompt string `json:"prompt"`
		Mode   string `json:"mode"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if in.Model == "" || len(in.Model) > 100 {
		return bad("model_id is required")
	}
	if len(in.Prompt) > 4000 {
		return bad("test prompt is too long")
	}
	if in.Mode != "" && in.Mode != "text" {
		return bad("test mode is not supported by this endpoint yet")
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	send := func(event any) error {
		b, _ := json.Marshal(event)
		_, err := fmt.Fprintf(w, "data: %s\n\n", b)
		if err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	if err = send(map[string]any{"type": "test_start", "model": in.Model}); err != nil {
		return nil
	}
	result := a.runAccountTest(r.Context(), u, in.Model, in.Prompt)
	if result.Status == "success" {
		if err = a.recoverTestAccount(r.Context(), u); err != nil {
			_ = send(map[string]any{"type": "error", "error": "test succeeded but health state could not be saved"})
			return nil
		}
		if err = send(map[string]any{"type": "content", "text": result.Text}); err != nil {
			return nil
		}
		_ = send(map[string]any{"type": "test_complete", "success": true})
	} else {
		_ = send(map[string]any{"type": "error", "error": result.Error})
	}
	return nil
}
func (a *App) accountModels(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	data, err := a.fetchModels(ctx, u, true)
	if err != nil {
		return err
	}
	return reply(w, data)
}
