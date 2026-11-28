package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/apikey"
	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/connector"
	"github.com/nexusflow/nexusflow/backend/internal/feedback"
	"github.com/nexusflow/nexusflow/backend/internal/gateway"
	"github.com/nexusflow/nexusflow/backend/internal/metering"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/revocation"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	cfg := config.Load()
	logger := log.New("gateway")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	objects, err := store.NewLocalObjectStore(cfg.ObjectStoreDir, cfg.ObjectStoreBase)
	if err != nil {
		logger.Error("init object store", "error", err)
		os.Exit(1)
	}

	feedbackStore, err := feedback.NewStore(cfg.FeedbackDriver, cfg.FeedbackDatabaseURL)
	if err != nil {
		logger.Error("init feedback store", "error", err)
		os.Exit(1)
	}
	defer feedbackStore.Close()

	apiKeyStore, err := apikey.NewStore(cfg.APIKeyDriver, cfg.APIKeyDatabaseURL)
	if err != nil {
		logger.Error("init api key store", "error", err)
		os.Exit(1)
	}
	defer apiKeyStore.Close()

	webhookStore, err := webhook.NewStore(cfg.WebhookDriver, cfg.WebhookDatabaseURL)
	if err != nil {
		logger.Error("init webhook store", "error", err)
		os.Exit(1)
	}
	defer webhookStore.Close()
	webhooks := webhook.NewDispatcher(
		webhookStore,
		connector.NewCipher(cfg.WebhookEncryptionKey),
		logger,
		cfg.WebhookMaxAttempts,
		cfg.WebhookRetryBase,
		webhook.TargetPolicy{AllowPrivate: cfg.WebhookAllowPrivate},
	)

	meterStore, err := metering.NewStore(cfg.MeteringDriver, cfg.RedisAddr)
	if err != nil {
		logger.Warn("metering backend unavailable, falling back to memory", "error", err)
		meterStore = metering.NewMemoryStore()
	}

	blacklist, err := revocation.New(cfg.TokenBlacklistDriver, cfg.RedisAddr)
	if err != nil {
		logger.Warn("token blacklist backend unavailable, falling back to memory", "error", err)
	}
	defer blacklist.Close()

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

	g := gateway.New(
		pb.NewIngestionServiceClient(ingestConn),
		pb.NewQueryServiceClient(queryConn),
		pb.NewSandboxExecutionServiceClient(sandboxConn),
		objects,
		logger, cfg.JWTSecret, cfg.AllowAnonymous,
		gateway.WithIdentity(
			pb.NewAuthServiceClient(authConn),
			pb.NewUserServiceClient(userConn),
		),
		gateway.WithKnowledgeBases(pb.NewKnowledgeBaseServiceClient(userConn)),
		gateway.WithRAGAgent(cfg.RAGAgentURL, cfg.AgentHubToken),
		gateway.WithWorkflow(cfg.WorkflowTarget, cfg.WorkflowServiceToken),
		gateway.WithConnectors(cfg.ConnectorTarget),
		gateway.WithFeedbackStore(feedbackStore),
		gateway.WithAPIKeys(apiKeyStore),
		gateway.WithWebhooks(webhooks, cfg.WebhookEventsToken),
		gateway.WithMetering(meterStore, metering.Quota{
			MaxStorageBytes: cfg.QuotaMaxStorageBytes,
			MaxFiles:        cfg.QuotaMaxFiles,
			MaxQueries:      cfg.QuotaMaxQueriesPerMonth,
		}),
		gateway.WithTokenBlacklist(blacklist),
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
