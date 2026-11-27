package gateway

import (
	"io"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
)

const maxAgentBodyBytes = 1 << 20 // 1 MiB

// hubGet relays read-only Agent Hub endpoints (approvals, audit).
func (g *Gateway) hubGet(c *gin.Context, path string) {
	if !g.requireRAG(c) {
		return
	}
	if query := c.Request.URL.Query().Encode(); query != "" {
		path += "?" + query
	}
	g.proxyRAG(c, http.MethodGet, path, nil)
}

// hubPost relays a JSON body to the Agent Hub with a size limit.
func (g *Gateway) hubPost(c *gin.Context, path string) {
	if !g.requireRAG(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return
	}
	g.proxyRAG(c, http.MethodPost, path, body)
}

func (g *Gateway) listApprovals(c *gin.Context) {
	g.hubGet(c, "/v1/approvals")
}

func (g *Gateway) getApproval(c *gin.Context) {
	g.hubGet(c, "/v1/approvals/"+url.PathEscape(c.Param("id")))
}

func (g *Gateway) decideApproval(c *gin.Context) {
	g.hubPost(c, "/v1/approvals/"+url.PathEscape(c.Param("id"))+"/decide")
}

func (g *Gateway) listAudit(c *gin.Context) {
	g.hubGet(c, "/v1/audit")
}
