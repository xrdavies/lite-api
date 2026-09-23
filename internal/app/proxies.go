package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type nullableInt struct {
	Set   bool
	Value *int64
}

func (n *nullableInt) UnmarshalJSON(b []byte) error { n.Set = true; return json.Unmarshal(b, &n.Value) }

type proxyInput struct {
	Name     *string     `json:"name"`
	Protocol *string     `json:"protocol"`
	Host     *string     `json:"host"`
	Port     *int        `json:"port"`
	Username *string     `json:"username"`
	Password *string     `json:"password"`
	Status   *string     `json:"status"`
	Expires  nullableInt `json:"expires_at"`
	Fallback *string     `json:"fallback_mode"`
	Backup   nullableInt `json:"backup_proxy_id"`
	WarnDays *int        `json:"expiry_warn_days"`
}

func (in *proxyInput) validate(create bool) error {
	if create && (in.Name == nil || in.Protocol == nil || in.Host == nil || in.Port == nil) {
		return bad("name, protocol, host and port are required")
	}
	if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len([]rune(*in.Name)) > 100) {
		return bad("invalid proxy name")
	}
	if in.Protocol != nil {
		switch *in.Protocol {
		case "http", "https", "socks5", "socks5h":
		default:
			return bad("invalid proxy protocol")
		}
	}
	if in.Host != nil {
		if len(*in.Host) > 255 || strings.ContainsAny(*in.Host, "/@?#\\\r\n ") || *in.Host == "" {
			return bad("invalid proxy host")
		}
		if strings.Contains(*in.Host, ":") && net.ParseIP(*in.Host) == nil {
			return bad("invalid proxy host")
		}
	}
	if in.Port != nil && (*in.Port < 1 || *in.Port > 65535) {
		return bad("invalid proxy port")
	}
	for _, v := range []*string{in.Username, in.Password} {
		if v != nil && (len([]rune(*v)) > 100 || strings.ContainsAny(*v, "\r\n")) {
			return bad("invalid proxy credentials")
		}
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "inactive" {
		return bad("invalid proxy status")
	}
	if in.Fallback != nil && *in.Fallback != "none" && *in.Fallback != "direct" && *in.Fallback != "proxy" {
		return bad("invalid fallback_mode")
	}
	if in.Expires.Value != nil && (*in.Expires.Value < 0 || *in.Expires.Value > 253402300799) {
		return bad("invalid expires_at")
	}
	if in.Backup.Value != nil && *in.Backup.Value <= 0 {
		return bad("invalid backup_proxy_id")
	}
	if in.WarnDays != nil && (*in.WarnDays < 0 || *in.WarnDays > 3650) {
		return bad("invalid expiry_warn_days")
	}
	return nil
}
func (a *App) saveProxy(w http.ResponseWriter, r *http.Request) error {
	create := r.Method == "POST"
	var id int64
	var err error
	if !create {
		id, err = pathID(r)
		if err != nil {
			return err
		}
	}
	var in proxyInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(create); err != nil {
		return err
	}
	if in.Host != nil {
		if _, err = a.resolveUpstream(r.Context(), *in.Host); err != nil {
			return err
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize graph edits so two administrators cannot create a fallback cycle.
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720034)"); err != nil {
		return err
	}
	fallback := "none"
	var backup *int64
	if !create {
		if err = tx.QueryRowContext(r.Context(), "SELECT fallback_mode,backup_proxy_id FROM proxies WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&fallback, &backup); err != nil {
			return err
		}
	}
	if in.Fallback != nil {
		fallback = *in.Fallback
	}
	if in.Backup.Set {
		backup = in.Backup.Value
	}
	if fallback == "proxy" && backup == nil {
		return bad("proxy fallback requires backup_proxy_id")
	}
	seen := map[int64]bool{id: true}
	next := backup
	for next != nil {
		if seen[*next] || len(seen) > 8 {
			return bad("proxy fallback cycle or excessive chain")
		}
		seen[*next] = true
		var value *int64
		var mode string
		if err = tx.QueryRowContext(r.Context(), "SELECT backup_proxy_id,fallback_mode FROM proxies WHERE id=$1 AND deleted_at IS NULL", *next).Scan(&value, &mode); err != nil {
			return bad("backup proxy does not exist")
		}
		next = value
		if mode != "proxy" {
			break
		}
	}
	values := map[string]any{}
	for key, p := range map[string]*string{"name": in.Name, "protocol": in.Protocol, "host": in.Host, "username": in.Username, "password": in.Password, "status": in.Status, "fallback_mode": in.Fallback} {
		if p != nil {
			values[key] = *p
		}
	}
	if in.Port != nil {
		values["port"] = *in.Port
	}
	if in.WarnDays != nil {
		values["expiry_warn_days"] = *in.WarnDays
	}
	if in.Backup.Set {
		values["backup_proxy_id"] = backup
	}
	if in.Expires.Set {
		var exp any
		if in.Expires.Value != nil && *in.Expires.Value > 0 {
			exp = time.Unix(*in.Expires.Value, 0)
		}
		values["expires_at"] = exp
	}
	fields, marks, args := []string{}, []string{}, []any{}
	for key, value := range values {
		fields = append(fields, key)
		args = append(args, value)
		marks = append(marks, fmt.Sprintf("$%d", len(args)))
	}
	if create {
		err = tx.QueryRowContext(r.Context(), "INSERT INTO proxies("+strings.Join(fields, ",")+") VALUES("+strings.Join(marks, ",")+") RETURNING id", args...).Scan(&id)
	} else {
		sets := []string{"updated_at=now()"}
		for i, field := range fields {
			sets = append(sets, field+"="+marks[i])
		}
		args = append(args, id)
		_, err = tx.ExecContext(r.Context(), "UPDATE proxies SET "+strings.Join(sets, ",")+fmt.Sprintf(" WHERE id=$%d", len(args)), args...)
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.SetPathValue("id", strconv.FormatInt(id, 10))
	return a.getProxy(w, r)
}

const proxyProjection = "(to_jsonb(p)-'deleted_at'-'password') || jsonb_build_object('has_password',COALESCE(password,'')<>'')"

func (a *App) getProxy(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	raw, err := jsonRow(a.DB.QueryRowContext(r.Context(), "SELECT "+proxyProjection+" FROM proxies p WHERE id=$1 AND deleted_at IS NULL", id))
	if err != nil {
		return err
	}
	return reply(w, raw)
}
func (a *App) listProxies(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	search, status, protocol := "%"+r.URL.Query().Get("search")+"%", r.URL.Query().Get("status"), r.URL.Query().Get("protocol")
	all := strings.HasSuffix(r.URL.Path, "/all")
	if all {
		status = "active"
	}
	where := ` WHERE deleted_at IS NULL AND name ILIKE $1 AND ($2='' OR status=$2) AND ($3='' OR protocol=$3)`
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM proxies"+where, search, status, protocol).Scan(&total); err != nil {
		return err
	}
	query := "SELECT " + proxyProjection + " || jsonb_build_object('account_count',(SELECT count(*) FROM accounts WHERE proxy_id=p.id AND deleted_at IS NULL)) FROM proxies p" + where + " ORDER BY id DESC"
	args := []any{search, status, protocol}
	if !all {
		query += " LIMIT $4 OFFSET $5"
		args = append(args, size, (page-1)*size)
	}
	rows, err := a.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	if all {
		return reply(w, items)
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) deleteProxy(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720034)"); err != nil {
		return err
	}
	var used bool
	if err = tx.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM accounts WHERE proxy_id=$1 AND deleted_at IS NULL) OR EXISTS(SELECT 1 FROM proxies WHERE backup_proxy_id=$1 AND deleted_at IS NULL)`, id).Scan(&used); err != nil {
		return err
	}
	if used {
		return conflict("proxy is referenced by an account or fallback")
	}
	result, err := tx.ExecContext(r.Context(), "UPDATE proxies SET deleted_at=now(),status='inactive',updated_at=now() WHERE id=$1 AND deleted_at IS NULL", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing()
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]bool{"deleted": true})
}
func (a *App) proxyAccounts(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT jsonb_build_object('id',id,'name',name,'platform',platform,'status',status) FROM accounts WHERE proxy_id=$1 AND deleted_at IS NULL ORDER BY id", id)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, items)
}
func (a *App) proxyRoutes() {
	a.route("POST /api/v1/admin/proxies/{id}/test", "admin", a.testProxy)
	a.route("GET /api/v1/admin/proxies", "admin", a.listProxies)
	a.route("GET /api/v1/admin/proxies/all", "admin", a.listProxies)
	a.route("POST /api/v1/admin/proxies", "admin", a.saveProxy)
	a.route("GET /api/v1/admin/proxies/{id}", "admin", a.getProxy)
	a.route("PUT /api/v1/admin/proxies/{id}", "admin", a.saveProxy)
	a.route("DELETE /api/v1/admin/proxies/{id}", "admin", a.deleteProxy)
	a.route("GET /api/v1/admin/proxies/{id}/accounts", "admin", a.proxyAccounts)
}

func (a *App) testProxy(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	started := time.Now()
	u := &upstreamAccount{Platform: "openai", ProxyID: &id, Credentials: map[string]json.RawMessage{"base_url": json.RawMessage(`"https://www.cloudflare.com"`)}}
	response, err := a.upstreamRequest(ctx, u, http.MethodGet, "/cdn-cgi/trace", nil)
	if err != nil {
		return reply(w, map[string]any{"success": false, "latency_ms": time.Since(started).Milliseconds(), "error": "proxy connectivity test failed"})
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 || response.StatusCode != 200 {
		return reply(w, map[string]any{"success": false, "error": "proxy test endpoint returned an invalid response"})
	}
	result := map[string]any{"success": true, "latency_ms": time.Since(started).Milliseconds()}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && (key == "ip" || key == "loc" || key == "colo") {
			result[key] = value
		}
	}
	return reply(w, result)
}
