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

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
)

// DocumentIndexer is the write boundary of the retrieval index. *query.Engine
// satisfies it in dev mode; keeping it as an interface lets the pipeline run
// end-to-end without a real vector store and keeps this package testable.
type DocumentIndexer interface {
	IndexMarkdown(ctx context.Context, docID, markdown string, meta map[string]string) int
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
	// Idempotent: redelivery of an already-indexed task is a no-op.
	switch task.State {
	case state.Chunked, state.Embedded, state.Indexed, state.Completed:
		return nil
	}
	if task.State != state.Parsed {
		// Not our stage yet (still parsing / already failed); skip.
		return nil
	}

	meta := map[string]string{
		"tenant_id": task.TenantID,
		"task_id":   task.ID,
	}
	if task.Metadata != nil {
		if fn, ok := task.Metadata["file_name"]; ok {
			meta["file_name"] = fn
		}
	}

	n := idx.indexer.IndexMarkdown(ctx, task.ID, evt.Markdown, meta)

	// Advance through the indexing stages. The state machine validates each
	// transition; a stale parallel update is surfaced as an error here.
	for _, to := range []state.State{state.Chunked, state.Embedded, state.Indexed, state.Completed} {
		if _, err := idx.machine.Transition(ctx, task.ID, to, ""); err != nil {
			idx.logger.Error("index transition failed", "task_id", task.ID, "to", to, "error", err)
			return err
		}
	}
	idx.logger.Info("index completed", "task_id", task.ID, "chunks", n)
	return nil
}
