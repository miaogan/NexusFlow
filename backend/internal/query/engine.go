package query

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/nexusflow/nexusflow/backend/internal/platform/tracing"
)

// Engine runs multi-route recall, RRF fusion and optional reranking.
type Engine struct {
	retrievers map[string]Retriever
	reranker   Reranker
	rrfK       int
	store      *DocStore
	logger     *slog.Logger
	tracer     tracing.Tracer
}

// EngineOption configures an Engine at construction time.
type EngineOption func(*Engine)

// WithTracer attaches an observability tracer. Defaults to NoopTracer.
func WithTracer(t tracing.Tracer) EngineOption {
	return func(e *Engine) { e.tracer = t }
}

func NewEngine(logger *slog.Logger, reranker Reranker, opts ...EngineOption) *Engine {
	e := &Engine{
		retrievers: make(map[string]Retriever),
		reranker:   reranker,
		rrfK:       60,
		store:      NewDocStore(),
		logger:     logger,
		tracer:     tracing.NoopTracer{},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Tracer returns the engine's tracer (useful for callers that want to start a
// parent span before invoking Search, e.g. the Answer RPC).
func (e *Engine) Tracer() tracing.Tracer { return e.tracer }

// Store returns the shared in-memory document store backing dev-mode retrievers.
func (e *Engine) Store() *DocStore { return e.store }

// IndexMarkdown chunks a parsed document and writes it into the shared store,
// making it immediately searchable by every registered retriever. Re-indexing
// the same docID replaces the previous chunks (idempotent). Returns the number
// of chunks created.
func (e *Engine) IndexMarkdown(_ context.Context, docID, markdown string, meta map[string]string) int {
	chunks := ChunkMarkdown(markdown, 0)
	if len(chunks) == 0 {
		return 0
	}
	for i := range chunks {
		if chunks[i].Meta == nil {
			chunks[i].Meta = make(map[string]string, len(meta)+1)
		}
		for k, v := range meta {
			chunks[i].Meta[k] = v
		}
		chunks[i].ID = fmt.Sprintf("%s-%d", docID, i)
	}
	e.store.Add(docID, chunks)
	return len(chunks)
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

	root, ctx := e.tracer.Start(ctx, "query.search",
		tracing.WithInput(map[string]any{"query": opts.Query}),
		tracing.WithAttributes(map[string]any{"routes": opts.Routes, "filters": opts.Filters}),
	)
	defer root.End()

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
			rspan, _ := e.tracer.Start(ctx, "recall."+r.Name(),
				tracing.WithInput(map[string]any{"query": opts.Query, "top_k": topK}),
			)
			chunks, err := r.Retrieve(ctx, opts.Query, topK, opts.Filters)
			if err != nil {
				rspan.RecordError(err)
			} else {
				rspan.SetOutput(map[string]any{"count": len(chunks)})
			}
			rspan.End()
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
		root.SetStatus(tracing.StatusError, "no recall")
		return Result{}, ErrNoRecall
	}

	fspan, _ := e.tracer.Start(ctx, "fusion.rrf",
		tracing.WithInput(map[string]any{"list_count": len(lists), "k": e.rrfK}),
	)
	fused := Fuse(lists, e.rrfK)
	fspan.SetOutput(map[string]any{"fused_count": len(fused)})
	fspan.End()

	out := Result{Chunks: fused, RecallCount: recallCount}

	topN := opts.FinalTopN
	if topN <= 0 {
		topN = 5
	}

	if e.reranker != nil && len(fused) > 0 {
		attrs := map[string]any{"input_count": len(fused), "top_n": topN}
		if m, ok := e.reranker.(interface{ Model() string }); ok {
			attrs["model"] = m.Model()
		}
		rspan, _ := e.tracer.Start(ctx, "rerank", tracing.WithAttributes(attrs))
		reranked, err := e.reranker.Rerank(ctx, opts.Query, fused, topN)
		if err != nil {
			rspan.RecordError(err)
			e.logger.Warn("rerank failed, falling back to RRF", "error", err)
		} else {
			rspan.SetOutput(map[string]any{"output_count": len(reranked)})
			fused = reranked
			out.RerankApplied = true
		}
		rspan.End()
	}

	if len(fused) > topN {
		fused = fused[:topN]
	}
	out.Chunks = fused
	root.SetOutput(map[string]any{
		"recall_count":   recallCount,
		"rerank_applied": out.RerankApplied,
		"returned":       len(fused),
	})
	return out, nil
}
