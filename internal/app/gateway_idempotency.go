package app

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
)

type gatewayResponse struct {
	http.ResponseWriter
	status    int
	body      bytes.Buffer
	oversized bool
	succeeded bool
}

func (w *gatewayResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *gatewayResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.body.Len()+len(b) > 16<<20 {
		w.oversized = true
		w.body.Reset()
	}
	if !w.oversized {
		_, _ = w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}
func (w *gatewayResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *gatewayResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Claim and completion use short transactions. A surviving "processing" row means
// the previous process may have sent an upstream request, so it is never stolen.
func (a *App) claimGatewayRequest(w http.ResponseWriter, r *http.Request, g *gatewayIdentity, operation, payload string, stream bool) (*gatewayResponse, func(), bool, error) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		return nil, nil, false, nil
	}
	if len(key) > 128 {
		return nil, nil, false, bad("invalid Idempotency-Key")
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return nil, nil, false, bad("invalid Idempotency-Key")
		}
	}
	scope := fmt.Sprintf("gateway.%s.%d", operation, g.Key.ID)
	keyHash := digest(key)
	fingerprint := digest(operation + "\n" + payload)
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return nil, nil, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", scope+keyHash); err != nil {
		return nil, nil, false, err
	}
	var stored, status string
	var responseStatus sql.NullInt64
	var body sql.NullString
	err = tx.QueryRowContext(r.Context(), "SELECT request_fingerprint,status,response_status,response_body FROM idempotency_records WHERE scope=$1 AND idempotency_key_hash=$2 AND (expires_at>now() OR status='processing')", scope, keyHash).Scan(&stored, &status, &responseStatus, &body)
	if err == nil {
		if stored != fingerprint {
			return nil, nil, false, conflict("Idempotency-Key reused with different request")
		}
		if status == "processing" {
			return nil, nil, false, conflict("request is in progress or requires review; it will not be sent twice")
		}
		if !body.Valid {
			return nil, nil, false, conflict("request already completed; response is too large to replay")
		}
		// Do not hold a database connection or lock while replaying to a slow client.
		if err = tx.Rollback(); err != nil {
			return nil, nil, false, err
		}
		w.Header().Set("Idempotency-Replayed", "true")
		contentType := "application/json"
		if stream && responseStatus.Int64 == 200 {
			contentType = "text/event-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(int(responseStatus.Int64))
		_, err = w.Write([]byte(body.String))
		return nil, nil, true, err
	}
	if err != sql.ErrNoRows {
		return nil, nil, false, err
	}
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO idempotency_records(scope,idempotency_key_hash,request_fingerprint,status,expires_at) VALUES($1,$2,$3,'processing',now()+interval '24 hours') ON CONFLICT(scope,idempotency_key_hash) DO UPDATE SET request_fingerprint=EXCLUDED.request_fingerprint,status='processing',response_status=NULL,response_body=NULL,error_reason=NULL,expires_at=EXCLUDED.expires_at,updated_at=now()`, scope, keyHash, fingerprint); err != nil {
		return nil, nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, nil, false, err
	}
	writer := &gatewayResponse{ResponseWriter: w}
	finish := func() {
		code := writer.status
		if code == 0 {
			code = 500
		}
		state := "failed"
		if code >= 200 && code < 300 && writer.succeeded {
			state = "succeeded"
		}
		var response any
		if !writer.oversized {
			response = writer.body.String()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// On a persistence failure the original processing row remains, preventing an unsafe replay.
		_, _ = a.DB.ExecContext(ctx, `UPDATE idempotency_records SET status=$3,response_status=$4,response_body=$5,updated_at=now() WHERE scope=$1 AND idempotency_key_hash=$2 AND request_fingerprint=$6 AND status='processing'`, scope, keyHash, state, code, response, fingerprint)
	}
	return writer, finish, false, nil
}
