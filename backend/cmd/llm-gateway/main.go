package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/llmgateway"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
)

func main() {
	cfg := config.Load()
	logger := log.New("llm-gateway")

	routes := make([]llmgateway.Route, 0, 2)
	if cfg.LLMPrimaryBaseURL != "" {
		routes = append(routes, llmgateway.Route{
			Name:    "primary",
			BaseURL: cfg.LLMPrimaryBaseURL,
			APIKey:  cfg.LLMPrimaryAPIKey,
			Model:   cfg.LLMPrimaryModel,
		})
	}
	if cfg.LLMBackupBaseURL != "" {
		routes = append(routes, llmgateway.Route{
			Name:    "backup",
			BaseURL: cfg.LLMBackupBaseURL,
			APIKey:  cfg.LLMBackupAPIKey,
			Model:   cfg.LLMBackupModel,
		})
	}
	if len(routes) == 0 {
		// Stay up so health checks pass; every request then returns 502 with a
		// clear message until an operator configures an upstream.
		logger.Warn("no LLM routes configured (set LLM_PRIMARY_BASE_URL / LLM_BACKUP_BASE_URL)")
	}

	quota := llmgateway.NewQuota(cfg.LLMQuotaDriver, cfg.RedisAddr, cfg.LLMDailyTokenLimit)
	if cfg.LLMDailyTokenLimit <= 0 {
		logger.Warn("LLM_DAILY_TOKEN_LIMIT is 0; tenant quotas are unlimited")
	}
	gateway := llmgateway.New(llmgateway.Options{
		Routes:        routes,
		FailThreshold: cfg.LLMBreakerThreshold,
		Cooldown:      cfg.LLMBreakerCooldown,
		Timeout:       cfg.LLMUpstreamTimeout,
		Quota:         quota,
		Logger:        logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.LLMGatewayAddr,
		Handler:           llmgateway.NewHandler(gateway),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpserver.Serve(ctx, srv, logger); err != nil {
		logger.Error("llm gateway stopped", "error", err)
		os.Exit(1)
	}
}
