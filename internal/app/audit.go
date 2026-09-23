package app

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
	var count int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM audit_logs"+where, args...).Scan(&count); err != nil {
		return err
	}
	page, size := pagination(r)
	size = min(size, 200)
	args = append(args, size, (page-1)*size)
	rows, err := a.DB.QueryContext(r.Context(), fmt.Sprintf("SELECT to_jsonb(l) FROM audit_logs l%s ORDER BY id DESC LIMIT $%d OFFSET $%d", where, len(args)-1, len(args)), args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, count, page, size)
}
