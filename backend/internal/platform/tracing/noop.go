package tracing

import "context"

// NoopTracer discards everything. It is the default when observability is not
// configured, so uninstrumented call paths pay nothing.
type NoopTracer struct{}

func (NoopTracer) Start(ctx context.Context, _ string, _ ...StartOption) (Span, context.Context) {
	ctx, id := EnsureTraceID(ctx)
	s := noopSpan{traceID: id, spanID: NewID()}
	return s, withSpan(ctx, s)
}

func (NoopTracer) AddScore(context.Context, string, string, float64, string) error { return nil }
func (NoopTracer) Shutdown(context.Context) error                                  { return nil }

type noopSpan struct {
	traceID string
	spanID  string
}

func (s noopSpan) TraceID() string            { return s.traceID }
func (s noopSpan) SpanID() string             { return s.spanID }
func (noopSpan) ParentID() string             { return "" }
func (noopSpan) Name() string                 { return "" }
func (noopSpan) SetInput(any)                 {}
func (noopSpan) SetOutput(any)                {}
func (noopSpan) SetAttributes(map[string]any) {}
func (noopSpan) SetStatus(SpanStatus, string) {}
func (noopSpan) RecordError(error)            {}
func (noopSpan) End()                         {}
