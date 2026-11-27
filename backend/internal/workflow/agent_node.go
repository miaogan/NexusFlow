package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxAgentResponseBytes caps the hub response kept in memory.
const maxAgentResponseBytes = 4 << 20

// AgentNode calls the Agent Hub through POST /v1/call. It makes the DAG a
// first-class consumer of hub capabilities (and tools) while forwarding the
// request identity so traces stay connected across Go and Python.
type AgentNode struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewAgentNode builds the node. An empty base URL keeps the node registered
// but makes every run fail with a clear configuration error.
func NewAgentNode(baseURL, token string, timeout time.Duration) *AgentNode {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &AgentNode{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   token,
		client:  &http.Client{Timeout: timeout},
	}
}

func (n *AgentNode) Type() string { return "agent" }

func (n *AgentNode) Run(
	ctx context.Context,
	ec *ExecutionContext,
	config map[string]any,
) (map[string]any, error) {
	if n.baseURL == "" {
		return nil, fmt.Errorf("agent node requires AGENT_HUB_URL")
	}

	input := map[string]any{}
	if raw, ok := config["input"].(map[string]any); ok {
		for k, v := range raw {
			input[k] = resolveInput(ec, v)
		}
	}

	payload := map[string]any{}
	label := ""
	if tool, _ := config["tool"].(string); tool != "" {
		label = tool
		payload["tool"] = map[string]any{"name": tool, "input": input}
	} else {
		agent, _ := config["agent"].(string)
		capability, _ := config["capability"].(string)
		if agent == "" && capability == "" {
			return nil, fmt.Errorf("agent node requires config.agent, config.capability or config.tool")
		}
		label = agent
		if label == "" {
			label = capability
		}
		payload["agent"] = map[string]any{
			"name":       agent,
			"capability": capability,
			"input":      input,
		}
	}
	payload["envelope"] = envelopeFor(ctx, ec)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode agent request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, n.baseURL+"/v1/call", bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("build agent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	meta := ec.Meta()
	for header, value := range map[string]string{
		"x-tenant-id":  meta.TenantID,
		"x-user-id":    meta.UserID,
		"x-trace-id":   meta.TraceID,
		"x-request-id": meta.RequestID,
	} {
		if value != "" {
			req.Header.Set(header, value)
		}
	}
	if n.token != "" {
		req.Header.Set("x-service-token", n.token)
	}

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent hub unavailable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read agent response: %w", err)
	}
	if resp.StatusCode == http.StatusAccepted {
		return nil, fmt.Errorf("agent call requires approval: %s", strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("agent call failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var decoded struct {
		OK    bool           `json:"ok"`
		Agent string         `json:"agent"`
		Data  map[string]any `json:"data"`
		Error string         `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode agent response: %w", err)
	}
	if !decoded.OK {
		return nil, fmt.Errorf("agent call rejected: %s", decoded.Error)
	}
	agent := decoded.Agent
	if agent == "" {
		agent = label
	}
	data := decoded.Data
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{"agent": agent, "data": data}, nil
}

// resolveInput expands "${node.field}" references from the execution context
// so a DAG can feed upstream outputs into an agent/tool call.
func resolveInput(ec *ExecutionContext, value any) any {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, "${") || !strings.HasSuffix(text, "}") {
		return value
	}
	key := strings.TrimSuffix(strings.TrimPrefix(text, "${"), "}")
	if resolved, ok := ec.Get(key); ok {
		return resolved
	}
	return value
}

// envelopeFor forwards the workflow run identity to the hub.
func envelopeFor(ctx context.Context, ec *ExecutionContext) map[string]any {
	meta := ec.Meta()
	envelope := map[string]any{
		"tenant_id":  meta.TenantID,
		"user_id":    meta.UserID,
		"trace_id":   meta.TraceID,
		"request_id": meta.RequestID,
	}
	if deadline, ok := ctx.Deadline(); ok {
		envelope["deadline_ms"] = deadline.UnixMilli()
	}
	return envelope
}
