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
