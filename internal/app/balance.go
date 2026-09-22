package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (a *App) adjustBalance(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var in struct {
		Balance   json.Number `json:"balance"`
		Operation string      `json:"operation"`
		Notes     string      `json:"notes"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	if !validDecimal(in.Balance, 12, 8) {
		return bad("invalid balance")
	}
	if in.Operation != "set" && in.Operation != "add" && in.Operation != "subtract" {
		return bad("invalid balance operation")
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > 128 {
		return bad("invalid Idempotency-Key")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return bad("invalid Idempotency-Key")
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	scope := "admin.users.balance"
	keyHash := digest(key)
	raw, _ := json.Marshal(in)
	fingerprint := digest(r.Method + "\n" + r.URL.Path + "\n" + fmt.Sprintf("admin:%d", current(r).ID) + "\n" + string(raw))
	if key != "" {
		// Transaction ownership serializes duplicate submissions, including concurrent callers.
		if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", scope+keyHash); err != nil {
			return err
		}
		var storedFingerprint, body string
		var exists bool
		if err = tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM idempotency_records WHERE scope=$1 AND idempotency_key_hash=$2 AND expires_at>now())", scope, keyHash).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if err = tx.QueryRowContext(r.Context(), "SELECT request_fingerprint,response_body FROM idempotency_records WHERE scope=$1 AND idempotency_key_hash=$2 AND status='succeeded'", scope, keyHash).Scan(&storedFingerprint, &body); err != nil {
				return err
			}
			if storedFingerprint != fingerprint {
				return conflict("Idempotency-Key reused with different request")
			}
			w.Header().Set("Idempotency-Replayed", "true")
			return reply(w, json.RawMessage(body))
		}
	}
	var old string
	if err = tx.QueryRowContext(r.Context(), "SELECT balance::text FROM users WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&old); err != nil {
		return err
	}
	expression := "$2::numeric"
	if in.Operation == "add" {
		expression = "balance+$2::numeric"
	}
	if in.Operation == "subtract" {
		expression = "balance-$2::numeric"
	}
	var next, delta string
	err = tx.QueryRowContext(r.Context(), "UPDATE users SET balance="+expression+",updated_at=now() WHERE id=$1 AND "+expression+">=0 RETURNING balance::text,(balance-$3::numeric)::text", id, in.Balance.String(), old).Scan(&next, &delta)
	if errors.Is(err, sql.ErrNoRows) {
		return bad("balance cannot become negative")
	}
	if err != nil {
		return err
	}
	if err = recordBalance(r.Context(), tx, id, delta, in.Notes); err != nil {
		return err
	}
	data, err := a.userJSON(r.Context(), tx, id, true)
	if err != nil {
		return err
	}
	if key != "" {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO idempotency_records(scope,idempotency_key_hash,request_fingerprint,status,response_status,response_body,expires_at) VALUES($1,$2,$3,'succeeded',200,$4,now()+interval '24 hours') ON CONFLICT(scope,idempotency_key_hash) DO UPDATE SET request_fingerprint=EXCLUDED.request_fingerprint,status='succeeded',response_status=200,response_body=EXCLUDED.response_body,expires_at=EXCLUDED.expires_at,updated_at=now()`, scope, keyHash, fingerprint, string(data))
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, data)
}
func (a *App) balanceHistory(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	page, size := pagination(r)
	var total int
	if err = a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM redeem_codes WHERE used_by=$1 AND type IN ('admin_balance','admin_concurrency')", id).Scan(&total); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT to_jsonb(h) FROM (SELECT id,type,value,status,used_at,created_at,notes FROM redeem_codes WHERE used_by=$1 AND type IN ('admin_balance','admin_concurrency') ORDER BY id DESC LIMIT $2 OFFSET $3) h`, id, size, (page-1)*size)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return pageReply(w, items, total, page, size)
}
