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

	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

// LlamaIndexRetriever calls one recall route (vector / bm25 / graph) on the
// LlamaIndex service, so the Go query path searches the real Qdrant index
// instead of the in-memory demo corpus. Results keep their chunk ids, letting
// RRF fuse them with any locally registered routes.
type LlamaIndexRetriever struct {
	route   string
	baseURL string
	timeout time.Duration
	client  *http.Client
}

// NewLlamaIndexRetriever builds a retriever for a single LlamaIndex route type.
func NewLlamaIndexRetriever(route, baseURL string, timeout time.Duration) *LlamaIndexRetriever {
	if route == "" {
		route = "vector"
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &LlamaIndexRetriever{
		route:   route,
		baseURL: strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// NewGraphRetriever is the LightRAG-style entity graph route.
func NewGraphRetriever(baseURL string, timeout time.Duration) *LlamaIndexRetriever {
	return NewLlamaIndexRetriever("graph", baseURL, timeout)
}

func (r *LlamaIndexRetriever) Name() string { return r.route }

type llamaIndexSearchRequest struct {
	Query     string            `json:"query"`
	Routes    []llamaIndexRoute `json:"routes"`
	FinalTopN int               `json:"final_top_n"`
	KBID      string            `json:"kb_id"`
	Filters   map[string]string `json:"filters,omitempty"`
}

type llamaIndexRoute struct {
	Type string `json:"type"`
	TopK int    `json:"top_k"`
}

func (r *LlamaIndexRetriever) Retrieve(ctx context.Context, query string, topK int, filters map[string]string) ([]Chunk, error) {
	if r.baseURL == "" {
		return nil, fmt.Errorf("llamaindex %s recall: url is empty", r.route)
	}
	if topK <= 0 {
		topK = 20
	}
	kbID := filters["kb_id"]
	if kbID == "" {
		kbID = "default"
	}
	// kb_id selects the Qdrant collection on the Python side, so it is passed
	// separately from the remaining metadata filters.
	trimmed := make(map[string]string, len(filters)+1)
	for key, value := range filters {
		if key == "kb_id" {
			continue
		}
		trimmed[key] = value
	}
	// Tenant isolation is enforced server-side: the gateway verifies the JWT and
	// propagates the tenant on gRPC metadata, and indexed chunks carry tenant_id.
	if _, ok := trimmed["tenant_id"]; !ok {
		if p := tenant.FromIncoming(ctx); p.TenantID != "" {
			trimmed["tenant_id"] = p.TenantID
		}
	}

	body, err := json.Marshal(llamaIndexSearchRequest{
		Query:     query,
		Routes:    []llamaIndexRoute{{Type: r.route, TopK: topK}},
		FinalTopN: topK,
		KBID:      kbID,
		Filters:   trimmed,
	})
	if err != nil {
		return nil, fmt.Errorf("llamaindex %s recall: encode request: %w", r.route, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.baseURL+"/v1/search", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llamaindex %s recall: build request: %w", r.route, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llamaindex %s recall: %w", r.route, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("llamaindex %s recall: HTTP %d: %s", r.route, resp.StatusCode, strings.TrimSpace(string(payload)))
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
		return nil, fmt.Errorf("llamaindex %s recall: decode response: %w", r.route, err)
	}

	chunks := make([]Chunk, 0, len(out.Chunks))
	for _, c := range out.Chunks {
		chunks = append(chunks, Chunk{
			ID:      c.ChunkID,
			Content: c.Content,
			Score:   c.Score,
			Meta:    c.Meta,
			Source:  r.route,
		})
	}
	return chunks, nil
}
