package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const maxConnectorBodyBytes = 256 << 10

// connectorProxy relays connector management to the ingestion service's HTTP
// listener. The gateway owns RBAC; ingestion trusts only the internal network.
type connectorProxy struct {
	baseURL string
	client  *http.Client
}

func newConnectorProxy(raw string) *connectorProxy {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil
	}
	return &connectorProxy{
		baseURL: raw,
		client:  &http.Client{Timeout: 180 * time.Second},
	}
}

// WithConnectors enables the /v1/connectors endpoints.
func WithConnectors(raw string) Option {
	return func(g *Gateway) { g.connectors = newConnectorProxy(raw) }
}

func (g *Gateway) listConnectors(c *gin.Context) {
	path := "/v1/connectors"
	if query := c.Request.URL.Query().Encode(); query != "" {
		path += "?" + query
	}
	g.proxyConnector(c, http.MethodGet, path)
}

func (g *Gateway) createConnector(c *gin.Context) {
	g.proxyConnector(c, http.MethodPost, "/v1/connectors")
}

func (g *Gateway) getConnector(c *gin.Context) {
	g.proxyConnector(c, http.MethodGet, "/v1/connectors/"+c.Param("id"))
}

func (g *Gateway) deleteConnector(c *gin.Context) {
	g.proxyConnector(c, http.MethodDelete, "/v1/connectors/"+c.Param("id"))
}

func (g *Gateway) syncConnector(c *gin.Context) {
	g.proxyConnector(c, http.MethodPost, "/v1/connectors/"+c.Param("id")+"/sync")
}

func (g *Gateway) proxyConnector(c *gin.Context, method, path string) {
	if g.connectors == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "connector API not configured (set CONNECTOR_TARGET)"})
		return
	}
	var body []byte
	if method == http.MethodPost || method == http.MethodPut {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxConnectorBodyBytes)
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}
		body = raw
	}
	req, err := http.NewRequestWithContext(
		c.Request.Context(), method, g.connectors.baseURL+path, bytes.NewReader(body),
	)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	principal, _ := tenant.From(c.Request.Context())
	for header, value := range map[string]string{
		"x-tenant-id":  principal.TenantID,
		"x-user-id":    principal.UserID,
		"x-trace-id":   c.GetHeader("x-trace-id"),
		"x-request-id": c.GetHeader("x-request-id"),
	} {
		if value != "" {
			req.Header.Set(header, value)
		}
	}
	resp, err := g.connectors.client.Do(req)
	if err != nil {
		g.logger.Error("connector request failed", "path", path, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "connector service unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}
