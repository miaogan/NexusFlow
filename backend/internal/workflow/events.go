package workflow

import (
	"context"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/webhook"
)

// Node lifecycle statuses emitted through an EventSink.
const (
	NodeStatusStarted         = "STARTED"
	NodeStatusCompleted       = "COMPLETED"
	NodeStatusFailed          = "FAILED"
	NodeStatusWaitingApproval = "WAITING_APPROVAL"
	NodeStatusSkipped         = "SKIPPED"
)

// NodeEvent is one node-level lifecycle notification. Emit may be called
// concurrently from sibling nodes, so sinks must be safe for concurrent use.
type NodeEvent struct {
	RunID    string        `json:"run_id"`
	DAG      string        `json:"dag"`
	NodeID   string        `json:"node_id"`
	NodeType string        `json:"node_type"`
	Status   string        `json:"status"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
	TenantID string        `json:"tenant_id,omitempty"`
}

// EventSink receives node-level lifecycle events. Implementations should
// return quickly; the engine calls Emit inline from the scheduling goroutine.
type EventSink interface {
	Emit(ctx context.Context, event NodeEvent)
}

// EventSinkFunc adapts a function to the EventSink interface.
type EventSinkFunc func(ctx context.Context, event NodeEvent)

func (f EventSinkFunc) Emit(ctx context.Context, event NodeEvent) { f(ctx, event) }

// emitNode publishes a node event unless the run is a loop sub-run.
func (e *Engine) emitNode(
	ctx context.Context,
	opts RunOptions,
	dag *DAG,
	runID string,
	spec NodeSpec,
	status string,
	duration time.Duration,
	errText string,
) {
	if e == nil || e.sink == nil || opts.Nested {
		return
	}
	e.sink.Emit(ctx, NodeEvent{
		RunID:    runID,
		DAG:      dag.Name,
		NodeID:   spec.ID,
		NodeType: spec.Type,
		Status:   status,
		Duration: duration,
		Error:    errText,
		TenantID: firstNonEmpty(opts.Meta.TenantID, "default"),
	})
}

// WebhookEventType maps a node status to the public webhook event name.
func (e NodeEvent) WebhookEventType() string {
	switch e.Status {
	case NodeStatusStarted:
		return webhook.EventWorkflowNodeStarted
	case NodeStatusCompleted:
		return webhook.EventWorkflowNodeCompleted
	case NodeStatusFailed:
		return webhook.EventWorkflowNodeFailed
	case NodeStatusWaitingApproval:
		return webhook.EventWorkflowNodeWaitingApproval
	case NodeStatusSkipped:
		return webhook.EventWorkflowNodeSkipped
	default:
		return ""
	}
}

// WebhookSink bridges node events to the platform webhook dispatcher.
type WebhookSink struct {
	publisher webhook.Publisher
}

// NewWebhookSink returns a sink that publishes workflow.node.* events.
func NewWebhookSink(publisher webhook.Publisher) *WebhookSink {
	return &WebhookSink{publisher: publisher}
}

func (s *WebhookSink) Emit(ctx context.Context, event NodeEvent) {
	if s == nil || s.publisher == nil {
		return
	}
	eventType := event.WebhookEventType()
	if eventType == "" {
		return
	}
	s.publisher.Publish(ctx, webhook.Event{
		Type:     eventType,
		TenantID: event.TenantID,
		Payload: map[string]any{
			"run_id":      event.RunID,
			"dag":         event.DAG,
			"node_id":     event.NodeID,
			"node_type":   event.NodeType,
			"status":      event.Status,
			"duration_ms": event.Duration.Milliseconds(),
			"error":       event.Error,
			"tenant_id":   event.TenantID,
		},
	})
}
