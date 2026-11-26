package workflow

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HTTPRunRequest is the shared body shape for validate/run endpoints.
type HTTPRunRequest struct {
	DAG   DAG            `json:"dag"`
	Input map[string]any `json:"input"`
}

// NewHTTPHandler builds the workflow engine's HTTP API (used by both the
// standalone workflow-engine and the all-in-one dev process).
func NewHTTPHandler(engine *Engine, registry *Registry) http.Handler {
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
		var body HTTPRunRequest
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
		var body HTTPRunRequest
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

	return r
}
