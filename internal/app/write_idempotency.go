package app

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

// The mutation and its replay record share one transaction, so a crash commits
// both or neither. Only use this for writes without external side effects.
func writeIdempotency(w http.ResponseWriter, r *http.Request, tx *sql.Tx, scope, actor string, payload any) (func(json.RawMessage) error, bool, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(r.Header.Values("Idempotency-Key")) > 1 || len(key) > 128 {
		return nil, false, bad("invalid Idempotency-Key")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return nil, false, bad("invalid Idempotency-Key")
		}
	}
	if key == "" {
		return nil, false, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	keyHash := digest(key)
	fingerprint := digest(r.Method + "\n" + r.URL.Path + "\n" + actor + "\n" + string(raw))
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", scope+keyHash); err != nil {
		return nil, false, err
	}
	var stored, status string
	var body sql.NullString
	err = tx.QueryRowContext(r.Context(), `SELECT request_fingerprint,status,response_body FROM idempotency_records
 WHERE scope=$1 AND idempotency_key_hash=$2 AND (expires_at>now() OR status='processing')`, scope, keyHash).Scan(&stored, &status, &body)
	if err == nil {
		if stored != fingerprint {
			return nil, false, conflict("Idempotency-Key reused with different request")
		}
		if status != "succeeded" || !body.Valid {
			return nil, false, conflict("previous write requires review")
		}
		if err = tx.Rollback(); err != nil {
			return nil, false, err
		}
		w.Header().Set("Idempotency-Replayed", "true")
		w.Header().Set("X-Idempotency-Replayed", "true")
		return nil, true, reply(w, json.RawMessage(body.String))
	}
	if err != sql.ErrNoRows {
		return nil, false, err
	}
	return func(data json.RawMessage) error {
		_, err := tx.ExecContext(r.Context(), `INSERT INTO idempotency_records(scope,idempotency_key_hash,request_fingerprint,status,response_status,response_body,expires_at)
 VALUES($1,$2,$3,'succeeded',200,$4,now()+interval '24 hours')
 ON CONFLICT(scope,idempotency_key_hash) DO UPDATE SET request_fingerprint=EXCLUDED.request_fingerprint,status='succeeded',response_status=200,
 response_body=EXCLUDED.response_body,error_reason=NULL,locked_until=NULL,expires_at=EXCLUDED.expires_at,updated_at=now()`, scope, keyHash, fingerprint, string(data))
		return err
	}, false, nil
}
