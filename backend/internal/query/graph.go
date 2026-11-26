package query

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

// GraphRetriever is the LightRAG-style graph recall route. It calls the
// LlamaIndex service's /v1/search with a single "graph" route, so entity graph
// candidates are fused with the local vector/BM25 routes by the same RRF pass.
type GraphRetriever struct {
	baseURL string
	timeout time.Duration
	client  *http.Client
}

func NewGraphRetriever(baseURL string, timeout time.Duration) *GraphRetriever {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &GraphRetriever{
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

func (g *GraphRetriever) Name() string { return "graph" }

type graphSearchRequest struct {
	Query     string             `json:"query"`
	Routes    []graphSearchRoute `json:"routes"`
	FinalTopN int                `json:"final_top_n"`
	KBID      string             `json:"kb_id"`
	Filters   map[string]string  `json:"filters,omitempty"`
}

type graphSearchRoute struct {
	Type string `json:"type"`
	TopK int    `json:"top_k"`
}

func (g *GraphRetriever) Retrieve(ctx context.Context, query string, topK int, filters map[string]string) ([]Chunk, error) {
	if g.baseURL == "" {
		return nil, fmt.Errorf("graph recall: llamaindex url is empty")
	}
	if topK <= 0 {
		topK = 20
	}
	kbID := filters["kb_id"]
	if kbID == "" {
		kbID = "default"
	}
	// kb_id selects the collection on the Python side, so it is passed
	// separately from the remaining metadata filters.
	trimmed := make(map[string]string, len(filters))
	for key, value := range filters {
		if key == "kb_id" {
			continue
		}
		trimmed[key] = value
	}

	body, err := json.Marshal(graphSearchRequest{
		Query:     query,
		Routes:    []graphSearchRoute{{Type: "graph", TopK: topK}},
		FinalTopN: topK,
		KBID:      kbID,
		Filters:   trimmed,
	})
	if err != nil {
		return nil, fmt.Errorf("graph recall: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/v1/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("graph recall: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graph recall: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("graph recall: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var out struct {
		Chunks []struct {
			ChunkID string            `json:"chunk_id"`
			Content string            `json:"content"`
			Score   float64           `json:"score"`
			Meta    map[string]string `json:"meta"`
		} `json:"chunks"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("graph recall: decode response: %w", err)
	}

	chunks := make([]Chunk, 0, len(out.Chunks))
	for _, c := range out.Chunks {
		chunks = append(chunks, Chunk{
			ID:      c.ChunkID,
			Content: c.Content,
			Score:   c.Score,
			Meta:    c.Meta,
			Source:  "graph",
		})
	}
	return chunks, nil
}
