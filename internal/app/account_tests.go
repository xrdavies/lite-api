package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type accountTestInput struct {
	Model  string `json:"model_id"`
	Prompt string `json:"prompt"`
	Mode   string `json:"mode"`
	Image  string `json:"image_data_url"`
	Audio  string `json:"audio_data_url"`
}

type accountTestResult struct {
	Status   string           `json:"status"`
	Media    []map[string]any `json:"-"`
	Text     string           `json:"response_text"`
	Error    string           `json:"error_message"`
	Latency  int64            `json:"latency_ms"`
	Started  time.Time        `json:"started_at"`
	Finished time.Time        `json:"finished_at"`
}

func (a *App) runAccountTest(ctx context.Context, u *upstreamAccount, in accountTestInput) accountTestResult {
	result := accountTestResult{Status: "failed", Started: time.Now().UTC()}
	err := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mapped, mode, err := prepareAccountTest(u, in)
		if err != nil {
			return err
		}
		if !a.takeSlot("account-test", u.ID, 1) {
			return conflict("an account test is already running")
		}
		defer a.releaseSlot("account-test", u.ID)
		timeout := 45 * time.Second
		if mode != "text" {
			timeout = 90 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if a.DB != nil {
			release, err := a.acquireAccountSlot(ctx, u.ID)
			if err != nil {
				return err
			}
			defer release()
		}
		if mode == "search" || voiceProtocol(mode) {
			return a.testGrokAccount(ctx, u, mapped, mode, in, &result)
		}
		if mode != "text" {
			return a.testAccountMedia(ctx, u, mapped, mode, in, &result)
		}
		prompt := in.Prompt
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
	if key := credentialString(u.Credentials, "api_key"); key != "" {
		result.Text = strings.ReplaceAll(result.Text, key, "[REDACTED]")
		for _, event := range result.Media {
			raw, _ := json.Marshal(event)
			if strings.Contains(string(raw), key) {
				result.Media = nil
				result.Status, result.Error = "failed", "upstream test returned sensitive content"
				break
			}
		}
	}
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
	var in accountTestInput
	r.Body = http.MaxBytesReader(w, r.Body, 12<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&in); err != nil {
		return bad("invalid account test JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return bad("exactly one JSON object is required")
	}
	u, err := a.loadAccount(r.Context(), id)
	if err != nil {
		return err
	}
	mapped, _, err := prepareAccountTest(u, in)
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
	if err = send(map[string]any{"type": "test_start", "model": mapped}); err != nil {
		return nil
	}
	result := a.runAccountTest(r.Context(), u, in)
	if result.Status == "success" {
		if err = a.recoverTestAccount(r.Context(), u); err != nil {
			_ = send(map[string]any{"type": "error", "error": "test succeeded but health state could not be saved"})
			return nil
		}
		if err = send(map[string]any{"type": "content", "text": result.Text}); err != nil {
			return nil
		}
		for _, event := range result.Media {
			if err = send(event); err != nil {
				return nil
			}
		}
		_ = send(map[string]any{"type": "test_complete", "success": true})
	} else {
		if result.Text != "" {
			if err = send(map[string]any{"type": "content", "text": result.Text}); err != nil {
				return nil
			}
		}
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
