package app

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Only implemented product settings are writable. Deployment secrets are never returned.
func (a *App) settingsJSON(r *http.Request) (json.RawMessage, error) {
	return jsonRow(a.DB.QueryRowContext(r.Context(), `SELECT jsonb_build_object('site_name',COALESCE((SELECT value FROM settings WHERE key='site_name'),'lite-api'),'available_channels_enabled',EXISTS(SELECT 1 FROM settings WHERE key='available_channels_enabled' AND value='true'),'allow_user_view_error_requests',EXISTS(SELECT 1 FROM settings WHERE key='allow_user_view_error_requests' AND value='true'),'registration_enabled',false,'email_verify_enabled',false,'backend_mode_enabled',false,'model_plaza_enabled',EXISTS(SELECT 1 FROM settings WHERE key='model_plaza_enabled' AND value='true'),'model_plaza_require_auth',EXISTS(SELECT 1 FROM settings WHERE key='model_plaza_require_auth' AND value='true')) || CASE WHEN $1 THEN jsonb_build_object('model_plaza_description',COALESCE((SELECT value FROM settings WHERE key='model_plaza_description'),'')) ELSE '{}'::jsonb END`, strings.HasPrefix(r.URL.Path, "/api/v1/admin/")))
}
func (a *App) getSettings(w http.ResponseWriter, r *http.Request) error {
	raw, err := a.settingsJSON(r)
	if err != nil {
		return err
	}
	if strings.HasPrefix(r.URL.Path, "/api/v1/admin/") {
		policy, err := a.geminiQuotaOverride(r.Context())
		if err != nil {
			return err
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		fields[geminiQuotaSetting] = policy
		fast, err := a.loadFastPolicy(r.Context())
		if err != nil {
			return err
		}
		fields[fastPolicySetting], _ = json.Marshal(fast)
		extensions, err := a.loadResponseExtensions(r.Context())
		if err != nil {
			return err
		}
		fields[responseExtensionsSetting], _ = json.Marshal(extensions)
		raw, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	return reply(w, raw)
}
func (a *App) updateSettings(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name             *string             `json:"site_name"`
		Available        *bool               `json:"available_channels_enabled"`
		UserErrors       *bool               `json:"allow_user_view_error_requests"`
		PlazaEnabled     *bool               `json:"model_plaza_enabled"`
		PlazaRequireAuth *bool               `json:"model_plaza_require_auth"`
		PlazaDescription *string             `json:"model_plaza_description"`
		GeminiPolicy     json.RawMessage     `json:"gemini_quota_policy"`
		FastPolicy       *fastPolicySettings `json:"openai_fast_policy_settings"`
		ResponsePaths    *[]string           `json:"responses_extension_paths"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid site_name")
	}
	if in.PlazaDescription != nil && len(*in.PlazaDescription) > 10000 {
		return bad("model_plaza_description is too long")
	}
	if in.FastPolicy != nil {
		if err := in.FastPolicy.validate(); err != nil {
			return err
		}
	}
	if in.ResponsePaths != nil {
		if err := validateResponseExtensions(*in.ResponsePaths); err != nil {
			return err
		}
	}
	if in.GeminiPolicy != nil && string(in.GeminiPolicy) != "null" {
		if _, err := parseGeminiQuotaPolicy(in.GeminiPolicy, true); err != nil {
			return err
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{}
	if in.ResponsePaths != nil {
		raw, _ := json.Marshal(in.ResponsePaths)
		values[responseExtensionsSetting] = string(raw)
	}
	if in.FastPolicy != nil {
		raw, _ := json.Marshal(in.FastPolicy)
		values[fastPolicySetting] = string(raw)
	}
	if in.Name != nil {
		values["site_name"] = *in.Name
	}
	if in.Available != nil {
		values["available_channels_enabled"] = strconv.FormatBool(*in.Available)
	}
	if in.UserErrors != nil {
		values["allow_user_view_error_requests"] = strconv.FormatBool(*in.UserErrors)
	}
	if in.PlazaEnabled != nil {
		values["model_plaza_enabled"] = strconv.FormatBool(*in.PlazaEnabled)
	}
	if in.PlazaRequireAuth != nil {
		values["model_plaza_require_auth"] = strconv.FormatBool(*in.PlazaRequireAuth)
	}
	if in.PlazaDescription != nil {
		values["model_plaza_description"] = *in.PlazaDescription
	}
	if in.GeminiPolicy != nil && string(in.GeminiPolicy) != "null" {
		values[geminiQuotaSetting] = string(in.GeminiPolicy)
	}
	for _, key := range []string{"site_name", "available_channels_enabled", "allow_user_view_error_requests", "model_plaza_enabled", "model_plaza_require_auth", "model_plaza_description", geminiQuotaSetting, fastPolicySetting, responseExtensionsSetting} {
		value, ok := values[key]
		if !ok {
			continue
		}
		if _, err = tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()", key, value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.getSettings(w, r)
}
func (a *App) settingsRoutes() {
	a.route("GET /api/v1/settings/public", "public", a.getSettings)
	a.route("GET /api/v1/admin/settings", "admin", a.getSettings)
	a.route("PUT /api/v1/admin/settings", "admin", a.updateSettings)
	a.route("GET /api/v1/model-plaza", "public", a.modelPlaza)
}
