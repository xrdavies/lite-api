package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
)

type batchImageRequest struct {
	Model    string            `json:"model"`
	TaskName string            `json:"task_name"`
	Parent   string            `json:"parent_batch_id"`
	Provider string            `json:"provider"`
	Items    []batchImageInput `json:"items"`
	MIME     string            `json:"response_mime_type"`
	Aspect   string            `json:"aspect_ratio"`
	Size     string            `json:"image_size"`
	Metadata map[string]string `json:"metadata"`
}
type batchImageInput struct {
	ID         string                `json:"custom_id"`
	Prompt     string                `json:"prompt"`
	Count      int                   `json:"output_count,omitempty"`
	References []batchImageReference `json:"reference_images,omitempty"`
}
type batchImageReference struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
	MIME string `json:"mime_type"`
	Data []byte `json:"data,omitempty"`
	URI  string `json:"file_uri,omitempty"`
}

func (in *batchImageRequest) normalize() error {
	in.Model, in.TaskName, in.Parent = strings.TrimSpace(in.Model), strings.TrimSpace(in.TaskName), strings.TrimSpace(in.Parent)
	if !validNativeModel(in.Model) || len(in.Model) > 100 {
		return bad("invalid batch image model")
	}
	if in.Provider != "" && in.Provider != "gemini_api" {
		return bad("batch images require a Gemini API key provider")
	}
	in.Provider = "gemini_api"
	if len([]rune(in.TaskName)) > 255 || in.Parent != "" && !validNativeModel(in.Parent) {
		return bad("invalid batch task name or parent")
	}
	if in.Size == "" {
		in.Size = "1K"
	}
	if !strings.EqualFold(in.Size, "1K") {
		return bad("batch images require image_size=1K")
	}
	in.Size = "1K"
	if in.MIME == "" {
		in.MIME = "image/png"
	}
	if in.MIME != "image/png" {
		return bad("batch response_mime_type must be image/png")
	}
	if in.Aspect != "" {
		allowed := false
		for _, v := range []string{"1:1", "2:3", "3:2", "3:4", "4:3", "4:5", "5:4", "9:16", "16:9", "21:9"} {
			allowed = allowed || v == in.Aspect
		}
		if !allowed {
			return bad("invalid batch aspect_ratio")
		}
	}
	if len(in.Items) == 0 || len(in.Items) > 200 || len(in.Metadata) > 20 {
		return bad("invalid batch item or metadata count")
	}
	for k, v := range in.Metadata {
		if len(k) > 128 || len(v) > 1024 {
			return bad("batch metadata exceeds limit")
		}
	}
	items := []batchImageInput{}
	seen := map[string]bool{}
	referenceBytes, referenceCount := 0, 0
	for i, item := range in.Items {
		item.ID = strings.TrimSpace(item.ID)
		item.Prompt = strings.TrimSpace(item.Prompt)
		if item.ID == "" {
			item.ID = fmt.Sprintf("item_%06d", i+1)
		}
		if len(item.ID) > 250 || strings.ContainsAny(item.ID, "\r\n\x00") || item.Prompt == "" || len(item.Prompt) > 8000 {
			return bad("invalid batch custom_id or prompt")
		}
		if item.Count == 0 {
			item.Count = 1
		}
		if item.Count < 1 || item.Count > 4 || len(items)+item.Count > 200 {
			return bad("batch output count exceeds limit")
		}
		maxRefs := 0
		if strings.Contains(in.Model, "pro-image") {
			maxRefs = 14
		} else if strings.Contains(in.Model, "flash-image") {
			maxRefs = 3
		}
		if len(item.References) > maxRefs {
			return bad("too many reference images for batch model")
		}
		for j := range item.References {
			ref := &item.References[j]
			ref.MIME = strings.ToLower(strings.TrimSpace(ref.MIME))
			if ref.MIME == "image/jpg" {
				ref.MIME = "image/jpeg"
			}
			ref.URI = strings.TrimSpace(ref.URI)
			if ref.MIME != "image/png" && ref.MIME != "image/jpeg" && ref.MIME != "image/webp" || len(ref.Data) > 10<<20 || (len(ref.Data) == 0) == (ref.URI == "") {
				return bad("invalid reference image")
			}
			if ref.URI != "" && (!strings.HasPrefix(ref.URI, "gs://") || strings.ContainsAny(ref.URI, "\r\n\x00") || len(ref.URI) > 2048) {
				return bad("reference file_uri must be a GCS URI")
			}
			referenceBytes += len(ref.Data) * item.Count
			referenceCount += item.Count
		}
		for repeat := 1; repeat <= item.Count; repeat++ {
			copy := item
			copy.Count = 0
			if item.Count > 1 {
				copy.ID = fmt.Sprintf("%s_%02d", item.ID, repeat)
			}
			if seen[copy.ID] {
				return bad("duplicate batch custom_id")
			}
			seen[copy.ID] = true
			items = append(items, copy)
		}
	}
	if referenceBytes > 128<<20 || referenceCount > 1000 {
		return bad("batch references exceed limit")
	}
	in.Items = items
	return nil
}

func (in batchImageRequest) jsonl() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, item := range in.Items {
		parts := []any{map[string]any{"text": item.Prompt}}
		for _, ref := range item.References {
			if len(ref.Data) > 0 {
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": ref.MIME, "data": ref.Data}})
			} else {
				parts = append(parts, map[string]any{"fileData": map[string]any{"mimeType": ref.MIME, "fileUri": ref.URI}})
			}
		}
		image := map[string]any{"imageSize": in.Size}
		if in.Aspect != "" {
			image["aspectRatio"] = in.Aspect
		}
		line := map[string]any{"key": item.ID, "request": map[string]any{"contents": []any{map[string]any{"parts": parts}}, "generationConfig": map[string]any{"responseModalities": []string{"TEXT", "IMAGE"}, "imageConfig": image}}}
		if err := enc.Encode(line); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// Resource names are never interpreted as URLs or arbitrary upstream paths.
func batchResource(name, kind string) bool {
	value, ok := strings.CutPrefix(name, kind+"/")
	return ok && validNativeModel(value) && !strings.Contains(value, "/")
}

type geminiBatchJob struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	DisplayName string `json:"displayName"`
	Metadata    struct {
		State       string `json:"state"`
		DisplayName string `json:"displayName"`
	} `json:"metadata"`
	Dest struct {
		File  string `json:"fileName"`
		Snake string `json:"file_name"`
	} `json:"dest"`
	Response struct {
		File  string `json:"responsesFile"`
		Snake string `json:"responses_file"`
	} `json:"response"`
	Error json.RawMessage `json:"error"`
}

func (j geminiBatchJob) state() string {
	if j.State != "" {
		return j.State
	}
	return j.Metadata.State
}
func (j geminiBatchJob) output() string {
	for _, s := range []string{j.Dest.File, j.Dest.Snake, j.Response.File, j.Response.Snake} {
		if s != "" {
			return s
		}
	}
	return ""
}
func (j geminiBatchJob) display() string {
	if j.DisplayName != "" {
		return j.DisplayName
	}
	return j.Metadata.DisplayName
}

// Shared transport supplies DNS pinning, proxy policy and redirect rejection.
// Native files endpoints live outside /v1beta, so remove that suffix once here.
func (a *App) batchProviderRequest(ctx context.Context, u *upstreamAccount, method, path, contentType string, body []byte) (*http.Response, error) {
	if u.Platform != "gemini" || u.Type != "apikey" || u.protocol() != "gemini" {
		return nil, bad("batch provider requires a native Gemini API key account")
	}
	base, err := u.baseURL()
	if err != nil {
		return nil, err
	}
	endpoint, err := upstreamURL(strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1beta"), path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Goog-Api-Key", credentialString(u.Credentials, "api_key"))
	req.Header.Set("User-Agent", "lite-api/1")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	tr, err := a.upstreamTransport(ctx, u, req)
	if err != nil {
		return nil, err
	}
	client := http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, &apiError{502, "batch provider connection failed"}
	}
	resp.Body = &transportBody{ReadCloser: resp.Body, transport: tr}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		if method == http.MethodDelete && resp.StatusCode == 404 {
			return nil, nil
		}
		return nil, &apiError{502, fmt.Sprintf("batch provider rejected request (HTTP %d)", resp.StatusCode)}
	}
	return resp, nil
}
func (a *App) batchProviderJSON(ctx context.Context, u *upstreamAccount, method, path, kind string, body []byte, out any) error {
	resp, err := a.batchProviderRequest(ctx, u, method, path, kind, body)
	if err != nil || resp == nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 || json.Unmarshal(raw, out) != nil {
		return &apiError{502, "invalid batch provider response"}
	}
	return nil
}
func (a *App) uploadBatchInput(ctx context.Context, u *upstreamAccount, id string, input []byte) (string, error) {
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	meta, err := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="metadata"`}, "Content-Type": {"application/json"}})
	if err != nil {
		return "", err
	}
	_ = json.NewEncoder(meta).Encode(map[string]any{"file": map[string]string{"displayName": id, "mimeType": "application/jsonl"}})
	file, err := form.CreatePart(textproto.MIMEHeader{"Content-Disposition": {`form-data; name="file"; filename="batch.jsonl"`}, "Content-Type": {"application/jsonl"}})
	if err != nil {
		return "", err
	}
	_, _ = file.Write(input)
	if err = form.Close(); err != nil {
		return "", err
	}
	var result struct {
		Name string `json:"name"`
		File struct {
			Name string `json:"name"`
		} `json:"file"`
	}
	err = a.batchProviderJSON(ctx, u, "POST", "/upload/v1beta/files?uploadType=multipart", form.FormDataContentType(), buf.Bytes(), &result)
	if err != nil {
		return "", err
	}
	if result.File.Name != "" {
		result.Name = result.File.Name
	}
	if !batchResource(result.Name, "files") {
		return "", &apiError{502, "invalid batch input file name"}
	}
	return result.Name, nil
}
func (a *App) createProviderBatch(ctx context.Context, u *upstreamAccount, id, model, file string) (geminiBatchJob, error) {
	var job geminiBatchJob
	if !validNativeModel(model) || !batchResource(file, "files") {
		return job, bad("invalid batch model or file")
	}
	body, _ := json.Marshal(map[string]any{"batch": map[string]any{"displayName": id, "inputConfig": map[string]string{"fileName": file}}})
	err := a.batchProviderJSON(ctx, u, "POST", "/v1beta/models/"+model+":batchGenerateContent", "application/json", body, &job)
	if err == nil && !batchResource(job.Name, "batches") {
		err = &apiError{502, "invalid batch provider job name"}
	}
	return job, err
}
func (a *App) getProviderBatch(ctx context.Context, u *upstreamAccount, name string) (geminiBatchJob, error) {
	var job geminiBatchJob
	if !batchResource(name, "batches") {
		return job, bad("invalid provider batch name")
	}
	err := a.batchProviderJSON(ctx, u, "GET", "/v1beta/"+name, "", nil, &job)
	if err == nil && job.Name != name {
		err = &apiError{502, "batch provider returned a different job"}
	}
	return job, err
}
func (a *App) findProviderBatch(ctx context.Context, u *upstreamAccount, id string) (geminiBatchJob, error) {
	var found geminiBatchJob
	next := ""
	for pages := 0; pages < 100; pages++ {
		var result struct {
			Batches    []geminiBatchJob `json:"batches"`
			Operations []geminiBatchJob `json:"operations"`
			Next       string           `json:"nextPageToken"`
		}
		path := "/v1beta/batches?pageSize=100"
		if next != "" {
			path += "&pageToken=" + url.QueryEscape(next)
		}
		if err := a.batchProviderJSON(ctx, u, "GET", path, "", nil, &result); err != nil {
			return found, err
		}
		for _, job := range append(result.Batches, result.Operations...) {
			if job.display() == id {
				if found.Name != "" && found.Name != job.Name || !batchResource(job.Name, "batches") {
					return found, conflict("ambiguous recovered batch job")
				}
				found = job
			}
		}
		if result.Next == "" {
			return found, nil
		}
		if result.Next == next {
			return found, &apiError{502, "invalid batch pagination"}
		}
		next = result.Next
	}
	return found, &apiError{502, "batch recovery listing exceeds limit"}
}
func (a *App) cancelProviderBatch(ctx context.Context, u *upstreamAccount, name string) error {
	if !batchResource(name, "batches") {
		return bad("invalid provider batch name")
	}
	return a.batchProviderJSON(ctx, u, "POST", "/v1beta/"+name+":cancel", "application/json", []byte(`{}`), nil)
}
func (a *App) deleteBatchFile(ctx context.Context, u *upstreamAccount, name string) error {
	if name == "" {
		return nil
	}
	if !batchResource(name, "files") {
		return bad("invalid batch file name")
	}
	return a.batchProviderJSON(ctx, u, "DELETE", "/v1beta/"+name, "", nil, nil)
}

type batchImageResult struct {
	ID                             string
	Offset, Length                 int64
	Line, Count                    int
	MIME, Extension, Code, Message string
	Images                         [][]byte
}

func parseBatchImageLine(raw []byte) (batchImageResult, error) {
	var out batchImageResult
	var line struct {
		Key      string          `json:"key"`
		CustomID string          `json:"custom_id"`
		Error    json.RawMessage `json:"error"`
		Status   json.RawMessage `json:"status"`
		Response struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Inline struct {
							MIME string `json:"mimeType"`
							Data []byte `json:"data"`
						} `json:"inlineData"`
						Snake struct {
							MIME string `json:"mime_type"`
							Data []byte `json:"data"`
						} `json:"inline_data"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &line) != nil {
		return out, &apiError{502, "invalid batch result line"}
	}
	out.ID = line.Key
	if out.ID == "" {
		out.ID = line.CustomID
	}
	if out.ID == "" || len(out.ID) > 255 {
		return out, &apiError{502, "missing batch result key"}
	}
	for _, candidate := range line.Response.Candidates {
		for _, part := range candidate.Content.Parts {
			mime, data := part.Inline.MIME, part.Inline.Data
			if len(data) == 0 {
				mime, data = part.Snake.MIME, part.Snake.Data
			}
			if len(data) == 0 {
				continue
			}
			ext := map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}[mime]
			if ext == "" || http.DetectContentType(data) != mime || len(data) > 32<<20 || len(out.Images) >= 4 {
				return out, &apiError{502, "invalid batch image data"}
			}
			if out.MIME == "" {
				out.MIME, out.Extension = mime, ext
			}
			out.Images = append(out.Images, data)
		}
	}
	out.Count = len(out.Images)
	if out.Count == 0 {
		out.Code, out.Message = "EMPTY_IMAGE_OUTPUT", "provider returned no image"
		if len(line.Error) > 0 && string(line.Error) != "null" || len(line.Status) > 0 && string(line.Status) != "null" {
			out.Code, out.Message = "PROVIDER_ITEM_FAILED", "provider could not generate this item"
		}
	}
	return out, nil
}

// Parse one bounded JSONL line at a time. Raw provider data never enters SQL.
func (a *App) readBatchOutput(ctx context.Context, u *upstreamAccount, file string, visit func(batchImageResult) error) error {
	if !batchResource(file, "files") {
		return bad("invalid batch output file")
	}
	resp, err := a.batchProviderRequest(ctx, u, "GET", "/download/v1beta/"+file+":download?alt=media", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, (512<<20)+1))
	scanner.Buffer(make([]byte, 64<<10), 48<<20)
	var offset int64
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		length := int64(len(raw))
		if offset+length > 512<<20 || line > 1000 {
			return &apiError{502, "batch results exceed limit"}
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			item, err := parseBatchImageLine(raw)
			if err != nil {
				return err
			}
			item.Offset, item.Length, item.Line = offset, length, line
			if err = visit(item); err != nil {
				return err
			}
		}
		offset += length + 1
	}
	if scanner.Err() != nil {
		return &apiError{502, "batch output interrupted or oversized"}
	}
	return nil
}
