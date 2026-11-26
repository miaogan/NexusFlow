package tracing

import (
	"log/slog"
	"os"
	"strconv"
)

// NewFromEnv selects a tracer backend from environment configuration:
//
//   - LANGFUSE_BASE_URL set    → LangfuseTracer (exports to a Langfuse instance)
//   - TRACING_MEMORY=true      → MemoryTracer (in-process recorder; default in dev)
//   - otherwise                → NoopTracer (zero overhead)
//
// In dev (APP_ENV=dev) MemoryTracer is enabled automatically so the trace
// viewer works out of the box; production defaults to NoopTracer unless
// Langfuse is configured.
func NewFromEnv(logger *slog.Logger) Tracer {
	if base := os.Getenv("LANGFUSE_BASE_URL"); base != "" {
		return NewLangfuseTracer(base, os.Getenv("LANGFUSE_PUBLIC_KEY"), os.Getenv("LANGFUSE_SECRET_KEY"), logger)
	}
	if envBool("TRACING_MEMORY", os.Getenv("APP_ENV") == "dev") {
		return NewMemoryTracer()
	}
	return NoopTracer{}
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
