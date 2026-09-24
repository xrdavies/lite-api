package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	finish, replayed, err := writeIdempotency(w, r, tx, "admin.users.balance", fmt.Sprintf("admin:%d", current(r).ID), in)
	if err != nil || replayed {
		return err
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
	if finish != nil {
		if err = finish(data); err != nil {
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
