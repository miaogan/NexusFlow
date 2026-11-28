package query

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type failingRetriever struct{}

func (failingRetriever) Name() string { return "vector" }

func (failingRetriever) Retrieve(context.Context, string, int, map[string]string) ([]Chunk, error) {
	return nil, errors.New("qdrant down")
}

func emptyEngine() *Engine {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(logger, nil)
	engine.Register(NewMemoryRetriever("vector", NewDocStore()))
	return engine
}

func TestSearchReturnsEmptyWhenRoutesSucceed(t *testing.T) {
	engine := emptyEngine()

	result, err := engine.Search(context.Background(), SearchOptions{
		Query:  "anything",
		Routes: []Route{{Type: "vector", TopK: 5}},
	})
	if err != nil {
		t.Fatalf("search on an empty KB must not fail: %v", err)
	}
	if len(result.Chunks) != 0 || result.RecallCount != 0 {
		t.Fatalf("result = %+v, want empty chunks", result)
	}
}

func TestSearchFailsWhenEveryRouteErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(logger, nil)
	engine.Register(failingRetriever{})

	if _, err := engine.Search(context.Background(), SearchOptions{
		Query:  "anything",
		Routes: []Route{{Type: "vector", TopK: 5}},
	}); !errors.Is(err, ErrNoRecall) {
		t.Fatalf("err = %v, want ErrNoRecall", err)
	}
}
