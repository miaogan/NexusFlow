package mineru

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// HTTPClient talks to a MinerU-compatible HTTP service.
type HTTPClient struct {
	BaseURL     string
	HTTP        *http.Client
	Interval    time.Duration
	MaxAttempts int
}

func NewHTTPClient(baseURL string, interval time.Duration, maxAttempts int) *HTTPClient {
	return &HTTPClient{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		HTTP:        &http.Client{Timeout: 120 * time.Second},
		Interval:    interval,
		MaxAttempts: maxAttempts,
	}
}

type submitBody struct {
	TaskID   string `json:"task_id"`
	FileURL  string `json:"file_url"`
	FileType string `json:"file_type"`
}

func (c *HTTPClient) Submit(ctx context.Context, taskID, fileURL, fileType string) (string, error) {
	body, _ := json.Marshal(submitBody{TaskID: taskID, FileURL: fileURL, FileType: fileType})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/parse", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("mineru submit: status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		JobID string `json:"job_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.JobID == "" {
		return "", fmt.Errorf("mineru submit: empty job_id")
	}
	return out.JobID, nil
}

func (c *HTTPClient) Poll(ctx context.Context, jobID string) (string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/parse/"+jobID, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", false, fmt.Errorf("mineru poll: status %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Status   string `json:"status"`
		Markdown string `json:"markdown"`
		Error    string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	switch out.Status {
	case "done", "succeeded":
		return out.Markdown, true, nil
	case "failed", "error":
		return "", true, fmt.Errorf("mineru job failed: %s", out.Error)
	default:
		return "", false, nil
	}
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
