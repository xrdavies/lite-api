package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

type errorRule struct {
	Name            string   `json:"name"`
	Enabled         bool     `json:"enabled"`
	Priority        int      `json:"priority"`
	Codes           []int    `json:"error_codes"`
	Keywords        []string `json:"keywords"`
	Mode            string   `json:"match_mode"`
	Platforms       []string `json:"platforms"`
	PassthroughCode bool     `json:"passthrough_code"`
	ResponseCode    *int     `json:"response_code"`
	PassthroughBody bool     `json:"passthrough_body"`
	Message         *string  `json:"custom_message"`
	SkipMonitoring  bool     `json:"skip_monitoring"`
	Description     *string  `json:"description"`
}

const errorRuleColumns = "name,enabled,priority,error_codes,keywords,match_mode,platforms,passthrough_code,response_code,passthrough_body,custom_message,skip_monitoring,description"

func (rule *errorRule) validate() error {
	if strings.TrimSpace(rule.Name) == "" || strings.ContainsRune(rule.Name, 0) || len([]rune(rule.Name)) > 100 || rule.Priority < -2147483648 || rule.Priority > 2147483647 || rule.Mode != "any" && rule.Mode != "all" {
		return bad("invalid error rule name, priority or match_mode")
	}
	if len(rule.Codes)+len(rule.Keywords) == 0 || len(rule.Codes) > 200 || len(rule.Keywords) > 100 || len(rule.Platforms) > 8 {
		return bad("error rule requires bounded status or keyword conditions")
	}
	for _, status := range rule.Codes {
		if status < 400 || status > 599 {
			return bad("error_codes must be HTTP errors (400 to 599)")
		}
	}
	for _, word := range rule.Keywords {
		if strings.TrimSpace(word) == "" || len(word) > 512 || strings.ContainsRune(word, 0) {
			return bad("invalid error rule keyword")
		}
	}
	for i, p := range rule.Platforms {
		p = strings.ToLower(strings.TrimSpace(p))
		if !supportedPlatform(p) {
			return bad("unsupported error rule platform")
		}
		rule.Platforms[i] = p
	}
	if rule.ResponseCode != nil && (*rule.ResponseCode < 400 || *rule.ResponseCode > 599) || !rule.PassthroughCode && rule.ResponseCode == nil {
		return bad("response_code must be an HTTP error when overriding status")
	}
	if rule.Message != nil && (len(*rule.Message) > 4096 || strings.ContainsRune(*rule.Message, 0)) || !rule.PassthroughBody && (rule.Message == nil || strings.TrimSpace(*rule.Message) == "") {
		return bad("custom_message is required when overriding the error message (maximum 4096 bytes)")
	}
	if rule.Description != nil && (len(*rule.Description) > 10000 || strings.ContainsRune(*rule.Description, 0)) {
		return bad("invalid error rule description")
	}
	if rule.Codes == nil {
		rule.Codes = []int{}
	}
	if rule.Keywords == nil {
		rule.Keywords = []string{}
	}
	if rule.Platforms == nil {
		rule.Platforms = []string{}
	}
	return nil
}

func (a *App) errorRules(w http.ResponseWriter, r *http.Request) error {
	var id int64
	var err error
	if r.PathValue("id") != "" {
		if id, err = pathID(r); err != nil {
			return err
		}
	}
	if r.Method == "GET" {
		if id != 0 {
			raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(e) FROM error_passthrough_rules e WHERE id=$1", id))
			if err != nil {
				return err
			}
			return reply(w, raw)
		}
		rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(e) FROM error_passthrough_rules e ORDER BY priority,id")
		if err != nil {
			return err
		}
		raw, err := jsonRows(rows)
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	if r.Method == "DELETE" {
		result, err := a.DB.ExecContext(r.Context(), "DELETE FROM error_passthrough_rules WHERE id=$1", id)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			return missing()
		}
		return reply(w, map[string]string{"message": "Rule deleted successfully"})
	}
	var patch map[string]json.RawMessage
	if err = decode(w, r, &patch); err != nil {
		return err
	}
	if patch == nil {
		return bad("error rule must be a JSON object")
	}
	for field, value := range patch {
		if !slices.Contains(strings.Split(errorRuleColumns, ","), field) {
			return bad("unsupported error rule field")
		}
		// Match the established partial-update contract: null/omitted retain
		// values; empty arrays explicitly clear condition/platform lists.
		if string(value) == "null" {
			delete(patch, field)
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rule := errorRule{Enabled: true, Mode: "any", PassthroughCode: true, PassthroughBody: true}
	if id != 0 {
		raw, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(e) FROM error_passthrough_rules e WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &rule); err != nil {
			return err
		}
	} else {
		if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720037)"); err != nil {
			return err
		}
		var count int
		if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM error_passthrough_rules").Scan(&count); err != nil {
			return err
		}
		if count >= 500 {
			return bad("maximum 500 error rules")
		}
	}
	raw, _ := json.Marshal(patch)
	if json.Unmarshal(raw, &rule) != nil {
		return bad("invalid error rule fields")
	}
	if err = rule.validate(); err != nil {
		return err
	}
	raw, _ = json.Marshal(rule)
	if id == 0 {
		err = tx.QueryRowContext(r.Context(), "INSERT INTO error_passthrough_rules("+errorRuleColumns+") SELECT "+errorRuleColumns+" FROM jsonb_populate_record(NULL::error_passthrough_rules,$1::jsonb) RETURNING id", string(raw)).Scan(&id)
	} else {
		_, err = tx.ExecContext(r.Context(), "UPDATE error_passthrough_rules SET ("+errorRuleColumns+")=(SELECT "+errorRuleColumns+" FROM jsonb_populate_record(NULL::error_passthrough_rules,$2::jsonb)),updated_at=clock_timestamp() WHERE id=$1", id, string(raw))
	}
	if err != nil {
		return err
	}
	result, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(e) FROM error_passthrough_rules e WHERE id=$1", id))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, result)
}

func (rule errorRule) matches(platform string, status int, body []byte) bool {
	if !rule.Enabled || len(rule.Platforms) > 0 && !slices.Contains(rule.Platforms, strings.ToLower(platform)) {
		return false
	}
	code := slices.Contains(rule.Codes, status)
	text := strings.ToLower(string(body[:min(len(body), 8<<10)]))
	keyword := false
	for _, word := range rule.Keywords {
		keyword = keyword || strings.Contains(text, strings.ToLower(word))
	}
	if rule.Mode == "all" {
		return len(rule.Codes)+len(rule.Keywords) > 0 && (len(rule.Codes) == 0 || code) && (len(rule.Keywords) == 0 || keyword)
	}
	return code || keyword
}

type passthroughError struct {
	*apiError
	UpstreamStatus int
	SkipMonitoring bool
}

var errNoUpstream = &apiError{503, "no available upstream account"}

func (e *passthroughError) Unwrap() error { return e.apiError }

func skipErrorMonitoring(err error) bool {
	var e *passthroughError
	return errors.As(err, &e) && e.SkipMonitoring
}

func gatewayErrorType(err error) string {
	var e *passthroughError
	if errors.As(err, &e) {
		return "upstream_error"
	}
	return "gateway_error"
}

func readUpstreamError(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	// Bound both memory and time spent on a rejected response. Partial or
	// oversized JSON may match a status rule, but cannot expose a body message.
	timer := time.AfterFunc(3*time.Second, func() { _ = resp.Body.Close() })
	defer timer.Stop()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBalanceBody+1))
	if err != nil {
		return nil
	}
	return body
}

var errorSecretPattern = regexp.MustCompile(`(?i)(?:bearer\s+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|password|secret|key|token)["']?\s*[:=]\s*["']?)[^\s"'&,;<>]+|sk-[a-z0-9_-]{8,}`)

func upstreamErrorMessage(body []byte, key string) string {
	if len(body) > maxBalanceBody {
		return ""
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil {
		return ""
	}
	var nested map[string]json.RawMessage
	_ = json.Unmarshal(object["error"], &nested)
	message := credentialString(nested, "message")
	if strings.HasPrefix(strings.TrimSpace(message), "{") {
		var inner struct{ Error struct{ Message string } }
		if json.Unmarshal([]byte(message), &inner) == nil && inner.Error.Message != "" {
			message = inner.Error.Message
		}
	}
	if strings.TrimSpace(message) == "" {
		message = credentialString(object, "detail")
	}
	if strings.TrimSpace(message) == "" {
		message = credentialString(object, "message")
	}
	if key != "" {
		for _, value := range []string{key, url.QueryEscape(key), url.PathEscape(key)} {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	message = errorSecretPattern.ReplaceAllString(message, "[redacted]")
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	return truncate(message, 4096)
}

// Only actual upstream HTTP rejections are eligible. Rules never change retry,
// quota, settlement, or task-reconciliation decisions based on the original status.
func (a *App) upstreamError(ctx context.Context, u *upstreamAccount, status int, body []byte, fallback error) error {
	if a.DB == nil || u == nil || status < 400 || status > 599 {
		return fallback
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := a.DB.QueryContext(ctx, "SELECT to_jsonb(e) FROM error_passthrough_rules e WHERE enabled ORDER BY priority,id LIMIT 501")
	if err != nil {
		return fallback
	}
	raw, err := jsonRows(rows)
	if err != nil || len(raw) > 500 {
		return fallback
	}
	for _, data := range raw {
		var rule errorRule
		if json.Unmarshal(data, &rule) != nil || rule.validate() != nil || !rule.matches(u.Platform, status, body) {
			continue
		}
		code := status
		if !rule.PassthroughCode {
			code = *rule.ResponseCode
		}
		message := upstreamErrorMessage(body, credentialString(u.Credentials, "api_key"))
		if ctx.Value(mcpRequestKey{}) == true {
			// Provider messages may echo arbitrary client-supplied MCP headers.
			message = "upstream MCP request rejected"
		}
		if !rule.PassthroughBody {
			message = *rule.Message
		}
		if strings.TrimSpace(message) == "" {
			message = "upstream request rejected"
		}
		return &passthroughError{&apiError{code, message}, status, rule.SkipMonitoring}
	}
	return fallback
}

func (a *App) errorRuleRoutes() {
	for _, path := range []string{"", "/{id}"} {
		a.route("GET /api/v1/admin/error-passthrough-rules"+path, "admin", a.errorRules)
	}
	a.route("POST /api/v1/admin/error-passthrough-rules", "admin", a.errorRules)
	a.route("PUT /api/v1/admin/error-passthrough-rules/{id}", "admin", a.errorRules)
	a.route("DELETE /api/v1/admin/error-passthrough-rules/{id}", "admin", a.errorRules)
}
