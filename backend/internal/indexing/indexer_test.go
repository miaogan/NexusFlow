package indexing

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
)

type stubIndexer struct {
	calls int
	last  string
}

func (s *stubIndexer) IndexMarkdown(_ context.Context, _ string, md string, _ map[string]string) int {
	s.calls++
	s.last = md
	return 2
}

func seedTask(t *testing.T, st state.Store, taskID, tenant string, s state.State) {
	t.Helper()
	_, _, err := st.Create(context.Background(), state.Task{ID: taskID, TenantID: tenant, State: s})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
}

func TestIndexer_AdvancesToCompleted(t *testing.T) {
	st, _ := state.NewStore("memory", "")
	seedTask(t, st, "task-1", "t1", state.Parsed)
	idx := &stubIndexer{}
	i := New(idx, st, slog.Default())

	payload, _ := json.Marshal(parsedEvent{TaskID: "task-1", TenantID: "t1", Markdown: "# hi"})
	if err := i.Handle(context.Background(), queue.Message{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	task, ok, _ := st.Get(context.Background(), "task-1")
	if !ok {
		t.Fatal("task missing")
	}
	if task.State != state.Completed {
		t.Fatalf("state = %s, want COMPLETED", task.State)
	}
	if idx.calls != 1 {
		t.Fatalf("index calls = %d, want 1", idx.calls)
	}
	if idx.last != "# hi" {
		t.Fatalf("markdown not passed to indexer: %q", idx.last)
	}
}

func TestIndexer_IdempotentSkipCompleted(t *testing.T) {
	st, _ := state.NewStore("memory", "")
	seedTask(t, st, "task-1", "t1", state.Completed)
	idx := &stubIndexer{}
	i := New(idx, st, slog.Default())
	payload, _ := json.Marshal(parsedEvent{TaskID: "task-1", TenantID: "t1", Markdown: "# hi"})
	if err := i.Handle(context.Background(), queue.Message{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if idx.calls != 0 {
		t.Fatalf("index calls = %d, want 0 (should skip completed)", idx.calls)
	}
}

func TestIndexer_SkipsNonParsed(t *testing.T) {
	st, _ := state.NewStore("memory", "")
	seedTask(t, st, "task-1", "t1", state.Fetched)
	idx := &stubIndexer{}
	i := New(idx, st, slog.Default())
	payload, _ := json.Marshal(parsedEvent{TaskID: "task-1", TenantID: "t1", Markdown: "# hi"})
	if err := i.Handle(context.Background(), queue.Message{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if idx.calls != 0 {
		t.Fatalf("index calls = %d, want 0 (not our stage)", idx.calls)
	}
	task, _, _ := st.Get(context.Background(), "task-1")
	if task.State != state.Fetched {
		t.Fatalf("state changed unexpectedly: %s", task.State)
	}
}
