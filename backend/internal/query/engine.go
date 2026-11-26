package query

import (
	"context"
	"log/slog"
	"sort"
	"sync"
)

// Engine runs multi-route recall, RRF fusion and optional reranking.
type Engine struct {
	retrievers map[string]Retriever
	reranker   Reranker
	rrfK       int
	logger     *slog.Logger
}

func NewEngine(logger *slog.Logger, reranker Reranker) *Engine {
	return &Engine{
		retrievers: make(map[string]Retriever),
		reranker:   reranker,
		rrfK:       60,
		logger:     logger,
	}
}

func (e *Engine) Register(r Retriever) {
	e.retrievers[r.Name()] = r
}

func (e *Engine) Routes() []string {
	out := make([]string, 0, len(e.retrievers))
	for name := range e.retrievers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type recallResult struct {
	name   string
	chunks []Chunk
	err    error
}

func (e *Engine) Search(ctx context.Context, opts SearchOptions) (Result, error) {
	if opts.Query == "" {
		return Result{}, ErrNoRecall
	}

	routes := opts.Routes
	if len(routes) == 0 {
		for _, name := range e.Routes() {
			routes = append(routes, Route{Type: name, TopK: 20})
		}
	}

	var wg sync.WaitGroup
	results := make(chan recallResult, len(routes))
	for _, route := range routes {
		retriever, ok := e.retrievers[route.Type]
		if !ok {
			e.logger.Warn("unknown recall route", "route", route.Type)
			continue
		}
		topK := route.TopK
		if topK <= 0 {
			topK = 20
		}
		wg.Add(1)
		go func(r Retriever, topK int) {
			defer wg.Done()
			chunks, err := r.Retrieve(ctx, opts.Query, topK, opts.Filters)
			results <- recallResult{name: r.Name(), chunks: chunks, err: err}
		}(retriever, topK)
	}
	wg.Wait()
	close(results)

	var lists [][]Chunk
	recallCount := 0
	for res := range results {
		if res.err != nil {
			// Fail-open: a single broken route must not take the query down.
			e.logger.Warn("recall route failed", "route", res.name, "error", res.err)
			continue
		}
		if len(res.chunks) == 0 {
			continue
		}
		lists = append(lists, res.chunks)
		recallCount += len(res.chunks)
	}
	if len(lists) == 0 {
		return Result{}, ErrNoRecall
	}

	fused := Fuse(lists, e.rrfK)
	out := Result{Chunks: fused, RecallCount: recallCount}

	topN := opts.FinalTopN
	if topN <= 0 {
		topN = 5
	}

	if e.reranker != nil && len(fused) > 0 {
		reranked, err := e.reranker.Rerank(ctx, opts.Query, fused, topN)
		if err != nil {
			// Degrade to RRF ordering (fail-open).
			e.logger.Warn("rerank failed, falling back to RRF", "error", err)
		} else {
			fused = reranked
			out.RerankApplied = true
		}
	}

	if len(fused) > topN {
		fused = fused[:topN]
	}
	out.Chunks = fused
	return out, nil
}
