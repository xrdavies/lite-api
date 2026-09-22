// Package schema owns the immutable database baseline used by fresh installations.
package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
)

//go:embed baseline.sql
var baseline string

//go:embed contract.sql
var contractQuery string

//go:embed contract.json
var contract string

// Initialize creates a new empty database schema; serve never calls this.
func Initialize(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(720031)"); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public'").Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return errors.New("database is not empty; initialization refused")
	}
	if _, err = tx.ExecContext(ctx, baseline); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	if _, err = tx.ExecContext(ctx, "RESET search_path"); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(baseline))
	if _, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(filename,checksum) VALUES('baseline.sql',$1)", hex.EncodeToString(sum[:])); err != nil {
		return err
	}
	return tx.Commit()
}

// Validate detects schema drift without issuing DDL or rewriting stored data.
func Validate(ctx context.Context, db *sql.DB) error {
	var actual string
	if err := db.QueryRowContext(ctx, contractQuery).Scan(&actual); err != nil {
		return err
	}
	var same bool
	if err := db.QueryRowContext(ctx, "SELECT $1::jsonb = $2::jsonb", actual, contract).Scan(&same); err != nil {
		return err
	}
	if !same {
		return errors.New("database schema differs from the supported baseline")
	}
	return nil
}
