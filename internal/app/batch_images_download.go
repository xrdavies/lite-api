package app

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

func (a *App) batchItems(w http.ResponseWriter, r *http.Request, g *gatewayIdentity) error {
	j, err := a.batchJob(r.Context(), r.PathValue("id"), g)
	if err != nil {
		return err
	}
	n, offset, err := batchPagination(r, 100, 500)
	if err != nil {
		return err
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	switch status {
	case "succeeded":
		status = "success"
	case "all":
		status = ""
	case "", "pending", "success", "failed":
	default:
		return bad("invalid batch item status")
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT jsonb_build_object('custom_id',custom_id,'status',CASE WHEN status='success' THEN 'succeeded' ELSE status END,'prompt_preview',prompt_preview,'mime_type',mime_type,'file_extension',file_extension,'image_count',image_count,'error',CASE WHEN status<>'failed' THEN NULL ELSE jsonb_build_object('code',COALESCE(error_code,''),'message',COALESCE(error_message,''),'source',CASE WHEN error_code IS NULL THEN '' WHEN COALESCE(provider_source_object,'')<>'' THEN 'provider' WHEN btrim(error_code) IN ('EMPTY_IMAGE_OUTPUT','PROVIDER_ITEM_FAILED') THEN 'provider' WHEN btrim(error_code) IN ('INDEX_OUTPUT_MISSING','INDEX_PARSE_FAILED','DUPLICATE_CUSTOM_ID_IN_OUTPUT') THEN 'system' ELSE '' END) END) FROM batch_image_items WHERE job_id=$1 AND ($2='' OR status=$2) ORDER BY id LIMIT $3 OFFSET $4`, j.ID, status, n+1, offset)
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
	for i, raw := range items {
		var item map[string]json.RawMessage
		if err = json.Unmarshal(raw, &item); err != nil {
			return err
		}
		var failure map[string]string
		if err = json.Unmarshal(item["error"], &failure); err != nil {
			return err
		}
		if failure == nil {
			continue
		}
		failure["message"] = batchErrorMessage(failure["message"])
		if failure["source"] == "" {
			delete(failure, "source")
		}
		item["error"], _ = json.Marshal(failure)
		items[i], err = json.Marshal(item)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items, "has_more": more})
}

func batchErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	for _, marker := range []string{"gs://", "files/", "projects/"} {
		if strings.Contains(message, marker) {
			return "upstream provider operation failed"
		}
	}
	message = cleanErrorMessage(message, "")
	return strings.ToValidUTF8(message[:min(len(message), 500)], "")
}

func batchFilename(id, extension string, index int) string {
	base := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, strings.TrimSpace(id))
	for strings.Contains(base, "..") {
		base = strings.ReplaceAll(base, "..", "_")
	}
	base = strings.Trim(strings.ToValidUTF8(base[:min(len(base), 120)], ""), ". ")
	if base == "" {
		base = "image"
	}
	if index > 0 {
		base += "_" + strconv.Itoa(index+1)
	}
	// Extensions come from the validated provider image MIME, never a URL or ID.
	return base + "." + extension
}

func (a *App) markBatchDownloaded(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.DB.ExecContext(ctx, "UPDATE batch_image_jobs SET downloaded_at=COALESCE(downloaded_at,now()) WHERE batch_id=$1", id); err != nil {
		// The download has already been sent; an error body would corrupt the file.
		slog.Error("batch download status update failed", "batch_id", id)
	}
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

// Downloads use the persisted index, so changed or missing provider images cannot
// turn a settled result into a successful but incomplete response.
func (a *App) readIndexedBatchOutput(ctx context.Context, j batchImageJob, u *upstreamAccount, visit func(int, batchImageResult) error) error {
	rows, err := a.DB.QueryContext(ctx, "SELECT custom_id,image_count,COALESCE(mime_type,'') FROM batch_image_items WHERE job_id=$1 ORDER BY id", j.ID)
	if err != nil {
		return err
	}
	type indexed struct {
		count, sequence int
		mime            string
	}
	expected := map[string]indexed{}
	sequence := 0
	for rows.Next() {
		var id string
		var item indexed
		if err = rows.Scan(&id, &item.count, &item.mime); err != nil {
			rows.Close()
			return err
		}
		if item.count > 0 {
			sequence++
			item.sequence = sequence
		}
		expected[id] = item
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	if err = a.readBatchOutput(ctx, u, j.Output, func(item batchImageResult) error {
		stored, ok := expected[item.ID]
		if !ok || seen[item.ID] || item.Count != stored.count || item.Count > 0 && item.MIME != stored.mime {
			return &apiError{502, "provider batch results changed"}
		}
		seen[item.ID] = true
		if item.Count > 0 {
			return visit(stored.sequence, item)
		}
		return nil
	}); err != nil {
		return err
	}
	for id, item := range expected {
		if item.count > 0 && !seen[id] {
			return &apiError{502, "batch download results are incomplete"}
		}
	}
	return nil
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
	var filename string
	err = a.readIndexedBatchOutput(r.Context(), j, u, func(_ int, item batchImageResult) error {
		if item.ID == id {
			data = item.Images[index]
			filename = batchFilename(item.ID, item.Extension, index)
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
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	if n, err := w.Write(data); err != nil || n != len(data) {
		slog.Error("batch image download interrupted", "batch_id", j.ID)
		return nil
	}
	a.markBatchDownloaded(r.Context(), j.ID)
	return nil
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
	limit := 200
	if raw := strings.TrimSpace(r.URL.Query().Get("max_items")); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return bad("max_items must be between 1 and 200")
		}
	}
	var count int
	if err = a.DB.QueryRowContext(r.Context(), "SELECT count(*) FROM batch_image_items WHERE job_id=$1 AND status='success'", j.ID).Scan(&count); err != nil {
		return err
	}
	if j.Success > limit || count > limit {
		return bad("batch ZIP exceeds max_items; download individual items")
	}
	rows, err := a.DB.QueryContext(r.Context(), "SELECT custom_id,COALESCE(error_code,''),COALESCE(error_message,'') FROM batch_image_items WHERE job_id=$1 AND status='failed' ORDER BY id", j.ID)
	if err != nil {
		return err
	}
	failures := []map[string]string{}
	for rows.Next() {
		var id, code, message string
		if err = rows.Scan(&id, &code, &message); err != nil {
			rows.Close()
			return err
		}
		failures = append(failures, map[string]string{"custom_id": id, "code": code, "message": batchErrorMessage(message)})
	}
	err = rows.Err()
	rows.Close()
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
	var total int64
	files := []map[string]any{}
	names := map[string]bool{}
	err = a.readIndexedBatchOutput(r.Context(), j, u, func(sequence int, item batchImageResult) error {
		for i, data := range item.Images {
			total += int64(len(data))
			if total > 256<<20 {
				return &apiError{413, "batch ZIP exceeds 256 MiB; download individual items"}
			}
			name := "images/" + batchFilename(item.ID, item.Extension, i)
			// Sanitization may collapse distinct custom IDs. Keep every file unique
			// and record the final path against its original ID in the manifest.
			for names[name] {
				name = fmt.Sprintf("images/%06d_%02d_", sequence, i+1) + strings.TrimPrefix(name, "images/")
			}
			names[name] = true
			part, err := archive.Create(name)
			if err != nil {
				return err
			}
			if _, err = part.Write(data); err != nil {
				return err
			}
			files = append(files, map[string]any{"custom_id": item.ID, "filename": name, "mime_type": item.MIME, "image_index": i})
		}
		return nil
	})
	if err != nil {
		archive.Close()
		return err
	}
	for _, entry := range []struct {
		name  string
		value any
	}{
		{"manifest.json", map[string]any{"batch_id": j.ID, "model": j.Model, "item_count": j.Count, "success_count": j.Success, "fail_count": j.Fail, "files": files}},
		{"errors.json", failures},
	} {
		part, err := archive.Create(entry.name)
		if err == nil {
			err = json.NewEncoder(part).Encode(entry.value)
		}
		if err != nil {
			archive.Close()
			return err
		}
	}
	if err = archive.Close(); err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > 256<<20 {
		return &apiError{413, "batch ZIP exceeds 256 MiB; download individual items"}
	}
	if _, err = file.Seek(0, 0); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+j.ID+`.zip"`)
	if n, err := file.WriteTo(w); err != nil || n != stat.Size() {
		slog.Error("batch ZIP download interrupted", "batch_id", j.ID)
		return nil
	}
	a.markBatchDownloaded(r.Context(), j.ID)
	return nil
}
