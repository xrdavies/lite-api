package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const overloadSetting = "overload_cooldown_settings"
const rate429Setting = "rate_limit_429_cooldown_settings"
const panelSetting = "panel_rate_limit_settings"

type overloadSettings struct {
	Enabled bool `json:"enabled"`
	Minutes int  `json:"cooldown_minutes"`
}
type rate429Settings struct {
	Enabled bool `json:"enabled"`
	Seconds int  `json:"cooldown_seconds"`
}
type panelRateSettings struct {
	Enabled     bool `json:"enabled"`
	UserRPM     int  `json:"user_rpm"`
	HeavyRPM    int  `json:"heavy_rpm"`
	ExemptAdmin bool `json:"exempt_admin"`
	PublicIPRPM int  `json:"public_ip_rpm"`
}

func defaultPanelRate() panelRateSettings {
	return panelRateSettings{Enabled: true, UserRPM: 240, HeavyRPM: 60, ExemptAdmin: true, PublicIPRPM: 300}
}

// Missing or malformed values use the established defaults. Database failures
// remain errors on management endpoints; runtime callers retain safe defaults.
func (a *App) readRuntimeSetting(ctx context.Context, key string, out any) (bool, error) {
	var raw string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(strings.TrimSpace(raw), "{") && json.Unmarshal([]byte(raw), out) == nil, nil
}
func (a *App) writeRuntimeSetting(ctx context.Context, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", key, string(raw))
	return err
}
func (a *App) loadOverloadSettings(ctx context.Context) (overloadSettings, error) {
	var s overloadSettings
	ok, err := a.readRuntimeSetting(ctx, overloadSetting, &s)
	if !ok {
		return overloadSettings{true, 10}, err
	}
	s.Minutes = min(max(s.Minutes, 1), 120)
	return s, nil
}
func (a *App) loadRate429Settings(ctx context.Context) (rate429Settings, error) {
	var s rate429Settings
	ok, err := a.readRuntimeSetting(ctx, rate429Setting, &s)
	if !ok {
		return rate429Settings{true, 5}, err
	}
	s.Seconds = min(max(s.Seconds, 1), 7200)
	return s, nil
}
func (a *App) loadPanelSettings(ctx context.Context) (panelRateSettings, error) {
	var s panelRateSettings
	ok, err := a.readRuntimeSetting(ctx, panelSetting, &s)
	if !ok {
		return defaultPanelRate(), err
	}
	s.UserRPM = min(max(s.UserRPM, 0), 100000)
	s.HeavyRPM = min(max(s.HeavyRPM, 0), 100000)
	s.PublicIPRPM = min(max(s.PublicIPRPM, 0), 100000)
	return s, nil
}

func (a *App) overloadConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadOverloadSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var in *overloadSettings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in == nil {
		return bad("settings must be an object")
	}
	if in.Minutes < 1 || in.Minutes > 120 {
		if in.Enabled {
			return bad("cooldown_minutes must be between 1 and 120")
		}
		in.Minutes = 10
	}
	if err := a.writeRuntimeSetting(r.Context(), overloadSetting, in); err != nil {
		return err
	}
	return reply(w, in)
}
func (a *App) rate429Config(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadRate429Settings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var in *rate429Settings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in == nil {
		return bad("settings must be an object")
	}
	if in.Seconds < 1 || in.Seconds > 7200 {
		if in.Enabled {
			return bad("cooldown_seconds must be between 1 and 7200")
		}
		in.Seconds = 5
	}
	if err := a.writeRuntimeSetting(r.Context(), rate429Setting, in); err != nil {
		return err
	}
	return reply(w, in)
}
func (a *App) panelConfig(w http.ResponseWriter, r *http.Request) error {
	if r.Method == "GET" {
		s, err := a.loadPanelSettings(r.Context())
		if err != nil {
			return err
		}
		return reply(w, s)
	}
	var in *panelRateSettings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in == nil {
		return bad("settings must be an object")
	}
	for _, n := range []int{in.UserRPM, in.HeavyRPM, in.PublicIPRPM} {
		if n < 0 || n > 100000 {
			return bad("rate limits must be between 0 and 100000")
		}
	}
	// Serialize the write with refresh: an older read cannot replace a new policy.
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	if err := a.writeRuntimeSetting(r.Context(), panelSetting, in); err != nil {
		return err
	}
	a.panelCache, a.panelExpires = in, time.Now().Add(time.Minute)
	return reply(w, in)
}
func (a *App) cachedPanelSettings(ctx context.Context) panelRateSettings {
	a.panelMu.Lock()
	defer a.panelMu.Unlock()
	if a.panelCache != nil && time.Now().Before(a.panelExpires) {
		return *a.panelCache
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	s, err := a.loadPanelSettings(ctx)
	ttl := time.Minute
	if err != nil {
		if a.panelCache != nil {
			s = *a.panelCache
		}
		ttl = 5 * time.Second
		slog.Warn("panel policy read failed; retaining last known policy")
	}
	a.panelCache, a.panelExpires = &s, time.Now().Add(ttl)
	return s
}

func (a *App) panelRateLimit(w http.ResponseWriter, r *http.Request, user *identity) error {
	public := r.URL.Path == "/api/v1/settings/public" || r.URL.Path == "/api/v1/model-plaza"
	if user == nil && !public {
		return nil // Login has its own stricter, fail-closed protection.
	}
	s := a.cachedPanelSettings(r.Context())
	if !s.Enabled || user != nil && user.Role == "admin" && s.ExemptAdmin {
		return nil
	}
	type bucket struct {
		key string
		rpm int
	}
	buckets := []bucket{}
	if user != nil {
		id := strconv.FormatInt(user.ID, 10)
		buckets = append(buckets, bucket{"global:user:" + id, s.UserRPM})
		if strings.HasPrefix(r.URL.Path, "/api/v1/usage") || strings.HasPrefix(r.URL.Path, "/api/v1/user/api-keys/") && strings.HasSuffix(r.URL.Path, "/usage/daily") {
			buckets = append(buckets, bucket{"heavy:user:" + id, s.HeavyRPM})
		}
	} else {
		ip, err := netip.ParseAddr(clientIP(r))
		if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
			return nil
		}
		buckets = append(buckets, bucket{"public:ip:" + ip.Unmap().String(), s.PublicIPRPM})
	}
	for _, b := range buckets {
		if b.rpm == 0 {
			continue
		}
		// Atomic fixed window from the first request; repair missing expiry without
		// extending a live window. This quota is separate from gateway RPM.
		ttl, err := a.Redis.Eval(r.Context(), `local n=redis.call('INCR',KEYS[1]);local ttl=redis.call('PTTL',KEYS[1]);if ttl<0 then redis.call('PEXPIRE',KEYS[1],60000);ttl=60000 end;if n>tonumber(ARGV[1]) then return math.max(ttl,1) end;return 0`, []string{"lite-api:panel:" + b.key}, b.rpm).Int64()
		if err != nil {
			slog.Warn("panel rate check unavailable; allowing request")
			continue // Best-effort panel protection; authentication still fails closed.
		}
		if ttl > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt((ttl+999)/1000, 10))
			return &apiError{429, "too many panel requests"}
		}
	}
	return nil
}

func retryAfterSeconds(raw string, now time.Time, limit int64) int64 {
	raw = strings.TrimSpace(raw)
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		return min(n, limit)
	}
	if at, err := http.ParseTime(raw); err == nil && at.After(now) {
		delay := min(at.Sub(now), time.Duration(limit)*time.Second)
		return int64((delay + time.Second - 1) / time.Second)
	}
	return 0
}

func (a *App) runtimeSettingsRoutes() {
	for path, h := range map[string]handler{
		"overload-cooldown": a.overloadConfig, "rate-limit-429-cooldown": a.rate429Config, "panel-rate-limit": a.panelConfig,
		"stream-timeout": a.streamTimeoutConfig,
		"beta-policy":    a.betaConfig, "rectifier": a.rectifierConfig,
	} {
		a.route("GET /api/v1/admin/settings/"+path, "admin", h)
		a.route("PUT /api/v1/admin/settings/"+path, "admin", h)
	}
}
