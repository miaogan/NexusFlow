package workflow

import (
	"context"
	"errors"
	"testing"
)

type echoNode struct{}

func (echoNode) Type() string { return "echo" }
func (echoNode) Run(_ context.Context, ec *ExecutionContext, _ map[string]any) (map[string]any, error) {
	item, _ := ec.Get("item")
	index, _ := ec.Get("index")
	return map[string]any{"item": item, "index": index}, nil
}

type failNode struct{}

func (failNode) Type() string { return "fail" }
func (failNode) Run(context.Context, *ExecutionContext, map[string]any) (map[string]any, error) {
	return nil, errors.New("boom")
}

func newControlFlowEngine() *Engine {
	registry := NewRegistry()
	registry.Register(SetNode{})
	registry.Register(NoopNode{})
	registry.Register(ConditionNode{})
	registry.Register(echoNode{})
	registry.Register(failNode{})
	engine := NewEngine(registry, testLogger())
	registry.Register(NewLoopNode(registry, testLogger()))
	return engine
}

func TestConditionRunsOnlyChosenBranch(t *testing.T) {
	engine := newControlFlowEngine()
	dag := &DAG{
		Name: "branch",
		Nodes: []NodeSpec{
			{ID: "set", Type: "set", Config: map[string]any{"key": "route", "value": "a"}},
			{ID: "cond", Type: "condition", Config: map[string]any{
				"field": "${set.route}", "equals": "a", "then": "branchA", "else": "branchB",
			}},
			{ID: "branchA", Type: "noop"},
			{ID: "branchB", Type: "noop"},
			{ID: "join", Type: "noop"},
		},
		Edges: []Edge{
			{From: "set", To: "cond"},
			{From: "cond", To: "branchA"},
			{From: "cond", To: "branchB"},
			{From: "branchA", To: "join"},
			{From: "branchB", To: "join"},
		},
	}
	result, err := engine.Run(context.Background(), dag, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != string(RunSucceeded) {
		t.Fatalf("status = %s", result.Status)
	}
	executed := map[string]bool{}
	for _, node := range result.Results {
		executed[node.NodeID] = true
	}
	if !executed["branchA"] || !executed["join"] {
		t.Fatalf("chosen branch did not run: %+v", executed)
	}
	if executed["branchB"] {
		t.Fatal("discarded branch must not run")
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "branchB" {
		t.Fatalf("skipped = %v, want [branchB]", result.Skipped)
	}
}

func TestConditionElseAndMissingTarget(t *testing.T) {
	engine := newControlFlowEngine()
	dag := &DAG{
		Name: "else",
		Nodes: []NodeSpec{
			{ID: "set", Type: "set", Config: map[string]any{"key": "route", "value": "b"}},
			{ID: "cond", Type: "condition", Config: map[string]any{
				"field": "set.route", "in": []any{"a"}, "then": "yes", "else": "no",
			}},
			{ID: "yes", Type: "noop"},
			{ID: "no", Type: "noop"},
		},
		Edges: []Edge{
			{From: "set", To: "cond"},
			{From: "cond", To: "yes"},
			{From: "cond", To: "no"},
		},
	}
	result, err := engine.Run(context.Background(), dag, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "yes" {
		t.Fatalf("skipped = %v, want [yes]", result.Skipped)
	}

	bad := &DAG{
		Name: "bad",
		Nodes: []NodeSpec{
			{ID: "cond", Type: "condition", Config: map[string]any{"field": "x", "then": "missing"}},
			{ID: "other", Type: "noop"},
		},
		Edges: []Edge{{From: "cond", To: "other"}},
	}
	if _, err := engine.Validate(bad); err == nil {
		t.Fatal("expected a non-downstream condition target to be rejected")
	}
}

func TestLoopAggregatesItemOutputs(t *testing.T) {
	engine := newControlFlowEngine()
	dag := &DAG{
		Name: "loop",
		Nodes: []NodeSpec{
			{ID: "loop", Type: "loop", Config: map[string]any{
				"items":           []any{"a", "b", "c"},
				"max_concurrency": 2,
				"sub_dag": map[string]any{
					"name":  "inner",
					"nodes": []any{map[string]any{"id": "echo", "type": "echo"}},
					"edges": []any{},
				},
			}},
		},
	}
	result, err := engine.Run(context.Background(), dag, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	loopOutput, ok := result.Output["loop.count"]
	if !ok || loopOutput != 3 {
		t.Fatalf("loop count = %v (output %+v)", loopOutput, result.Output)
	}
	if failed, _ := result.Output["loop.failed"].(int); failed != 0 {
		t.Fatalf("failed = %v", result.Output["loop.failed"])
	}
	items, ok := result.Output["loop.results"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("results = %#v", result.Output["loop.results"])
	}
}

func TestLoopFailsWhenItemFails(t *testing.T) {
	engine := newControlFlowEngine()
	dag := &DAG{
		Name: "loop-fail",
		Nodes: []NodeSpec{
			{ID: "loop", Type: "loop", Config: map[string]any{
				"items":   []any{1},
				"sub_dag": map[string]any{"nodes": []any{map[string]any{"id": "fail", "type": "fail"}}},
			}},
		},
	}
	result, err := engine.Run(context.Background(), dag, nil)
	if err == nil {
		t.Fatal("expected the loop to fail")
	}
	if result.Status != string(RunFailed) {
		t.Fatalf("status = %s", result.Status)
	}
}

func TestLoopValidationRejectsNestedLoop(t *testing.T) {
	engine := newControlFlowEngine()
	dag := &DAG{
		Name: "nested",
		Nodes: []NodeSpec{
			{ID: "loop", Type: "loop", Config: map[string]any{
				"items": []any{1},
				"sub_dag": map[string]any{
					"nodes": []any{map[string]any{"id": "inner", "type": "loop", "config": map[string]any{
						"items":   []any{1},
						"sub_dag": map[string]any{"nodes": []any{map[string]any{"id": "n", "type": "noop"}}},
					}}},
				},
			}},
		},
	}
	if _, err := engine.Validate(dag); err == nil {
		t.Fatal("expected nested loop to be rejected")
	}
}
