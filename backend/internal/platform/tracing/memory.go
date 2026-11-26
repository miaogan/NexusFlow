package tracing

import (
	"context"
	"sync"
	"time"
)

// Trace is a recorded trace: its root span, the full flattened span list and
// any scores attached to it.
type Trace struct {
	ID     string      `json:"id"`
	Root   *SpanData   `json:"root"`
	Spans  []*SpanData `json:"spans"`
	Scores []Score     `json:"scores"`
}

// MemoryTracer records traces in process memory. It backs the dev trace viewer
// (GET /v1/traces) and the tracing unit tests. It is NOT for production use —
// it keeps a bounded, in-memory ring of recent traces.
type MemoryTracer struct {
	mu        sync.Mutex
	traces    map[string]*Trace
	order     []string // trace ids in insertion order (oldest first)
	maxTraces int
}

// NewMemoryTracer creates a recorder keeping the most recent 500 traces.
func NewMemoryTracer() *MemoryTracer {
	return &MemoryTracer{traces: make(map[string]*Trace), maxTraces: 500}
}

func (t *MemoryTracer) Start(ctx context.Context, name string, opts ...StartOption) (Span, context.Context) {
	ctx, traceID := EnsureTraceID(ctx)

	cfg := &startConfig{}
	for _, o := range opts {
		o(cfg)
	}

	parent := spanFromContext(ctx)
	data := &SpanData{
		TraceID:    traceID,
		SpanID:     NewID(),
		Name:       name,
		StartTime:  time.Now(),
		Status:     StatusUnset,
		Input:      cfg.input,
		Attributes: cfg.attributes,
	}
	if parent != nil {
		data.ParentID = parent.SpanID()
	}

	t.mu.Lock()
	tr, ok := t.traces[traceID]
	if !ok {
		tr = &Trace{ID: traceID, Spans: []*SpanData{}, Scores: []Score{}}
		t.traces[traceID] = tr
		t.order = append(t.order, traceID)
		t.evictLocked()
	}
	if data.ParentID == "" {
		tr.Root = data
	}
	tr.Spans = append(tr.Spans, data)
	t.mu.Unlock()

	s := &memorySpan{data: data}
	return s, withSpan(ctx, s)
}

func (t *MemoryTracer) AddScore(_ context.Context, traceID, name string, value float64, comment string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.traces[traceID]
	if !ok {
		tr = &Trace{ID: traceID, Spans: []*SpanData{}, Scores: []Score{}}
		t.traces[traceID] = tr
		t.order = append(t.order, traceID)
		t.evictLocked()
	}
	tr.Scores = append(tr.Scores, Score{TraceID: traceID, Name: name, Value: value, Comment: comment, Ts: time.Now()})
	return nil
}

// Traces returns recorded traces newest-first.
func (t *MemoryTracer) Traces() []*Trace {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*Trace, 0, len(t.order))
	for i := len(t.order) - 1; i >= 0; i-- {
		if tr, ok := t.traces[t.order[i]]; ok {
			out = append(out, tr)
		}
	}
	return out
}

// Trace returns the trace with the given id, if recorded.
func (t *MemoryTracer) Trace(id string) (*Trace, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr, ok := t.traces[id]
	return tr, ok
}

func (t *MemoryTracer) Shutdown(context.Context) error { return nil }

func (t *MemoryTracer) evictLocked() {
	for len(t.order) > t.maxTraces {
		oldest := t.order[0]
		t.order = t.order[1:]
		delete(t.traces, oldest)
	}
}

type memorySpan struct {
	data *SpanData
	done bool
}

func (s *memorySpan) TraceID() string  { return s.data.TraceID }
func (s *memorySpan) SpanID() string   { return s.data.SpanID }
func (s *memorySpan) ParentID() string { return s.data.ParentID }
func (s *memorySpan) Name() string     { return s.data.Name }

func (s *memorySpan) SetInput(v any)                 { s.data.Input = v }
func (s *memorySpan) SetOutput(v any)                { s.data.Output = v }
func (s *memorySpan) SetAttributes(m map[string]any) { s.data.Attributes = m }
func (s *memorySpan) SetStatus(st SpanStatus, msg string) {
	s.data.Status = st
	s.data.StatusMsg = msg
}
func (s *memorySpan) RecordError(err error) {
	if err == nil {
		return
	}
	s.data.Status = StatusError
	s.data.StatusMsg = err.Error()
}

func (s *memorySpan) End() {
	if s.done {
		return
	}
	s.done = true
	s.data.EndTime = time.Now()
	if s.data.Status == StatusUnset {
		s.data.Status = StatusOK
	}
}
