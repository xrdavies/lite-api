package app

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

func (a *App) batchItems(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return err
	}
	n, offset, err := batchPagination(r)
	if err != nil {
		return err
	}
	status := r.URL.Query().Get("status")
	if status == "succeeded" {
		status = "success"
	}
	if status == "all" {
		status = ""
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT jsonb_build_object('custom_id',custom_id,'status',CASE WHEN status='success' THEN 'succeeded' ELSE status END,'prompt_preview',prompt_preview,'mime_type',mime_type,'file_extension',file_extension,'image_count',image_count,'error',CASE WHEN error_code IS NULL THEN NULL ELSE jsonb_build_object('code',error_code,'message',error_message) END) FROM batch_image_items WHERE job_id=$1 AND ($2='' OR status=$2) ORDER BY id LIMIT $3 OFFSET $4`, j.ID, status, n+1, offset)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	more := len(items) > n
	if more {
		items = items[:n]
	}
	return json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items, "has_more": more})
}
func (a *App) batchDownloadAccount(r *http.Request, g *gatewayIdentity) (batchImageJob, *upstreamAccount, error) {
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return j, nil, err
	}
	if j.OutputDeleted != nil || j.Expires != nil && !time.Now().Before(*j.Expires) {
		return j, nil, &apiError{410, "batch output has expired or been deleted"}
	}
	if j.Status != "completed" {
		return j, nil, conflict("batch results are not ready")
	}
	snap, err := a.batchSnapshot(r.Context(), j.ID)
	if err != nil {
		return j, nil, err
	}
	u, err := a.batchAccount(r.Context(), j, snap)
	return j, u, err
}
func (a *App) batchImageContent(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	if !a.takeSlot("batch-download", g.UserID, 1) {
		return &apiError{429, "another batch download is in progress"}
	}
	defer a.releaseSlot("batch-download", g.UserID)
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	j, u, err := a.batchDownloadAccount(r, g)
	if err != nil {
		return err
	}
	index := 0
	if v := r.URL.Query().Get("image_index"); v != "" {
		index, err = strconv.Atoi(v)
		if err != nil || index < 0 || index > 3 {
			return bad("invalid image_index")
		}
	}
	id := r.PathValue("custom_id")
	var status string
	var count int
	if err = a.DB.QueryRowContext(r.Context(), "SELECT status,image_count FROM batch_image_items WHERE job_id=$1 AND custom_id=$2", j.ID, id).Scan(&status, &count); err != nil {
		return err
	}
	if status != "success" || index >= count {
		return missing()
	}
	var data []byte
	err = a.readBatchOutput(r.Context(), u, j.Output, func(item batchImageResult) error {
		if item.ID == id {
			if index >= len(item.Images) {
				return &apiError{502, "provider image result changed"}
			}
			if data != nil {
				return &apiError{502, "duplicate batch result"}
			}
			data = item.Images[index]
		}
		return nil
	})
	if err != nil {
		return err
	}
	if data == nil {
		return missing()
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", `inline; filename="image"`)
	_, err = w.Write(data)
	return err
}
func (a *App) downloadBatchImages(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	if !a.takeSlot("batch-download", g.UserID, 1) {
		return &apiError{429, "another batch download is in progress"}
	}
	defer a.releaseSlot("batch-download", g.UserID)
	a.batchMu.Lock()
	defer a.batchMu.Unlock()
	j, u, err := a.batchDownloadAccount(r, g)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "lite-api-batch-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	archive := zip.NewWriter(file)
	rows, err := a.DB.QueryContext(r.Context(), "SELECT custom_id FROM batch_image_items WHERE job_id=$1 AND status='success' ORDER BY id", j.ID)
	if err != nil {
		return err
	}
	expected := map[string]int{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expected[id] = len(expected) + 1
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	var total int64
	err = a.readBatchOutput(r.Context(), u, j.Output, func(item batchImageResult) error {
		sequence := expected[item.ID]
		if sequence == 0 {
			return nil
		}
		if seen[item.ID] {
			return &apiError{502, "duplicate batch result"}
		}
		seen[item.ID] = true
		for i, data := range item.Images {
			total += int64(len(data))
			if total > 256<<20 {
				return &apiError{413, "batch ZIP exceeds 256 MiB; download individual items"}
			}
			// Numeric filenames cannot introduce client-controlled archive paths.
			part, err := archive.Create(fmt.Sprintf("%06d_%02d.%s", sequence, i+1, item.Extension))
			if err != nil {
				return err
			}
			if _, err = part.Write(data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		archive.Close()
		return err
	}
	if len(seen) != len(expected) {
		archive.Close()
		return &apiError{502, "batch download results are incomplete"}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+j.ID+`.zip"`)
	if _, err = file.WriteTo(w); err != nil {
		return err
	}
	_, err = a.DB.ExecContext(r.Context(), "UPDATE batch_image_jobs SET downloaded_at=COALESCE(downloaded_at,now()) WHERE batch_id=$1", j.ID)
	return err
}
