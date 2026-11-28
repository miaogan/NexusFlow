package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"log/slog"

	"github.com/nexusflow/nexusflow/backend/internal/webhook"
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
	RunID   string         `json:"run_id,omitempty"`
	Status  string         `json:"status,omitempty"`
	DAG     string         `json:"dag"`
	Order   []string       `json:"order"`
	Results []NodeResult   `json:"results"`
	Skipped []string       `json:"skipped,omitempty"`
	Output  map[string]any `json:"output"`
}

// Engine validates and executes DAGs with dependency-aware scheduling.
type Engine struct {
	registry *Registry
	logger   *slog.Logger
	events   webhook.Publisher
	sink     EventSink
}

func NewEngine(registry *Registry, logger *slog.Logger) *Engine {
	return &Engine{registry: registry, logger: logger}
}

// WithEventPublisher emits workflow.completed / workflow.failed webhooks for
// persisted top-level runs.
func (e *Engine) WithEventPublisher(publisher webhook.Publisher) *Engine {
	if publisher != nil {
		e.events = publisher
	}
	return e
}

// WithEventSink emits node-level lifecycle events for top-level runs.
func (e *Engine) WithEventSink(sink EventSink) *Engine {
	if sink != nil {
		e.sink = sink
	}
	return e
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

	// Control-flow nodes carry references and nested DAGs, so their config is
	// validated here where the full graph (and adjacency) is available.
	for _, n := range dag.Nodes {
		switch n.Type {
		case "condition":
			if err := e.validateCondition(n, adj); err != nil {
				return nil, err
			}
		case "loop":
			if err := e.validateLoop(n); err != nil {
				return nil, err
			}
		}
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

// RunOptions carries request identity, persistence and resume state.
type RunOptions struct {
	Meta Meta
	// RunID is generated when empty; the same ID is returned for resumes.
	RunID string
	// Store persists runs that suspend on a human_approval node.
	Store RunStore
	// Resume replays a suspended run from its saved checkpoint.
	Resume *RunState
	// Approved marks the pending human_approval node as approved.
	Approved       bool
	ApprovedBy     string
	ApprovalReason string
	// Nested marks loop sub-runs so node events are not duplicated by the
	// parent run.
	Nested bool
}

// Run executes a DAG with no request metadata.
func (e *Engine) Run(ctx context.Context, dag *DAG, input map[string]any) (*RunResult, error) {
	return e.RunWithOptions(ctx, dag, input, RunOptions{})
}

// RunWithOptions executes ready nodes concurrently, level by level.
func (e *Engine) RunWithOptions(
	ctx context.Context,
	dag *DAG,
	input map[string]any,
	opts RunOptions,
) (*RunResult, error) {
	order, err := e.Validate(dag)
	if err != nil {
		return nil, err
	}

	specs := make(map[string]NodeSpec, len(dag.Nodes))
	for _, n := range dag.Nodes {
		specs[n.ID] = n
	}
	adj := make(map[string][]string, len(dag.Nodes))
	for _, edge := range dag.Edges {
		adj[edge.From] = append(adj[edge.From], edge.To)
	}

	completed := make(map[string]bool, len(dag.Nodes))
	skipped := make(map[string]bool, len(dag.Nodes))
	skippedEmitted := make(map[string]bool, len(dag.Nodes))
	ec := NewExecutionContext(input)
	result := &RunResult{
		DAG:    dag.Name,
		Order:  order,
		Status: string(RunRunning),
		RunID:  opts.RunID,
	}
	if result.RunID == "" {
		result.RunID = fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	if opts.Resume != nil {
		state := opts.Resume
		result.RunID = state.ID
		result.Results = append(result.Results, state.Results...)
		result.Skipped = append(result.Skipped, state.Skipped...)
		ec = NewExecutionContext(state.Output)
		for _, res := range state.Results {
			completed[res.NodeID] = true
		}
		for _, id := range state.Skipped {
			skipped[id] = true
		}
		// Keep the original input available to downstream nodes.
		for key, value := range state.Input {
			if _, ok := ec.Get(key); !ok {
				ec.Set(key, value)
			}
		}
	}
	ec.SetMeta(opts.Meta)

	ready := make([]string, 0, len(specs))
	scheduled := make(map[string]bool, len(specs))
	for id := range specs {
		if completed[id] || skipped[id] {
			continue
		}
		pending := 0
		for _, edge := range dag.Edges {
			if edge.To == id && !completed[edge.From] && !skipped[edge.From] {
				pending++
			}
		}
		if pending == 0 {
			ready = append(ready, id)
			scheduled[id] = true
		}
	}

	for len(ready) > 0 {
		sort.Strings(ready)
		batch := ready
		ready = nil

		batchResults := make([]NodeResult, len(batch))
		batchErrs := make([]error, len(batch))
		var wg sync.WaitGroup
		for i, id := range batch {
			wg.Add(1)
			go func(i int, id string) {
				defer wg.Done()
				spec := specs[id]
				start := time.Now()
				e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusStarted, 0, "")
				if spec.Type == "human_approval" {
					if opts.Approved && opts.Resume != nil && id == opts.Resume.PendingNode {
						res := NodeResult{
							NodeID: id,
							Output: map[string]any{
								"approved": true,
								"by":       opts.ApprovedBy,
								"reason":   opts.ApprovalReason,
							},
							Duration: time.Since(start),
						}
						e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusCompleted, res.Duration, "")
						batchResults[i] = res
						return
					}
					batchErrs[i] = ErrWaitingApproval
					res := NodeResult{
						NodeID:   id,
						Duration: time.Since(start),
						Error:    ErrWaitingApproval.Error(),
					}
					e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusWaitingApproval, res.Duration, res.Error)
					batchResults[i] = res
					return
				}
				node, _ := e.registry.Get(spec.Type)
				out, runErr := node.Run(ctx, ec, spec.Config)
				res := NodeResult{NodeID: id, Output: out, Duration: time.Since(start)}
				if runErr != nil {
					res.Error = runErr.Error()
					e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusFailed, res.Duration, res.Error)
				} else {
					e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusCompleted, res.Duration, "")
				}
				batchResults[i] = res
				batchErrs[i] = runErr
			}(i, id)
		}
		wg.Wait()

		waiting := ""
		for i, id := range batch {
			res := batchResults[i]
			if batchErrs[i] != nil {
				if errors.Is(batchErrs[i], ErrWaitingApproval) {
					waiting = id
					continue
				}
				result.Results = append(result.Results, res)
				e.logger.Warn("node failed", "dag", dag.Name, "node", id, "error", res.Error)
				result.Output = ec.Snapshot()
				result.Status = string(RunFailed)
				e.save(opts, result, dag, input, res.Error)
				return result, fmt.Errorf("node %s failed: %s", id, res.Error)
			}
			ec.mergeOutput(id, res.Output)
			result.Results = append(result.Results, res)
			e.logger.Info("node completed", "dag", dag.Name, "node", id, "duration", res.Duration)
			completed[id] = true
			if specs[id].Type == "condition" {
				if err := applyConditionBranch(specs, adj, res.Output, skipped, completed); err != nil {
					result.Output = ec.Snapshot()
					result.Status = string(RunFailed)
					result.Skipped = skippedIDs(skipped)
					e.save(opts, result, dag, input, err.Error())
					return result, err
				}
				result.Skipped = skippedIDs(skipped)
				for skippedID := range skipped {
					if skippedEmitted[skippedID] {
						continue
					}
					skippedEmitted[skippedID] = true
					if spec, ok := specs[skippedID]; ok {
						e.emitNode(ctx, opts, dag, result.RunID, spec, NodeStatusSkipped, 0, "")
					}
				}
			}
			for _, child := range adj[id] {
				if completed[child] || skipped[child] {
					continue
				}
				pending := false
				for _, edge := range dag.Edges {
					if edge.To == child && !completed[edge.From] && !skipped[edge.From] {
						pending = true
						break
					}
				}
				if !pending && !scheduled[child] {
					scheduled[child] = true
					ready = append(ready, child)
				}
			}
		}

		if waiting != "" {
			result.Status = string(RunWaitingApproval)
			result.Output = ec.Snapshot()
			result.Skipped = skippedIDs(skipped)
			e.saveWithPending(opts, result, dag, input, waiting)
			e.logger.Info("run waiting for approval", "dag", dag.Name, "node", waiting, "run", result.RunID)
			return result, nil
		}
	}

	if len(completed)+len(skipped) != len(specs) {
		return result, fmt.Errorf("dag execution incomplete")
	}
	result.Status = string(RunSucceeded)
	result.Output = ec.Snapshot()
	result.Skipped = skippedIDs(skipped)
	e.save(opts, result, dag, input, "")
	return result, nil
}

// save persists a finished or failed run when a store is configured.
func (e *Engine) save(
	opts RunOptions,
	result *RunResult,
	dag *DAG,
	input map[string]any,
	errText string,
) {
	if opts.Store == nil {
		return
	}
	state := e.state(opts, result, dag, input, "", errText)
	opts.Store.Save(state)

	if e.events == nil {
		return
	}
	payload := map[string]any{
		"run_id": result.RunID,
		"dag":    dag.Name,
		"status": result.Status,
	}
	switch result.Status {
	case string(RunSucceeded):
		e.events.Publish(context.Background(), webhook.Event{
			Type: webhook.EventWorkflowCompleted, TenantID: firstNonEmpty(opts.Meta.TenantID, "default"),
			Payload: payload,
		})
	case string(RunFailed):
		payload["error"] = errText
		e.events.Publish(context.Background(), webhook.Event{
			Type: webhook.EventWorkflowFailed, TenantID: firstNonEmpty(opts.Meta.TenantID, "default"),
			Payload: payload,
		})
	}
}

func (e *Engine) saveWithPending(
	opts RunOptions,
	result *RunResult,
	dag *DAG,
	input map[string]any,
	pending string,
) {
	if opts.Store == nil {
		return
	}
	state := e.state(opts, result, dag, input, pending, "")
	opts.Store.Save(state)
}

func (e *Engine) state(
	opts RunOptions,
	result *RunResult,
	dag *DAG,
	input map[string]any,
	pending string,
	errText string,
) *RunState {
	now := time.Now()
	created := now
	if opts.Resume != nil {
		created = opts.Resume.CreatedAt
	}
	return &RunState{
		ID:             result.RunID,
		DAG:            *dag,
		Input:          input,
		Meta:           opts.Meta,
		Status:         result.Status,
		Order:          result.Order,
		Results:        result.Results,
		Skipped:        append([]string(nil), result.Skipped...),
		Output:         result.Output,
		PendingNode:    pending,
		Error:          errText,
		ApprovedBy:     opts.ApprovedBy,
		ApprovalReason: opts.ApprovalReason,
		CreatedAt:      created,
		UpdatedAt:      now,
	}
}

// reachable returns every node reachable from start (including start).
func reachable(adj map[string][]string, start string) map[string]bool {
	out := map[string]bool{start: true}
	stack := []string{start}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, child := range adj[id] {
			if !out[child] {
				out[child] = true
				stack = append(stack, child)
			}
		}
	}
	return out
}

func skippedIDs(skipped map[string]bool) []string {
	out := make([]string, 0, len(skipped))
	for id, ok := range skipped {
		if ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// applyConditionBranch prunes the branch the condition did not choose. A node
// is skipped when it is reachable from the discarded target but not from the
// chosen target, so a join node fed by both branches still runs.
func applyConditionBranch(
	specs map[string]NodeSpec,
	adj map[string][]string,
	output map[string]any,
	skipped map[string]bool,
	completed map[string]bool,
) error {
	branch, _ := output["branch"].(string)
	thenID, _ := output["then"].(string)
	elseID, _ := output["else"].(string)
	chosen, discarded := thenID, elseID
	if branch == "else" {
		chosen, discarded = elseID, thenID
	}
	if chosen == "" || discarded == "" {
		return nil
	}
	if _, ok := specs[chosen]; !ok {
		return fmt.Errorf("condition target %q does not exist", chosen)
	}
	if _, ok := specs[discarded]; !ok {
		return fmt.Errorf("condition target %q does not exist", discarded)
	}
	keep := reachable(adj, chosen)
	for id := range reachable(adj, discarded) {
		if keep[id] || completed[id] {
			continue
		}
		skipped[id] = true
	}
	return nil
}

func (e *Engine) validateCondition(n NodeSpec, adj map[string][]string) error {
	downstream := reachable(adj, n.ID)
	for _, key := range []string{"then", "else"} {
		target, _ := n.Config[key].(string)
		if target == "" {
			continue
		}
		if target == n.ID {
			return fmt.Errorf("node %s: condition %s cannot target itself", n.ID, key)
		}
		if !downstream[target] {
			return fmt.Errorf("node %s: condition %s target %q must be downstream", n.ID, key, target)
		}
	}
	return nil
}

func (e *Engine) validateLoop(n NodeSpec) error {
	if _, ok := n.Config["items"]; !ok {
		if _, hasFrom := n.Config["items_from"]; !hasFrom {
			return fmt.Errorf("node %s: loop requires config.items or config.items_from", n.ID)
		}
	}
	raw, ok := n.Config["sub_dag"]
	if !ok {
		return fmt.Errorf("node %s: loop requires config.sub_dag", n.ID)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("node %s: encode sub_dag: %w", n.ID, err)
	}
	var sub DAG
	if err := json.Unmarshal(encoded, &sub); err != nil {
		return fmt.Errorf("node %s: invalid sub_dag: %w", n.ID, err)
	}
	for _, inner := range sub.Nodes {
		if inner.Type == "loop" {
			return fmt.Errorf("node %s: nested loop is not supported", n.ID)
		}
	}
	if _, err := e.Validate(&sub); err != nil {
		return fmt.Errorf("node %s: sub_dag: %w", n.ID, err)
	}
	return nil
}
