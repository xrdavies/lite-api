package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type batchGroupInput struct {
	imagePrices
	AllowBatch    *bool        `json:"allow_batch_image_generation"`
	BatchDiscount *json.Number `json:"batch_image_discount_multiplier"`
	BatchHold     *json.Number `json:"batch_image_hold_multiplier"`
}

func (in batchGroupInput) apply(ctx context.Context, tx *sql.Tx, id int64) error {
	sets := []string{}
	args := []any{id}
	add := func(name string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s=$%d", name, len(args)))
	}
	for _, f := range []struct {
		name        string
		v           *json.Number
		whole, frac int
	}{{"image_price_1k", in.Image1K, 12, 8}, {"image_price_2k", in.Image2K, 12, 8}, {"image_price_4k", in.Image4K, 12, 8}, {"image_rate_multiplier", in.ImageRate, 6, 4}, {"batch_image_discount_multiplier", in.BatchDiscount, 6, 4}, {"batch_image_hold_multiplier", in.BatchHold, 6, 4}} {
		if f.v != nil {
			value := f.v.String()
			imagePrice := strings.HasPrefix(f.name, "image_price_")
			if imagePrice {
				value = strings.TrimPrefix(value, "-")
			}
			if !validDecimal(json.Number(value), f.whole, f.frac) {
				return bad("invalid " + f.name)
			}
			if imagePrice && rat(*f.v).Sign() < 0 {
				add(f.name, nil)
			} else {
				add(f.name, f.v.String())
			}
		}
	}
	if in.AllowBatch != nil {
		add("allow_batch_image_generation", *in.AllowBatch)
	}
	if in.IndependentImage != nil {
		add("image_rate_independent", *in.IndependentImage)
	}
	if len(sets) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE groups SET "+strings.Join(sets, ",")+" WHERE id=$1", args...); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, "SELECT batch_image_hold_multiplier>=batch_image_discount_multiplier AND (NOT allow_batch_image_generation OR platform='gemini') FROM groups WHERE id=$1", id).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return bad("batch images require a Gemini group and hold multiplier >= discount multiplier")
	}
	return nil
}

type batchImageJob struct {
	ID            string       `json:"batch_id"`
	UserID        int64        `json:"user_id"`
	KeyID         int64        `json:"api_key_id"`
	AccountID     int64        `json:"account_id"`
	Model         string       `json:"model"`
	TaskName      string       `json:"task_name"`
	Parent        *string      `json:"parent_batch_id"`
	Provider      string       `json:"provider"`
	Status        string       `json:"status"`
	ProviderJob   string       `json:"provider_job_name"`
	Input         string       `json:"provider_input_ref"`
	Output        string       `json:"provider_output_ref"`
	Count         int          `json:"item_count"`
	Success       int          `json:"success_count"`
	Fail          int          `json:"fail_count"`
	Estimate      json.Number  `json:"estimated_cost"`
	Hold          json.Number  `json:"hold_amount"`
	Actual        *json.Number `json:"actual_cost"`
	Unit          json.Number  `json:"billable_unit_price"`
	Rate          json.Number  `json:"group_rate_multiplier"`
	AccountRate   json.Number  `json:"account_rate_multiplier"`
	Discount      json.Number  `json:"batch_discount_multiplier"`
	Hash          string       `json:"request_hash"`
	Created       time.Time    `json:"created_at"`
	Submitted     *time.Time   `json:"submitted_at"`
	Settled       *time.Time   `json:"settled_at"`
	Downloaded    *time.Time   `json:"downloaded_at"`
	OutputDeleted *time.Time   `json:"output_deleted_at"`
	Expires       *time.Time   `json:"output_expires_at"`
	ErrorCode     *string      `json:"last_error_code"`
}

func batchPublicStatus(s string) string {
	switch s {
	case "created", "uploading", "submitted":
		return "queued"
	case "indexing":
		return "processing_results"
	}
	return s
}
func batchUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}
func (j batchImageJob) public() map[string]any {
	return map[string]any{"id": j.ID, "object": "image.batch", "task_name": j.TaskName, "parent_batch_id": j.Parent, "model": j.Model, "provider": j.Provider, "status": batchPublicStatus(j.Status), "item_count": j.Count, "success_count": j.Success, "fail_count": j.Fail, "estimated_cost": j.Estimate, "hold_amount": j.Hold, "actual_cost": j.Actual, "created_at": j.Created.Unix(), "submitted_at": batchUnix(j.Submitted), "settled_at": batchUnix(j.Settled), "downloaded_at": batchUnix(j.Downloaded), "output_deleted_at": batchUnix(j.OutputDeleted), "error_code": j.ErrorCode}
}
func batchScan(row *sql.Row) (batchImageJob, error) {
	var j batchImageJob
	var raw []byte
	err := row.Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &j)
	}
	return j, err
}
func (a *App) batchJob(ctx context.Context, id string, g *gatewayIdentity) (batchImageJob, error) {
	return batchScan(a.DB.QueryRowContext(ctx, "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE batch_id=$1 AND user_id=$2 AND api_key_id=$3 AND user_deleted_at IS NULL", id, g.UserID, g.Key.ID))
}
func batchTerminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled" || status == "output_deleted"
}

type batchImageSnapshot struct {
	Request                              batchImageRequest `json:"request"`
	GroupID                              int64             `json:"group_id"`
	Target, Model, RemoteAddr, UserAgent string
}

func (a *App) batchEnvelope(id string, in *batchImageSnapshot, encoded string) (string, error) {
	cipher := a.chatHistoryCipher()
	aad := []byte("lite-api/batch-image/" + id)
	if encoded == "" {
		raw, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		nonce := randomBytes(cipher.NonceSize())
		return base64.RawStdEncoding.EncodeToString(cipher.Seal(nonce, nonce, raw, aad)), nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < cipher.NonceSize() {
		return "", errors.New("invalid batch request envelope")
	}
	raw, err = cipher.Open(nil, raw[:cipher.NonceSize()], raw[cipher.NonceSize():], aad)
	if err == nil {
		err = json.Unmarshal(raw, in)
	}
	return "", err
}
func (a *App) batchSnapshot(ctx context.Context, id string) (batchImageSnapshot, error) {
	var in batchImageSnapshot
	var encoded string
	err := a.DB.QueryRowContext(ctx, "SELECT payload->>'encrypted_request' FROM batch_image_events WHERE job_id=$1 AND event_type='request_created' ORDER BY id LIMIT 1", id).Scan(&encoded)
	if err == nil {
		_, err = a.batchEnvelope(id, &in, encoded)
	}
	return in, err
}
func batchEvent(ctx context.Context, tx *sql.Tx, id, kind string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO batch_image_events(job_id,event_type,payload) VALUES($1,$2,$3)", id, kind, string(raw))
	return err
}

type batchImagePrice struct{ Base, Rate, Account, Discount, HoldRate, Unit, HoldUnit, Estimated, Held string }

func (a *App) batchPrice(ctx context.Context, g *gatewayIdentity, s *gatewaySelection, model string, n int) (batchImagePrice, error) {
	var p batchImagePrice
	var enabled, independent bool
	var base *string
	var imageRate string
	err := a.DB.QueryRowContext(ctx, `SELECT allow_batch_image_generation,image_price_1k::text,image_rate_independent,image_rate_multiplier::text,batch_image_discount_multiplier::text,batch_image_hold_multiplier::text FROM groups WHERE id=$1 AND platform='gemini' AND deleted_at IS NULL`, g.Key.GroupID).Scan(&enabled, &base, &independent, &imageRate, &p.Discount, &p.HoldRate)
	if err != nil {
		return p, err
	}
	if !enabled {
		return p, denied()
	}
	p.Rate = g.Group.Rate.String()
	if independent {
		p.Rate = imageRate
	}
	p.Account = s.Rate.String()
	if base != nil {
		p.Base = *base
	} else {
		price, err := s.price(model)
		if err != nil {
			return p, err
		}
		if (price.BillingMode == "image" || price.BillingMode == "per_request") && price.PerRequest != nil {
			p.Base = price.PerRequest.String()
		} else {
			return p, bad("batch image per-image price is not configured")
		}
	}
	for _, v := range []string{p.Base, p.Rate, p.Account, p.Discount, p.HoldRate} {
		if _, ok := new(big.Rat).SetString(v); !ok || rat(json.Number(v)).Sign() < 0 {
			return p, bad("invalid batch image pricing")
		}
	}
	if rat(json.Number(p.HoldRate)).Cmp(rat(json.Number(p.Discount))) < 0 {
		p.HoldRate = p.Discount
	}
	standard := new(big.Rat).Mul(rat(json.Number(p.Base)), rat(json.Number(p.Rate)))
	standard.Mul(standard, rat(json.Number(p.Account)))
	p.Unit = new(big.Rat).Mul(standard, rat(json.Number(p.Discount))).FloatString(10)
	p.HoldUnit = new(big.Rat).Mul(standard, rat(json.Number(p.HoldRate))).FloatString(10)
	p.Estimated = new(big.Rat).Mul(rat(json.Number(p.Unit)), big.NewRat(int64(n), 1)).FloatString(10)
	p.Held = new(big.Rat).Mul(rat(json.Number(p.HoldUnit)), big.NewRat(int64(n), 1)).FloatString(10)
	for _, v := range []string{p.Base, p.Unit, p.HoldUnit, p.Estimated, p.Held} {
		if !validDecimal(json.Number(v), 10, 10) {
			return p, bad("batch image amount exceeds database precision")
		}
	}
	return p, nil
}

func (a *App) submitBatchImage(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	var in batchImageRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 180<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF {
		return bad("invalid batch image request")
	}
	if err := in.normalize(); err != nil {
		return err
	}
	if g.Group.Platform != "gemini" || !g.Group.allows(in.Model) {
		return denied()
	}
	idem := r.Header.Get("Idempotency-Key")
	if len(idem) > 255 || strings.ContainsAny(idem, "\r\n\x00") {
		return bad("invalid Idempotency-Key")
	}
	raw, _ := json.Marshal(in)
	hash := digest(string(raw))
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock($1)", -g.Key.ID); err != nil {
		return err
	}
	if idem != "" {
		j, err := batchScan(tx.QueryRowContext(r.Context(), "SELECT to_jsonb(j) FROM batch_image_jobs j WHERE user_id=$1 AND api_key_id=$2 AND idempotency_key=$3", g.UserID, g.Key.ID, idem))
		if err == nil {
			if j.Hash != hash {
				return conflict("Idempotency-Key reused with different batch request")
			}
			w.Header().Set("Idempotency-Replayed", "true")
			return json.NewEncoder(w).Encode(j.public())
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	g, err = a.gatewayAuth(r, true)
	if err != nil {
		return err
	}
	if !g.Group.allows(in.Model) || g.Group.Platform != "gemini" {
		return denied()
	}
	if !a.takeSlot("user", g.UserID, g.Concurrency) {
		return &apiError{429, "user concurrency exhausted"}
	}
	defer a.releaseSlot("user", g.UserID)
	defer a.trackKeySlot(g.Key.ID)()
	if err = a.gatewayRPM(r.Context(), g); err != nil {
		return err
	}
	selection, err := a.chooseAccount(r.Context(), g, in.Model, textRequest{Protocol: "gemini", Model: in.Model}, map[int64]bool{}, nil, nil, a.prices.Load())
	if err != nil {
		return err
	}
	defer selection.Release()
	if !validNativeModel(selection.UpstreamModel) {
		return bad("invalid upstream batch model")
	}
	price, err := a.batchPrice(r.Context(), g, selection, in.Model, len(in.Items))
	if err != nil {
		return err
	}
	var pending int
	if err = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM batch_image_jobs WHERE user_id=$1 AND status NOT IN ('completed','failed','cancelled','output_deleted')", g.UserID).Scan(&pending); err != nil {
		return err
	}
	if pending >= 32 {
		return &apiError{429, "too many unfinished batch image jobs"}
	}
	if in.Parent != "" {
		var parent string
		err = tx.QueryRowContext(r.Context(), "SELECT COALESCE(parent_batch_id,batch_id) FROM batch_image_jobs WHERE batch_id=$1 AND user_id=$2 AND api_key_id=$3 AND user_deleted_at IS NULL", in.Parent, g.UserID, g.Key.ID).Scan(&parent)
		if err != nil {
			return err
		}
		in.Parent = parent
	}
	id := "bimg_" + randomToken(18)
	taskName := in.TaskName
	if taskName == "" {
		taskName = time.Now().UTC().Format("2006-01-02 15:04:05")
	}
	snap := batchImageSnapshot{Request: in, GroupID: g.Key.GroupID, Target: responseTarget(selection.Account), Model: selection.UpstreamModel, RemoteAddr: r.RemoteAddr, UserAgent: truncate(r.UserAgent(), 512)}
	encoded, err := a.batchEnvelope(id, &snap, "")
	if err != nil {
		return err
	}
	j, err := batchScan(tx.QueryRowContext(r.Context(), `WITH inserted AS (INSERT INTO batch_image_jobs(batch_id,user_id,api_key_id,account_id,provider,model,task_name,parent_batch_id,status,item_count,estimated_cost,hold_amount,base_unit_price,group_rate_multiplier,account_rate_multiplier,batch_discount_multiplier,hold_multiplier,billable_unit_price,hold_unit_price,pricing_snapshot_version,hold_id,idempotency_key,request_hash) VALUES($1,$2,$3,$4,'gemini_api',$5,$6,NULLIF($7,''),'created',$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,1,$18,NULLIF($19,''),$20) RETURNING *) SELECT to_jsonb(inserted) FROM inserted`, id, g.UserID, g.Key.ID, selection.Account.ID, in.Model, taskName, in.Parent, len(in.Items), price.Estimated, price.Held, price.Base, price.Rate, price.Account, price.Discount, price.HoldRate, price.Unit, price.HoldUnit, "batch_image_hold:"+id, idem, hash))
	if err != nil {
		return err
	}
	if err = batchBalance(r.Context(), tx, j, "hold", "0", hash); err != nil {
		return err
	}
	for _, item := range in.Items {
		if _, err = tx.ExecContext(r.Context(), "INSERT INTO batch_image_items(job_id,custom_id,status,request_hash,prompt_preview) VALUES($1,$2,'pending',$3,$4)", id, item.ID, hash, truncate(item.Prompt, 512)); err != nil {
			return err
		}
	}
	if err = batchEvent(r.Context(), tx, id, "request_created", map[string]string{"encrypted_request": encoded}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(j.public())
}

// Job-row locks serialize capture/release; the billing claims provide independent
// proof of a prior hold and prevent transferring another task's frozen funds.
func batchBalance(ctx context.Context, tx *sql.Tx, j batchImageJob, operation, actual, payload string) error {
	request := "batch_image_" + operation + ":" + j.ID
	fingerprint := digest(fmt.Sprintf("%d|%d|%s|%s|%s|%s", j.UserID, j.KeyID, j.ID, rat(json.Number(j.Hold)).FloatString(10), rat(json.Number(actual)).FloatString(10), payload))
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT request_fingerprint FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2 UNION ALL SELECT request_fingerprint FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$2", request, j.KeyID).Scan(&existing)
	if err == nil {
		if existing != fingerprint {
			return conflict("batch billing fingerprint conflict")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if operation != "hold" {
		var held, finished bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$3 UNION ALL SELECT 1 FROM usage_billing_dedup_archive WHERE request_id=$1 AND api_key_id=$3), EXISTS(SELECT 1 FROM usage_billing_dedup WHERE request_id=$2 AND api_key_id=$3 UNION ALL SELECT 1 FROM usage_billing_dedup_archive WHERE request_id=$2 AND api_key_id=$3)`, "batch_image_hold:"+j.ID, "batch_image_"+map[string]string{"capture": "release", "release": "capture"}[operation]+":"+j.ID, j.KeyID).Scan(&held, &finished); err != nil {
			return err
		}
		if !held || finished {
			return conflict("batch hold is missing or already finalized")
		}
	}
	held := rat(json.Number(j.Hold)).FloatString(8)
	cost := rat(json.Number(actual)).FloatString(8)
	if rat(json.Number(actual)).Sign() < 0 || rat(json.Number(actual)).Cmp(rat(json.Number(j.Hold))) > 0 {
		return conflict("batch actual cost exceeds hold")
	}
	var result sql.Result
	switch operation {
	case "hold":
		result, err = tx.ExecContext(ctx, "UPDATE users SET balance=balance-$2::numeric,frozen_balance=frozen_balance+$2::numeric,updated_at=now() WHERE id=$1 AND deleted_at IS NULL AND status='active' AND balance>=$2::numeric", j.UserID, held)
	case "capture", "release":
		result, err = tx.ExecContext(ctx, "UPDATE users SET balance=balance+$2::numeric-$3::numeric,frozen_balance=frozen_balance-$2::numeric,updated_at=now() WHERE id=$1 AND frozen_balance>=$2::numeric", j.UserID, held, cost)
	default:
		return errors.New("invalid batch balance operation")
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		if operation == "hold" {
			return &apiError{402, "insufficient batch image balance"}
		}
		return conflict("batch frozen balance is inconsistent")
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO usage_billing_dedup(request_id,api_key_id,request_fingerprint) VALUES($1,$2,$3)", request, j.KeyID, fingerprint)
	return err
}

func batchPagination(r *http.Request, n, maximum int) (int, int, error) {
	offset := 0
	var err error
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err = strconv.Atoi(v)
		if err != nil || n < 1 || n > maximum {
			return 0, 0, bad("invalid limit")
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("cursor")); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 || offset > 1000000 {
			return 0, 0, bad("invalid cursor")
		}
	}
	return n, offset, nil
}

func batchListTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 && seconds <= 253402300799 {
		at := time.Unix(seconds, 0).UTC()
		return &at, nil
	}
	for _, layout := range []string{time.RFC3339, time.DateOnly} {
		if at, err := time.Parse(layout, raw); err == nil && at.Year() >= 1 {
			return &at, nil
		}
	}
	return nil, bad("invalid batch time; use positive Unix seconds, RFC3339 or YYYY-MM-DD")
}

func (a *App) listBatchImages(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	n, offset, err := batchPagination(r, 20, 100)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	status := strings.TrimSpace(query.Get("status"))
	if status == "all" {
		status = ""
	}
	name := strings.TrimSpace(query.Get("task_name"))
	if !utf8.ValidString(name+status) || strings.ContainsRune(name+status, 0) || len(name) > 1020 || len(status) > 32 {
		return bad("invalid batch filter")
	}
	var downloaded *bool
	switch strings.ToLower(strings.TrimSpace(query.Get("downloaded"))) {
	case "", "all":
	case "true", "1", "yes", "downloaded":
		value := true
		downloaded = &value
	case "false", "0", "no", "not_downloaded":
		value := false
		downloaded = &value
	default:
		return bad("invalid downloaded filter")
	}
	from, err := batchListTime(query.Get("from"))
	if err != nil {
		return err
	}
	to, err := batchListTime(query.Get("to"))
	if err != nil {
		return err
	}
	if from != nil && to != nil && from.After(*to) {
		return bad("batch from must not be after to")
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT to_jsonb(j) FROM batch_image_jobs j
		WHERE user_id=$1 AND api_key_id=$2 AND user_deleted_at IS NULL
		AND ($3='' OR status=$3 OR $3='queued' AND status IN ('created','uploading','submitted') OR $3='processing_results' AND status='indexing')
		AND ($4='' OR task_name ILIKE '%'||$4||'%')
		AND ($5::boolean IS NULL OR (downloaded_at IS NOT NULL)=$5)
		AND ($6::timestamptz IS NULL OR created_at >= $6)
		AND ($7::timestamptz IS NULL OR created_at < $7)
		ORDER BY created_at DESC,id DESC LIMIT $8 OFFSET $9`, g.UserID, g.Key.ID, status, name, downloaded, from, to, n+1, offset)
	if err != nil {
		return err
	}
	raw, err := jsonRows(rows)
	if err != nil {
		return err
	}
	more := len(raw) > n
	if more {
		raw = raw[:n]
	}
	items := []any{}
	for _, b := range raw {
		var j batchImageJob
		if err = json.Unmarshal(b, &j); err != nil {
			return err
		}
		items = append(items, j.public())
	}
	return json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items, "has_more": more})
}
func (a *App) batchModels(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	var allowed bool
	if err := a.DB.QueryRowContext(r.Context(), "SELECT allow_batch_image_generation AND platform='gemini' FROM groups WHERE id=$1", g.Key.GroupID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return denied()
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT credentials->'model_mapping' FROM accounts a JOIN account_groups ag ON ag.account_id=a.id WHERE ag.group_id=$1 AND a.platform='gemini' AND a.type='apikey' AND a.status='active' AND a.schedulable AND a.deleted_at IS NULL", g.Key.GroupID)
	if err != nil {
		return err
	}
	candidates := map[string]bool{}
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var mapping map[string]string
		_ = json.Unmarshal(raw, &mapping)
		for name := range mapping {
			if !strings.Contains(name, "*") {
				candidates[name] = true
			}
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range []string{"gemini-2.5-flash-image", "gemini-3-pro-image-preview", "gemini-3.1-flash-image-preview"} {
		candidates[name] = true
	}
	names := []string{}
	for name := range candidates {
		names = append(names, name)
	}
	sort.Strings(names)
	models := []any{}
	for _, name := range names {
		if !g.Group.allows(name) {
			continue
		}
		s, err := a.chooseAccount(r.Context(), g, name, textRequest{Protocol: "gemini", Model: name}, map[int64]bool{}, nil, nil, a.prices.Load())
		if err != nil {
			continue
		}
		_, err = a.batchPrice(r.Context(), g, s, name, 1)
		s.Release()
		if err == nil {
			models = append(models, map[string]string{"id": name, "object": "image.batch.model", "provider": "gemini_api"})
		}
	}
	return json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": models})
}
func (a *App) batchImageRoutes() {
	handle := func(pattern string, h func(http.ResponseWriter, *http.Request, *gatewayIdentity) error) {
		a.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			err := a.checkInstance(r.Context())
			if err == nil {
				g, e := a.gatewayAuth(r, false)
				err = e
				if err == nil {
					err = h(w, r, g)
				}
			}
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					err = missing()
				}
				textGatewayError(w, "images", err)
			}
		})
	}
	handle("POST /v1/images/batches", a.submitBatchImage)
	handle("GET /v1/images/batches", a.listBatchImages)
	handle("GET /v1/images/batches/models", a.batchModels)
	handle("GET /v1/images/batches/{id}", func(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
		j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(j.public())
	})
	handle("GET /v1/images/batches/{id}/items", a.batchItems)
	handle("POST /v1/images/batches/{id}/cancel", a.cancelBatchImage)
	handle("DELETE /v1/images/batches/{id}", a.deleteBatchImage)
	handle("DELETE /v1/images/batches/{id}/outputs", a.deleteBatchOutputs)
	handle("GET /v1/images/batches/{id}/items/{custom_id}/content", a.batchImageContent)
	handle("GET /v1/images/batches/{id}/download", a.downloadBatchImages)
}
