package query

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type fakeCounter struct {
	counts map[string]int
	err    error
	tenant string
}

func (f *fakeCounter) NegativeCounts(_ context.Context, tenantID string, _ []string) (map[string]int, error) {
	f.tenant = tenantID
	return f.counts, f.err
}

func TestFeedbackPenaltyDownRanksRepeatedNegativeChunks(t *testing.T) {
	counter := &fakeCounter{counts: map[string]int{"bad": 3, "ok": 1}}
	penalty := NewFeedbackPenalty(counter)
	chunks := []Chunk{
		{ID: "bad", Score: 0.9},
		{ID: "ok", Score: 0.8},
		{ID: "new", Score: 0.1},
	}

	out := penalty.Apply(context.Background(), "t1", chunks)
	if counter.tenant != "t1" {
		t.Errorf("tenant passed to counter = %q, want t1", counter.tenant)
	}
	if got := []string{out[0].ID, out[1].ID, out[2].ID}; got[0] != "ok" || got[1] != "bad" || got[2] != "new" {
		t.Fatalf("order = %v, want [ok bad new]", got)
	}
	if out[1].Score != 0.45 {
		t.Errorf("penalised score = %v, want 0.45", out[1].Score)
	}
	if out[0].Score != 0.8 {
		t.Errorf("below-threshold score = %v, want 0.8", out[0].Score)
	}
	// The input must not be mutated: callers may reuse the fused list.
	if chunks[0].Score != 0.9 {
		t.Errorf("input score mutated to %v", chunks[0].Score)
	}
}

func TestFeedbackPenaltyFailsOpen(t *testing.T) {
	penalty := NewFeedbackPenalty(&fakeCounter{err: errors.New("store down")})
	chunks := []Chunk{{ID: "a", Score: 0.5}, {ID: "b", Score: 0.4}}
	out := penalty.Apply(context.Background(), "t1", chunks)
	if len(out) != 2 || out[0].ID != "a" || out[0].Score != 0.5 {
		t.Fatalf("fail-open result = %+v, want input unchanged", out)
	}

	var nilPenalty *FeedbackPenalty
	if got := nilPenalty.Apply(context.Background(), "t1", chunks); len(got) != 2 {
		t.Errorf("nil penalty changed chunks: %+v", got)
	}
}

func TestEngineAppliesFeedbackPenaltyBeforeTopN(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := NewDocStore()
	store.Add("doc", []Chunk{
		{ID: "a", Content: "nexusflow 检索架构"},
		{ID: "b", Content: "nexusflow 检索架构"},
	})
	engine := NewEngine(logger, nil,
		WithFeedbackPenalty(NewFeedbackPenalty(&fakeCounter{counts: map[string]int{"a": 3}})),
	)
	engine.Register(NewMemoryRetriever("vector", store))

	res, err := engine.Search(context.Background(), SearchOptions{
		Query: "nexusflow 检索架构", TenantID: "t1", FinalTopN: 1,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Chunks) != 1 || res.Chunks[0].ID != "b" {
		t.Fatalf("top chunk = %+v, want b (a was down-ranked)", res.Chunks)
	}
}
