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
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

const maxWorkflowBodyBytes = 4 << 20 // 4 MiB

// workflowProxy relays DAG validate/run calls to the Go workflow engine. The
// gateway owns authentication and RBAC; the engine trusts only the shared
// service token attached here.
type workflowProxy struct {
	baseURL      string
	serviceToken string
	client       *http.Client
}

func newWorkflowProxy(raw, serviceToken string) *workflowProxy {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return nil
	}
	return &workflowProxy{
		baseURL:      raw,
		serviceToken: serviceToken,
		client:       &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *workflowProxy) do(
	c *gin.Context,
	method, path string,
	body []byte,
) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(
		c.Request.Context(), method, p.baseURL+path, reader,
	)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if p.serviceToken != "" {
		req.Header.Set("x-service-token", p.serviceToken)
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
	return p.client.Do(req)
}

func (g *Gateway) requireWorkflow(c *gin.Context) bool {
	if g.workflow == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workflow engine not configured (set WORKFLOW_TARGET)"})
		return false
	}
	return true
}

func (g *Gateway) workflowValidate(c *gin.Context) {
	g.proxyWorkflow(c, http.MethodPost, "/v1/workflows/validate")
}

func (g *Gateway) workflowRun(c *gin.Context) {
	g.proxyWorkflow(c, http.MethodPost, "/v1/workflows/run")
}

func (g *Gateway) workflowRuns(c *gin.Context) {
	path := "/v1/workflows/runs"
	if query := c.Request.URL.Query().Encode(); query != "" {
		path += "?" + query
	}
	g.proxyWorkflow(c, http.MethodGet, path)
}

// Saved workflow definitions share the same tenant headers as runs so the
// engine can scope each tenant's library.

func (g *Gateway) workflowDefinitions(c *gin.Context) {
	g.proxyWorkflow(c, http.MethodGet, "/v1/workflows/definitions")
}

func (g *Gateway) workflowDefinitionCreate(c *gin.Context) {
	g.proxyWorkflow(c, http.MethodPost, "/v1/workflows/definitions")
}

func (g *Gateway) workflowDefinitionDetail(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodGet, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id")),
	)
}

func (g *Gateway) workflowDefinitionUpdate(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodPut, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id")),
	)
}

func (g *Gateway) workflowDefinitionDelete(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodDelete, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id")),
	)
}

func (g *Gateway) workflowDefinitionPublish(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodPost, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id"))+"/publish",
	)
}

func (g *Gateway) workflowDefinitionUnpublish(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodPost, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id"))+"/unpublish",
	)
}

func (g *Gateway) workflowDefinitionVersions(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodGet, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id"))+"/versions",
	)
}

func (g *Gateway) workflowDefinitionRollback(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodPost, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id"))+"/rollback",
	)
}

func (g *Gateway) workflowDefinitionDuplicate(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodPost, "/v1/workflows/definitions/"+url.PathEscape(c.Param("id"))+"/duplicate",
	)
}

func (g *Gateway) workflowRunDetail(c *gin.Context) {
	g.proxyWorkflow(
		c, http.MethodGet, "/v1/workflows/runs/"+url.PathEscape(c.Param("id")),
	)
}

// workflowRunChildren lists the persisted loop sub-runs of a parent execution.
func (g *Gateway) workflowRunChildren(c *gin.Context) {
	path := "/v1/workflows/runs/" + url.PathEscape(c.Param("id")) + "/children"
	if query := c.Request.URL.Query().Encode(); query != "" {
		path += "?" + query
	}
	g.proxyWorkflow(c, http.MethodGet, path)
}

func (g *Gateway) workflowApprove(c *gin.Context) {
	g.proxyWorkflowDecision(
		c,
		"/v1/workflows/runs/"+url.PathEscape(c.Param("id"))+"/approve",
		"approve",
	)
}

func (g *Gateway) workflowResume(c *gin.Context) {
	g.proxyWorkflowDecision(
		c,
		"/v1/workflows/runs/"+url.PathEscape(c.Param("id"))+"/resume",
		"resume",
	)
}

// proxyWorkflowDecision relays an approval decision and, on success, emits an
// approval.decided webhook event for the tenant.
func (g *Gateway) proxyWorkflowDecision(c *gin.Context, path, defaultDecision string) {
	if !g.requireWorkflow(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWorkflowBodyBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
		return
	}
	resp, err := g.workflow.do(c, http.MethodPost, path, body)
	if err != nil {
		g.logger.Error("workflow request failed", "path", path, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workflow engine unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, maxWorkflowBodyBytes))
	if trace := resp.Header.Get("x-trace-id"); trace != "" {
		c.Header("x-trace-id", trace)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = c.Writer.Write(payload)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		decision := defaultDecision
		var req struct {
			Decision string `json:"decision"`
		}
		if json.Unmarshal(body, &req) == nil && strings.TrimSpace(req.Decision) != "" {
			decision = strings.TrimSpace(req.Decision)
		}
		g.emitEvent(c, webhook.EventApprovalDecided, map[string]any{
			"source":   "workflow",
			"run_id":   c.Param("id"),
			"decision": decision,
		})
	}
}

func (g *Gateway) proxyWorkflow(c *gin.Context, method, path string) {
	if !g.requireWorkflow(c) {
		return
	}
	var body []byte
	if method == http.MethodPost || method == http.MethodPut {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWorkflowBodyBytes)
		var err error
		body, err = io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body: " + err.Error()})
			return
		}
	}
	resp, err := g.workflow.do(c, method, path, body)
	if err != nil {
		g.logger.Error("workflow request failed", "path", path, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "workflow engine unavailable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	if trace := resp.Header.Get("x-trace-id"); trace != "" {
		c.Header("x-trace-id", trace)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		c.Header("Content-Type", ct)
	}
	c.Status(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}
