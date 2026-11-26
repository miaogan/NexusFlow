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
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	cfg := config.Load()
	logger := log.New("gateway")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

	g := gateway.New(
		pb.NewIngestionServiceClient(ingestConn),
		pb.NewQueryServiceClient(queryConn),
		logger, cfg.JWTSecret, cfg.AllowAnonymous,
	)

	srv := &http.Server{
		Addr:              cfg.GatewayHTTPAddr,
		Handler:           g.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpserver.Serve(ctx, srv, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}
