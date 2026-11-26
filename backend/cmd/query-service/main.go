package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/feedback"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/query"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
)

func demoCorpus() []query.Chunk {
	return []query.Chunk{
		{
			ID:      "doc-1",
			Content: "NexusFlow 采用 Go 负责编排调度、Python LlamaIndex 负责语义切块与索引构建。",
			Meta:    map[string]string{"tenant_id": "default", "source": "architecture"},
		},
		{
			ID:      "doc-2",
			Content: "查询链路使用向量召回、BM25 与 LightRAG 三路召回，经 RRF 融合后由 Cross-Encoder 精排。",
			Meta:    map[string]string{"tenant_id": "default", "source": "retrieval"},
		},
		{
			ID:      "doc-3",
			Content: "自定义代码节点通过 OpenSandbox 执行，具备强隔离、强制 TTL、出口白名单与凭据不落盘。",
			Meta:    map[string]string{"tenant_id": "default", "source": "sandbox"},
		},
	}
}

func main() {
	cfg := config.Load()
	logger := log.New("query-service")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var reranker query.Reranker
	if cfg.RerankURL != "" {
		reranker = query.NewHTTPReranker(cfg.RerankURL, cfg.RerankModel, 0)
	}

	// Feedback drives online down-ranking; a failed backend disables only that
	// stage (memory fallback keeps the service usable).
	feedbackStore, err := feedback.NewStore(cfg.FeedbackDriver, cfg.FeedbackDatabaseURL)
	if err != nil {
		logger.Warn("feedback store unavailable, supporting down-ranking with memory", "error", err)
		feedbackStore = feedback.NewMemoryStore()
	}
	defer feedbackStore.Close()

	engine := query.NewEngine(logger, reranker,
		query.WithFeedbackPenalty(query.NewFeedbackPenalty(feedback.NewNegativeCounter(feedbackStore))),
	)
	docStore := engine.Store()
	docStore.Add("seed", demoCorpus())
	engine.Register(query.NewMemoryRetriever("vector", docStore))
	engine.Register(query.NewMemoryRetriever("bm25", docStore))
	// Graph recall runs in the LlamaIndex service (LightRAG-style entity graph);
	// registering it lets RRF fuse graph candidates with the local routes.
	if cfg.LlamaIndexURL != "" {
		engine.Register(query.NewGraphRetriever(cfg.LlamaIndexURL, 5*time.Second))
	}

	svc := query.NewGRPCService(engine)

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
			logger.Warn("metrics server stopped", "error", err)
		}
	}()

	err = grpcserver.Serve(ctx, cfg.QueryGRPCAddr, func(s *grpc.Server) {
		pb.RegisterQueryServiceServer(s, svc)
	}, logger)
	if err != nil {
		logger.Error("query service stopped", "error", err)
		os.Exit(1)
	}
}
