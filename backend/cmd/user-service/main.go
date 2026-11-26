package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	"github.com/nexusflow/nexusflow/backend/internal/platform/grpcserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
)

// user-service owns tenant-scoped user CRUD plus role and permission management.
func main() {
	cfg := config.Load()
	logger := log.New("user-service")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := identity.NewMemoryStore()
	if err := store.Seed(identity.SeedOptions{
		TenantID:      cfg.SeedTenantID,
		AdminUsername: cfg.SeedAdminUsername,
		AdminPassword: cfg.SeedAdminPassword,
	}); err != nil {
		logger.Error("seed identity store", "error", err)
		os.Exit(1)
	}

	svc := identity.NewUserService(store)

	if err := grpcserver.Serve(ctx, cfg.UserGRPCAddr, func(s *grpc.Server) {
		pb.RegisterUserServiceServer(s, svc)
	}, logger); err != nil {
		logger.Error("user service stopped", "error", err)
		os.Exit(1)
	}
}
