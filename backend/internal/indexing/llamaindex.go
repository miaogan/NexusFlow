package indexing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LlamaIndexIndexer implements DocumentIndexer by calling the LlamaIndex
// service's /v1/index endpoint, which performs real chunking, embedding and
// Qdrant upsert in production. It replaces the in-memory DocStore used by
// dev-all while keeping the same state-machine driver.
type LlamaIndexIndexer struct {
	baseURL string
	client  *http.Client
}

func NewLlamaIndexIndexer(baseURL string, timeout time.Duration) *LlamaIndexIndexer {
	if timeout <= 0 {
		timeout = 120 * time.Second // embedding batches can be slow
	}
	return &LlamaIndexIndexer{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: timeout},
	}
}

type llamaIndexIndexRequest struct {
	TaskID        string            `json:"task_id"`
	Markdown      string            `json:"markdown"`
	SplitStrategy string            `json:"split_strategy"`
	Metadata      map[string]string `json:"metadata"`
}

type llamaIndexIndexResponse struct {
	Success      bool   `json:"success"`
	ChunkCount   int    `json:"chunk_count"`
	IndexedCount int    `json:"indexed_count"`
	Collection   string `json:"collection"`
	ErrorMsg     string `json:"error_msg"`
}

type llamaIndexDeleteRequest struct {
	TaskID string `json:"task_id"`
	KBID   string `json:"kb_id"`
}

type llamaIndexDeleteResponse struct {
	Success    bool   `json:"success"`
	Deleted    int    `json:"deleted"`
	Collection string `json:"collection"`
	ErrorMsg   string `json:"error_msg"`
}

// DeleteDocument asks the LlamaIndex service to drop every chunk of docID from
// the knowledge-base collection, which also invalidates the graph route cache.
// It returns the number of removed chunks (0 when the document was never
// indexed) and is idempotent, so retries are safe.
func (l *LlamaIndexIndexer) DeleteDocument(ctx context.Context, docID, kbID string) (int, error) {
	if l.baseURL == "" {
		return 0, fmt.Errorf("llamaindex delete: url is empty")
	}
	payload, err := json.Marshal(llamaIndexDeleteRequest{TaskID: docID, KBID: kbID})
	if err != nil {
		return 0, fmt.Errorf("llamaindex delete: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/v1/delete", bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("llamaindex delete: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("llamaindex delete: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("llamaindex delete: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out llamaIndexDeleteResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, fmt.Errorf("llamaindex delete: decode response: %w", err)
	}
	if !out.Success || out.ErrorMsg != "" {
		return 0, fmt.Errorf("llamaindex delete: %s", out.ErrorMsg)
	}
	return out.Deleted, nil
}

func (l *LlamaIndexIndexer) IndexMarkdown(ctx context.Context, docID, markdown string, meta map[string]string) (int, error) {
	if l.baseURL == "" {
		return 0, fmt.Errorf("llamaindex index: url is empty")
	}
	payload, err := json.Marshal(llamaIndexIndexRequest{
		TaskID:        docID,
		Markdown:      markdown,
		SplitStrategy: "markdown",
		Metadata:      meta,
	})
	if err != nil {
		return 0, fmt.Errorf("llamaindex index: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/v1/index", bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("llamaindex index: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("llamaindex index: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("llamaindex index: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out llamaIndexIndexResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return 0, fmt.Errorf("llamaindex index: decode response: %w", err)
	}
	if !out.Success || out.ErrorMsg != "" {
		return 0, fmt.Errorf("llamaindex index: %s", out.ErrorMsg)
	}
	return out.IndexedCount, nil
}
