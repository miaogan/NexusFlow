package tracing

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryTracer_RootAndChildren(t *testing.T) {
	tr := NewMemoryTracer()
	ctx := context.Background()

	root, ctx := tr.Start(ctx, "query.search", WithInput(map[string]any{"q": "x"}))
	childA, _ := tr.Start(ctx, "recall.vector")
	childA.End()
	childB, _ := tr.Start(ctx, "recall.bm25")
	childB.End()
	root.SetOutput(map[string]any{"n": 2})
	root.End()

	traces := tr.Traces()
	if len(traces) != 1 {
		t.Fatalf("want 1 trace, got %d", len(traces))
	}
	got := traces[0]
	if got.Root == nil || got.Root.Name != "query.search" {
		t.Fatalf("root not recorded: %+v", got.Root)
	}
	if len(got.Spans) != 3 {
		t.Fatalf("want 3 spans, got %d", len(got.Spans))
	}
	for _, s := range got.Spans {
		if s.TraceID == "" || s.SpanID == "" || s.StartTime.IsZero() || s.EndTime.IsZero() {
			t.Errorf("span %q missing id/timing: %+v", s.Name, s)
		}
		if s.Status != StatusOK {
			t.Errorf("span %q status = %q, want ok", s.Name, s.Status)
		}
	}
	if got.Spans[1].ParentID != got.Root.SpanID || got.Spans[2].ParentID != got.Root.SpanID {
		t.Errorf("child spans not parented to root: %+v", got.Spans)
	}
}

func TestMemoryTracer_ErrorStatus(t *testing.T) {
	tr := NewMemoryTracer()
	root, ctx := tr.Start(context.Background(), "op")
	child, _ := tr.Start(ctx, "child")
	child.RecordError(errors.New("boom"))
	child.End()
	root.End()

	got, ok := tr.Trace(root.TraceID())
	if !ok {
		t.Fatal("trace not found")
	}
	var childSpan *SpanData
	for _, s := range got.Spans {
		if s.Name == "child" {
			childSpan = s
		}
	}
	if childSpan == nil || childSpan.Status != StatusError || childSpan.StatusMsg != "boom" {
		t.Fatalf("child error status not recorded: %+v", childSpan)
	}
}

func TestMemoryTracer_Score(t *testing.T) {
	tr := NewMemoryTracer()
	root, _ := tr.Start(context.Background(), "query.search")
	root.End()

	if err := tr.AddScore(context.Background(), root.TraceID(), "user.rating", 1, "赞"); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.Trace(root.TraceID())
	if len(got.Scores) != 1 || got.Scores[0].Name != "user.rating" || got.Scores[0].Value != 1 {
		t.Fatalf("score not recorded: %+v", got.Scores)
	}
}

func TestMemoryTracer_Eviction(t *testing.T) {
	tr := NewMemoryTracer()
	tr.maxTraces = 3
	for i := 0; i < 5; i++ {
		s, _ := tr.Start(context.Background(), "op")
		s.End()
	}
	if len(tr.Traces()) != 3 {
		t.Fatalf("want 3 traces after eviction, got %d", len(tr.Traces()))
	}
}

func TestNoopTracer(t *testing.T) {
	var tr Tracer = NoopTracer{}
	s, _ := tr.Start(context.Background(), "x")
	if s.TraceID() == "" {
		t.Fatal("noop span should still carry a trace id")
	}
	s.End()
	if err := tr.AddScore(context.Background(), "t", "n", 1, ""); err != nil {
		t.Fatal(err)
	}
}
