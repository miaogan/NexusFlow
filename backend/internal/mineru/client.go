package mineru

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Client is the MinerU parsing boundary. Processing Worker only depends on
// this interface, which keeps retries and polling testable.
type Client interface {
	Submit(ctx context.Context, taskID, fileURL, fileType string) (jobID string, err error)
	Poll(ctx context.Context, jobID string) (markdown string, done bool, err error)
}

// Document is a parsed result plus the images extracted from the result
// archive. Image keys are paths relative to the Markdown file (e.g.
// "images/1.jpg"), matching the links that MinerU writes. Layout is the raw
// content_list.json (reading order, block types and page boxes) when the
// archive includes one.
type Document struct {
	Markdown string
	Images   map[string][]byte
	Layout   []byte
}

// DocumentClient is implemented by parsers that also expose images. Callers
// should prefer ParseDocument, which falls back to Parse for plain parsers.
type DocumentClient interface {
	PollDocument(ctx context.Context, jobID string) (Document, bool, error)
}

const (
	maxResultImages  = 64
	maxResultImage   = 8 << 20
	maxResultImageIn = 32 << 20
	maxResultLayout  = 8 << 20
)

var resultImageExts = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".gif": {},
}

// HTTPClient implements the MinerU 4.x V1 Tasks protocol. Uploaded objects are
// read from NexusFlow's shared local volume and streamed to MinerU, so the
// parser does not need access to MinIO or to NexusFlow's internal object URL.
type HTTPClient struct {
	BaseURL     string
	APIKey      string
	Tier        string
	ObjectDir   string
	ObjectBase  string
	HTTP        *http.Client
	Interval    time.Duration
	MaxAttempts int

	mu       sync.Mutex
	textJobs map[string]string
}

const textJobPrefix = "nexusflow-text:"

func NewHTTPClient(baseURL string, interval time.Duration, maxAttempts int) *HTTPClient {
	return &HTTPClient{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		HTTP:        &http.Client{Timeout: 120 * time.Second},
		Interval:    interval,
		MaxAttempts: maxAttempts,
	}
}

type taskFile struct {
	FileID    string `json:"file_id"`
	FileName  string `json:"file_name"`
	Status    string `json:"status"`
	ResultURL string `json:"result_url"`
	Upload    *struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	} `json:"upload"`
	Error *taskError `json:"error"`
}

type taskError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *taskError) String() string {
	if e == nil {
		return "unknown error"
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// openObject maps only URLs below ObjectBase into ObjectDir. Both prefix and
// path traversal checks are required because the URL originates in a queue event.
func (c *HTTPClient) openObject(fileURL string) (*os.File, string, int64, error) {
	base, err := url.Parse(c.ObjectBase)
	if err != nil {
		return nil, "", 0, fmt.Errorf("mineru object base: %w", err)
	}
	source, err := url.Parse(fileURL)
	if err != nil {
		return nil, "", 0, fmt.Errorf("mineru object URL: %w", err)
	}
	prefix := strings.TrimRight(base.EscapedPath(), "/") + "/"
	if source.Scheme != base.Scheme || source.Host != base.Host || !strings.HasPrefix(source.EscapedPath(), prefix) {
		return nil, "", 0, fmt.Errorf("mineru object URL is outside configured object store")
	}
	key, err := url.PathUnescape(strings.TrimPrefix(source.EscapedPath(), prefix))
	if err != nil || key == "" || strings.ContainsRune(key, 0) {
		return nil, "", 0, fmt.Errorf("mineru object URL has an invalid object key")
	}
	name := path.Base(key)
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\:") {
		return nil, "", 0, fmt.Errorf("mineru object URL has an invalid file name")
	}
	full := filepath.Join(c.ObjectDir, filepath.FromSlash(key))
	rel, err := filepath.Rel(c.ObjectDir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, "", 0, fmt.Errorf("mineru object key escapes object store")
	}
	file, err := os.Open(full)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open uploaded object: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		file.Close()
		return nil, "", 0, fmt.Errorf("uploaded object is not a non-empty regular file")
	}
	return file, name, info.Size(), nil
}

func (c *HTTPClient) setAuth(req *http.Request, target string) {
	if c.APIKey == "" {
		return
	}
	base, baseErr := url.Parse(c.BaseURL)
	u, targetErr := url.Parse(target)
	if baseErr == nil && targetErr == nil && base.Scheme == u.Scheme && base.Host == u.Host {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
}

func responseError(operation string, resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("mineru %s: status %d: %s", operation, resp.StatusCode, strings.TrimSpace(string(raw)))
}

func (c *HTTPClient) Submit(ctx context.Context, taskID, fileURL, fileType string) (string, error) {
	file, name, size, err := c.openObject(fileURL)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// MinerU 4.x accepts documents and images, not plain Markdown/TXT. Preserve
	// NexusFlow's existing text uploads without manufacturing a parser request.
	switch strings.ToLower(fileType) {
	case "txt", "text", "md", "markdown":
		raw, err := io.ReadAll(file)
		if err != nil {
			return "", fmt.Errorf("read uploaded text: %w", err)
		}
		jobID := textJobPrefix + taskID
		c.mu.Lock()
		if c.textJobs == nil {
			c.textJobs = make(map[string]string)
		}
		c.textJobs[jobID] = string(raw)
		c.mu.Unlock()
		return jobID, nil
	}

	body, _ := json.Marshal(map[string]any{
		"files":          []map[string]any{{"file_name": name, "bytes": size}},
		"tier":           c.Tier,
		"output_formats": []string{"default"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req, c.BaseURL)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("mineru create task: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return "", responseError("create task", resp)
	}
	var created struct {
		TaskID string     `json:"task_id"`
		Files  []taskFile `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", fmt.Errorf("mineru create task response: %w", err)
	}
	if created.TaskID == "" || len(created.Files) != 1 || created.Files[0].Upload == nil || created.Files[0].Upload.URL == "" {
		return "", fmt.Errorf("mineru create task returned an invalid upload descriptor")
	}
	upload := created.Files[0].Upload
	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.URL, file)
	if err != nil {
		return "", err
	}
	uploadReq.ContentLength = size
	for key, value := range upload.Headers {
		uploadReq.Header.Set(key, value)
	}
	c.setAuth(uploadReq, upload.URL)
	uploadResp, err := c.HTTP.Do(uploadReq)
	if err != nil {
		return "", fmt.Errorf("mineru upload: %w", err)
	}
	defer uploadResp.Body.Close()
	if uploadResp.StatusCode < 200 || uploadResp.StatusCode >= 300 {
		return "", responseError("upload", uploadResp)
	}
	return created.TaskID, nil
}

func (c *HTTPClient) Poll(ctx context.Context, jobID string) (string, bool, error) {
	doc, done, err := c.PollDocument(ctx, jobID)
	return doc.Markdown, done, err
}

func (c *HTTPClient) PollDocument(ctx context.Context, jobID string) (Document, bool, error) {
	if strings.HasPrefix(jobID, textJobPrefix) {
		c.mu.Lock()
		markdown, ok := c.textJobs[jobID]
		delete(c.textJobs, jobID)
		c.mu.Unlock()
		if !ok {
			return Document{}, true, fmt.Errorf("mineru text job %s not found", jobID)
		}
		return Document{Markdown: markdown}, true, nil
	}
	target := c.BaseURL + "/v1/tasks/" + url.PathEscape(jobID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Document{}, false, err
	}
	c.setAuth(req, target)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Document{}, false, fmt.Errorf("mineru poll: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Document{}, false, responseError("poll", resp)
	}
	var task struct {
		Status string     `json:"status"`
		Files  []taskFile `json:"files"`
		Error  *taskError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return Document{}, false, fmt.Errorf("mineru poll response: %w", err)
	}
	switch task.Status {
	case "failed", "canceled", "partial":
		detail := task.Error
		if len(task.Files) == 1 && task.Files[0].Error != nil {
			detail = task.Files[0].Error
		}
		return Document{}, true, fmt.Errorf("mineru task %s: %s", task.Status, detail.String())
	case "completed":
		if len(task.Files) != 1 || task.Files[0].Status != "completed" || task.Files[0].ResultURL == "" {
			return Document{}, true, fmt.Errorf("mineru completed task has no result URL")
		}
		doc, err := c.downloadResult(ctx, task.Files[0].ResultURL)
		return doc, true, err
	default:
		if len(task.Files) == 1 && (task.Files[0].Status == "failed" || task.Files[0].Status == "canceled") {
			return Document{}, true, fmt.Errorf("mineru file %s: %s", task.Files[0].Status, task.Files[0].Error.String())
		}
		return Document{}, false, nil
	}
}

func (c *HTTPClient) downloadResult(ctx context.Context, resultURL string) (Document, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
	if err != nil {
		return Document{}, err
	}
	c.setAuth(req, resultURL)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Document{}, fmt.Errorf("mineru download result: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Document{}, responseError("download result", resp)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return Document{}, fmt.Errorf("mineru read result: %w", err)
	}
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return Document{}, fmt.Errorf("mineru result is not a ZIP archive: %w", err)
	}

	// Read the Markdown first so images can be named relative to it.
	doc := Document{Images: make(map[string][]byte)}
	markdownDir := ""
	markdownName := ""
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || !strings.EqualFold(path.Ext(entry.Name), ".md") {
			continue
		}
		markdown, readErr := readZipEntry(entry, 0)
		if readErr != nil {
			return Document{}, fmt.Errorf("mineru open Markdown result: %w", readErr)
		}
		if len(markdown) == 0 {
			return Document{}, fmt.Errorf("mineru returned empty Markdown")
		}
		doc.Markdown = string(markdown)
		markdownName = entry.Name
		if dir := path.Dir(entry.Name); dir != "." && dir != "/" {
			markdownDir = strings.TrimPrefix(path.Clean("/"+dir), "/")
		}
		break
	}
	if doc.Markdown == "" {
		return Document{}, fmt.Errorf("mineru result ZIP contains no Markdown")
	}
	doc.Layout = readLayout(archive, markdownDir, markdownName)

	total := 0
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() || len(doc.Images) >= maxResultImages {
			continue
		}
		name, ok := safeResultPath(entry.Name)
		if !ok {
			continue
		}
		if markdownDir != "" && strings.HasPrefix(name, markdownDir+"/") {
			name = strings.TrimPrefix(name, markdownDir+"/")
		}
		if _, ok := resultImageExts[strings.ToLower(path.Ext(name))]; !ok {
			continue
		}
		if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maxResultImage {
			continue
		}
		if total+int(entry.UncompressedSize64) > maxResultImageIn {
			continue
		}
		data, readErr := readZipEntry(entry, maxResultImage)
		if readErr != nil || len(data) == 0 {
			continue
		}
		doc.Images[name] = data
		total += len(data)
	}
	return doc, nil
}

// readLayout returns MinerU's content_list.json from the result archive,
// preferring the file that sits next to the parsed Markdown.
func readLayout(archive *zip.Reader, markdownDir, markdownName string) []byte {
	preferred := ""
	if markdownName != "" {
		preferred = strings.TrimSuffix(path.Base(markdownName), path.Ext(markdownName)) + "_content_list.json"
	}
	var fallback *zip.File
	for _, entry := range archive.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		normalized := strings.ReplaceAll(entry.Name, "\\", "/")
		base := path.Base(normalized)
		lowered := strings.ToLower(base)
		if lowered != "content_list.json" && !strings.HasSuffix(lowered, "_content_list.json") {
			continue
		}
		if preferred != "" && base == preferred && inDir(entry.Name, markdownDir) {
			return readLayoutEntry(entry)
		}
		if fallback == nil {
			fallback = entry
		}
	}
	if fallback == nil {
		return nil
	}
	return readLayoutEntry(fallback)
}

func inDir(name, dir string) bool {
	if dir == "" {
		return true
	}
	clean := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, "\\", "/")), "/")
	return strings.HasPrefix(clean, dir+"/")
}

func readLayoutEntry(entry *zip.File) []byte {
	if entry.UncompressedSize64 == 0 || entry.UncompressedSize64 > maxResultLayout {
		return nil
	}
	data, err := readZipEntry(entry, maxResultLayout)
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

// safeResultPath rejects absolute and traversal paths inside the result ZIP.
func safeResultPath(raw string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(raw, "\\", "/")), "/")
	if name == "" || name == "." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
		return "", false
	}
	return name, true
}

func readZipEntry(entry *zip.File, limit int64) ([]byte, error) {
	r, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var reader io.Reader = r
	if limit > 0 {
		reader = io.LimitReader(r, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("zip entry %s exceeds %d bytes", entry.Name, limit)
	}
	return data, nil
}

// Parse submits a job and polls until it finishes, respecting context
// cancellation and the configured retry budget.
func Parse(ctx context.Context, c Client, taskID, fileURL, fileType string, interval time.Duration, maxAttempts int) (string, error) {
	jobID, err := c.Submit(ctx, taskID, fileURL, fileType)
	if err != nil {
		return "", err
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if maxAttempts <= 0 {
		maxAttempts = 60
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
		md, done, err := c.Poll(ctx, jobID)
		if err != nil {
			return "", err
		}
		if done {
			return md, nil
		}
	}
	return "", fmt.Errorf("mineru job %s timed out after %d polls", jobID, maxAttempts)
}

// ParseDocument is Parse plus extracted images. Parsers that do not implement
// DocumentClient still work; they simply return Markdown without media.
func ParseDocument(
	ctx context.Context, c Client, taskID, fileURL, fileType string,
	interval time.Duration, maxAttempts int,
) (Document, error) {
	parser, ok := c.(DocumentClient)
	if !ok {
		markdown, err := Parse(ctx, c, taskID, fileURL, fileType, interval, maxAttempts)
		return Document{Markdown: markdown}, err
	}
	jobID, err := c.Submit(ctx, taskID, fileURL, fileType)
	if err != nil {
		return Document{}, err
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if maxAttempts <= 0 {
		maxAttempts = 60
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return Document{}, ctx.Err()
		case <-ticker.C:
		}
		doc, done, pollErr := parser.PollDocument(ctx, jobID)
		if pollErr != nil {
			return Document{}, pollErr
		}
		if done {
			return doc, nil
		}
	}
	return Document{}, fmt.Errorf("mineru job %s timed out after %d polls", jobID, maxAttempts)
}

// MockClient returns a fixed Markdown document and is used when no MinerU
// endpoint is configured, so that local demos run end to end. When ReadFile is
// set, uploaded text-like files are indexed verbatim (making upload→retrieval
// demos use real content); unsupported file types fall back to Markdown.
type MockClient struct {
	Markdown string
	ReadFile func(fileURL, fileType string) (string, error)

	mu   sync.Mutex
	jobs map[string]fileRef
}

type fileRef struct {
	url string
	typ string
}

func (m *MockClient) Submit(_ context.Context, taskID, fileURL, fileType string) (string, error) {
	m.mu.Lock()
	if m.jobs == nil {
		m.jobs = make(map[string]fileRef)
	}
	m.jobs[taskID] = fileRef{url: fileURL, typ: fileType}
	m.mu.Unlock()
	return taskID, nil
}

func (m *MockClient) Poll(_ context.Context, jobID string) (string, bool, error) {
	if m.ReadFile != nil {
		m.mu.Lock()
		ref := m.jobs[jobID]
		m.mu.Unlock()
		if content, err := m.ReadFile(ref.url, ref.typ); err == nil && content != "" {
			return content, true, nil
		}
	}
	return m.Markdown, true, nil
}
