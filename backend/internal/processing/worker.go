package processing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
)

type uploadedEvent struct {
	TaskID   string `json:"task_id"`
	TenantID string `json:"tenant_id"`
	FileURL  string `json:"file_url"`
	FileType string `json:"file_type"`
	FileName string `json:"file_name"`
}

type parsedEvent struct {
	TaskID   string `json:"task_id"`
	TenantID string `json:"tenant_id"`
	Markdown string `json:"markdown"`
}

// Worker consumes upload events, invokes MinerU with bounded retries and
// publishes the parsed Markdown for the LlamaIndex service.
type Worker struct {
	client       mineru.Client
	machine      state.Store
	pub          queue.Publisher
	logger       *slog.Logger
	pollInterval time.Duration
	maxPolls     int
	maxAttempts  int
	topicParsed  string
	topicDLQ     string
	serviceName  string
	// objects persists parsed markdown so Reindex can rebuild the index without
	// invoking MinerU again. Optional.
	objects store.ObjectStore
}

// WorkerOption configures optional Worker collaborators.
type WorkerOption func(*Worker)

// WithObjectStore enables parsed-markdown persistence.
func WithObjectStore(objects store.ObjectStore) WorkerOption {
	return func(w *Worker) { w.objects = objects }
}

func NewWorker(client mineru.Client, machine state.Store, pub queue.Publisher, logger *slog.Logger,
	pollInterval time.Duration, maxPolls, maxAttempts int, topicParsed, topicDLQ, serviceName string,
	opts ...WorkerOption) *Worker {
	worker := &Worker{
		client:       client,
		machine:      machine,
		pub:          pub,
		logger:       logger,
		pollInterval: pollInterval,
		maxPolls:     maxPolls,
		maxAttempts:  maxAttempts,
		topicParsed:  topicParsed,
		topicDLQ:     topicDLQ,
		serviceName:  serviceName,
	}
	for _, opt := range opts {
		opt(worker)
	}
	return worker
}

func (w *Worker) Handle(ctx context.Context, msg queue.Message) error {
	var evt uploadedEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		w.logger.Error("bad upload event", "error", err)
		return err
	}
	task, ok, err := w.machine.Get(ctx, evt.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown task %s", evt.TaskID)
	}
	if task.State == state.Parsed || task.State == state.Completed {
		return nil // idempotent redelivery
	}

	if task.State == state.Pending {
		if _, err := w.machine.Transition(ctx, task.ID, state.Fetched, ""); err != nil {
			w.logger.Warn("transition to FETCHED failed", "task_id", task.ID, "error", err)
		}
	}
	if _, err := w.machine.Transition(ctx, task.ID, state.Parsing, ""); err != nil {
		w.logger.Warn("transition to PARSING failed", "task_id", task.ID, "error", err)
	}

	start := time.Now()
	var markdown string
	var parseErr error
	for attempt := 1; attempt <= w.maxAttempts; attempt++ {
		markdown, parseErr = mineru.Parse(ctx, w.client, evt.TaskID, evt.FileURL, evt.FileType, w.pollInterval, w.maxPolls)
		if parseErr == nil {
			break
		}
		w.logger.Warn("mineru parse failed", "task_id", task.ID, "attempt", attempt, "error", parseErr)
		if attempt == w.maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
		}
	}

	metrics.ProcessingLatency.
		WithLabelValues(w.serviceName, "parse").
		Observe(time.Since(start).Seconds())

	if parseErr != nil {
		if _, err := w.machine.Transition(ctx, task.ID, state.Failed, parseErr.Error()); err != nil {
			w.logger.Warn("transition to FAILED failed", "task_id", task.ID, "error", err)
		}
		dead, _ := json.Marshal(map[string]string{
			"task_id": task.ID,
			"stage":   "parse",
			"error":   parseErr.Error(),
		})
		_ = w.pub.Publish(ctx, queue.Message{Topic: w.topicDLQ, Key: task.ID, Payload: dead})
		w.logger.Error("task moved to DLQ", "task_id", task.ID, "error", parseErr)
		return nil
	}

	// Persist the parsed markdown before publishing. Failure is non-fatal: the
	// document still indexes from the in-memory event, it just cannot be
	// re-indexed later without re-parsing.
	if w.objects != nil {
		if key, err := w.persistMarkdown(ctx, task, markdown); err != nil {
			w.logger.Warn("persist parsed markdown failed", "task_id", task.ID, "error", err)
		} else if err := w.machine.UpdateMetadata(ctx, task.ID, map[string]string{"markdown_key": key}); err != nil {
			w.logger.Warn("record markdown key failed", "task_id", task.ID, "error", err)
		}
	}

	// Publish before flipping state: if publishing fails we return an error and
	// the message is redelivered. The reverse order would mark the task PARSED
	// and then drop the event on redelivery (the state==PARSED short-circuit).
	parsed, _ := json.Marshal(parsedEvent{TaskID: task.ID, TenantID: task.TenantID, Markdown: markdown})
	if err := w.pub.Publish(ctx, queue.Message{
		Topic:   w.topicParsed,
		Key:     task.ID,
		Payload: parsed,
		Headers: map[string]string{"tenant_id": task.TenantID},
	}); err != nil {
		return fmt.Errorf("publish parsed event: %w", err)
	}

	if _, err := w.machine.Transition(ctx, task.ID, state.Parsed, ""); err != nil {
		return fmt.Errorf("transition to PARSED: %w", err)
	}

	w.logger.Info("parse completed", "task_id", task.ID, "markdown_bytes", len(markdown))
	return nil
}

// persistMarkdown stores the parsed markdown under tenant/kb/parsed/<task>.md.
func (w *Worker) persistMarkdown(ctx context.Context, task state.Task, markdown string) (string, error) {
	kbID := task.Metadata["kb_id"]
	if kbID == "" {
		kbID = "default"
	}
	key := fmt.Sprintf("%s/%s/parsed/%s.md", task.TenantID, kbID, task.ID)
	if err := w.objects.Put(ctx, key, strings.NewReader(markdown), int64(len(markdown)), "text/markdown"); err != nil {
		return "", err
	}
	return key, nil
}
