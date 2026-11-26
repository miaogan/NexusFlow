package workflow

import (
	"fmt"
	"sync"
)

// ExecutionContext carries data between DAG nodes. Upstream outputs are
// written into the context and read by downstream nodes by name.
type ExecutionContext struct {
	mu   sync.RWMutex
	vars map[string]any
}

func NewExecutionContext(input map[string]any) *ExecutionContext {
	vars := make(map[string]any, len(input)+8)
	for k, v := range input {
		vars[k] = v
	}
	return &ExecutionContext{vars: vars}
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
