package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const maxChatBodyBytes = 1 << 20 // 1 MiB

// ragProxy is a thin HTTP relay to the Python rag-agent service. The gateway
// stays the single entry point: it authenticates the caller, then forwards the
// principal so the RAG service can scope conversations per tenant/user.
type ragProxy struct {
	baseURL string
	client  *http.Client
}

func newRAGProxy(raw string) *ragProxy {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil
	}
	return &ragProxy{baseURL: raw, client: &http.Client{Timeout: 120 * time.Second}}
}

func (p *ragProxy) do(c *gin.Context, method, path string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), method, p.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Pass the bearer token through so the RAG service can reuse /v1/search
	// without inventing a second credential.
	for _, h := range []string{"Authorization", "X-Trace-ID"} {
		if v := c.GetHeader(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	principal, _ := tenant.From(c.Request.Context())
	req.Header.Set("X-Tenant-ID", principal.TenantID)
	req.Header.Set("X-User-ID", principal.UserID)
	req.Header.Set("X-Roles", strings.Join(principal.Roles, ","))
	req.Header.Set("X-Permissions", strings.Join(principal.Permissions, ","))
	return p.client.Do(req)
}

func (g *Gateway) requireRAG(c *gin.Context) bool {
	if g.rag == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "rag agent not configured (set RAG_AGENT_URL)"})
		return false
	}
	return true
}

// proxyRAG relays the upstream response verbatim (status, content type, body)
// so the Python service remains the source of truth for chat payloads. It
// returns the upstream status code (0 on transport failure) so callers can
// meter successful turns.
func (g *Gateway) proxyRAG(c *gin.Context, method, path string, body []byte) int {
	resp, err := g.rag.do(c, method, path, body)
	if err != nil {
		g.logger.Error("rag agent request failed", "path", path, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "rag agent unavailable: " + err.Error()})
		return 0
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
	return resp.StatusCode
}

type chatReq struct {
	Message        string            `json:"message"`
	ConversationID string            `json:"conversation_id"`
	TopK           int               `json:"top_k"`
	KBID           string            `json:"kb_id"`
	Filters        map[string]string `json:"filters"`
}

func (g *Gateway) chat(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	body, ok := g.readChatBody(c)
	if !ok {
		return
	}
	if !g.allowQuery(c) {
		return
	}
	if status := g.proxyRAG(c, http.MethodPost, "/v1/chat", body); status >= 200 && status < 300 {
		g.recordQuery(c)
	}
}

// chatStream relays the RAG agent's SSE stream without buffering it, so the
// console can render retrieve → generate → answer progressively.
func (g *Gateway) chatStream(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	body, ok := g.readChatBody(c)
	if !ok {
		return
	}
	if !g.allowQuery(c) {
		return
	}
	resp, err := g.rag.do(c, http.MethodPost, "/v1/chat/stream", body)
	if err != nil {
		g.logger.Error("rag agent stream failed", "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "rag agent unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes))
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		c.Data(resp.StatusCode, ct, payload)
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	g.recordQuery(c)

	flusher, _ := c.Writer.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

func (g *Gateway) readChatBody(c *gin.Context) ([]byte, bool) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxChatBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return nil, false
	}
	var req chatReq
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return nil, false
	}
	if strings.TrimSpace(req.Message) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "message is required"})
		return nil, false
	}
	kbID, role, ok := g.resolveKnowledgeBase(c, req.KBID)
	if !ok {
		return nil, false
	}
	if !requireKBRole(c, role, identity.KBRoleViewer) {
		return nil, false
	}
	req.KBID = kbID
	body, err = json.Marshal(req)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "encode chat request: " + err.Error()})
		return nil, false
	}
	return body, true
}

func (g *Gateway) listConversations(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	path := "/v1/conversations"
	if q := c.Request.URL.Query().Encode(); q != "" {
		path += "?" + q
	}
	g.proxyRAG(c, http.MethodGet, path, nil)
}

func (g *Gateway) getConversation(c *gin.Context) {
	if !g.requireRAG(c) {
		return
	}
	g.proxyRAG(c, http.MethodGet, "/v1/conversations/"+url.PathEscape(c.Param("id")), nil)
}
