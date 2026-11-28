package workflow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

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

type captureSink struct {
	mu     sync.Mutex
	events []NodeEvent
}

func (c *captureSink) Emit(_ context.Context, event NodeEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, event)
}

func (c *captureSink) list() []NodeEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]NodeEvent(nil), c.events...)
}

func TestEngineEmitsNodeLifecycle(t *testing.T) {
	var order []string
	sink := &captureSink{}
	engine := newTestEngine(&order).WithEventSink(sink)
	dag := &DAG{
		Name: "events",
		Nodes: []NodeSpec{
			{ID: "a", Type: "record", Config: map[string]any{"id": "a"}},
			{ID: "b", Type: "record", Config: map[string]any{"id": "b"}},
		},
		Edges: []Edge{{From: "a", To: "b"}},
	}

	res, err := engine.RunWithOptions(context.Background(), dag, nil, RunOptions{
		Meta: Meta{TenantID: "t1"},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	events := sink.list()
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	want := []struct {
		id     string
		status string
	}{
		{"a", NodeStatusStarted},
		{"a", NodeStatusCompleted},
		{"b", NodeStatusStarted},
		{"b", NodeStatusCompleted},
	}
	for i, item := range want {
		if events[i].NodeID != item.id || events[i].Status != item.status {
			t.Fatalf("event[%d] = %+v, want %s/%s", i, events[i], item.id, item.status)
		}
		if events[i].RunID != res.RunID || events[i].TenantID != "t1" || events[i].DAG != "events" {
			t.Fatalf("event metadata = %+v", events[i])
		}
	}
}

func TestEngineEmitsNodeFailed(t *testing.T) {
	sink := &captureSink{}
	reg := NewRegistry()
	reg.Register(failingNode{})
	engine := NewEngine(reg, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithEventSink(sink)
	dag := &DAG{Name: "events", Nodes: []NodeSpec{{ID: "a", Type: "failing"}}}

	if _, err := engine.RunWithOptions(context.Background(), dag, nil, RunOptions{
		Meta: Meta{TenantID: "t1"},
	}); err == nil {
		t.Fatal("expected run failure")
	}
	events := sink.list()
	if len(events) != 2 || events[0].Status != NodeStatusStarted || events[1].Status != NodeStatusFailed {
		t.Fatalf("events = %+v", events)
	}
	if events[1].Error != "boom" {
		t.Fatalf("error = %q", events[1].Error)
	}
}

func TestNestedRunSkipsNodeEvents(t *testing.T) {
	var order []string
	sink := &captureSink{}
	engine := newTestEngine(&order).WithEventSink(sink)
	dag := &DAG{Name: "events", Nodes: []NodeSpec{{ID: "a", Type: "record"}}}

	if _, err := engine.RunWithOptions(context.Background(), dag, nil, RunOptions{
		Meta: Meta{TenantID: "t1"}, Nested: true,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if events := sink.list(); len(events) != 0 {
		t.Fatalf("nested run emitted events: %+v", events)
	}
}

func TestWebhookSinkMapsNodeEvents(t *testing.T) {
	pub := &capturePublisher{}
	sink := NewWebhookSink(pub)
	sink.Emit(context.Background(), NodeEvent{
		RunID: "run-1", DAG: "etl", NodeID: "parse", NodeType: "parse",
		Status: NodeStatusCompleted, Duration: 1500 * time.Millisecond, TenantID: "t1",
	})

	events := pub.list()
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	event := events[0]
	if event.Type != webhook.EventWorkflowNodeCompleted || event.TenantID != "t1" {
		t.Fatalf("event = %+v", event)
	}
	if event.Payload["node_id"] != "parse" || event.Payload["duration_ms"] != int64(1500) {
		t.Fatalf("payload = %+v", event.Payload)
	}
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
