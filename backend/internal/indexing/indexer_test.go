package indexing

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
)

type stubIndexer struct {
	calls int
	last  string
}

func (s *stubIndexer) IndexMarkdown(_ context.Context, _ string, md string, _ map[string]string) (int, error) {
	s.calls++
	s.last = md
	return 2, nil
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

// The parser publishes file.parsed just before flipping the task to PARSED, so
// the indexer must wait out a short PARSING window instead of dropping the event.
func TestIndexer_WaitsForParsedTransition(t *testing.T) {
	st, _ := state.NewStore("memory", "")
	seedTask(t, st, "task-1", "t1", state.Parsing)
	idx := &stubIndexer{}
	i := New(idx, st, slog.Default())

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = st.Transition(context.Background(), "task-1", state.Parsed, "")
	}()

	payload, _ := json.Marshal(parsedEvent{TaskID: "task-1", TenantID: "t1", Markdown: "# hi"})
	if err := i.Handle(context.Background(), queue.Message{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	task, _, _ := st.Get(context.Background(), "task-1")
	if task.State != state.Completed {
		t.Fatalf("state = %s, want COMPLETED", task.State)
	}
	if idx.calls != 1 {
		t.Fatalf("index calls = %d, want 1 after the PARSED transition", idx.calls)
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

// A crash mid-way (state left at CHUNKED after the index was written) must be
// resumed to COMPLETED on redelivery rather than silently skipped.
func TestIndexer_ResumesFromPartialState(t *testing.T) {
	st, _ := state.NewStore("memory", "")
	seedTask(t, st, "task-1", "t1", state.Chunked)
	idx := &stubIndexer{}
	i := New(idx, st, slog.Default())
	payload, _ := json.Marshal(parsedEvent{TaskID: "task-1", TenantID: "t1", Markdown: "# hi"})
	if err := i.Handle(context.Background(), queue.Message{Payload: payload}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	task, _, _ := st.Get(context.Background(), "task-1")
	if task.State != state.Completed {
		t.Fatalf("state = %s, want COMPLETED", task.State)
	}
	if idx.calls != 0 {
		t.Fatalf("index calls = %d, want 0 (index written before crash)", idx.calls)
	}
}
