package query

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/nexusflow/nexusflow/backend/internal/platform/tracing"
)

func TestEngineSearch_RecordsSpanTree(t *testing.T) {
	mt := tracing.NewMemoryTracer()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(logger, nil, WithTracer(mt))
	engine.Store().Add("doc", []Chunk{{ID: "c1", Content: "NexusFlow 编排调度", Meta: map[string]string{}}})
	engine.Register(NewMemoryRetriever("vector", engine.Store()))
	engine.Register(NewMemoryRetriever("bm25", engine.Store()))

	res, err := engine.Search(context.Background(), SearchOptions{Query: "NexusFlow"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Chunks) == 0 {
		t.Fatal("expected recall hits")
	}

	traces := mt.Traces()
	if len(traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(traces))
	}
	tr := traces[0]
	if tr.Root == nil || tr.Root.Name != "query.search" {
		t.Fatalf("root span missing or wrong: %+v", tr.Root)
	}

	want := map[string]bool{
		"query.search":  true,
		"recall.vector": true,
		"recall.bm25":   true,
		"fusion.rrf":    true,
	}
	got := map[string]bool{}
	for _, s := range tr.Spans {
		got[s.Name] = true
		if s.StartTime.IsZero() || s.EndTime.IsZero() {
			t.Errorf("span %q missing start/end timing", s.Name)
		}
		if s.Status != tracing.StatusOK {
			t.Errorf("span %q status = %q, want ok", s.Name, s.Status)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing span %q; recorded names = %v", name, got)
		}
	}
	for _, s := range tr.Spans {
		if s.Name == "recall.vector" && s.ParentID != tr.Root.SpanID {
			t.Errorf("recall.vector parent = %q, want root %q", s.ParentID, tr.Root.SpanID)
		}
	}
}
