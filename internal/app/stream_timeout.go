package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const streamTimeoutSetting = "stream_timeout_settings"

type streamTimeoutSettings struct {
	Enabled bool   `json:"enabled"`
	Action  string `json:"action"`
	Minutes int    `json:"temp_unsched_minutes"`
	Count   int    `json:"threshold_count"`
	Window  int    `json:"threshold_window_minutes"`
}

func (a *App) loadStreamTimeoutSettings(ctx context.Context) (streamTimeoutSettings, error) {
	var s streamTimeoutSettings
	ok, err := a.readRuntimeSetting(ctx, streamTimeoutSetting, &s)
	if !ok {
		return streamTimeoutSettings{false, "temp_unsched", 5, 3, 10}, err
	}
	s.Minutes = min(max(s.Minutes, 1), 60)
	s.Count = min(max(s.Count, 1), 10)
	s.Window = min(max(s.Window, 1), 60)
	if s.Action != "temp_unsched" && s.Action != "error" && s.Action != "none" {
		s.Action = "temp_unsched"
	}
	return s, nil
}

func (a *App) streamTimeoutConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadStreamTimeoutSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var s *streamTimeoutSettings
	if err := decode(w, r, &s); err != nil {
		return err
	}
	if s == nil || s.Minutes < 1 || s.Minutes > 60 || s.Count < 1 || s.Count > 10 || s.Window < 1 || s.Window > 60 {
		return bad("temp_unsched_minutes and threshold_window_minutes must be 1 to 60; threshold_count must be 1 to 10")
	}
	if s.Action != "temp_unsched" && s.Action != "error" && s.Action != "none" {
		return bad("action must be temp_unsched, error or none")
	}
	if err := a.writeRuntimeSetting(r.Context(), streamTimeoutSetting, s); err != nil {
		return err
	}
	return reply(w, s)
}

func parseStreamIntervals(cfg Config) (time.Duration, time.Duration, error) {
	parse := func(raw string, fallback, low, high int) (time.Duration, error) {
		n := fallback
		if raw != "" {
			var err error
			n, err = strconv.Atoi(raw)
			if err != nil || n != 0 && (n < low || n > high) {
				return 0, fmt.Errorf("stream data interval must be 0 or %d to %d seconds", low, high)
			}
		}
		return time.Duration(n) * time.Second, nil
	}
	text, err := parse(cfg.StreamDataIntervalTimeout, 180, 30, 300)
	if err != nil {
		return 0, 0, err
	}
	image, err := parse(cfg.ImageStreamDataIntervalTimeout, 900, 60, 1800)
	return text, image, err
}

var errStreamIdle = errors.New("upstream stream data interval timed out")

// Only time spent waiting for upstream bytes counts. Slow downstream writes do
// not penalize an account. Cancel the transport context, never read concurrently.
type idleStreamBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelFunc
	idle   time.Duration
}

func (b *idleStreamBody) Read(p []byte) (int, error) {
	if b.idle == 0 {
		return b.ReadCloser.Read(p)
	}
	done := make(chan struct{})
	expired := false
	timer := time.AfterFunc(b.idle, func() {
		if b.ctx.Err() == nil {
			expired = true
			b.cancel()
		}
		close(done)
	})
	n, err := b.ReadCloser.Read(p)
	if !timer.Stop() {
		<-done // Synchronize with the callback before reading its result.
		if expired {
			return n, errStreamIdle
		}
	}
	return n, err
}

func streamTimeoutCounter(u *upstreamAccount) string {
	// An administrator edit/recovery starts a fresh generation. An old in-flight
	// failure cannot add to the new generation or undo that administrator action.
	return fmt.Sprintf("gateway:stream-timeout:%d:%d", u.ID, u.UpdatedAt.UnixMicro())
}

func (a *App) markStreamTimeout(ctx context.Context, u *upstreamAccount, model string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	s, err := a.loadStreamTimeoutSettings(ctx)
	if err != nil || !s.Enabled || s.Action == "none" {
		return
	}
	key := streamTimeoutCounter(u)
	count, err := a.Redis.Eval(ctx, `local n=redis.call('INCR',KEYS[1]);if redis.call('TTL',KEYS[1])<0 then redis.call('EXPIRE',KEYS[1],ARGV[1]) end;return n`, []string{key}, s.Window*60).Int64()
	if err != nil {
		slog.Warn("stream timeout counter unavailable", "account_id", u.ID)
		count = 1 // A single proven timeout cannot stand in for repeated failures.
	}
	if count < int64(s.Count) {
		return
	}
	now := time.Now()
	until := now.Add(time.Duration(s.Minutes) * time.Minute)
	message := "Stream data interval timeout for model: " + model
	reason, _ := json.Marshal(map[string]any{
		"until_unix": until.Unix(), "triggered_at_unix": now.Unix(), "status_code": 0,
		"matched_keyword": "stream_timeout", "rule_index": -1, "error_message": message,
		"trigger_count": count, "trigger_threshold": s.Count, "trigger_window_minutes": s.Window,
	})
	clause := `temp_unschedulable_reason=CASE WHEN temp_unschedulable_until IS NULL OR temp_unschedulable_until<$3 THEN $4 ELSE temp_unschedulable_reason END,
 temp_unschedulable_until=GREATEST(temp_unschedulable_until,$3)`
	args := []any{u.ID, u.UpdatedAt, until, string(reason)}
	if s.Action == "error" {
		clause = "status='error',error_message=$3"
		args = []any{u.ID, u.UpdatedAt, "Stream data interval timeout (repeated failures) for model: " + model}
	}
	result, err := a.DB.ExecContext(ctx, "UPDATE accounts SET "+clause+",updated_at=now() WHERE id=$1 AND updated_at=$2 AND status='active' AND schedulable AND deleted_at IS NULL", args...)
	if err != nil {
		slog.Error("stream timeout state update failed", "account_id", u.ID)
		return
	}
	if n, _ := result.RowsAffected(); n != 0 {
		_ = a.Redis.Del(ctx, key).Err()
	}
}
