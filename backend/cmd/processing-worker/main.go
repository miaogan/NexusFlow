package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/indexing"
	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/platform/broker"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/processing"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
)

const mockMarkdown = `# 示例文档

## 1. 概述

NexusFlow 采用 Go 负责编排调度、Python LlamaIndex 负责切块与索引。

## 2. 检索

查询链路使用向量召回、BM25 与 LightRAG 三路召回，经 RRF 融合后做 Cross-Encoder 重排。
`

func main() {
	cfg := config.Load()
	logger := log.New("processing-worker")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := state.NewStore(cfg.StateDriver, cfg.RedisAddr)
	if err != nil {
		logger.Error("init state store", "error", err)
		os.Exit(1)
	}

	b, err := broker.New(cfg.QueueDriver, cfg.QueueURL, logger)
	if err != nil {
		logger.Error("init broker", "error", err)
		os.Exit(1)
	}
	defer b.Close()

	var client mineru.Client
	if cfg.MinerUTarget == "" {
		logger.Warn("MINERU_TARGET is empty, using mock parser")
		client = &mineru.MockClient{Markdown: mockMarkdown}
	} else {
		httpClient := mineru.NewHTTPClient(cfg.MinerUTarget, cfg.ParsePollInterval, 60)
		httpClient.APIKey = cfg.MinerUAPIKey
		httpClient.Tier = cfg.MinerUTier
		httpClient.ObjectDir = cfg.ObjectStoreDir
		httpClient.ObjectBase = cfg.ObjectStoreBase
		client = httpClient
	}

	objects, objErr := store.NewLocalObjectStore(cfg.ObjectStoreDir, cfg.ObjectStoreBase)
	if objErr != nil {
		logger.Warn("object store unavailable, parsed markdown will not be persisted", "error", objErr)
	}
	workerOpts := []processing.WorkerOption{}
	if objErr == nil {
		workerOpts = append(workerOpts, processing.WithObjectStore(objects))
	}
	worker := processing.NewWorker(
		client, st, b, logger,
		cfg.ParsePollInterval, 60, cfg.ParseMaxAttempts,
		cfg.QueueTopicParsed, cfg.QueueTopicDLQ, "processing-worker",
		workerOpts...,
	)
	b.Subscribe(cfg.QueueTopicUploaded, worker.Handle)

	// Production indexing: parsed Markdown is written to the real vector store
	// through the LlamaIndex service (chunk + embed + Qdrant upsert).
	if cfg.LlamaIndexURL != "" {
		idxr := indexing.New(indexing.NewLlamaIndexIndexer(cfg.LlamaIndexURL, 0), st, logger)
		b.Subscribe(cfg.QueueTopicParsed, idxr.Handle)
		logger.Info("llamaindex indexing enabled", "url", cfg.LlamaIndexURL)
	} else {
		logger.Warn("LLAMAINDEX_URL is empty, parsed documents will not be indexed")
	}

	parseSvc := processing.NewParseService(client, st, logger)

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
			logger.Warn("metrics server stopped", "error", err)
		}
	}()

	err = grpcserver.Serve(ctx, cfg.ProcessingGRPCAddr, func(s *grpc.Server) {
		pb.RegisterProcessingServiceServer(s, parseSvc)
	}, logger)
	if err != nil {
		logger.Error("processing worker stopped", "error", err)
		os.Exit(1)
	}
}
