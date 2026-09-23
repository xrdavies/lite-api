package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"
)

func (a *App) startBatchImages(ctx context.Context) {
	a.batchWorkerDone = make(chan struct{})
	go func() {
		defer close(a.batchWorkerDone)
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			if err := a.runBatchImages(ctx); err != nil && ctx.Err() == nil {
				slog.Error("batch image recovery failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
}
func (a *App) runBatchImages(ctx context.Context) error {
	if !a.batchMu.TryLock() {
		return nil
	}
	defer a.batchMu.Unlock()
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(ctx, `SELECT batch_id FROM batch_image_jobs WHERE status NOT IN ('completed','failed','cancelled','output_deleted') OR input_deleted_at IS NULL AND provider_input_ref IS NOT NULL OR output_deleted_at IS NULL AND output_expires_at<=now() ORDER BY updated_at,id LIMIT 32`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		jobCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		err = a.processBatchImage(jobCtx, id)
		cancel()
		if err != nil && ctx.Err() == nil {
			_, _ = a.DB.ExecContext(ctx, `UPDATE batch_image_jobs SET retry_count=retry_count+1,last_error_code='RECOVERY_PENDING',last_error_message=$2,updated_at=now() WHERE batch_id=$1 AND status NOT IN ('completed','failed','cancelled','output_deleted')`, id, safeGatewayError(err))
			slog.Error("batch image cycle failed", "batch_id", id)
		}
	}
	return nil
}
func (a *App) loadBatchJob(ctx context.Context, id string) (batchImageJob, error) {
	return batchScan(a.DB.QueryRowContext(ctx, "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE batch_id=$1", id))
}
func (a *App) batchAccount(ctx context.Context, j batchImageJob, snap batchImageSnapshot) (*upstreamAccount, error) {
	u, err := a.loadAccount(ctx, j.AccountID)
	if err != nil {
		return nil, err
	}
	if u.Platform != "gemini" || u.protocol() != "gemini" || responseTarget(u) != snap.Target {
		return nil, conflict("batch upstream identity changed; restore the original account to recover")
	}
	return u, nil
}
func (a *App) processBatchImage(ctx context.Context, id string) error {
	j, err := a.loadBatchJob(ctx, id)
	if err != nil {
		return err
	}
	snap, err := a.batchSnapshot(ctx, id)
	if err != nil {
		return err
	}
	if j.Status == "settling" {
		return a.settleBatchImage(ctx, id, snap)
	}
	u, err := a.batchAccount(ctx, j, snap)
	if err != nil {
		// No billable provider job exists in created state. Revoked credentials
		// or a deleted account must not strand its unused balance hold.
		var api *apiError
		if j.Status == "created" && (errors.Is(err, sql.ErrNoRows) || errors.As(err, &api) && api.status >= 400 && api.status < 500) {
			return a.failBatchImage(ctx, id, "cancelled", "ACCOUNT_UNAVAILABLE")
		}
		return err
	}
	if batchTerminal(j.Status) {
		if j.Input != "" {
			var deleted bool
			if err = a.DB.QueryRowContext(ctx, "SELECT input_deleted_at IS NOT NULL FROM batch_image_jobs WHERE batch_id=$1", id).Scan(&deleted); err != nil {
				return err
			}
			if !deleted {
				if err = a.deleteBatchFile(ctx, u, j.Input); err != nil {
					return err
				}
				if _, err = a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET input_deleted_at=now(),updated_at=now() WHERE batch_id=$1", id); err != nil {
					return err
				}
			}
		}
		if j.OutputDeleted == nil && j.Expires != nil && !time.Now().Before(*j.Expires) {
			return a.removeBatchOutput(ctx, j, u)
		}
		return nil
	}
	if j.Status == "created" {
		// No provider create has been attempted; permission changes may safely cancel
		// before any upstream work. The held balance is deliberately not rechecked.
		var token string
		err = a.DB.QueryRowContext(ctx, "SELECT key FROM api_keys WHERE id=$1 AND user_id=$2 AND group_id=$3 AND deleted_at IS NULL", j.KeyID, j.UserID, snap.GroupID).Scan(&token)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return a.failBatchImage(ctx, id, "cancelled", "KEY_UNAVAILABLE")
			}
			return err
		}
		r, _ := http.NewRequestWithContext(ctx, "POST", "/v1/images/batches", nil)
		r.RemoteAddr = snap.RemoteAddr
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("User-Agent", snap.UserAgent)
		g, err := a.gatewayAuth(r, false)
		if err != nil {
			var api *apiError
			if errors.As(err, &api) && api.status >= 400 && api.status < 500 {
				return a.failBatchImage(ctx, id, "cancelled", "PERMISSION_CHANGED")
			}
			return err
		}
		var allowed bool
		if err = a.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM groups g JOIN account_groups ag ON ag.group_id=g.id JOIN accounts a ON a.id=ag.account_id WHERE g.id=$1 AND ag.account_id=$2 AND g.allow_batch_image_generation AND g.platform='gemini' AND g.status='active' AND g.deleted_at IS NULL AND a.status='active' AND a.schedulable AND a.deleted_at IS NULL AND (NOT a.auto_pause_on_expired OR a.expires_at IS NULL OR a.expires_at>now()) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at<=now()) AND (a.overload_until IS NULL OR a.overload_until<=now()) AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until<=now()))`, snap.GroupID, j.AccountID).Scan(&allowed); err != nil {
			return err
		}
		if !g.Group.allows(j.Model) || !allowed || !accountQuotaAvailable(u.Extra, time.Now()) {
			return a.failBatchImage(ctx, id, "cancelled", "RESOURCE_UNAVAILABLE")
		}
		data, err := snap.Request.jsonl()
		if err != nil {
			return err
		}
		file, err := a.uploadBatchInput(ctx, u, id, data)
		if err != nil {
			return err
		}
		// Uploading the input is repeatable; creation of the billable provider job is not.
		if _, err = a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET status='uploading',provider_input_ref=$2,updated_at=now(),version=version+1 WHERE batch_id=$1 AND status='created'", id, file); err != nil {
			return err
		}
		provider, err := a.createProviderBatch(ctx, u, id, snap.Model, file)
		if err != nil {
			return err
		} // Recovery searches displayName; never blind-resubmits.
		if err = a.bindBatchProvider(ctx, id, provider.Name); err != nil {
			return err
		}
		j, err = a.loadBatchJob(ctx, id)
		if err != nil {
			return err
		}
	} else if j.Status == "uploading" {
		provider, err := a.findProviderBatch(ctx, u, id)
		if err != nil {
			return err
		}
		if provider.Name == "" {
			return &apiError{503, "batch submission outcome is unknown; awaiting provider reconciliation"}
		}
		if err = a.bindBatchProvider(ctx, id, provider.Name); err != nil {
			return err
		}
		j, err = a.loadBatchJob(ctx, id)
		if err != nil {
			return err
		}
	}
	if j.Status == "indexing" {
		return a.indexBatchImage(ctx, j, u)
	}
	if j.Status != "submitted" && j.Status != "running" {
		return errors.New("invalid batch image state")
	}
	var cancelRequested bool
	if err = a.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM batch_image_events WHERE job_id=$1 AND event_type='job_cancel_requested')", id).Scan(&cancelRequested); err != nil {
		return err
	}
	if cancelRequested {
		if err = a.cancelProviderBatch(ctx, u, j.ProviderJob); err != nil {
			return err
		}
	}
	provider, err := a.getProviderBatch(ctx, u, j.ProviderJob)
	if err != nil {
		return err
	}
	switch provider.state() {
	case "JOB_STATE_PENDING", "JOB_STATE_QUEUED":
		return nil
	case "JOB_STATE_RUNNING":
		_, err = a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET status='running',started_at=COALESCE(started_at,now()),updated_at=now() WHERE batch_id=$1", id)
		return err
	case "JOB_STATE_FAILED", "JOB_STATE_EXPIRED":
		return a.failBatchImage(ctx, id, "failed", "PROVIDER_BATCH_FAILED")
	case "JOB_STATE_CANCELLED":
		return a.failBatchImage(ctx, id, "cancelled", "PROVIDER_BATCH_CANCELLED")
	case "JOB_STATE_SUCCEEDED":
		if !batchResource(provider.output(), "files") {
			return &apiError{502, "batch provider output file is missing or unsafe"}
		}
		if _, err = a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET status='indexing',provider_output_ref=$2,updated_at=now(),version=version+1 WHERE batch_id=$1", id, provider.output()); err != nil {
			return err
		}
		j.Output = provider.output()
		j.Status = "indexing"
		return a.indexBatchImage(ctx, j, u)
	default:
		return &apiError{502, "unknown provider batch state"}
	}
}
func (a *App) bindBatchProvider(ctx context.Context, id, name string) error {
	if !batchResource(name, "batches") {
		return bad("invalid provider batch name")
	}
	_, err := a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET status='submitted',provider_job_name=$2,submitted_at=now(),updated_at=now(),last_error_code=NULL,last_error_message=NULL,version=version+1 WHERE batch_id=$1 AND status='uploading'", id, name)
	return err
}

func (a *App) failBatchImage(ctx context.Context, id, status, code string) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := batchScan(tx.QueryRowContext(ctx, "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE batch_id=$1 FOR UPDATE", id))
	if err != nil {
		return err
	}
	if batchTerminal(j.Status) {
		return nil
	}
	if j.Status == "indexing" || j.Status == "settling" {
		return conflict("generated batch results must settle before cancellation")
	}
	if err = batchBalance(ctx, tx, j, "release", "0", j.Hash); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE batch_image_jobs SET status=$2::varchar,actual_cost=0,cancelled_count=CASE WHEN $2::varchar='cancelled' THEN item_count ELSE 0 END,fail_count=CASE WHEN $2::varchar='failed' THEN item_count ELSE 0 END,finished_at=now(),settled_at=now(),output_expires_at=now()+interval '72 hours',last_error_code=$3,updated_at=now(),version=version+1 WHERE batch_id=$1", id, status, code)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE batch_image_items SET status=$2,error_code=$3,error_message='batch did not complete',indexed_at=now() WHERE job_id=$1 AND status='pending'", id, map[string]string{"cancelled": "cancelled", "failed": "failed"}[status], code)
	if err != nil {
		return err
	}
	if err = batchEvent(ctx, tx, id, "hold_released", map[string]string{"status": status, "code": code}); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) indexBatchImage(ctx context.Context, j batchImageJob, u *upstreamAccount) error {
	rows, err := a.DB.QueryContext(ctx, "SELECT custom_id FROM batch_image_items WHERE job_id=$1", j.ID)
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expected[id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	results := map[string]batchImageResult{}
	err = a.readBatchOutput(ctx, u, j.Output, func(item batchImageResult) error {
		if !expected[item.ID] {
			return &apiError{502, "unexpected item in batch output"}
		}
		if _, exists := results[item.ID]; exists {
			return &apiError{502, "duplicate item in batch output"}
		}
		item.Images = nil
		results[item.ID] = item
		return nil
	})
	if err != nil {
		return err
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := batchScan(tx.QueryRowContext(ctx, "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE batch_id=$1 FOR UPDATE", j.ID))
	if err != nil {
		return err
	}
	if current.Status != "indexing" {
		return nil
	}
	success, failed := 0, 0
	for id := range expected {
		item, ok := results[id]
		if !ok {
			item = batchImageResult{ID: id, Code: "INDEX_OUTPUT_MISSING", Message: "provider omitted this item"}
		}
		status := "failed"
		amount := "0"
		if item.Count > 0 {
			status = "success"
			amount = j.Unit.String()
			success++
		} else {
			failed++
		}
		_, err = tx.ExecContext(ctx, `UPDATE batch_image_items SET status=$3,provider_source_object=$4,source_line_number=$5,source_byte_offset=$6,source_byte_length=$7,mime_type=NULLIF($8,''),file_extension=NULLIF($9,''),image_count=$10,error_code=NULLIF($11,''),error_message=NULLIF($12,''),billed_amount=$13,indexed_at=now() WHERE job_id=$1 AND custom_id=$2`, j.ID, id, status, j.Output, item.Line, item.Offset, item.Length, item.MIME, item.Extension, item.Count, item.Code, item.Message, amount)
		if err != nil {
			return err
		}
	}
	hash := digest(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d", j.ID, j.Provider, j.Model, j.ProviderJob, j.Output, success, failed, j.Count))
	if _, err = tx.ExecContext(ctx, "UPDATE batch_image_jobs SET status='settling',success_count=$2,fail_count=$3,manifest_hash=$4,updated_at=now(),version=version+1 WHERE batch_id=$1", j.ID, success, failed, hash); err != nil {
		return err
	}
	if err = batchEvent(ctx, tx, j.ID, "results_indexed", map[string]any{"success_count": success, "fail_count": failed, "manifest_hash": hash}); err != nil {
		return err
	}
	return tx.Commit()
}
func (a *App) settleBatchImage(ctx context.Context, id string, snap batchImageSnapshot) error {
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	j, err := batchScan(tx.QueryRowContext(ctx, "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE batch_id=$1 FOR UPDATE", id))
	if err != nil {
		return err
	}
	if j.Status == "completed" {
		return nil
	}
	if j.Status != "settling" {
		return conflict("batch is not ready to settle")
	}
	if j.Success < 0 || j.Fail < 0 || j.Success+j.Fail != j.Count {
		return conflict("inconsistent batch result counts")
	}
	actual := new(big.Rat).Mul(rat(json.Number(j.Unit)), big.NewRat(int64(j.Success), 1)).FloatString(10)
	var manifest string
	if err = tx.QueryRowContext(ctx, "SELECT manifest_hash FROM batch_image_jobs WHERE batch_id=$1", id).Scan(&manifest); err != nil {
		return err
	}
	if err = batchBalance(ctx, tx, j, "capture", actual, manifest); err != nil {
		return err
	}
	// Batch settlement retains its original ledger meaning: capture/refund the hold
	// and record image usage. It does not masquerade as a second synchronous charge.
	rate := new(big.Rat).Mul(rat(json.Number(j.Rate)), rat(json.Number(j.Discount))).FloatString(8)
	_, err = tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,requested_model,upstream_model,group_id,billing_type,request_type,billing_mode,image_count,image_size,image_output_cost,total_cost,actual_cost,rate_multiplier,account_rate_multiplier,inbound_endpoint,upstream_endpoint,created_at) VALUES($1,$2,$3,$4,$5,$5,$6,$7,0,1,'image',$8,'1K',$9,$9,$9,$10,$11,'/v1/images/batches','gemini:batchGenerateContent',now())`, j.UserID, j.KeyID, j.AccountID, "batch_image_capture:"+id, j.Model, snap.Model, snap.GroupID, j.Success, actual, rate, j.AccountRate.String())
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE batch_image_jobs SET status='completed',actual_cost=$2,settled_at=now(),finished_at=now(),output_expires_at=now()+interval '72 hours',updated_at=now(),last_error_code=NULL,last_error_message=NULL,version=version+1 WHERE batch_id=$1", id, actual); err != nil {
		return err
	}
	if err = batchEvent(ctx, tx, id, "settled", map[string]string{"actual_cost": actual, "manifest_hash": manifest}); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) cancelBatchImage(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return err
	}
	if !batchTerminal(j.Status) {
		if j.Status == "created" {
			err = a.failBatchImage(r.Context(), j.ID, "cancelled", "USER_CANCELLED")
		} else if j.Status == "indexing" || j.Status == "settling" {
			return conflict("batch results are already being settled")
		} else {
			tx, e := a.DB.BeginTx(r.Context(), nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			if err = batchEvent(r.Context(), tx, j.ID, "job_cancel_requested", nil); err != nil {
				return err
			}
			err = tx.Commit()
			// A cancellation intent survives restarts. Polling confirms the provider's
			// terminal state before any money is released.
		}
		if err != nil {
			return err
		}
		j, err = a.batchJob(r.Context(), j.ID, g)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(w).Encode(j.public())
}
func (a *App) deleteBatchImage(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return err
	}
	if !batchTerminal(j.Status) {
		return conflict("batch record can only be deleted after completion")
	}
	if _, err = a.DB.ExecContext(r.Context(), "UPDATE batch_image_jobs SET user_deleted_at=now(),updated_at=now() WHERE batch_id=$1", j.ID); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(map[string]any{"id": j.ID, "deleted": true})
}
func (a *App) removeBatchOutput(ctx context.Context, j batchImageJob, u *upstreamAccount) error {
	if j.OutputDeleted != nil {
		return nil
	}
	if !batchTerminal(j.Status) {
		return conflict("batch output is not ready for deletion")
	}
	if err := a.deleteBatchFile(ctx, u, j.Output); err != nil {
		return err
	}
	_, err := a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET status='output_deleted',output_deleted_at=now(),updated_at=now(),version=version+1 WHERE batch_id=$1", j.ID)
	return err
}
func (a *App) deleteBatchOutputs(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return err
	}
	snap, err := a.batchSnapshot(r.Context(), j.ID)
	if err != nil {
		return err
	}
	u, err := a.batchAccount(r.Context(), j, snap)
	if err != nil {
		return err
	}
	if err = a.removeBatchOutput(r.Context(), j, u); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(map[string]any{"id": j.ID, "deleted": true})
}
