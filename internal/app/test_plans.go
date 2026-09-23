package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func nextTestRun(expression string, from time.Time) (time.Time, error) {
	if len(expression) > 100 {
		return time.Time{}, bad("cron expression is too long")
	}
	schedule, err := cronParser.Parse(expression)
	if err != nil {
		return time.Time{}, bad("invalid five-field cron expression")
	}
	next := schedule.Next(from.UTC())
	if next.IsZero() {
		return next, bad("cron expression has no future occurrence")
	}
	return next, nil
}

type testPlan struct {
	ID          int64      `json:"id"`
	AccountID   int64      `json:"account_id"`
	Model       string     `json:"model_id"`
	Cron        string     `json:"cron_expression"`
	Enabled     bool       `json:"enabled"`
	MaxResults  int        `json:"max_results"`
	AutoRecover bool       `json:"auto_recover"`
	LastRun     *time.Time `json:"last_run_at"`
	NextRun     *time.Time `json:"next_run_at"`
	Created     time.Time  `json:"created_at"`
	Updated     time.Time  `json:"updated_at"`
}

func rawReply(w http.ResponseWriter, data any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(data)
}
func (a *App) saveTestPlan(w http.ResponseWriter, r *http.Request) error {
	create := r.Method == "POST"
	var id int64
	var err error
	if !create {
		id, err = pathID(r)
		if err != nil {
			return err
		}
	}
	var in struct {
		AccountID *int64  `json:"account_id"`
		Model     *string `json:"model_id"`
		Cron      *string `json:"cron_expression"`
		Enabled   *bool   `json:"enabled"`
		Max       *int    `json:"max_results"`
		Recover   *bool   `json:"auto_recover"`
	}
	if err = decode(w, r, &in); err != nil {
		return err
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	plan := testPlan{Enabled: true, MaxResults: 50}
	if !create {
		raw, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(p) FROM scheduled_test_plans p WHERE id=$1 FOR UPDATE", id))
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &plan); err != nil {
			return err
		}
		if in.AccountID != nil && *in.AccountID != plan.AccountID {
			return bad("plan account cannot change")
		}
	}
	if in.AccountID != nil {
		plan.AccountID = *in.AccountID
	}
	if in.Model != nil {
		plan.Model = *in.Model
	}
	if in.Cron != nil {
		plan.Cron = *in.Cron
	}
	if in.Enabled != nil {
		plan.Enabled = *in.Enabled
	}
	if in.Max != nil {
		plan.MaxResults = *in.Max
	}
	if in.Recover != nil {
		plan.AutoRecover = *in.Recover
	}
	if plan.AccountID <= 0 || plan.Model == "" || len(plan.Model) > 100 || plan.MaxResults < 1 || plan.MaxResults > 1000 {
		return bad("invalid plan account, model_id or max_results")
	}
	if _, err = a.loadAccount(r.Context(), plan.AccountID); err != nil {
		return err
	}
	next, err := nextTestRun(plan.Cron, time.Now())
	if err != nil {
		return err
	}
	if create {
		err = tx.QueryRowContext(r.Context(), `INSERT INTO scheduled_test_plans(account_id,model_id,cron_expression,enabled,max_results,auto_recover,next_run_at) VALUES($1,$2,$3,$4,$5,$6,$7) RETURNING id`, plan.AccountID, plan.Model, plan.Cron, plan.Enabled, plan.MaxResults, plan.AutoRecover, next).Scan(&id)
	} else {
		_, err = tx.ExecContext(r.Context(), `UPDATE scheduled_test_plans SET model_id=$1,cron_expression=$2,enabled=$3,max_results=$4,auto_recover=$5,next_run_at=$6,updated_at=now() WHERE id=$7`, plan.Model, plan.Cron, plan.Enabled, plan.MaxResults, plan.AutoRecover, next, id)
	}
	if err != nil {
		return err
	}
	raw, err := jsonRow(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(p) FROM scheduled_test_plans p WHERE id=$1", id))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return rawReply(w, raw)
}
func (a *App) listAccountPlans(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if _, err = a.loadAccount(r.Context(), id); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(p) FROM scheduled_test_plans p WHERE account_id=$1 ORDER BY id", id)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return rawReply(w, items)
}
func (a *App) deleteTestPlan(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	result, err := a.DB.ExecContext(r.Context(), "DELETE FROM scheduled_test_plans WHERE id=$1", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return missing()
	}
	return rawReply(w, map[string]string{"message": "deleted"})
}
func (a *App) testResults(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	var exists bool
	if err = a.DB.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 FROM scheduled_test_plans WHERE id=$1)", id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return missing()
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT to_jsonb(t) FROM scheduled_test_results t WHERE plan_id=$1 ORDER BY id DESC LIMIT $2", id, limit)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return rawReply(w, items)
}
func (a *App) startWorkers() {
	ctx, cancel := context.WithCancel(context.Background())
	a.workerCancel = cancel
	a.workerDone = make(chan struct{})
	go func() {
		defer close(a.workerDone)
		if err := a.recoverReceipts(ctx); err != nil {
			slog.Error("pending usage settlement recovery failed")
		}
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		pricingTick := time.NewTicker(time.Minute)
		defer pricingTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pricingTick.C:
				if err := a.reloadPrices(); err != nil {
					slog.Error("price catalog reload failed; retaining previous prices")
				}
			case <-ticker.C:
				if err := a.checkInstance(ctx); err != nil {
					slog.Error("instance lock connection lost; background work stopped")
					return
				}
				if err := a.recoverReceipts(ctx); err != nil && ctx.Err() == nil {
					slog.Error("pending usage settlement recovery failed")
				}
				if err := a.runDueTests(ctx); err != nil && ctx.Err() == nil {
					slog.Error("scheduled test cycle failed")
				}
			}
		}
	}()
}
func (a *App) runDueTests(ctx context.Context) error {
	if !a.planMu.TryLock() {
		return nil
	}
	defer a.planMu.Unlock()
	rows, err := a.DB.QueryContext(ctx, `SELECT to_jsonb(p) FROM scheduled_test_plans p JOIN accounts a ON a.id=p.account_id WHERE p.enabled AND p.next_run_at<=now() AND a.deleted_at IS NULL AND a.type='apikey' ORDER BY p.next_run_at,p.id LIMIT 50`)
	if err != nil {
		return err
	}
	raw, err := jsonRows(rows)
	if err != nil {
		return err
	}
	// ponytail: sequential health tests (45s cap each); use a bounded worker pool if due-plan latency grows.
	for _, b := range raw {
		var plan testPlan
		if err = json.Unmarshal(b, &plan); err != nil {
			return err
		}
		if err = a.runTestPlan(ctx, plan); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Error("scheduled test execution failed", "plan_id", plan.ID)
		}
	}
	return nil
}
func (a *App) runTestPlan(ctx context.Context, plan testPlan) error {
	if err := a.checkInstance(ctx); err != nil {
		return err
	}
	// Recheck after waiting behind another due plan.
	var valid bool
	if err := a.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM scheduled_test_plans WHERE id=$1 AND enabled AND updated_at=$2)", plan.ID, plan.Updated).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return nil
	}
	u, err := a.loadAccount(ctx, plan.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	result := a.runAccountTest(ctx, u, plan.Model, "")
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tx, err := a.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Account deletion uses the same account -> plan lock order.
	var accountID int64
	if err = tx.QueryRowContext(ctx, "SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL FOR UPDATE", plan.AccountID).Scan(&accountID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	var updated time.Time
	var maxResults int
	err = tx.QueryRowContext(ctx, "SELECT updated_at,max_results FROM scheduled_test_plans WHERE id=$1 FOR UPDATE", plan.ID).Scan(&updated, &maxResults)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO scheduled_test_results(plan_id,status,response_text,error_message,latency_ms,started_at,finished_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, plan.ID, result.Status, result.Text, result.Error, result.Latency, result.Started, result.Finished); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM scheduled_test_results WHERE plan_id=$1 AND id IN(SELECT id FROM scheduled_test_results WHERE plan_id=$1 ORDER BY id DESC OFFSET $2)`, plan.ID, maxResults); err != nil {
		return err
	}
	if updated.Equal(plan.Updated) {
		next, err := nextTestRun(plan.Cron, result.Finished)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE scheduled_test_plans SET last_run_at=$1,next_run_at=$2 WHERE id=$3", result.Finished, next, plan.ID); err != nil {
			return err
		}
	}
	if result.Status == "success" && plan.AutoRecover && updated.Equal(plan.Updated) {
		if _, err = tx.ExecContext(ctx, recoverTestAccountSQL, u.ID, u.UpdatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (a *App) testPlanRoutes() {
	a.route("POST /api/v1/admin/scheduled-test-plans", "admin", a.saveTestPlan)
	a.route("PUT /api/v1/admin/scheduled-test-plans/{id}", "admin", a.saveTestPlan)
	a.route("DELETE /api/v1/admin/scheduled-test-plans/{id}", "admin", a.deleteTestPlan)
	a.route("GET /api/v1/admin/scheduled-test-plans/{id}/results", "admin", a.testResults)
	a.route("GET /api/v1/admin/accounts/{id}/scheduled-test-plans", "admin", a.listAccountPlans)
}
