package workflow

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// HTTPRunRequest is the shared body shape for validate/run endpoints.
type HTTPRunRequest struct {
	DAG   DAG            `json:"dag"`
	Input map[string]any `json:"input"`
}

type httpOptions struct {
	serviceToken string
	store        RunStore
}

// HTTPOption configures the workflow HTTP API.
type HTTPOption func(*httpOptions)

// WithServiceToken rejects direct calls that do not carry the shared token.
// Gateway calls attach it, so the workflow service cannot be reached around
// the gateway's RBAC and rate limiting.
func WithServiceToken(token string) HTTPOption {
	return func(o *httpOptions) { o.serviceToken = token }
}

// WithRunStore persists runs that suspend on human approval.
func WithRunStore(store RunStore) HTTPOption {
	return func(o *httpOptions) { o.store = store }
}

// NewHTTPHandler builds the workflow engine's HTTP API (used by both the
// standalone workflow-engine and the all-in-one dev process).
func NewHTTPHandler(engine *Engine, registry *Registry, opts ...HTTPOption) http.Handler {
	options := httpOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/v1/nodes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"nodes": registry.Types()})
	})

	v1 := r.Group("/v1/workflows", serviceAuth(options.serviceToken))
	v1.POST("/validate", func(c *gin.Context) {
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
	v1.POST("/run", func(c *gin.Context) {
		var body HTTPRunRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		meta := requestMeta(c)
		if meta.TraceID != "" {
			c.Header("x-trace-id", meta.TraceID)
		}
		result, err := engine.RunWithOptions(
			c.Request.Context(), &body.DAG, body.Input,
			RunOptions{Meta: meta, Store: options.store},
		)
		if err != nil {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "result": result})
			return
		}
		if result.Status == string(RunWaitingApproval) {
			c.JSON(http.StatusAccepted, result)
			return
		}
		c.JSON(http.StatusOK, result)
	})

	v1.GET("/runs", func(c *gin.Context) {
		if options.store == nil {
			c.JSON(http.StatusOK, gin.H{"runs": []any{}})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		c.JSON(http.StatusOK, gin.H{"runs": options.store.List(limit)})
	})
	v1.GET("/runs/:id", func(c *gin.Context) {
		if options.store == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run store not configured"})
			return
		}
		state, ok := options.store.Get(c.Param("id"))
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
			return
		}
		c.JSON(http.StatusOK, state)
	})
	v1.POST("/runs/:id/approve", func(c *gin.Context) {
		decideRun(c, engine, options)
	})
	v1.POST("/runs/:id/resume", func(c *gin.Context) {
		decideRun(c, engine, options)
	})

	return r
}

type approvalRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// decideRun approves or rejects a suspended run and resumes execution when
// approved. Re-deciding a finished run returns 409.
func decideRun(c *gin.Context, engine *Engine, options httpOptions) {
	if options.store == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "run store not configured"})
		return
	}
	state, ok := options.store.Get(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if state.Status != string(RunWaitingApproval) {
		c.JSON(http.StatusConflict, gin.H{"error": "run is not waiting for approval", "status": state.Status})
		return
	}

	var body approvalRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.Decision == "" {
		body.Decision = "approve"
	}
	meta := requestMeta(c)
	if headerTenant := c.GetHeader("x-tenant-id"); headerTenant != "" &&
		state.Meta.TenantID != "" && headerTenant != state.Meta.TenantID {
		c.JSON(http.StatusNotFound, gin.H{"error": "run not found"})
		return
	}
	if meta.TenantID == "" {
		meta.TenantID = state.Meta.TenantID
	}
	if body.Decision == "reject" {
		state.Status = string(RunRejected)
		state.Error = body.Reason
		state.UpdatedAt = time.Now()
		options.store.Save(state)
		c.JSON(http.StatusOK, state)
		return
	}
	if body.Decision != "approve" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "decision must be approve or reject"})
		return
	}

	result, err := engine.RunWithOptions(
		c.Request.Context(), &state.DAG, state.Input,
		RunOptions{
			Meta:           state.Meta,
			RunID:          state.ID,
			Store:          options.store,
			Resume:         state,
			Approved:       true,
			ApprovedBy:     meta.UserID,
			ApprovalReason: body.Reason,
		},
	)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error(), "result": result})
		return
	}
	if result.Status == string(RunWaitingApproval) {
		c.JSON(http.StatusAccepted, result)
		return
	}
	c.JSON(http.StatusOK, result)
}

func serviceAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}
		given := c.GetHeader("x-service-token")
		if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid service token"})
			return
		}
		c.Next()
	}
}

func requestMeta(c *gin.Context) Meta {
	meta := Meta{
		TenantID:  c.GetHeader("x-tenant-id"),
		UserID:    c.GetHeader("x-user-id"),
		TraceID:   c.GetHeader("x-trace-id"),
		RequestID: c.GetHeader("x-request-id"),
	}
	if meta.TenantID == "" {
		meta.TenantID = "default"
	}
	if meta.UserID == "" {
		meta.UserID = "anonymous"
	}
	return meta
}
