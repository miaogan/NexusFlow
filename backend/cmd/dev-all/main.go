package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/gateway"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	"github.com/nexusflow/nexusflow/backend/internal/indexing"
	"github.com/nexusflow/nexusflow/backend/internal/ingestion"
	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/platform/tracing"
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

	tracer := tracing.NewFromEnv(logger)
	defer func() { _ = tracer.Shutdown(context.Background()) }()

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
	bus.SetErrorHandler(func(topic string, err error) {
		logger.Error("queue handler failed", "topic", topic, "error", err)
	})
	client := &mineru.MockClient{
		Markdown: mockMarkdown,
		ReadFile: func(fileURL, fileType string) (string, error) {
			// Index uploaded text-like files verbatim so demos search real content.
			key := strings.TrimPrefix(fileURL, cfg.ObjectStoreBase+"/")
			rc, err := objects.Get(context.Background(), key)
			if err != nil {
				return "", err
			}
			defer rc.Close()
			raw, err := io.ReadAll(rc)
			if err != nil {
				return "", err
			}
			switch fileType {
			case "txt", "md", "markdown", "text", "csv", "json", "log":
				return string(raw), nil
			}
			return "", nil // unsupported type → fall back to mock markdown
		},
	}

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
	engine := query.NewEngine(logger, nil, query.WithTracer(tracer))
	docStore := engine.Store()
	docStore.Add("seed", citations) // demo corpus; real uploads are indexed below
	engine.Register(query.NewMemoryRetriever("vector", docStore))
	engine.Register(query.NewMemoryRetriever("bm25", docStore))

	// Indexer turns parsed markdown into searchable chunks and advances the
	// task from PARSED through CHUNKED/EMBEDDED/INDEXED to COMPLETED.
	idxr := indexing.New(engine, st, logger)
	bus.Subscribe(cfg.QueueTopicParsed, idxr.Handle)

	ingestSvc := ingestion.NewService(objects, st, bus, logger, cfg.ObjectStoreBase, cfg.QueueTopicUploaded)
	parseSvc := processing.NewParseService(client, st, logger)
	sandboxSvc := sandbox.NewGRPCService(sandbox.NewService(sandbox.NewLocalBackend(cfg.SandboxWorkDir), logger))
	querySvc := query.NewGRPCService(engine)

	identityStore := identity.NewMemoryStore()
	if err := identityStore.Seed(identity.SeedOptions{
		TenantID:      cfg.SeedTenantID,
		AdminUsername: cfg.SeedAdminUsername,
		AdminPassword: cfg.SeedAdminPassword,
	}); err != nil {
		logger.Error("seed identity store", "error", err)
		os.Exit(1)
	}
	authSvc := identity.NewAuthService(identityStore, identity.AuthConfig{
		Secret:            cfg.JWTSecret,
		AccessTTL:         cfg.AccessTokenTTL,
		RefreshTTL:        cfg.RefreshTokenTTL,
		AllowSelfRegister: cfg.AllowSelfRegister,
	})
	userSvc := identity.NewUserService(identityStore)

	wfRegistry := workflow.NewRegistry()
	wfRegistry.Register(workflow.NoopNode{})
	wfRegistry.Register(workflow.SetNode{})
	for _, stage := range []string{"ingest", "parse", "index", "sandbox", "query"} {
		wfRegistry.Register(workflow.StageNode{Name: stage})
	}
	wfEngine := workflow.NewEngine(wfRegistry, logger)

	errCh := make(chan error, 12)
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
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.AuthGRPCAddr, func(s *grpc.Server) {
			pb.RegisterAuthServiceServer(s, authSvc)
		}, logger)
	}()
	go func() {
		errCh <- grpcserver.Serve(ctx, cfg.UserGRPCAddr, func(s *grpc.Server) {
			pb.RegisterUserServiceServer(s, userSvc)
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

	authConn, err := grpc.NewClient(cfg.AuthTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("connect auth", "error", err)
		os.Exit(1)
	}
	defer authConn.Close()
	userConn, err := grpc.NewClient(cfg.UserTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		logger.Error("connect user", "error", err)
		os.Exit(1)
	}
	defer userConn.Close()

	gw := gateway.New(
		pb.NewIngestionServiceClient(ingestConn),
		pb.NewQueryServiceClient(queryConn),
		pb.NewSandboxExecutionServiceClient(sandboxConn),
		objects,
		logger, cfg.JWTSecret, cfg.AllowAnonymous,
		gateway.WithTracer(tracer),
		gateway.WithAllowedUploadHosts(cfg.ObjectStoreAllowedHosts),
		gateway.WithIdentity(
			pb.NewAuthServiceClient(authConn),
			pb.NewUserServiceClient(userConn),
		),
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
