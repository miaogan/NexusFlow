package workflow

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// HTTPRunRequest is the shared body shape for validate/run endpoints.
type HTTPRunRequest struct {
	DAG          DAG            `json:"dag"`
	Input        map[string]any `json:"input"`
	DefinitionID string         `json:"definition_id"`
}

// definitionRequest is the create/update body for saved workflows.
type definitionRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Category    string          `json:"category"`
	DAG         DAG             `json:"dag"`
	Layout      map[string]Vec2 `json:"layout"`
}

func (r *definitionRequest) normalize(engine *Engine) error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if r.Category == "" {
		r.Category = "custom"
	}
	if len(r.DAG.Nodes) > 0 {
		if _, err := engine.Validate(&r.DAG); err != nil {
			return err
		}
	}
	if r.DAG.Name == "" {
		r.DAG.Name = r.Name
	}
	return nil
}

type httpOptions struct {
	serviceToken string
	store        RunStore
	definitions  DefinitionStore
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

// WithDefinitionStore persists reusable workflow definitions. Without it the
// API keeps them in memory for the lifetime of the process.
func WithDefinitionStore(store DefinitionStore) HTTPOption {
	return func(o *httpOptions) { o.definitions = store }
}

// NewHTTPHandler builds the workflow engine's HTTP API (used by both the
// standalone workflow-engine and the all-in-one dev process).
func NewHTTPHandler(engine *Engine, registry *Registry, opts ...HTTPOption) http.Handler {
	options := httpOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	if options.definitions == nil {
		options.definitions = NewMemoryDefinitionStore()
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
			RunOptions{
				Meta: meta, Store: options.store, DefinitionID: body.DefinitionID,
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
	})

	v1.GET("/definitions", func(c *gin.Context) {
		meta := requestMeta(c)
		defs, err := options.definitions.List(c.Request.Context(), meta.TenantID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definitions": defs})
	})
	v1.POST("/definitions", func(c *gin.Context) {
		var body definitionRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := body.normalize(engine); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		meta := requestMeta(c)
		now := time.Now().UTC()
		def := &Definition{
			ID:          NewDefinitionID(),
			TenantID:    meta.TenantID,
			Name:        body.Name,
			Description: body.Description,
			Category:    body.Category,
			DAG:         body.DAG,
			Layout:      body.Layout,
			Status:      DefinitionDraft,
			CreatedBy:   meta.UserID,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := options.definitions.Save(c.Request.Context(), def); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"definition": def})
	})
	v1.GET("/definitions/:id", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definition": def})
	})
	v1.PUT("/definitions/:id", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		if def.Status == DefinitionPublished {
			c.JSON(http.StatusConflict, gin.H{
				"error": "workflow is published and locked; unpublish it before editing",
			})
			return
		}
		var body definitionRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := body.normalize(engine); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		def.Name = body.Name
		def.Description = body.Description
		def.Category = body.Category
		def.DAG = body.DAG
		def.Layout = body.Layout
		def.UpdatedAt = time.Now().UTC()
		if err := options.definitions.Save(c.Request.Context(), def); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definition": def})
	})
	v1.DELETE("/definitions/:id", func(c *gin.Context) {
		meta := requestMeta(c)
		found, err := options.definitions.Delete(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "found": found})
	})
	// Publish freezes the current DAG as an immutable, versioned snapshot.
	v1.POST("/definitions/:id/publish", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		if def.Status == DefinitionPublished {
			c.JSON(http.StatusConflict, gin.H{"error": "workflow is already published"})
			return
		}
		if len(def.DAG.Nodes) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "dag has no nodes"})
			return
		}
		if _, err := engine.Validate(&def.DAG); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		now := time.Now().UTC()
		def.Version++
		def.Status = DefinitionPublished
		def.PublishedAt = &now
		def.UpdatedAt = now
		version := &DefinitionVersion{
			DefinitionID: def.ID, TenantID: def.TenantID, Version: def.Version,
			Name: def.Name, Description: def.Description, Category: def.Category,
			DAG: def.DAG, Layout: def.Layout, CreatedBy: meta.UserID, PublishedAt: now,
		}
		if err := options.definitions.SaveVersion(c.Request.Context(), version); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := options.definitions.Save(c.Request.Context(), def); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definition": def, "version": def.Version})
	})
	v1.POST("/definitions/:id/unpublish", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		def.Status = DefinitionDraft
		def.UpdatedAt = time.Now().UTC()
		if err := options.definitions.Save(c.Request.Context(), def); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definition": def})
	})
	v1.GET("/definitions/:id/versions", func(c *gin.Context) {
		meta := requestMeta(c)
		versions, err := options.definitions.ListVersions(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"versions": versions})
	})
	v1.POST("/definitions/:id/rollback", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		var body struct {
			Version int `json:"version"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		versions, err := options.definitions.ListVersions(
			c.Request.Context(), meta.TenantID, def.ID,
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var snapshot *DefinitionVersion
		for _, version := range versions {
			if version.Version == body.Version {
				snapshot = version
				break
			}
		}
		if snapshot == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow version not found"})
			return
		}
		// Rollback restores the snapshot as a draft so it can be reviewed
		// before the next publish.
		def.Name = snapshot.Name
		def.Description = snapshot.Description
		def.Category = snapshot.Category
		def.DAG = snapshot.DAG
		def.Layout = snapshot.Layout
		def.Status = DefinitionDraft
		def.UpdatedAt = time.Now().UTC()
		if err := options.definitions.Save(c.Request.Context(), def); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"definition": def, "restored_version": snapshot.Version})
	})
	v1.POST("/definitions/:id/duplicate", func(c *gin.Context) {
		meta := requestMeta(c)
		def, ok, err := options.definitions.Get(
			c.Request.Context(), meta.TenantID, c.Param("id"),
		)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "workflow definition not found"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if c.Request.ContentLength > 0 {
			if err := c.ShouldBindJSON(&body); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			name = def.Name + " 副本"
		}
		now := time.Now().UTC()
		copyDef := &Definition{
			ID: NewDefinitionID(), TenantID: def.TenantID, Name: name,
			Description: def.Description, Category: def.Category,
			DAG: def.DAG, Layout: def.Layout, Status: DefinitionDraft,
			CreatedBy: meta.UserID, CreatedAt: now, UpdatedAt: now,
		}
		if err := options.definitions.Save(c.Request.Context(), copyDef); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"definition": copyDef})
	})

	v1.GET("/runs", func(c *gin.Context) {
		if options.store == nil {
			c.JSON(http.StatusOK, gin.H{"runs": []any{}})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		if limit <= 0 || limit > 500 {
			limit = 50
		}
		meta := requestMeta(c)
		definitionID := strings.TrimSpace(c.Query("definition_id"))
		status := strings.TrimSpace(c.Query("status"))
		// Over-fetch before filtering so a busy tenant still gets the newest
		// runs of the requested workflow/status.
		listLimit := limit
		if definitionID != "" || status != "" {
			listLimit = 500
		}
		runs := options.store.List(listLimit)
		// Runs are tenant-scoped: never leak another tenant's executions.
		filtered := make([]*RunState, 0, limit)
		for _, run := range runs {
			if definitionID != "" && run.DefinitionID != definitionID {
				continue
			}
			if status != "" && run.Status != status {
				continue
			}
			if run.Meta.TenantID != "" && run.Meta.TenantID != meta.TenantID {
				continue
			}
			filtered = append(filtered, run)
			if len(filtered) >= limit {
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"runs": filtered})
	})
	v1.GET("/runs/:id/children", func(c *gin.Context) {
		if options.store == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "run store not configured"})
			return
		}
		limit, _ := strconv.Atoi(c.Query("limit"))
		meta := requestMeta(c)
		children := options.store.Children(c.Param("id"), limit)
		// Loop sub-runs inherit the parent's identity; keep the same tenant guard.
		filtered := make([]*RunState, 0, len(children))
		for _, run := range children {
			if run.Meta.TenantID == "" || run.Meta.TenantID == meta.TenantID {
				filtered = append(filtered, run)
			}
		}
		c.JSON(http.StatusOK, gin.H{"runs": filtered})
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
		meta := requestMeta(c)
		if state.Meta.TenantID != "" && meta.TenantID != state.Meta.TenantID {
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
	if body.Decision != "approve" && body.Decision != "reject" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "decision must be approve or reject"})
		return
	}
	if body.Decision == "reject" {
		var rejected *RunState
		ok, err := transitionRun(options.store, state.ID, RunWaitingApproval, func(st *RunState) error {
			st.Status = string(RunRejected)
			st.Error = body.Reason
			st.ApprovedBy = meta.UserID
			st.ApprovalReason = body.Reason
			st.UpdatedAt = time.Now()
			rejected = st
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !ok {
			c.JSON(http.StatusConflict, gin.H{"error": "run is not waiting for approval", "status": state.Status})
			return
		}
		updated, found := options.store.Get(state.ID)
		if !found {
			updated = rejected
		}
		c.JSON(http.StatusOK, updated)
		return
	}

	// Claim the run before resuming so two replicas cannot both execute the
	// approved node; the loser gets 409.
	ok, err := transitionRun(options.store, state.ID, RunWaitingApproval, func(st *RunState) error {
		st.Status = string(RunRunning)
		st.ApprovedBy = meta.UserID
		st.ApprovalReason = body.Reason
		st.UpdatedAt = time.Now()
		return nil
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "run is not waiting for approval", "status": state.Status})
		return
	}

	result, err := engine.RunWithOptions(
		c.Request.Context(), &state.DAG, state.Input,
		RunOptions{
			Meta:           state.Meta,
			RunID:          state.ID,
			Store:          options.store,
			Resume:         state,
			DefinitionID:   state.DefinitionID,
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

// transitionRun uses the store's atomic transition when available. The
// Get + Save fallback is best-effort and only used by third-party stores.
func transitionRun(
	store RunStore, id string, from RunStatus, mutate func(*RunState) error,
) (bool, error) {
	if transitioner, ok := store.(RunTransitioner); ok {
		return transitioner.Transition(id, from, mutate)
	}
	state, ok := store.Get(id)
	if !ok || RunStatus(state.Status) != from {
		return false, nil
	}
	if err := mutate(state); err != nil {
		return false, err
	}
	store.Save(state)
	return true, nil
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
