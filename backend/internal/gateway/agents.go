package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
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
	body, ok := g.readAgentBody(c)
	if !ok {
		return
	}
	g.proxyRAG(c, http.MethodPost, path, body)
}

// readAgentBody reads a size-limited Agent Hub request body.
func (g *Gateway) readAgentBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return nil, false
	}
	return body, true
}

func (g *Gateway) hubDelete(c *gin.Context, path string) {
	if !g.requireRAG(c) {
		return
	}
	g.proxyRAG(c, http.MethodDelete, path, nil)
}

func (g *Gateway) listApprovals(c *gin.Context) {
	g.hubGet(c, "/v1/approvals")
}

func (g *Gateway) getApproval(c *gin.Context) {
	g.hubGet(c, "/v1/approvals/"+url.PathEscape(c.Param("id")))
}

func (g *Gateway) decideApproval(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	body, ok := g.readAgentBody(c)
	if !ok {
		return
	}
	path := "/v1/approvals/" + url.PathEscape(c.Param("id")) + "/decide"
	resp, err := g.rag.do(c, http.MethodPost, path, body)
	if err != nil {
		g.logger.Error("rag agent request failed", "path", path, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "rag agent unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxAgentBodyBytes))
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = c.Writer.Write(payload)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		decision := "approve"
		var req struct {
			Decision string `json:"decision"`
		}
		if json.Unmarshal(body, &req) == nil && strings.TrimSpace(req.Decision) != "" {
			decision = strings.TrimSpace(req.Decision)
		}
		g.emitEvent(c, webhook.EventApprovalDecided, map[string]any{
			"source":      "agent_hub",
			"approval_id": c.Param("id"),
			"decision":    decision,
		})
	}
}

func (g *Gateway) listAudit(c *gin.Context) {
	path := "/v1/audit"
	if query := c.Request.URL.Query().Encode(); query != "" {
		path += "?" + query
	}
	g.hubGet(c, path)
}

func (g *Gateway) listAgents(c *gin.Context) {
	g.hubGet(c, "/v1/agents")
}

func (g *Gateway) getAgent(c *gin.Context) {
	g.hubGet(c, "/v1/agents/"+url.PathEscape(c.Param("name")))
}

func (g *Gateway) createAgent(c *gin.Context) {
	g.hubPost(c, "/v1/agents")
}

func (g *Gateway) deleteAgent(c *gin.Context) {
	g.hubDelete(c, "/v1/agents/"+url.PathEscape(c.Param("name")))
}

func (g *Gateway) heartbeatAgent(c *gin.Context) {
	g.hubPost(c, "/v1/agents/"+url.PathEscape(c.Param("name"))+"/heartbeat")
}

func (g *Gateway) listSkills(c *gin.Context) {
	g.hubGet(c, "/v1/skills")
}

type agentCallRequest struct {
	Capability string         `json:"capability"`
	Input      map[string]any `json:"input"`
	ApprovalID string         `json:"approval_id"`
}

// callAgent invokes a custom SubAgent through the hub's /v1/call. The caller's
// bearer token is forwarded so a sandbox agent still has to pass the gateway's
// sandbox:execute check on its own execution request.
func (g *Gateway) callAgent(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxAgentBodyBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return
	}
	var req agentCallRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	capability := strings.TrimSpace(req.Capability)
	if capability == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "capability is required"})
		return
	}
	if req.Input == nil {
		req.Input = map[string]any{}
	}
	envelope := map[string]any{}
	if req.ApprovalID != "" {
		envelope["approval_id"] = req.ApprovalID
	}
	payload := map[string]any{
		"agent": map[string]any{
			"name":       c.Param("name"),
			"capability": capability,
			"input":      req.Input,
		},
	}
	if len(envelope) > 0 {
		payload["envelope"] = envelope
	}
	body, err := json.Marshal(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encode agent call: " + err.Error()})
		return
	}
	g.proxyRAG(c, http.MethodPost, "/v1/call", body)
}
