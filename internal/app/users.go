package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const userFields = "id,email,username,role,balance,frozen_balance,total_recharged,concurrency,rpm_limit,status,created_at,updated_at,last_login_at,last_active_at"

func userView(admin bool) string {
	fields := userFields
	if admin {
		fields += ",notes,restrict_public_groups"
	}
	view := `(SELECT to_jsonb(profile) FROM (SELECT u.` + strings.ReplaceAll(fields, ",", ",u.") + `) profile)
 || jsonb_build_object('allowed_groups',COALESCE((SELECT jsonb_agg(group_id ORDER BY group_id) FROM user_allowed_groups WHERE user_id=u.id),'[]'::jsonb))`
	if admin {
		view += ` || jsonb_build_object(
 'last_used_at',(SELECT max(created_at) FROM usage_logs WHERE user_id=u.id),
 'group_rates',COALESCE((SELECT jsonb_object_agg(group_id::text,rate_multiplier) FROM user_group_rate_multipliers WHERE user_id=u.id AND rate_multiplier IS NOT NULL),'{}'::jsonb))`
	}
	return view
}

func (a *App) userJSON(ctx context.Context, q queryer, id int64, admin bool) (json.RawMessage, error) {
	return jsonRow(q.QueryRowContext(ctx, "SELECT "+userView(admin)+" FROM users u WHERE id=$1 AND deleted_at IS NULL", id))
}
func (a *App) profile(w http.ResponseWriter, r *http.Request) error {
	u, err := a.userJSON(r.Context(), a.DB, current(r).ID, false)
	if err != nil {
		return err
	}
	return reply(w, u)
}
func (a *App) updateProfile(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username *string `json:"username"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Username == nil || len([]rune(*in.Username)) > 100 {
		return bad("invalid username")
	}
	if _, err := a.DB.ExecContext(r.Context(), "UPDATE users SET username=$1,updated_at=now() WHERE id=$2 AND deleted_at IS NULL", *in.Username, current(r).ID); err != nil {
		return err
	}
	return a.profile(w, r)
}
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	u := current(r)
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Old)) != nil {
		return unauthorized()
	}
	hash, err := passwordHash(in.New)
	if err != nil {
		return err
	}
	result, err := a.DB.ExecContext(r.Context(), "UPDATE users SET password_hash=$1,updated_at=now() WHERE id=$2 AND password_hash=$3 AND deleted_at IS NULL", hash, u.ID, u.PasswordHash)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return conflict("password changed concurrently")
	}
	// Every session includes the password hash fingerprint; this also revokes tokens if Redis is unavailable.
	return reply(w, map[string]string{"message": "password changed; log in again"})
}

type userInput struct {
	Email         *string                `json:"email"`
	Password      *string                `json:"password"`
	Username      *string                `json:"username"`
	Notes         *string                `json:"notes"`
	Role          *string                `json:"role"`
	Status        *string                `json:"status"`
	Balance       *json.Number           `json:"balance"`
	Concurrency   *int                   `json:"concurrency"`
	RPMLimit      *int                   `json:"rpm_limit"`
	AllowedGroups *[]int64               `json:"allowed_groups"`
	Restrict      *bool                  `json:"restrict_public_groups"`
	GroupRates    map[int64]*json.Number `json:"group_rates"`
}

func (in *userInput) validate(create bool) error {
	if create && (in.Email == nil || in.Password == nil) {
		return bad("email and password are required")
	}
	if in.Email != nil {
		*in.Email = strings.ToLower(strings.TrimSpace(*in.Email))
		if !validateEmail(*in.Email) {
			return bad("invalid email")
		}
	}
	if in.Username != nil && len([]rune(*in.Username)) > 100 {
		return bad("username too long")
	}
	if in.Role != nil && *in.Role != "admin" && *in.Role != "user" {
		return bad("invalid role")
	}
	if in.Status != nil && *in.Status != "active" && *in.Status != "disabled" {
		return bad("invalid status")
	}
	if in.Concurrency != nil && (*in.Concurrency < 1 || *in.Concurrency > 10000) {
		return bad("invalid concurrency")
	}
	if in.RPMLimit != nil && (*in.RPMLimit < 0 || *in.RPMLimit > 1000000) {
		return bad("invalid rpm_limit")
	}
	if in.Balance != nil && !validDecimal(*in.Balance, 12, 8) {
		return bad("invalid balance")
	}
	for id, v := range in.GroupRates {
		if id <= 0 || (v != nil && !validDecimal(*v, 6, 4)) {
			return bad("invalid group rate")
		}
	}
	return nil
}
func validDecimal(n json.Number, integer, scale int) bool {
	s := n.String()
	parts := strings.Split(s, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > integer {
		return false
	}
	if len(parts) == 2 && (len(parts[1]) == 0 || len(parts[1]) > scale) {
		return false
	}
	for _, part := range parts {
		for _, c := range part {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}
func allowedGroups(ctx context.Context, tx *sql.Tx, userID int64, ids []int64) error {
	if len(ids) > 1000 {
		return bad("too many groups")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM user_allowed_groups WHERE user_id=$1", userID); err != nil {
		return err
	}
	for _, id := range ids {
		var ok bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM groups WHERE id=$1 AND deleted_at IS NULL AND subscription_type='standard' AND NOT require_oauth_only AND platform IN ('openai','anthropic','gemini','grok','kimi','zhipu','deepseek','minimax','composite'))", id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return bad("group is unavailable")
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO user_allowed_groups(user_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING", userID, id); err != nil {
			return err
		}
	}
	return nil
}
func (a *App) createUser(w http.ResponseWriter, r *http.Request) error {
	var in userInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := in.validate(true); err != nil {
		return err
	}
	hash, err := passwordHash(*in.Password)
	if err != nil {
		return err
	}
	role, status, name, notes, balance, concurrency, rpm, restrict := "user", "active", "", "", "0", 5, 0, false
	if in.Role != nil {
		role = *in.Role
	}
	if in.Status != nil {
		status = *in.Status
	}
	if in.Username != nil {
		name = *in.Username
	}
	if in.Notes != nil {
		notes = *in.Notes
	}
	if in.Balance != nil {
		balance = in.Balance.String()
	}
	if in.Concurrency != nil {
		concurrency = *in.Concurrency
	}
	if in.RPMLimit != nil {
		rpm = *in.RPMLimit
	}
	if in.Restrict != nil {
		restrict = *in.Restrict
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(r.Context(), `INSERT INTO users(email,password_hash,username,notes,role,status,balance,concurrency,rpm_limit,restrict_public_groups) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`, *in.Email, hash, name, notes, role, status, balance, concurrency, rpm, restrict).Scan(&id)
	if err != nil {
		return err
	}
	if in.AllowedGroups != nil {
		if err = allowedGroups(r.Context(), tx, id, *in.AllowedGroups); err != nil {
			return err
		}
	}
	if err = setGroupRates(r.Context(), tx, id, in.GroupRates); err != nil {
		return err
	}
	if balance != "0" {
		if err = recordBalance(r.Context(), tx, id, balance, "Initial balance"); err != nil {
			return err
		}
	}
	u, err := a.userJSON(r.Context(), tx, id, true)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, u)
}
func recordBalance(ctx context.Context, tx *sql.Tx, id int64, delta, notes string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO redeem_codes(code,type,value,status,used_by,used_at,notes) SELECT $1,'admin_balance',$2::numeric,'used',$3,now(),$4 WHERE $2::numeric<>0`, randomToken(24), delta, id, notes)
	return err
}
func setGroupRates(ctx context.Context, tx *sql.Tx, id int64, rates map[int64]*json.Number) error {
	if rates == nil {
		return nil
	}
	if len(rates) == 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE user_group_rate_multipliers SET rate_multiplier=NULL,updated_at=now() WHERE user_id=$1", id); err != nil {
			return err
		}
	}
	for gid, rate := range rates {
		if rate == nil {
			if _, err := tx.ExecContext(ctx, "UPDATE user_group_rate_multipliers SET rate_multiplier=NULL,updated_at=now() WHERE user_id=$1 AND group_id=$2", id, gid); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_group_rate_multipliers(user_id,group_id,rate_multiplier) VALUES($1,$2,$3) ON CONFLICT(user_id,group_id) DO UPDATE SET rate_multiplier=EXCLUDED.rate_multiplier,updated_at=now()`, id, gid, rate.String()); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM user_group_rate_multipliers WHERE user_id=$1 AND rate_multiplier IS NULL AND rpm_override IS NULL", id)
	return err
}
func (a *App) updateUser(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in userInput
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if err = in.validate(false); err != nil {
		return err
	}
	if in.Balance != nil {
		return bad("use the balance adjustment endpoint")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720032)"); err != nil {
		return err
	}
	var role, status string
	var oldConcurrency int
	if err = tx.QueryRowContext(r.Context(), "SELECT role,status,concurrency FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&role, &status, &oldConcurrency); err != nil {
		return err
	}
	demoting := role == "admin" && ((in.Role != nil && *in.Role != "admin") || (in.Status != nil && *in.Status != "active"))
	if demoting {
		if id == current(r).ID {
			return bad("cannot disable or demote your own administrator")
		}
		var count int
		if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM users WHERE role='admin' AND status='active' AND deleted_at IS NULL AND id<>$1", id).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return conflict("cannot remove the last active administrator")
		}
	}
	sets := []string{"updated_at=now()"}
	args := []any{id}
	add := func(field string, value any) {
		args = append(args, value)
		sets = append(sets, fmt.Sprintf("%s=$%d", field, len(args)))
	}
	if in.Email != nil {
		add("email", *in.Email)
	}
	if in.Username != nil {
		add("username", *in.Username)
	}
	if in.Notes != nil {
		add("notes", *in.Notes)
	}
	if in.Role != nil {
		add("role", *in.Role)
	}
	if in.Status != nil {
		add("status", *in.Status)
	}
	if in.Concurrency != nil {
		add("concurrency", *in.Concurrency)
	}
	if in.RPMLimit != nil {
		add("rpm_limit", *in.RPMLimit)
	}
	if in.Restrict != nil {
		add("restrict_public_groups", *in.Restrict)
	}
	if in.Password != nil {
		hash, err := passwordHash(*in.Password)
		if err != nil {
			return err
		}
		add("password_hash", hash)
	}
	if in.Status != nil && *in.Status != status {
		if err = a.revoke(r.Context(), id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(r.Context(), "UPDATE users SET "+strings.Join(sets, ",")+" WHERE id=$1", args...); err != nil {
		return err
	}
	if in.Concurrency != nil && *in.Concurrency != oldConcurrency {
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO redeem_codes(code,type,value,status,used_by,used_at,notes) VALUES($1,'admin_concurrency',$2,'used',$3,now(),'Concurrency adjustment')`, randomToken(24), *in.Concurrency-oldConcurrency, id); err != nil {
			return err
		}
	}
	if in.AllowedGroups != nil {
		if err = allowedGroups(r.Context(), tx, id, *in.AllowedGroups); err != nil {
			return err
		}
	}
	if err = setGroupRates(r.Context(), tx, id, in.GroupRates); err != nil {
		return err
	}
	u, err := a.userJSON(r.Context(), tx, id, true)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, u)
}
func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if id == current(r).ID {
		return bad("cannot delete your own account")
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(720032)"); err != nil {
		return err
	}
	var role string
	if err = tx.QueryRowContext(r.Context(), "SELECT role FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&role); err != nil {
		return err
	}
	if role == "admin" {
		var n int
		if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM users WHERE id<>$1 AND role='admin' AND status='active' AND deleted_at IS NULL", id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return conflict("cannot delete the last active administrator")
		}
	}
	if err = a.revoke(r.Context(), id); err != nil {
		return err
	}
	for _, query := range []string{"UPDATE api_keys SET deleted_at=now(),status='inactive',updated_at=now() WHERE user_id=$1 AND deleted_at IS NULL", "DELETE FROM user_allowed_groups WHERE user_id=$1", "UPDATE users SET deleted_at=now(),status='disabled',updated_at=now() WHERE id=$1"} {
		if _, err = tx.ExecContext(r.Context(), query, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]bool{"deleted": true})
}
func (a *App) listUsers(w http.ResponseWriter, r *http.Request) error {
	page, size := pagination(r)
	search := "%" + r.URL.Query().Get("search") + "%"
	status := r.URL.Query().Get("status")
	role := r.URL.Query().Get("role")
	where := ` WHERE deleted_at IS NULL AND (email ILIKE $1 OR username ILIKE $1) AND ($2='' OR status=$2) AND ($3='' OR role=$3)`
	var total int
	if err := a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM users"+where, search, status, role).Scan(&total); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT "+userView(true)+" FROM users u"+where+" ORDER BY id DESC LIMIT $4 OFFSET $5", search, status, role, size, (page-1)*size)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}
func (a *App) getUser(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	u, err := a.userJSON(r.Context(), a.DB, id, true)
	if err != nil {
		return err
	}
	return reply(w, u)
}
func (a *App) userRoutes() {
	a.route("GET /api/v1/admin/users/{id}/rpm-status", "admin", a.userRPMStatus)
	a.route("POST /api/v1/admin/users/{id}/balance", "admin", a.adjustBalance)
	a.route("GET /api/v1/admin/users/{id}/balance-history", "admin", a.balanceHistory)
	a.route("GET /api/v1/user/profile", "user", a.profile)
	a.route("PUT /api/v1/user", "user", a.updateProfile)
	a.route("PUT /api/v1/user/password", "user", a.changePassword)
	a.route("GET /api/v1/admin/users", "admin", a.listUsers)
	a.route("POST /api/v1/admin/users", "admin", a.createUser)
	a.route("GET /api/v1/admin/users/{id}", "admin", a.getUser)
	a.route("PUT /api/v1/admin/users/{id}", "admin", a.updateUser)
	a.route("DELETE /api/v1/admin/users/{id}", "admin", a.deleteUser)
}

// Bootstrap creates the first administrator on an initialized database.
func Bootstrap(ctx context.Context, db *sql.DB, email, password string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if !validateEmail(email) {
		return bad("ADMIN_EMAIL must be a valid email address")
	}
	hash, err := passwordHash(password)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(720032)"); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return conflict("bootstrap requires a database without users")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO users(email,password_hash,role,username) VALUES($1,$2,'admin','Administrator')", email, hash)
	if err != nil {
		return err
	}
	return tx.Commit()
}
