package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

const maxWebhookBodyBytes = 1 << 20 // 1 MiB

type createWebhookReq struct {
	Name      string   `json:"name" binding:"required"`
	TargetURL string   `json:"target_url" binding:"required"`
	Events    []string `json:"events"`
	Secret    string   `json:"secret"`
	IsActive  *bool    `json:"is_active"`
}

// createWebhook registers a tenant subscription. When no secret is supplied a
// random one is generated and returned exactly once.
func (g *Gateway) createWebhook(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	var body createWebhookReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must be 1-128 characters"})
		return
	}
	if err := g.webhooks.Policy().Validate(c.Request.Context(), body.TargetURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	events, err := webhook.NormalizeEvents(body.Events)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	secret := strings.TrimSpace(body.Secret)
	if secret == "" {
		secret, err = webhook.NewSecret()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "generate secret: " + err.Error()})
			return
		}
	} else if len(secret) < 16 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "secret must be at least 16 characters"})
		return
	}
	encrypted, err := g.webhooks.EncryptSecret(secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret: " + err.Error()})
		return
	}

	active := true
	if body.IsActive != nil {
		active = *body.IsActive
	}
	p, _ := tenant.From(c.Request.Context())
	sub := webhook.Subscription{
		ID:              uuid.NewString(),
		TenantID:        p.TenantID,
		Name:            name,
		TargetURL:       strings.TrimSpace(body.TargetURL),
		SecretEncrypted: encrypted,
		Events:          events,
		IsActive:        active,
	}
	saved, err := g.webhooks.Store().CreateSubscription(c.Request.Context(), sub)
	if err != nil {
		g.logger.Error("create webhook failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"webhook": saved, "secret": secret})
}

func (g *Gateway) listWebhooks(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	items, err := g.webhooks.Store().ListSubscriptions(c.Request.Context(), p.TenantID)
	if err != nil {
		g.logger.Error("list webhooks failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []webhook.Subscription{}
	}
	c.JSON(http.StatusOK, gin.H{"webhooks": items, "count": len(items)})
}

func (g *Gateway) deleteWebhook(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	if err := g.webhooks.Store().DeleteSubscription(c.Request.Context(), p.TenantID, c.Param("id")); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		g.logger.Error("delete webhook failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "id": c.Param("id")})
}

// testWebhook queues a synthetic webhook.test delivery for one subscription.
func (g *Gateway) testWebhook(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	if err := g.webhooks.SendTest(c.Request.Context(), p.TenantID, c.Param("id")); err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"scheduled": true})
}

// rotateWebhookSecret replaces the HMAC signing secret and returns the new
// plaintext exactly once.
func (g *Gateway) rotateWebhookSecret(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	sub, err := g.webhooks.Store().GetSubscription(c.Request.Context(), p.TenantID, c.Param("id"))
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	secret, err := webhook.NewSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "generate secret: " + err.Error()})
		return
	}
	encrypted, err := g.webhooks.EncryptSecret(secret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt secret: " + err.Error()})
		return
	}
	sub.SecretEncrypted = encrypted
	updated, err := g.webhooks.Store().UpdateSubscription(c.Request.Context(), sub)
	if err != nil {
		g.logger.Error("rotate webhook secret failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"webhook": updated, "secret": secret})
}

// replayWebhookDelivery re-sends a stored delivery payload as a new attempt.
func (g *Gateway) replayWebhookDelivery(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	p, _ := tenant.From(c.Request.Context())
	err := g.webhooks.Replay(
		c.Request.Context(), p.TenantID, c.Param("id"), c.Param("deliveryId"),
	)
	if err != nil {
		if errors.Is(err, webhook.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "delivery or webhook not found"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"scheduled": true})
}

func (g *Gateway) listWebhookDeliveries(c *gin.Context) {
	if g.webhooks == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook store not configured (set WEBHOOK_DRIVER)"})
		return
	}
	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 200 {
		limit = 200
	}
	p, _ := tenant.From(c.Request.Context())
	items, err := g.webhooks.Store().ListDeliveries(
		c.Request.Context(), p.TenantID, c.Param("id"), limit,
	)
	if err != nil {
		g.logger.Error("list webhook deliveries failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if items == nil {
		items = []webhook.Delivery{}
	}
	c.JSON(http.StatusOK, gin.H{"deliveries": items, "count": len(items)})
}

// ingestWebhookEvent lets trusted services publish platform events. The
// gateway authenticates the caller with the shared WEBHOOK_EVENTS_TOKEN.
func (g *Gateway) ingestWebhookEvent(c *gin.Context) {
	if g.webhooks == nil || g.webhookToken == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook event ingest is disabled"})
		return
	}
	token := c.GetHeader("X-Webhook-Token")
	if token == "" {
		token = bearerToken(c.GetHeader("Authorization"))
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(g.webhookToken)) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook token"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return
	}
	var evt webhook.Event
	if err := json.Unmarshal(raw, &evt); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	if strings.TrimSpace(evt.Type) == "" || strings.TrimSpace(evt.TenantID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "type and tenant_id are required"})
		return
	}
	if evt.ID == "" {
		evt.ID = uuid.NewString()
	}
	g.webhooks.Publish(c.Request.Context(), evt)
	c.JSON(http.StatusAccepted, gin.H{"accepted": true, "event_id": evt.ID})
}

// emitEvent publishes a platform event for the current caller's tenant.
func (g *Gateway) emitEvent(c *gin.Context, eventType string, payload map[string]any) {
	if g.webhooks == nil {
		return
	}
	p, _ := tenant.From(c.Request.Context())
	if p.TenantID == "" {
		return
	}
	g.webhooks.Publish(c.Request.Context(), webhook.Event{
		Type: eventType, TenantID: p.TenantID, Payload: payload,
	})
}
