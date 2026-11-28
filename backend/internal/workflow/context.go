package workflow

import (
	"fmt"
	"sync"
)

// Meta is the request identity propagated to agent/tool/MCP calls so the same
// tenant and trace span the gateway, workflow engine and Agent Hub.
type Meta struct {
	TenantID  string
	UserID    string
	TraceID   string
	RequestID string
}

// ExecutionContext carries data between DAG nodes. Upstream outputs are
// written into the context and read by downstream nodes by name.
type ExecutionContext struct {
	mu    sync.RWMutex
	vars  map[string]any
	meta  Meta
	runID string
	store RunStore
}

func NewExecutionContext(input map[string]any) *ExecutionContext {
	vars := make(map[string]any, len(input)+8)
	for k, v := range input {
		vars[k] = v
	}
	return &ExecutionContext{vars: vars}
}

// SetRun records the current run identity and store so composite nodes (loop)
// can persist their sub-runs under the same parent.
func (c *ExecutionContext) SetRun(runID string, store RunStore) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runID = runID
	c.store = store
}

// RunID returns the current top-level run ID.
func (c *ExecutionContext) RunID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.runID
}

// Store returns the store backing the current run, or nil when persistence is
// disabled.
func (c *ExecutionContext) Store() RunStore {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.store
}

// SetMeta records the caller identity for nodes that call external services.
func (c *ExecutionContext) SetMeta(meta Meta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.meta = meta
}

// Meta returns the request identity attached to this run.
func (c *ExecutionContext) Meta() Meta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.meta
}

func (c *ExecutionContext) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vars[key] = value
}

func (c *ExecutionContext) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.vars[key]
	return v, ok
}

func (c *ExecutionContext) String(key string) string {
	v, _ := c.Get(key)
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (c *ExecutionContext) Snapshot() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]any, len(c.vars))
	for k, v := range c.vars {
		out[k] = v
	}
	return out
}

// mergeOutput stores a node result under its node ID and also exposes each
// field as "<nodeID>.<field>" for downstream variable references.
func (c *ExecutionContext) mergeOutput(nodeID string, out map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vars["nodes."+nodeID] = out
	for k, v := range out {
		c.vars[nodeID+"."+k] = v
	}
}
