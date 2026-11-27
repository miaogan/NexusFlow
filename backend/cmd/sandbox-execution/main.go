package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/sandbox"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()
	logger := log.New("sandbox-execution")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var backend sandbox.Backend
	if cfg.SandboxBackend == "remote" {
		logger.Info("using remote OpenSandbox backend",
			"url", cfg.OpenSandboxURL,
			"python_image", cfg.OpenSandboxPythonImage,
			"node_image", cfg.OpenSandboxNodeImage,
		)
		backend = sandbox.NewRemoteBackendWithImages(
			cfg.OpenSandboxURL, cfg.OpenSandboxToken,
			cfg.OpenSandboxPythonImage, cfg.OpenSandboxNodeImage,
		)
	} else {
		if cfg.Env == "prod" && !cfg.SandboxAllowLocal {
			logger.Error(
				"local sandbox backend is not a security boundary; " +
					"set SANDBOX_BACKEND=remote or explicitly allow it with SANDBOX_ALLOW_LOCAL=true",
			)
			os.Exit(1)
		}
		logger.Warn("using local sandbox backend, not a security boundary")
		backend = sandbox.NewLocalBackend(cfg.SandboxWorkDir)
	}

	svc := sandbox.NewGRPCService(sandbox.NewService(backend, logger))

	go func() {
		if err := metrics.Serve(ctx, cfg.MetricsAddr, logger); err != nil {
			logger.Warn("metrics server stopped", "error", err)
		}
	}()

	err := grpcserver.Serve(ctx, cfg.SandboxGRPCAddr, func(s *grpc.Server) {
		pb.RegisterSandboxExecutionServiceServer(s, svc)
	}, logger)
	if err != nil {
		logger.Error("sandbox service stopped", "error", err)
		os.Exit(1)
	}
}
