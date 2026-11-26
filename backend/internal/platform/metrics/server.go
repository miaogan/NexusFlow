package metrics

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
)

// Serve exposes /metrics on addr for Prometheus scraping. An empty addr
// disables the endpoint (used by the single-process dev binary).
func Serve(ctx context.Context, addr string, logger *slog.Logger) error {
	if addr == "" {
		<-ctx.Done()
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return httpserver.Serve(ctx, srv, logger)
}
