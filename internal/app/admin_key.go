package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
)

const adminKeySetting = "admin_api_key"

func (a *App) readAdminKey(ctx context.Context) (string, error) {
	var key string
	err := a.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", adminKeySetting).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return key, err
}

// Only administrator routes call this method. Machine credentials never create
// a user session and are never accepted by personal or model gateway routes.
func (a *App) authenticateAdmin(r *http.Request) (*identity, error) {
	values, provided := r.Header[http.CanonicalHeaderKey("X-Api-Key")]
	if !provided {
		return a.authenticate(r)
	}
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 128 {
		return nil, unauthorized()
	}
	key, err := a.readAdminKey(r.Context())
	if err != nil {
		return nil, err
	}
	if key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(values[0])) != 1 {
		return nil, unauthorized()
	}
	// The key is global, not owned by the administrator who generated it. Resolve
	// its actor on every request so disabled/deleted/demoted users are never used.
	u := &identity{AuthMethod: "admin_api_key"}
	err = a.DB.QueryRowContext(r.Context(), `SELECT id,email,role FROM users
 WHERE role='admin' AND status='active' AND deleted_at IS NULL AND NOT totp_enabled
 ORDER BY id LIMIT 1`).Scan(&u.ID, &u.Email, &u.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, unauthorized()
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (a *App) adminKeyStatus(w http.ResponseWriter, r *http.Request) error {
	key, err := a.readAdminKey(r.Context())
	if err != nil {
		return err
	}
	masked := ""
	if key != "" {
		masked = "****"
		if len(key) > 14 {
			masked = key[:10] + "..." + key[len(key)-4:]
		}
	}
	return reply(w, map[string]any{"exists": key != "", "masked_key": masked})
}

func (a *App) regenerateAdminKey(w http.ResponseWriter, r *http.Request) error {
	key := "admin-" + hex.EncodeToString(randomBytes(32))
	// Preserve the existing setting's string meaning. Unlike JSON settings this
	// is a raw credential, never exposed by general settings or audit responses.
	_, err := a.DB.ExecContext(r.Context(), `INSERT INTO settings(key,value) VALUES($1,$2)
 ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value,updated_at=now()`, adminKeySetting, key)
	if err != nil {
		return err
	}
	return reply(w, map[string]string{"key": key})
}

func (a *App) deleteAdminKey(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.DB.ExecContext(r.Context(), "DELETE FROM settings WHERE key=$1", adminKeySetting); err != nil {
		return err
	}
	return reply(w, map[string]string{"message": "Admin API key deleted"})
}

func (a *App) adminKeyRoutes() {
	a.route("GET /api/v1/admin/settings/admin-api-key", "admin", a.adminKeyStatus)
	a.route("POST /api/v1/admin/settings/admin-api-key/regenerate", "admin", a.regenerateAdminKey)
	a.route("DELETE /api/v1/admin/settings/admin-api-key", "admin", a.deleteAdminKey)
}
