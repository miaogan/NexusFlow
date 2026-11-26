// Package tracing provides a thin, vendor-neutral observability layer for
// NexusFlow. It models traces as trees of spans (plus numeric scores attached
// to a trace) and ships three backends:
//
//   - NoopTracer: zero-cost default, used when observability is disabled.
//   - MemoryTracer: records spans in-process, powering the dev trace viewer
//     and unit tests.
//   - LangfuseTracer: batches spans to a self-hosted / cloud Langfuse instance
//     over its public ingestion API (fail-open).
//
// The package intentionally avoids pulling in OpenTelemetry or any heavy SDK:
// the data model is a small struct that maps cleanly to Langfuse's ingestion
// contract, and call sites only depend on the Tracer/Span interfaces.
package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// SpanStatus mirrors the canonical OpenTelemetry / Langfuse status names.
type SpanStatus string

const (
	StatusUnset SpanStatus = "unset"
	StatusOK    SpanStatus = "ok"
	StatusError SpanStatus = "error"
)

// SpanData is the serializable, backend-agnostic representation of a span.
// Both MemoryTracer and LangfuseTracer consume it.
type SpanData struct {
	TraceID    string         `json:"trace_id"`
	SpanID     string         `json:"span_id"`
	ParentID   string         `json:"parent_id,omitempty"`
	Name       string         `json:"name"`
	StartTime  time.Time      `json:"start_time"`
	EndTime    time.Time      `json:"end_time"`
	Status     SpanStatus     `json:"status"`
	StatusMsg  string         `json:"status_msg,omitempty"`
	Input      any            `json:"input,omitempty"`
	Output     any            `json:"output,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Score attaches a numeric evaluation (e.g. user 赞/踩, LLM-as-Judge verdict)
// to a trace, feeding the feedback loop.
type Score struct {
	TraceID string    `json:"trace_id"`
	Name    string    `json:"name"`
	Value   float64   `json:"value"`
	Comment string    `json:"comment,omitempty"`
	Ts      time.Time `json:"ts"`
}

// Span is a single timed operation within a trace.
type Span interface {
	TraceID() string
	SpanID() string
	ParentID() string
	Name() string
	SetInput(v any)
	SetOutput(v any)
	SetAttributes(m map[string]any)
	SetStatus(status SpanStatus, msg string)
	RecordError(err error)
	End()
}

// Tracer creates spans and records scores. Implementations must be safe for
// concurrent use.
type Tracer interface {
	Start(ctx context.Context, name string, opts ...StartOption) (Span, context.Context)
	AddScore(ctx context.Context, traceID, name string, value float64, comment string) error
	Shutdown(ctx context.Context) error
}

type startConfig struct {
	attributes map[string]any
	input      any
}

// StartOption customizes a span at creation time.
type StartOption func(*startConfig)

// WithAttributes tags the span with a metadata map (e.g. model name, route
// count). Merged into SpanData.Attributes.
func WithAttributes(m map[string]any) StartOption {
	return func(c *startConfig) { c.attributes = m }
}

// WithInput records the span's input payload for inspection.
func WithInput(v any) StartOption {
	return func(c *startConfig) { c.input = v }
}

// ---- trace id / span context propagation ----

type ctxKey int

const (
	traceIDKey ctxKey = iota
	spanKey
)

// WithTraceID pins a trace id onto the context.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, traceIDKey, id)
}

// TraceIDFrom returns the trace id stored in the context, or "".
func TraceIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(traceIDKey).(string); ok {
		return v
	}
	return ""
}

// EnsureTraceID returns a context carrying a trace id, generating a new one
// when absent. The second return value is the id.
func EnsureTraceID(ctx context.Context) (context.Context, string) {
	if id := TraceIDFrom(ctx); id != "" {
		return ctx, id
	}
	id := NewID()
	return WithTraceID(ctx, id), id
}

func withSpan(ctx context.Context, s Span) context.Context {
	return context.WithValue(ctx, spanKey, s)
}

func spanFromContext(ctx context.Context) Span {
	if s, ok := ctx.Value(spanKey).(Span); ok {
		return s
	}
	return nil
}

// NewID returns a 128-bit hex identifier (trace / span ids).
func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
