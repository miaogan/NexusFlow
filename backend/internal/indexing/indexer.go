// Package indexing consumes parsed-document events and writes them into the
// retrieval index, driving the task from PARSED through to COMPLETED.
//
// In dev mode the indexer writes to an in-memory DocStore (backed by
// query.Engine). In production the same event is consumed by the LlamaIndex
// service which performs real chunking, embedding and vector-store upsert.
package indexing

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
)

// parsedWaitTimeout bounds how long an indexing event waits for the parser to
// flip the task to PARSED; the parsed event is published just before that
// transition, so consumers may observe PARSING for a moment.
const parsedWaitTimeout = 5 * time.Second

// DocumentIndexer is the write boundary of the retrieval index. *query.Engine
// satisfies it in dev mode; keeping it as an interface lets the pipeline run
// end-to-end without a real vector store and keeps this package testable.
type DocumentIndexer interface {
	IndexMarkdown(ctx context.Context, docID, markdown string, meta map[string]string) (int, error)
}

type parsedEvent struct {
	TaskID   string `json:"task_id"`
	TenantID string `json:"tenant_id"`
	Markdown string `json:"markdown"`
}

// Indexer turns parsed markdown into searchable chunks and advances the task
// state machine through CHUNKED → EMBEDDED → INDEXED → COMPLETED.
type Indexer struct {
	indexer DocumentIndexer
	machine state.Store
	logger  *slog.Logger
}

func New(indexer DocumentIndexer, machine state.Store, logger *slog.Logger) *Indexer {
	return &Indexer{indexer: indexer, machine: machine, logger: logger}
}

// Handle is a queue.Handler compatible entry point.
func (idx *Indexer) Handle(ctx context.Context, msg queue.Message) error {
	var evt parsedEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("unmarshal parsed event: %w", err)
	}
	task, ok, err := idx.machine.Get(ctx, evt.TaskID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("unknown task %s", evt.TaskID)
	}
	if task.State == state.Completed {
		return nil // fully indexed
	}
	if task.State == state.Parsing {
		// The parser publishes file.parsed right before flipping the state to
		// PARSED. Wait for that transition instead of dropping the event as
		// "not our stage yet" (which would leave the task stuck at PARSED).
		waited, err := idx.waitForParsed(ctx, task.ID)
		if err != nil {
			return err
		}
		task = waited
	}
	// Resume from whichever indexing stage we stopped at, so a partial failure
	// (e.g. crash between CHUNKED and EMBEDDED) is retried instead of skipped.
	order := []state.State{state.Chunked, state.Embedded, state.Indexed, state.Completed}
	var start int
	switch task.State {
	case state.Parsed:
		start = 0
	case state.Reindexing:
		// In-place rebuild: the parsed markdown is re-indexed and chunking
		// advances straight to CHUNKED.
		start = 1
	case state.Chunked:
		start = 1
	case state.Embedded:
		start = 2
	case state.Indexed:
		start = 3
	default:
		return nil // not our stage yet (still parsing / already failed)
	}

	meta := map[string]string{
		"tenant_id": task.TenantID,
		"task_id":   task.ID,
	}
	if task.Metadata != nil {
		// Only retrieval-relevant keys are forwarded to the index so internal
		// bookkeeping does not leak into chunk metadata.
		for _, key := range []string{"file_name", "kb_id"} {
			if value, ok := task.Metadata[key]; ok && value != "" {
				meta[key] = value
			}
		}
	}

	n := 0
	if task.State == state.Parsed || task.State == state.Reindexing {
		// Indexing is idempotent (docstore replaces by docID), so re-running on
		// redelivery is safe.
		n, err = idx.indexer.IndexMarkdown(ctx, task.ID, evt.Markdown, meta)
		if err != nil {
			if _, terr := idx.machine.Transition(ctx, task.ID, state.Failed, err.Error()); terr != nil {
				idx.logger.Warn("transition to FAILED failed", "task_id", task.ID, "error", terr)
			}
			return fmt.Errorf("index markdown: %w", err)
		}
	}

	// Advance through the remaining indexing stages. The state machine
	// validates each transition; a stale parallel update surfaces as an error.
	for _, to := range order[start:] {
		if _, err := idx.machine.Transition(ctx, task.ID, to, ""); err != nil {
			idx.logger.Error("index transition failed", "task_id", task.ID, "to", to, "error", err)
			return err
		}
	}
	idx.logger.Info("index completed", "task_id", task.ID, "chunks", n)
	return nil
}

// waitForParsed polls until the task leaves PARSING (normally PARSED) or the
// short grace period expires.
func (idx *Indexer) waitForParsed(ctx context.Context, taskID string) (state.Task, error) {
	deadline := time.Now().Add(parsedWaitTimeout)
	for {
		select {
		case <-ctx.Done():
			return state.Task{}, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
		task, ok, err := idx.machine.Get(ctx, taskID)
		if err != nil {
			return state.Task{}, err
		}
		if !ok {
			return state.Task{}, fmt.Errorf("unknown task %s", taskID)
		}
		if task.State != state.Parsing {
			return task, nil
		}
		if time.Now().After(deadline) {
			return state.Task{}, fmt.Errorf("task %s is still PARSING after %s", taskID, parsedWaitTimeout)
		}
	}
}
