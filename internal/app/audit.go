package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type auditIdentityKey struct{}

// Public authentication handlers may identify an actor only after verifying
// credentials. An email or a session ID supplied by a caller is not identity.
func setAuditIdentity(r *http.Request, u *identity) {
	if actor, ok := r.Context().Value(auditIdentityKey{}).(*identity); ok {
		*actor = identity{ID: u.ID, Email: u.Email, Role: u.Role, AuthMethod: "jwt"}
	}
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *auditResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *auditResponseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func auditAction(pattern string, authenticated bool, status int) string {
	switch pattern {
	case "POST /api/v1/auth/login":
		return "auth.login"
	case "POST /api/v1/auth/refresh":
		if status >= 400 {
			return "auth.token.refresh"
		}
		return ""
	case "POST /api/v1/auth/logout":
		return "auth.logout.create"
	}
	if !authenticated {
		return ""
	}
	switch pattern {
	case "GET /api/v1/admin/settings/admin-api-key":
		return "admin.admin_api_key.read"
	case "GET /api/v1/admin/users/{id}/api-keys":
		return "admin.users.api_keys.read"
	case "GET /api/v1/admin/groups/{id}/api-keys":
		return "admin.groups.api_keys.read"
	case "POST /api/v1/admin/settings/admin-api-key/regenerate":
		return "admin.admin_api_key.regenerate"
	case "DELETE /api/v1/admin/settings/admin-api-key":
		return "admin.admin_api_key.delete"
	}
	method, path, _ := strings.Cut(pattern, " ")
	verb := map[string]string{"POST": "create", "PUT": "update", "PATCH": "update", "DELETE": "delete"}[method]
	if verb == "" {
		return ""
	}
	parts := []string{}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/") {
		if part != "" && !strings.HasPrefix(part, "{") {
			parts = append(parts, strings.ReplaceAll(part, "-", "_"))
		}
	}
	return strings.Join(append(parts, verb), ".")
}

func (a *App) recordAudit(r *http.Request, pattern string, actor *identity, status int, requestID string, started time.Time) {
	action := auditAction(pattern, actor.ID > 0, status)
	if action == "" {
		return
	}
	var uid any
	if actor.ID > 0 {
		uid = actor.ID
	}
	// Bodies, query strings and credentials are deliberately not captured.
	// Keep diagnostics bounded and valid for PostgreSQL text columns.
	clean := func(s string, n int) string { return truncate(strings.ReplaceAll(s, "\x00", ""), n) }
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	_, err := a.DB.ExecContext(ctx, `INSERT INTO audit_logs(actor_user_id,actor_email,actor_role,auth_method,action,method,path,request_id,client_ip,user_agent,status_code,latency_ms)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, uid, clean(actor.Email, 255), clean(actor.Role, 32), clean(actor.AuthMethod, 32), clean(action, 128), clean(r.Method, 16), clean(r.URL.Path, 512), requestID, clean(clientIP(r), 64), clean(r.UserAgent(), 512), status, time.Since(started).Milliseconds())
	if err != nil {
		slog.Error("audit record failed", "request_id", requestID)
	}
}

func (a *App) auditLogs(w http.ResponseWriter, r *http.Request) error {
	if r.PathValue("id") != "" {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT to_jsonb(l) FROM audit_logs l WHERE id=$1", id))
		if err != nil {
			return err
		}
		return reply(w, raw)
	}
	clauses, args := []string{"true"}, []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, strings.ReplaceAll(clause, "?", fmt.Sprintf("$%d", len(args))))
	}
	q := r.URL.Query()
	for _, filter := range []struct {
		key string
		max int
	}{
		{"actor_user_id", 20}, {"start_time", 64}, {"end_time", 64},
		{"actor_email", 255}, {"action", 128}, {"auth_method", 32},
		{"method", 16}, {"client_ip", 64}, {"q", 512}, {"success", 5},
	} {
		value := strings.TrimSpace(q.Get(filter.key))
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) > filter.max {
			return bad("invalid " + filter.key)
		}
		q.Set(filter.key, value)
	}
	if raw := q.Get("actor_user_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id < 1 {
			return bad("invalid actor_user_id")
		}
		add("actor_user_id=?", id)
	}
	var start, end time.Time
	for _, filter := range []struct{ key, clause string }{{"start_time", "created_at>=?"}, {"end_time", "created_at<=?"}} {
		if raw := q.Get(filter.key); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return bad("invalid " + filter.key)
			}
			if filter.key == "start_time" {
				start = at
			} else {
				end = at
			}
			add(filter.clause, at)
		}
	}
	if !start.IsZero() && !end.IsZero() && end.Before(start) {
		return bad("invalid audit time range")
	}
	for _, key := range []string{"actor_email", "action"} {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			add("strpos(lower("+key+"),lower(?))>0", value)
		}
	}
	for _, key := range []string{"auth_method", "method", "client_ip"} {
		if value := strings.TrimSpace(q.Get(key)); value != "" {
			if key == "method" {
				value = strings.ToUpper(value)
			}
			add(key+"=?", value)
		}
	}
	if value := strings.TrimSpace(q.Get("q")); value != "" {
		add("(strpos(lower(path),lower(?))>0 OR strpos(lower(action),lower(?))>0 OR strpos(lower(actor_email),lower(?))>0)", value)
	}
	if raw := q.Get("success"); raw != "" {
		if raw != "true" && raw != "false" {
			return bad("invalid success filter")
		}
		add("(status_code<400)=?", raw == "true")
	}
	where := " WHERE " + strings.Join(clauses, " AND ")
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(r.Context(), "SELECT count(*) FROM audit_logs"+where, args...).Scan(&count); err != nil {
		return err
	}
	page, size := pagination(r)
	size = min(size, 200)
	args = append(args, size, (page-1)*size)
	rows, err := tx.QueryContext(r.Context(), fmt.Sprintf("SELECT to_jsonb(l) || jsonb_build_object('request_body','') FROM audit_logs l%s ORDER BY created_at DESC,id DESC LIMIT $%d OFFSET $%d", where, len(args)-1, len(args)), args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, count, page, size)
}
