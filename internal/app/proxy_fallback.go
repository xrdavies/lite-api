package app

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

type proxyTarget struct {
	ID                           int64
	Protocol, Host, Status, Mode string
	Port                         int
	Username, Password           sql.NullString
	Expires                      *time.Time
	Backup                       *int64
	UpdatedAt                    time.Time
}

func loadProxyTarget(ctx context.Context, q queryer, id int64) (*proxyTarget, error) {
	p := &proxyTarget{ID: id}
	err := q.QueryRowContext(ctx, `SELECT protocol,host,port,username,password,status,expires_at,fallback_mode,backup_proxy_id,updated_at
 FROM proxies WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&p.Protocol, &p.Host, &p.Port, &p.Username, &p.Password, &p.Status, &p.Expires, &p.Mode, &p.Backup, &p.UpdatedAt)
	return p, err
}

// Configuration, HTTP and WebSocket callers use the same fallback traversal.
// A disabled root never enables fallback; an explicitly chosen backup chain
// may traverse unavailable nodes, but cannot select them for a connection.
func resolveProxyTarget(ctx context.Context, q queryer, id int64, at time.Time) (*proxyTarget, error) {
	seen := map[int64]bool{}
	for len(seen) < 9 {
		if seen[id] {
			return nil, bad("proxy fallback cycle")
		}
		p, err := loadProxyTarget(ctx, q, id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, bad("proxy is unavailable")
		}
		if err != nil {
			return nil, err
		}
		if len(seen) == 0 && p.Status != "active" && p.Status != "expired" {
			return nil, bad("proxy is inactive")
		}
		seen[id] = true
		if p.Status == "active" && (p.Expires == nil || p.Expires.After(at)) {
			return p, nil
		}
		switch p.Mode {
		case "direct":
			return nil, nil
		case "proxy":
			if p.Backup != nil {
				id = *p.Backup
				continue
			}
		}
		return nil, bad("proxy fallback is unavailable")
	}
	return nil, bad("proxy fallback chain is too long")
}

// Invalidate snapshots on this proxy and every route that could use it.
// UNION makes the recursive query terminate even for malformed stored cycles.
func invalidateProxySnapshots(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `WITH RECURSIVE affected(id) AS (
 SELECT $1::bigint UNION SELECT p.id FROM proxies p JOIN affected a ON p.backup_proxy_id=a.id WHERE p.deleted_at IS NULL
) UPDATE accounts SET extra=extra - 'upstream_billing_probe' - 'upstream_model_metadata' - 'grok_usage_snapshot',updated_at=clock_timestamp()
 WHERE proxy_id IN (SELECT id FROM affected) AND deleted_at IS NULL`, id)
	return err
}

func (a *App) expireProxies(ctx context.Context) error {
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// One graph lock covers proxy edits, account assignment, expiry and revert.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(720034)"); err != nil {
		return err
	}
	at := time.Now().UTC()
	// ponytail: at most 100 expirations per minute; increase the batch only if
	// deployments regularly expire larger fleets together. Request routing is immediate.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM proxies WHERE deleted_at IS NULL AND status='active' AND expires_at<=$1 ORDER BY expires_at,id LIMIT 100`, at)
	if err != nil {
		return err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		target, resolveErr := resolveProxyTarget(ctx, tx, id, at)
		var invalid *apiError
		if resolveErr != nil && !errors.As(resolveErr, &invalid) {
			return resolveErr
		}
		if _, err = tx.ExecContext(ctx, "UPDATE proxies SET status='expired',updated_at=clock_timestamp() WHERE id=$1", id); err != nil {
			return err
		}
		if err = invalidateProxySnapshots(ctx, tx, id); err != nil {
			return err
		}
		if resolveErr != nil {
			continue // Keep binding when no authorized fallback can be resolved.
		}
		var targetID any
		if target != nil {
			targetID = target.ID
		}
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET proxy_id=$2,proxy_fallback_origin_id=COALESCE(proxy_fallback_origin_id,$1),
 extra=extra - 'upstream_billing_probe' - 'upstream_model_metadata' - 'grok_usage_snapshot',updated_at=clock_timestamp()
 WHERE proxy_id=$1 AND deleted_at IS NULL`, id, targetID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) revertProxyFallback(w http.ResponseWriter, r *http.Request) error {
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
	var origin *int64
	if err = tx.QueryRowContext(r.Context(), "SELECT proxy_fallback_origin_id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", id).Scan(&origin); err != nil {
		return err
	}
	if origin == nil {
		return conflict("account is not using a proxy fallback")
	}
	var available bool
	if err = tx.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM proxies WHERE id=$1 AND deleted_at IS NULL AND status='active' AND (expires_at IS NULL OR expires_at>clock_timestamp()))", *origin).Scan(&available); err != nil {
		return err
	}
	if !available {
		return conflict("renew and activate the original proxy before reverting")
	}
	if _, err = tx.ExecContext(r.Context(), `UPDATE accounts SET proxy_id=proxy_fallback_origin_id,proxy_fallback_origin_id=NULL,
 extra=extra - 'upstream_billing_probe' - 'upstream_model_metadata' - 'grok_usage_snapshot',updated_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]string{"message": "reverted"})
}

func (a *App) startProxyExpiry(ctx context.Context) {
	a.proxyWorkerDone = make(chan struct{})
	go func() {
		defer close(a.proxyWorkerDone)
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			if err := a.expireProxies(ctx); err != nil && ctx.Err() == nil {
				slog.Error("proxy expiry cycle failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
