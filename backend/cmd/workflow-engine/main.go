package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/workflow"
)

func main() {
	cfg := config.Load()
	logger := log.New("workflow-engine")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := workflow.NewRegistry()
	registry.Register(workflow.NoopNode{})
	registry.Register(workflow.SetNode{})
	for _, stage := range []string{"ingest", "parse", "index", "sandbox", "query"} {
		registry.Register(workflow.StageNode{Name: stage})
	}
	engine := workflow.NewEngine(registry, logger)

	srv := &http.Server{
		Addr:              cfg.WorkflowHTTPAddr,
		Handler:           workflow.NewHTTPHandler(engine, registry),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpserver.Serve(ctx, srv, logger); err != nil {
		logger.Error("workflow engine stopped", "error", err)
		os.Exit(1)
	}
}
