package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/config"
	"github.com/nexusflow/nexusflow/backend/internal/platform/httpserver"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/workflow"
)

type runRequest struct {
	DAG   workflow.DAG   `json:"dag"`
	Input map[string]any `json:"input"`
}

func main() {
	cfg := config.Load()
	logger := log.New("workflow-engine")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry := workflow.NewRegistry()
	registry.Register(workflow.NoopNode{})
	registry.Register(workflow.SetNode{})
	for _, stage := range []string{"ingest", "parse", "index", "sandbox", "query"} {
		registry.Register(workflow.StageNode{Name: stage})
	}
	engine := workflow.NewEngine(registry, logger)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/v1/nodes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"nodes": registry.Types()})
	})
	r.POST("/v1/workflows/validate", func(c *gin.Context) {
		var body runRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		order, err := engine.Validate(&body.DAG)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"valid": true, "order": order})
	})
	r.POST("/v1/workflows/run", func(c *gin.Context) {
		var body runRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := engine.Run(c.Request.Context(), &body.DAG, body.Input)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "result": result})
			return
		}
		c.JSON(http.StatusOK, result)
	})

	srv := &http.Server{
		Addr:              cfg.WorkflowHTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpserver.Serve(ctx, srv, logger); err != nil {
		logger.Error("workflow engine stopped", "error", err)
		os.Exit(1)
	}
}
