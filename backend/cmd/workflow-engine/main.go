package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
	"github.com/nexusflow/nexusflow/backend/internal/workflow"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

func main() {
	cfg := config.Load()
	logger := log.New("workflow-engine")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := workflow.NewRegistry()
	registry.Register(workflow.NoopNode{})
	registry.Register(workflow.SetNode{})
	// Ingest/parse/index are asynchronous stages owned by the upload pipeline;
	// the DAG nodes are declarative markers. Query and sandbox execute for real
	// through their services.
	for _, stage := range []string{"ingest", "parse", "index"} {
		registry.Register(workflow.StageNode{Name: stage})
	}
	if conn, err := grpc.NewClient(
		cfg.QueryTarget, grpc.WithTransportCredentials(insecure.NewCredentials()),
	); err != nil {
		logger.Warn("query node disabled: cannot create client", "target", cfg.QueryTarget, "error", err)
	} else {
		defer conn.Close()
		registry.Register(workflow.NewQueryStageNode(pb.NewQueryServiceClient(conn)))
	}
	if conn, err := grpc.NewClient(
		cfg.SandboxTarget, grpc.WithTransportCredentials(insecure.NewCredentials()),
	); err != nil {
		logger.Warn("sandbox node disabled: cannot create client", "target", cfg.SandboxTarget, "error", err)
	} else {
		defer conn.Close()
		registry.Register(workflow.NewSandboxStageNode(pb.NewSandboxExecutionServiceClient(conn)))
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
			workflow.WithDefinitionStore(workflow.DefinitionStoreFor(runStore)),
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
