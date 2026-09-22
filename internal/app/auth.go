package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type identityKey struct{}
type identity struct {
	ID                                   int64
	Email, Role, PasswordHash, SessionID string
}

func current(r *http.Request) *identity { return r.Context().Value(identityKey{}).(*identity) }

const accessSeconds = 900
const sessionSeconds = 30 * 24 * 60 * 60
const jwtHeader = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

type claims struct {
	Issuer    string `json:"iss"`
	UserID    int64  `json:"sub"`
	SessionID string `json:"sid"`
	IssuedAt  int64  `json:"iat"`
	Expires   int64  `json:"exp"`
}

func digest(s string) string      { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }
func sessionKey(id string) string { return "lite-api:session:" + id }
func epochKey(id int64) string    { return "lite-api:sessions-epoch:" + strconv.FormatInt(id, 10) }
func (a *App) sign(c claims) string {
	raw, _ := json.Marshal(c)
	body := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *App) parseToken(token string) (claims, error) {
	var c claims
	if len(token) > 2048 {
		return c, unauthorized()
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return c, unauthorized()
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, unauthorized()
	}
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return c, unauthorized()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, unauthorized()
	}
	now := time.Now().Unix()
	if c.Issuer != "lite-api" || c.UserID <= 0 || len(c.SessionID) != 32 || c.IssuedAt > now+30 || c.Expires <= now || c.Expires-c.IssuedAt != accessSeconds {
		return c, unauthorized()
	}
	return c, nil
}
func (a *App) loadIdentity(ctx context.Context, id int64) (*identity, error) {
	u := &identity{}
	var status string
	var totp bool
	err := a.DB.QueryRowContext(ctx, "SELECT id,email,role,password_hash,status,totp_enabled FROM users WHERE id=$1 AND deleted_at IS NULL", id).Scan(&u.ID, &u.Email, &u.Role, &u.PasswordHash, &status, &totp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unauthorized()
	}
	if err != nil {
		return nil, err
	}
	if status != "active" || totp {
		return nil, unauthorized()
	}
	return u, nil
}
func (a *App) validSession(ctx context.Context, sid string, uid int64, password string) (map[string]string, error) {
	fields, err := a.Redis.HGetAll(ctx, sessionKey(sid)).Result()
	if err != nil {
		return nil, err
	}
	epoch, err := a.Redis.Get(ctx, epochKey(uid)).Result()
	if errors.Is(err, redis.Nil) {
		epoch = "0"
	} else if err != nil {
		return nil, err
	}
	if len(fields) == 0 || fields["user"] != strconv.FormatInt(uid, 10) || fields["password"] != digest(password) || fields["epoch"] != epoch {
		return nil, unauthorized()
	}
	return fields, nil
}
func (a *App) authenticate(r *http.Request) (*identity, error) {
	c, err := a.parseToken(bearer(r))
	if err != nil {
		return nil, err
	}
	u, err := a.loadIdentity(r.Context(), c.UserID)
	if err != nil {
		return nil, err
	}
	if _, err = a.validSession(r.Context(), c.SessionID, u.ID, u.PasswordHash); err != nil {
		return nil, err
	}
	u.SessionID = c.SessionID
	return u, nil
}
func (a *App) tokenReply(sid, refresh string, uid int64) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"access_token": a.sign(claims{"lite-api", uid, sid, now, now + accessSeconds}), "refresh_token": sid + "." + refresh, "expires_in": accessSeconds, "token_type": "Bearer"}
}
func (a *App) createSession(ctx context.Context, u *identity) (map[string]any, error) {
	sid, refresh := randomToken(24), randomToken(32)
	epoch, err := a.Redis.Get(ctx, epochKey(u.ID)).Result()
	if errors.Is(err, redis.Nil) {
		epoch = "0"
	} else if err != nil {
		return nil, err
	}
	result, err := a.Redis.Eval(ctx, `if (redis.call('GET',KEYS[2]) or '0') ~= ARGV[1] then return 0 end
redis.call('HSET',KEYS[1],'epoch',ARGV[1],'user',ARGV[2],'password',ARGV[3],'refresh',ARGV[4]); redis.call('EXPIRE',KEYS[1],ARGV[5]); return 1`, []string{sessionKey(sid), epochKey(u.ID)}, epoch, u.ID, digest(u.PasswordHash), digest(refresh), sessionSeconds).Int()
	if err != nil {
		return nil, err
	}
	if result != 1 {
		return nil, unauthorized()
	}
	return a.tokenReply(sid, refresh, u.ID), nil
}
func validateEmail(email string) bool {
	a, err := mail.ParseAddress(email)
	return err == nil && a.Address == email && len(email) <= 255
}
func passwordHash(password string) (string, error) {
	if len(password) < 6 || len(password) > 72 {
		return "", bad("password must contain 6 to 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}
func (a *App) login(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	for _, key := range []string{"ip:" + clientIP(r), "email:" + digest(in.Email)} {
		n, err := a.Redis.Eval(r.Context(), `local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('EXPIRE',KEYS[1],60) end;return n`, []string{"lite-api:login:" + key}).Int()
		if err != nil {
			return err
		}
		if n > 10 {
			return &apiError{429, "too many login attempts"}
		}
	}
	var id int64
	var hash string
	err := a.DB.QueryRowContext(r.Context(), "SELECT id,password_hash FROM users WHERE lower(email)=$1 AND deleted_at IS NULL", in.Email).Scan(&id, &hash)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if hash == "" {
		hash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	}
	if len(in.Password) > 72 || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || id == 0 {
		return unauthorized()
	}
	// Serialize session creation with account disable/delete and password changes.
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	u := &identity{}
	var status string
	var totp bool
	err = tx.QueryRowContext(r.Context(), "SELECT id,email,role,password_hash,status,totp_enabled FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&u.ID, &u.Email, &u.Role, &u.PasswordHash, &status, &totp)
	if errors.Is(err, sql.ErrNoRows) {
		return unauthorized()
	}
	if err != nil {
		return err
	}
	if u.PasswordHash != hash || status != "active" || totp {
		return unauthorized()
	}
	tokens, err := a.createSession(r.Context(), u)
	if err != nil {
		return err
	}
	profile, err := a.userJSON(r.Context(), tx, id, false)
	if err != nil {
		return err
	}
	tokens["user"] = profile
	if _, err = tx.ExecContext(r.Context(), "UPDATE users SET last_login_at=now(),last_active_at=now() WHERE id=$1", id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, tokens)
}
func splitRefresh(token string) (string, string, error) {
	sid, secret, ok := strings.Cut(token, ".")
	if !ok || len(sid) != 32 || len(secret) != 43 {
		return "", "", unauthorized()
	}
	return sid, secret, nil
}
func (a *App) refresh(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Token string `json:"refresh_token"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sid, old, err := splitRefresh(in.Token)
	if err != nil {
		return err
	}
	uid, err := a.Redis.HGet(r.Context(), sessionKey(sid), "user").Int64()
	if errors.Is(err, redis.Nil) {
		return unauthorized()
	}
	if err != nil {
		return err
	}
	u, err := a.loadIdentity(r.Context(), uid)
	if err != nil {
		return err
	}
	fields, err := a.validSession(r.Context(), sid, uid, u.PasswordHash)
	if err != nil {
		return err
	}
	next := randomToken(32)
	ok, err := a.Redis.Eval(r.Context(), `if (redis.call('GET',KEYS[2]) or '0') ~= ARGV[1] or redis.call('HGET',KEYS[1],'refresh') ~= ARGV[2] then return 0 end
redis.call('HSET',KEYS[1],'refresh',ARGV[3]); return 1`, []string{sessionKey(sid), epochKey(uid)}, fields["epoch"], digest(old), digest(next)).Int()
	if err != nil {
		return err
	}
	if ok != 1 {
		return unauthorized()
	}
	return reply(w, a.tokenReply(sid, next, uid))
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Token string `json:"refresh_token"`
	}
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	var accessSession string
	if bearer(r) != "" {
		u, err := a.authenticate(r)
		if err != nil {
			return err
		}
		accessSession = u.SessionID
	}
	if in.Token != "" {
		sid, secret, err := splitRefresh(in.Token)
		if err != nil {
			return err
		}
		if _, err = a.Redis.Eval(r.Context(), `if redis.call('HGET',KEYS[1],'refresh')==ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`, []string{sessionKey(sid)}, digest(secret)).Result(); err != nil {
			return err
		}
	}
	if accessSession != "" {
		if err := a.Redis.Del(r.Context(), sessionKey(accessSession)).Err(); err != nil {
			return err
		}
	}
	return reply(w, map[string]string{"message": "logged out"})
}
func (a *App) revoke(ctx context.Context, uid int64) error {
	return a.Redis.Incr(ctx, epochKey(uid)).Err()
}
func (a *App) authRoutes() {
	a.route("POST /api/v1/auth/login", "public", a.login)
	a.route("POST /api/v1/auth/refresh", "public", a.refresh)
	a.route("POST /api/v1/auth/logout", "public", a.logout)
	a.route("GET /api/v1/auth/me", "user", a.profile)
	a.route("POST /api/v1/auth/revoke-all-sessions", "user", func(w http.ResponseWriter, r *http.Request) error {
		if err := a.revoke(r.Context(), current(r).ID); err != nil {
			return err
		}
		return reply(w, map[string]string{"message": "all sessions revoked"})
	})
}
