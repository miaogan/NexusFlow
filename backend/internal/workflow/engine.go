package workflow

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"log/slog"
)

// Node is the unified contract for every executable step: parsing, chunking,
// indexing, embedding, sandbox execution, etc.
type Node interface {
	Type() string
	Run(ctx context.Context, ec *ExecutionContext, config map[string]any) (map[string]any, error)
}

// Registry maps node types to implementations so the DAG stays declarative.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]Node
}

func NewRegistry() *Registry {
	return &Registry{nodes: make(map[string]Node)}
}

func (r *Registry) Register(n Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[n.Type()] = n
}

func (r *Registry) Get(nodeType string) (Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[nodeType]
	return n, ok
}

func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for t := range r.nodes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

type NodeSpec struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DAG is the JSON/YAML document emitted by the visual canvas.
type DAG struct {
	Name  string     `json:"name"`
	Nodes []NodeSpec `json:"nodes"`
	Edges []Edge     `json:"edges"`
}

type NodeResult struct {
	NodeID   string         `json:"node_id"`
	Output   map[string]any `json:"output,omitempty"`
	Duration time.Duration  `json:"duration"`
	Error    string         `json:"error,omitempty"`
}

type RunResult struct {
	DAG     string         `json:"dag"`
	Order   []string       `json:"order"`
	Results []NodeResult   `json:"results"`
	Output  map[string]any `json:"output"`
}

// Engine validates and executes DAGs with dependency-aware scheduling.
type Engine struct {
	registry *Registry
	logger   *slog.Logger
}

func NewEngine(registry *Registry, logger *slog.Logger) *Engine {
	return &Engine{registry: registry, logger: logger}
}

// Validate checks references, duplicate IDs and cycles, and returns a
// topological order executed level by level at runtime.
func (e *Engine) Validate(dag *DAG) ([]string, error) {
	if len(dag.Nodes) == 0 {
		return nil, fmt.Errorf("dag has no nodes")
	}
	ids := make(map[string]struct{}, len(dag.Nodes))
	for _, n := range dag.Nodes {
		if n.ID == "" {
			return nil, fmt.Errorf("node id is empty")
		}
		if _, dup := ids[n.ID]; dup {
			return nil, fmt.Errorf("duplicate node id %q", n.ID)
		}
		if _, ok := e.registry.Get(n.Type); !ok {
			return nil, fmt.Errorf("unknown node type %q (node %s)", n.Type, n.ID)
		}
		ids[n.ID] = struct{}{}
	}

	indeg := make(map[string]int, len(dag.Nodes))
	adj := make(map[string][]string, len(dag.Nodes))
	for id := range ids {
		indeg[id] = 0
	}
	for _, edge := range dag.Edges {
		if _, ok := ids[edge.From]; !ok {
			return nil, fmt.Errorf("edge references unknown node %q", edge.From)
		}
		if _, ok := ids[edge.To]; !ok {
			return nil, fmt.Errorf("edge references unknown node %q", edge.To)
		}
		adj[edge.From] = append(adj[edge.From], edge.To)
		indeg[edge.To]++
	}

	order := make([]string, 0, len(ids))
	queue := make([]string, 0, len(ids))
	for id, deg := range indeg {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		next := append([]string(nil), adj[id]...)
		sort.Strings(next)
		for _, child := range next {
			indeg[child]--
			if indeg[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if len(order) != len(ids) {
		return nil, fmt.Errorf("dag contains a cycle")
	}
	return order, nil
}

// Run executes ready nodes concurrently, level by level.
func (e *Engine) Run(ctx context.Context, dag *DAG, input map[string]any) (*RunResult, error) {
	order, err := e.Validate(dag)
	if err != nil {
		return nil, err
	}

	specs := make(map[string]NodeSpec, len(dag.Nodes))
	for _, n := range dag.Nodes {
		specs[n.ID] = n
	}
	indeg := make(map[string]int, len(dag.Nodes))
	adj := make(map[string][]string, len(dag.Nodes))
	remaining := make(map[string]int, len(dag.Nodes))
	for id := range specs {
		indeg[id] = 0
		remaining[id] = 0
	}
	for _, edge := range dag.Edges {
		adj[edge.From] = append(adj[edge.From], edge.To)
		indeg[edge.To]++
		remaining[edge.To]++
	}

	ec := NewExecutionContext(input)
	result := &RunResult{DAG: dag.Name, Order: order}
	ready := make([]string, 0)
	for id, deg := range indeg {
		if deg == 0 {
			ready = append(ready, id)
		}
	}

	doneCount := 0
	for len(ready) > 0 {
		sort.Strings(ready)
		batch := ready
		ready = nil

		batchResults := make([]NodeResult, len(batch))
		var wg sync.WaitGroup
		for i, id := range batch {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				spec := specs[id]
				node, _ := e.registry.Get(spec.Type)
				start := time.Now()
				out, runErr := node.Run(ctx, ec, spec.Config)
				res := NodeResult{NodeID: id, Output: out, Duration: time.Since(start)}
				if runErr != nil {
					res.Error = runErr.Error()
				}
				batchResults[i] = res
			}(i, id)
		}
		wg.Wait()

		for i, id := range batch {
			res := batchResults[i]
			if res.Error != "" {
				result.Results = append(result.Results, res)
				e.logger.Warn("node failed", "dag", dag.Name, "node", id, "error", res.Error)
				result.Output = ec.Snapshot()
				return result, fmt.Errorf("node %s failed: %s", id, res.Error)
			}
			ec.mergeOutput(id, res.Output)
			result.Results = append(result.Results, res)
			e.logger.Info("node completed", "dag", dag.Name, "node", id, "duration", res.Duration)
			doneCount++
			for _, child := range adj[id] {
				remaining[child]--
				if remaining[child] == 0 {
					ready = append(ready, child)
				}
			}
		}
	}

	if doneCount != len(specs) {
		return result, fmt.Errorf("dag execution incomplete")
	}
	result.Output = ec.Snapshot()
	return result, nil
}
