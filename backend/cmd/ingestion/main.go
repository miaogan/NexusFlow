package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/indexing"
	"github.com/nexusflow/nexusflow/backend/internal/ingestion"
	"github.com/nexusflow/nexusflow/backend/internal/platform/broker"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()
	logger := log.New("ingestion")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := state.NewStore(cfg.StateDriver, cfg.RedisAddr)
	if err != nil {
		logger.Error("init state store", "error", err)
		os.Exit(1)
	}

	objects, err := store.NewLocalObjectStore(cfg.ObjectStoreDir, cfg.ObjectStoreBase)
	if err != nil {
		logger.Error("init object store", "error", err)
		os.Exit(1)
	}

	b, err := broker.New(cfg.QueueDriver, cfg.QueueURL, logger)
	if err != nil {
		logger.Error("init broker", "error", err)
		os.Exit(1)
	}
	defer b.Close()

	// Cascade deletion needs the retrieval index; without LLAMAINDEX_URL the
	// cleanup step is skipped and only the object + task record are removed.
	var opts []ingestion.Option
	if cfg.LlamaIndexURL != "" {
		opts = append(opts, ingestion.WithIndexCleaner(indexing.NewLlamaIndexIndexer(cfg.LlamaIndexURL, 0)))
		logger.Info("cascade index cleanup enabled", "url", cfg.LlamaIndexURL)
	}
	svc := ingestion.NewService(objects, st, b, logger, cfg.ObjectStoreBase, cfg.QueueTopicUploaded, opts...)

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
			logger.Warn("metrics server stopped", "error", err)
		}
	}()

	err = grpcserver.Serve(ctx, cfg.IngestionGRPCAddr, func(s *grpc.Server) {
		pb.RegisterIngestionServiceServer(s, svc)
	}, logger)
	if err != nil {
		logger.Error("ingestion stopped", "error", err)
		os.Exit(1)
	}
}
