package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/gateway"
	"github.com/nexusflow/nexusflow/backend/internal/ingestion"
	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/processing"
	"github.com/nexusflow/nexusflow/backend/internal/query"
	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/sandbox"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/workflow"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const mockMarkdown = `# 示例文档

## 1. 概述

NexusFlow 采用 Go 负责编排调度、Python LlamaIndex 负责语义切块与索引构建。

## 2. 检索

查询链路使用向量召回、BM25 与 LightRAG 三路召回，经 RRF 融合后由 Cross-Encoder 精排。

## 3. 沙箱

自定义代码节点通过 OpenSandbox 执行，具备强隔离、强制 TTL、出口白名单与凭据不落盘。
`

// dev-all runs every service in a single process with an in-memory broker,
// which makes the whole pipeline demoable with `go run ./cmd/dev-all`.
func main() {
	cfg := config.Load()
	logger := log.New("dev-all")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := state.NewStore("memory", "")
	if err != nil {
		logger.Error("init state store", "error", err)
		os.Exit(1)
	}

	objects, err := store.NewLocalObjectStore(cfg.ObjectStoreDir, cfg.ObjectStoreBase)
	if err != nil {
		logger.Error("init object store", "error", err)
		os.Exit(1)
	}

	bus := queue.NewMemoryBus()
	client := &mineru.MockClient{Markdown: mockMarkdown}

	worker := processing.NewWorker(
		client, st, bus, logger,
		cfg.ParsePollInterval, 10, cfg.ParseMaxAttempts,
		cfg.QueueTopicParsed, cfg.QueueTopicDLQ, "dev-all",
	)
	bus.Subscribe(cfg.QueueTopicUploaded, worker.Handle)

	citations := []query.Chunk{
		{ID: "doc-1", Content: "NexusFlow 采用 Go 负责编排调度、Python LlamaIndex 负责语义切块与索引构建。", Meta: map[string]string{"tenant_id": "default", "source": "architecture"}},
		{ID: "doc-2", Content: "查询链路使用向量召回、BM25 与 LightRAG 三路召回，经 RRF 融合后由 Cross-Encoder 精排。", Meta: map[string]string{"tenant_id": "default", "source": "retrieval"}},
		{ID: "doc-3", Content: "自定义代码节点通过 OpenSandbox 执行，具备强隔离、强制 TTL、出口白名单与凭据不落盘。", Meta: map[string]string{"tenant_id": "default", "source": "sandbox"}},
	}
	engine := query.NewEngine(logger, nil)
	engine.Register(query.NewMemoryRetriever("vector", citations))
	engine.Register(query.NewMemoryRetriever("bm25", citations))

	ingestSvc := ingestion.NewService(objects, st, bus, logger, cfg.ObjectStoreBase, cfg.QueueTopicUploaded)
	parseSvc := processing.NewParseService(client, st, logger)
	sandboxSvc := sandbox.NewGRPCService(sandbox.NewService(sandbox.NewLocalBackend(cfg.SandboxWorkDir), logger))
	querySvc := query.NewGRPCService(engine)

	wfRegistry := workflow.NewRegistry()
	wfRegistry.Register(workflow.NoopNode{})
	wfRegistry.Register(workflow.SetNode{})
	for _, stage := range []string{"ingest", "parse", "index", "sandbox", "query"} {
		wfRegistry.Register(workflow.StageNode{Name: stage})
	}
	wfEngine := workflow.NewEngine(wfRegistry, logger)

	errCh := make(chan error, 8)
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.IngestionGRPCAddr, func(s *grpc.Server) {
			pb.RegisterIngestionServiceServer(s, ingestSvc)
		}, logger)
	}()
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.ProcessingGRPCAddr, func(s *grpc.Server) {
			pb.RegisterProcessingServiceServer(s, parseSvc)
		}, logger)
	}()
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.SandboxGRPCAddr, func(s *grpc.Server) {
			pb.RegisterSandboxExecutionServiceServer(s, sandboxSvc)
		}, logger)
	}()
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.QueryGRPCAddr, func(s *grpc.Server) {
			pb.RegisterQueryServiceServer(s, querySvc)
		}, logger)
	}()

	ingestConn, err := grpc.NewClient(cfg.IngestionTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("connect ingestion", "error", err)
		os.Exit(1)
	}
	defer ingestConn.Close()
	queryConn, err := grpc.NewClient(cfg.QueryTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("connect query", "error", err)
		os.Exit(1)
	}
	defer queryConn.Close()

	sandboxConn, err := grpc.NewClient(cfg.SandboxTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("connect sandbox", "error", err)
		os.Exit(1)
	}
	defer sandboxConn.Close()

	gw := gateway.New(
		pb.NewIngestionServiceClient(ingestConn),
		pb.NewQueryServiceClient(queryConn),
		pb.NewSandboxExecutionServiceClient(sandboxConn),
		objects,
		logger, cfg.JWTSecret, cfg.AllowAnonymous,
	)
	srv := &http.Server{
		Addr:              cfg.GatewayHTTPAddr,
		Handler:           gw.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		errCh <- httpserver.Serve(ctx, srv, logger)
	}()

	wfSrv := &http.Server{
		Addr:              cfg.WorkflowHTTPAddr,
		Handler:           workflow.NewHTTPHandler(wfEngine, wfRegistry),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		errCh <- httpserver.Serve(ctx, wfSrv, logger)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Error("service stopped", "error", err)
			stop()
		}
	case <-ctx.Done():
	}

	_ = bus.Close()
}
