package gateway

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

// usage returns the tenant consumption snapshot together with the configured
// quota and the remaining allowance, powering the console overview cards.
func (g *Gateway) usage(c *gin.Context) {
	ctx, p := g.callerContext(c)
	usage, err := g.metering.Usage(ctx, p.TenantID)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "usage unavailable: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":     usage,
		"quota":     g.quota,
		"remaining": g.quota.Remaining(usage),
	})
}

// allowUpload verifies the storage/file quota before bytes are accepted. A
// metering outage fails open so uploads are never blocked by an observability
// dependency.
func (g *Gateway) allowUpload(c *gin.Context, addBytes int64) bool {
	p, _ := tenant.From(c.Request.Context())
	usage, err := g.metering.Usage(c.Request.Context(), p.TenantID)
	if err != nil {
		g.logger.Warn("metering usage unavailable, allowing upload", "error", err)
		return true
	}
	if err := g.quota.CheckUpload(usage, addBytes); err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": err.Error(),
			"usage": usage,
			"quota": g.quota,
		})
		return false
	}
	return true
}

// recordUpload counts a newly stored file; deduplicated uploads are not counted.
func (g *Gateway) recordUpload(c *gin.Context, kbID string, bytes int64) {
	if bytes <= 0 {
		return
	}
	p, _ := tenant.From(c.Request.Context())
	if err := g.metering.AddStorage(c.Request.Context(), p.TenantID, kbID, bytes, 1); err != nil {
		g.logger.Warn("record upload usage failed", "error", err)
	}
}

// releaseUpload reverses the storage counters after a cascade delete.
func (g *Gateway) releaseUpload(c *gin.Context, kbID string, bytes int64) {
	if bytes <= 0 {
		return
	}
	p, _ := tenant.From(c.Request.Context())
	if err := g.metering.AddStorage(c.Request.Context(), p.TenantID, kbID, -bytes, -1); err != nil {
		g.logger.Warn("release upload usage failed", "error", err)
	}
}

// allowQuery rejects retrieval/chat turns once the query quota is exhausted.
func (g *Gateway) allowQuery(c *gin.Context) bool {
	p, _ := tenant.From(c.Request.Context())
	usage, err := g.metering.Usage(c.Request.Context(), p.TenantID)
	if err != nil {
		g.logger.Warn("metering usage unavailable, allowing query", "error", err)
		return true
	}
	if err := g.quota.CheckQuery(usage); err != nil {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": err.Error(),
			"usage": usage,
			"quota": g.quota,
		})
		return false
	}
	return true
}

// recordQuery counts one successful retrieval or chat turn.
func (g *Gateway) recordQuery(c *gin.Context) {
	p, _ := tenant.From(c.Request.Context())
	if err := g.metering.AddQuery(c.Request.Context(), p.TenantID, 1); err != nil {
		g.logger.Warn("record query usage failed", "error", err)
	}
}
