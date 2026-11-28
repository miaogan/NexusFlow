package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
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
	registry.Register(newNL2SQLNode(ctx, cfg, logger))
	if cfg.WorkflowAgentNode {
		registry.Register(workflow.NewAgentNode(cfg.AgentHubURL, cfg.AgentHubToken, cfg.AgentHubTimeout))
	}
	if cfg.WorkflowHITL {
		registry.Register(workflow.HumanApprovalNode{})
	}
	// Control flow: condition branches and bounded concurrent loops.
	registry.Register(workflow.ConditionNode{})
	registry.Register(workflow.NewLoopNode(registry, logger))
	engine := workflow.NewEngine(registry, logger)
	if publisher := webhook.NewHTTPPublisher(cfg.GatewayURL, cfg.WebhookEventsToken, logger); publisher != nil {
		engine.WithEventPublisher(publisher).WithEventSink(workflow.NewWebhookSink(publisher))
	}
	runStore, closeRunStore := workflow.NewRunStore(
		ctx, cfg.WorkflowRunStoreDriver, cfg.WorkflowRunDatabaseURL,
	)
	defer closeRunStore()

	srv := &http.Server{
		Addr: cfg.WorkflowHTTPAddr,
		Handler: workflow.NewHTTPHandler(
			engine, registry,
			workflow.WithServiceToken(cfg.WorkflowServiceToken),
			workflow.WithRunStore(runStore),
		),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpserver.Serve(ctx, srv, logger); err != nil {
		logger.Error("workflow engine stopped", "error", err)
		os.Exit(1)
	}
}

// newNL2SQLNode builds the read-only SQL node. Database and LLM are optional:
// without them the node still appears in /v1/nodes and returns a clear
// configuration error when a run tries to execute it.
func newNL2SQLNode(ctx context.Context, cfg config.Config, logger *slog.Logger) *workflow.NL2SQLNode {
	opts := []workflow.NL2SQLOption{
		workflow.WithNL2SQLMaxRows(cfg.NL2SQLMaxRows),
		workflow.WithNL2SQLTimeout(cfg.NL2SQLTimeout),
	}
	if cfg.NL2SQLLLMBaseURL != "" && cfg.NL2SQLLLMAPIKey != "" {
		opts = append(opts, workflow.WithNL2SQLGenerator(workflow.NewHTTPNL2SQLGenerator(
			cfg.NL2SQLLLMBaseURL, cfg.NL2SQLLLMAPIKey, cfg.NL2SQLModel, cfg.NL2SQLSchema, 0,
		)))
	}
	if cfg.NL2SQLDatabaseURL != "" {
		db, err := workflow.OpenNL2SQLDB(ctx, cfg.NL2SQLDatabaseURL)
		if err != nil {
			logger.Warn("nl2sql database unavailable, node registered without a database", "error", err)
		} else {
			go func() {
				<-ctx.Done()
				_ = db.Close()
			}()
			opts = append(opts, workflow.WithNL2SQLDB(db))
		}
	}
	return workflow.NewNL2SQLNode(opts...)
}
