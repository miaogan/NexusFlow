package workflow

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
)

type recordNode struct {
	mu    sync.Mutex
	order *[]string
}

func (n *recordNode) Type() string { return "record" }

func (n *recordNode) Run(_ context.Context, _ *ExecutionContext, config map[string]any) (map[string]any, error) {
	id, _ := config["id"].(string)
	n.mu.Lock()
	*n.order = append(*n.order, id)
	n.mu.Unlock()
	return map[string]any{"id": id}, nil
}

func newTestEngine(order *[]string) *Engine {
	reg := NewRegistry()
	reg.Register(&recordNode{order: order})
	return NewEngine(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEngineRunsInDependencyOrder(t *testing.T) {
	var order []string
	engine := newTestEngine(&order)

	dag := &DAG{
		Name: "test",
		Nodes: []NodeSpec{
			{ID: "a", Type: "record", Config: map[string]any{"id": "a"}},
			{ID: "b", Type: "record", Config: map[string]any{"id": "b"}},
			{ID: "c", Type: "record", Config: map[string]any{"id": "c"}},
		},
		Edges: []Edge{{From: "a", To: "b"}, {From: "c", To: "b"}},
	}

	res, err := engine.Run(context.Background(), dag, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(res.Results))
	}
	if len(order) != 3 || order[2] != "b" {
		t.Fatalf("expected b to run last, got %v", order)
	}
	if v, ok := res.Output["b.id"]; !ok || v != "b" {
		t.Fatalf("expected b.id output in context, got %v", res.Output)
	}
}

func TestEngineRejectsCycle(t *testing.T) {
	engine := newTestEngine(&[]string{})
	dag := &DAG{
		Nodes: []NodeSpec{
			{ID: "a", Type: "record"},
			{ID: "b", Type: "record"},
		},
		Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}
	if _, err := engine.Validate(dag); err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestEngineRejectsUnknownType(t *testing.T) {
	engine := newTestEngine(&[]string{})
	dag := &DAG{Nodes: []NodeSpec{{ID: "a", Type: "missing"}}}
	if _, err := engine.Validate(dag); err == nil {
		t.Fatal("expected unknown node type error")
	}
}
