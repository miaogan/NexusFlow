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

// auth-service issues and validates JWTs, and handles register/login/refresh.
func main() {
	cfg := config.Load()
	logger := log.New("auth-service")

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

	svc := identity.NewAuthService(store, identity.AuthConfig{
		Secret:            cfg.JWTSecret,
		AccessTTL:         cfg.AccessTokenTTL,
		RefreshTTL:        cfg.RefreshTokenTTL,
		AllowSelfRegister: cfg.AllowSelfRegister,
	})

	if err := grpcserver.Serve(ctx, cfg.AuthGRPCAddr, func(s *grpc.Server) {
		pb.RegisterAuthServiceServer(s, svc)
	}, logger); err != nil {
		logger.Error("auth service stopped", "error", err)
		os.Exit(1)
	}
}
