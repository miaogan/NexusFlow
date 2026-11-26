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

// Supported rerank wire protocols.
const (
	// RerankProtocolGeneric is the OpenAI/Cohere-style payload:
	// {model, query, documents, top_n} -> {results: [{index, relevance_score}]}
	RerankProtocolGeneric = "generic"
	// RerankProtocolDashScope targets Aliyun DashScope's native text-rerank API:
	// {model, input:{query, documents}, parameters:{top_n}} -> {output:{results}}
	RerankProtocolDashScope = "dashscope"
)

// RerankerConfig configures the HTTP reranker.
type RerankerConfig struct {
	URL      string
	Model    string
	Protocol string
	APIKey   string
	// Pool truncates the candidate list before reranking (DashScope recommends
	// a bounded pool). Zero means "send every fused candidate".
	Pool    int
	Timeout time.Duration
}

// HTTPReranker calls a Cross-Encoder rerank service (bge-reranker, DashScope,
// ...). A missing pool or unknown protocol keeps the previous generic behaviour.
type HTTPReranker struct {
	URL      string
	Model    string
	Protocol string
	APIKey   string
	Pool     int
	Timeout  time.Duration
	HTTP     *http.Client
}

func NewHTTPReranker(cfg RerankerConfig) *HTTPReranker {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	protocol := strings.ToLower(strings.TrimSpace(cfg.Protocol))
	if protocol == "" {
		protocol = RerankProtocolGeneric
	}
	return &HTTPReranker{
		URL:      cfg.URL,
		Model:    cfg.Model,
		Protocol: protocol,
		APIKey:   cfg.APIKey,
		Pool:     cfg.Pool,
		Timeout:  timeout,
		HTTP:     &http.Client{Timeout: timeout},
	}
}

type rerankItem struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, chunks []Chunk, topN int) ([]Chunk, error) {
	if r.URL == "" {
		return nil, fmt.Errorf("reranker url is empty")
	}
	candidates := chunks
	if r.Pool > 0 && len(candidates) > r.Pool {
		candidates = candidates[:r.Pool]
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	docs := make([]string, len(candidates))
	for i, c := range candidates {
		docs[i] = c.Content
	}

	var payload []byte
	var err error
	if r.Protocol == RerankProtocolDashScope {
		payload, err = json.Marshal(map[string]any{
			"model": r.Model,
			"input": map[string]any{
				"query":     query,
				"documents": docs,
			},
			"parameters": map[string]any{
				"return_documents": false,
				"top_n":            topN,
			},
		})
	} else {
		payload, err = json.Marshal(map[string]any{
			"model":     r.Model,
			"query":     query,
			"documents": docs,
			"top_n":     topN,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("rerank: encode request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Protocol == RerankProtocolDashScope && r.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+r.APIKey)
	}

	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("rerank status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var results []rerankItem
	if r.Protocol == RerankProtocolDashScope {
		var out struct {
			Output struct {
				Results []rerankItem `json:"results"`
			} `json:"output"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
			return nil, err
		}
		results = out.Output.Results
	} else {
		var out struct {
			Results []rerankItem `json:"results"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
			return nil, err
		}
		results = out.Results
	}

	reranked := make([]Chunk, 0, len(results))
	for _, item := range results {
		if item.Index < 0 || item.Index >= len(candidates) {
			continue
		}
		c := candidates[item.Index]
		c.Score = item.Score
		reranked = append(reranked, c)
	}
	if len(reranked) > topN {
		reranked = reranked[:topN]
	}
	return reranked, nil
}
