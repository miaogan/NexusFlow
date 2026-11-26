package query

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"sort"
)

// Chunk is a retrieved unit of context.
type Chunk struct {
	ID      string            `json:"chunk_id"`
	Content string            `json:"content"`
	Score   float64           `json:"score"`
	Meta    map[string]string `json:"meta,omitempty"`
	Source  string            `json:"source,omitempty"`
}

// Retriever is one recall path: vector, BM25, scalar filter, parent or graph.
type Retriever interface {
	Name() string
	Retrieve(ctx context.Context, query string, topK int, filters map[string]string) ([]Chunk, error)
}

type Reranker interface {
	Rerank(ctx context.Context, query string, chunks []Chunk, topN int) ([]Chunk, error)
}

type Route struct {
	Type       string
	TopK       int
	FilterExpr string
}

type SearchOptions struct {
	Query     string
	Routes    []Route
	FinalTopN int
	Filters   map[string]string
}

type Result struct {
	Chunks        []Chunk
	RecallCount   int
	RerankApplied bool
}

func chunkKey(c Chunk) string {
	if c.ID != "" {
		return c.ID
	}
	sum := sha1.Sum([]byte(c.Content))
	return hex.EncodeToString(sum[:])
}

// Fuse merges ranked lists with Reciprocal Rank Fusion and de-duplicates by
// chunk ID (falling back to a content hash).
func Fuse(lists [][]Chunk, k int) []Chunk {
	if k <= 0 {
		k = 60
	}
	scores := make(map[string]float64)
	best := make(map[string]Chunk)

	for _, list := range lists {
		seen := make(map[string]struct{}, len(list))
		for rank, c := range list {
			key := chunkKey(c)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}

			scores[key] += 1.0 / float64(k+rank+1)
			cur, ok := best[key]
			if !ok {
				best[key] = c
				continue
			}
			if cur.Content == "" && c.Content != "" {
				cur.Content = c.Content
			}
			if cur.Meta == nil && len(c.Meta) > 0 {
				cur.Meta = c.Meta
			}
			if cur.Source == "" {
				cur.Source = c.Source
			}
			best[key] = cur
		}
	}

	out := make([]Chunk, 0, len(scores))
	for key, score := range scores {
		c := best[key]
		c.Score = score
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return chunkKey(out[i]) < chunkKey(out[j])
	})
	return out
}

var ErrNoRecall = errors.New("no recall route produced results")

func mergeFilters(base, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
