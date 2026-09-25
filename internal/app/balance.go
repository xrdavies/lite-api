package app

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
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
	codeType := r.URL.Query().Get("type")
	if len(codeType) > 20 || !utf8.ValidString(codeType) || strings.ContainsRune(codeType, 0) {
		return bad("invalid balance history type")
	}
	tx, err := a.DB.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep the internal adjustment ledger separate from excluded redemption and
	// referral products. The positive balance sum is independent of the filter.
	const where = " WHERE used_by=$1 AND type IN ('admin_balance','admin_concurrency')"
	var total int
	var recharged string
	if err = tx.QueryRowContext(r.Context(), `SELECT count(*) FILTER (WHERE $2='' OR type=$2),
 COALESCE(sum(value) FILTER (WHERE type='admin_balance' AND value>0),0)::text FROM redeem_codes`+where, id, codeType).Scan(&total, &recharged); err != nil {
		return err
	}
	order := "COALESCE(used_at,created_at) DESC,id DESC"
	if codeType != "" {
		order = "used_at DESC,id DESC"
	}
	rows, err := tx.QueryContext(r.Context(), `SELECT to_jsonb(h) FROM
 (SELECT id,code,type,value,status,used_by,used_at,created_at,COALESCE(notes,'') AS notes,group_id,validity_days,expires_at
 FROM redeem_codes`+where+` AND ($2='' OR type=$2) ORDER BY `+order+` LIMIT $3 OFFSET $4) h`, id, codeType, size, (page-1)*size)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, map[string]any{"items": items, "total": total, "page": page, "page_size": size,
		"pages": max(1, (total+size-1)/size), "total_recharged": json.Number(recharged)})
}
