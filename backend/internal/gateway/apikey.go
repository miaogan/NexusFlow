package gateway

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/apikey"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const (
	defaultAPIKeyRPM = 60
	maxAPIKeyRPM     = 6000
)

type createAPIKeyReq struct {
	Name         string   `json:"name" binding:"required"`
	AllowedKBIDs []string `json:"allowed_kb_ids"`
	Scopes       []string `json:"scopes"`
	RateLimitRPM int      `json:"rate_limit_rpm"`
	// ExpiresInDays omits expiry when zero (never expires).
	ExpiresInDays int `json:"expires_in_days"`
}

// createAPIKey issues a tenant-scoped key. The plaintext value is returned
// exactly once; only its SHA-256 hash is persisted.
func (g *Gateway) createAPIKey(c *gin.Context) {
	if g.apiKeys == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key store not configured (set API_KEY_DRIVER)"})
		return
	}
	var body createAPIKeyReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be 1-128 characters"})
		return
	}
	scopes, err := apikey.ValidateScopes(body.Scopes)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rpm := body.RateLimitRPM
	if rpm <= 0 {
		rpm = defaultAPIKeyRPM
	}
	if rpm > maxAPIKeyRPM {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rate_limit_rpm exceeds the maximum"})
		return
	}
	if body.ExpiresInDays < 0 || body.ExpiresInDays > 3650 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expires_in_days must be between 0 and 3650"})
		return
	}

	plain, prefix, err := apikey.NewSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "generate api key: " + err.Error()})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	key := apikey.Key{
		ID:           uuid.NewString(),
		TenantID:     p.TenantID,
		Name:         name,
		KeyPrefix:    prefix,
		KeyHash:      apikey.Hash(plain),
		AllowedKBIDs: apikey.NormalizeKBIDs(body.AllowedKBIDs),
		Scopes:       scopes,
		RateLimitRPM: rpm,
		Status:       apikey.StatusActive,
		CreatedBy:    p.UserID,
		CreatedAt:    time.Now().UTC(),
	}
	if body.ExpiresInDays > 0 {
		expiry := time.Now().UTC().Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour)
		key.ExpiresAt = &expiry
	}

	saved, err := g.apiKeys.Create(c.Request.Context(), key)
	if err != nil {
		if errors.Is(err, apikey.ErrDuplicate) {
			c.JSON(http.StatusConflict, gin.H{"error": "api key collision, retry"})
			return
		}
		g.logger.Error("create api key failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"api_key": saved, "key": plain})
}

// listAPIKeys returns the tenant's keys with their display prefix only.
func (g *Gateway) listAPIKeys(c *gin.Context) {
	if g.apiKeys == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key store not configured (set API_KEY_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	items, err := g.apiKeys.List(c.Request.Context(), p.TenantID)
	if err != nil {
		g.logger.Error("list api keys failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []apikey.Key{}
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": items, "count": len(items)})
}

// revokeAPIKey permanently disables a key; the row is kept for audit.
func (g *Gateway) revokeAPIKey(c *gin.Context) {
	if g.apiKeys == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "api key store not configured (set API_KEY_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	key, err := g.apiKeys.Revoke(c.Request.Context(), p.TenantID, c.Param("id"))
	if err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "api key not found"})
			return
		}
		g.logger.Error("revoke api key failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"api_key": key})
}
