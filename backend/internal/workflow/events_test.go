package workflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

type capturePublisher struct {
	mu     sync.Mutex
	events []webhook.Event
}

func (p *capturePublisher) Publish(_ context.Context, evt webhook.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, evt)
}

func (p *capturePublisher) list() []webhook.Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]webhook.Event(nil), p.events...)
}

type failingNode struct{}

func (failingNode) Type() string { return "failing" }

func (failingNode) Run(context.Context, *ExecutionContext, map[string]any) (map[string]any, error) {
	return nil, errors.New("boom")
}

func TestEngineEmitsWorkflowCompleted(t *testing.T) {
	var order []string
	pub := &capturePublisher{}
	engine := newTestEngine(&order).WithEventPublisher(pub)
	store := NewMemoryRunStore(10)
	dag := &DAG{
		Name:  "events",
		Nodes: []NodeSpec{{ID: "a", Type: "record", Config: map[string]any{"id": "a"}}},
	}

	res, err := engine.RunWithOptions(context.Background(), dag, nil, RunOptions{
		Meta: Meta{TenantID: "t1"}, Store: store,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	events := pub.list()
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Type != webhook.EventWorkflowCompleted || events[0].TenantID != "t1" {
		t.Fatalf("event = %+v", events[0])
	}
	if events[0].Payload["run_id"] != res.RunID {
		t.Fatalf("payload = %+v", events[0].Payload)
	}
}

func TestEngineEmitsWorkflowFailed(t *testing.T) {
	pub := &capturePublisher{}
	reg := NewRegistry()
	reg.Register(failingNode{})
	engine := NewEngine(reg, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithEventPublisher(pub)
	dag := &DAG{
		Name:  "events",
		Nodes: []NodeSpec{{ID: "a", Type: "failing"}},
	}

	if _, err := engine.RunWithOptions(context.Background(), dag, nil, RunOptions{
		Meta: Meta{TenantID: "t1"}, Store: NewMemoryRunStore(10),
	}); err == nil {
		t.Fatal("expected run failure")
	}
	events := pub.list()
	if len(events) != 1 || events[0].Type != webhook.EventWorkflowFailed {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Payload["error"] != "boom" {
		t.Fatalf("payload = %+v", events[0].Payload)
	}
}
