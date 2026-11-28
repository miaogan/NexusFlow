package connector

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Handler exposes connector management and manual sync over HTTP. It is served
// by the ingestion service on an internal port and proxied by the gateway.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/v1/connectors")
	v1.GET("", func(c *gin.Context) {
		tenantID, ok := tenant(c)
		if !ok {
			return
		}
		specs, err := svc.List(c.Request.Context(), tenantID, c.Query("kb_id"))
		if err != nil {
			fail(c, err)
			return
		}
		out := make([]gin.H, 0, len(specs))
		for _, spec := range specs {
			out = append(out, connectorView(spec))
		}
		c.JSON(http.StatusOK, gin.H{"connectors": out})
	})
	v1.POST("", func(c *gin.Context) {
		tenantID, ok := tenant(c)
		if !ok {
			return
		}
		var body createRequest
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		spec, err := svc.Create(c.Request.Context(), tenantID, Spec{
			Name:         body.Name,
			KBID:         body.KBID,
			Type:         body.Type,
			Config:       body.Config,
			ScheduleCron: body.ScheduleCron,
		})
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(http.StatusCreated, connectorView(spec))
	})
	v1.GET("/:id", func(c *gin.Context) {
		tenantID, ok := tenant(c)
		if !ok {
			return
		}
		spec, found, err := svc.Get(c.Request.Context(), tenantID, c.Param("id"))
		if err != nil {
			fail(c, err)
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": ErrNotFound.Error()})
			return
		}
		c.JSON(http.StatusOK, connectorView(spec))
	})
	v1.DELETE("/:id", func(c *gin.Context) {
		tenantID, ok := tenant(c)
		if !ok {
			return
		}
		removed, err := svc.Delete(c.Request.Context(), tenantID, c.Param("id"))
		if err != nil {
			fail(c, err)
			return
		}
		if !removed {
			c.JSON(http.StatusNotFound, gin.H{"error": ErrNotFound.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"removed": c.Param("id")})
	})
	v1.POST("/:id/sync", func(c *gin.Context) {
		tenantID, ok := tenant(c)
		if !ok {
			return
		}
		result, err := svc.Sync(c.Request.Context(), tenantID, c.Param("id"))
		if err != nil {
			fail(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	return r
}

type createRequest struct {
	Name         string `json:"name"`
	KBID         string `json:"kb_id"`
	Type         Type   `json:"type"`
	Config       Config `json:"config"`
	ScheduleCron string `json:"schedule_cron"`
}

func tenant(c *gin.Context) (string, bool) {
	tenantID := c.GetHeader("x-tenant-id")
	if tenantID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "x-tenant-id is required"})
		return "", false
	}
	return tenantID, true
}

func connectorView(spec Spec) gin.H {
	view := gin.H{
		"id":            spec.ID,
		"kb_id":         spec.KBID,
		"name":          spec.Name,
		"type":          spec.Type,
		"config":        MaskConfig(spec.Config),
		"schedule_cron": spec.ScheduleCron,
		"status":        spec.Status,
		"sync_cursor":   spec.SyncCursor,
		"last_error":    spec.LastError,
		"created_at":    spec.CreatedAt,
		"updated_at":    spec.UpdatedAt,
	}
	if !spec.LastSyncAt.IsZero() {
		view["last_sync_at"] = spec.LastSyncAt
	}
	return view
}

func fail(c *gin.Context, err error) {
	if errors.Is(err, ErrNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
}
