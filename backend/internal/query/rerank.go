package query

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// HTTPReranker calls a Cross-Encoder rerank service (e.g. bge-reranker).
type HTTPReranker struct {
	URL     string
	Model   string
	Timeout time.Duration
	HTTP    *http.Client
}

func NewHTTPReranker(url, model string, timeout time.Duration) *HTTPReranker {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPReranker{
		URL:     url,
		Model:   model,
		Timeout: timeout,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, chunks []Chunk, topN int) ([]Chunk, error) {
	if r.URL == "" {
		return nil, fmt.Errorf("reranker url is empty")
	}
	docs := make([]string, len(chunks))
	for i, c := range chunks {
		docs[i] = c.Content
	}
	payload, _ := json.Marshal(map[string]any{
		"model":     r.Model,
		"query":     query,
		"documents": docs,
		"top_n":     topN,
	})

	reqCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank status %d", resp.StatusCode)
	}

	var out struct {
		Results []struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	reranked := make([]Chunk, 0, len(out.Results))
	for _, item := range out.Results {
		if item.Index < 0 || item.Index >= len(chunks) {
			continue
		}
		c := chunks[item.Index]
		c.Score = item.Score
		reranked = append(reranked, c)
	}
	if len(reranked) > topN {
		reranked = reranked[:topN]
	}
	return reranked, nil
}
