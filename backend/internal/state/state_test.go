package state

import (
	"context"
	"testing"
)

func TestMatchesStatusRunning(t *testing.T) {
	if !matchesStatus(Pending, "RUNNING") || !matchesStatus(Indexed, "RUNNING") {
		t.Fatal("non-terminal states should match RUNNING")
	}
	if matchesStatus(Completed, "RUNNING") || matchesStatus(Failed, "RUNNING") {
		t.Fatal("terminal states must not match RUNNING")
	}
	if !matchesStatus(Completed, "COMPLETED") || !matchesStatus(Completed, "") {
		t.Fatal("exact and empty filters should match")
	}
	if matchesStatus(Completed, "FAILED") {
		t.Fatal("COMPLETED must not match FAILED")
	}
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		from, to State
		want     bool
	}{
		{Pending, Fetched, true},
		{Pending, Completed, false},
		{Parsed, Chunked, true},
		{Indexed, Completed, true},
		{Failed, Pending, true},
		{Completed, Reindexing, true},
		{Reindexing, Chunked, true},
		{Reindexing, Completed, true},
		{SandboxRunning, SandboxDone, true},
		{Completed, Parsed, false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.want {
			t.Errorf("CanTransition(%s,%s)=%v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestMachineIdempotency(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()

	first, created, err := m.Create(ctx, Task{ID: "t1", TenantID: "tenant", ContentHash: "hash"})
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	second, created, err := m.Create(ctx, Task{ID: "t2", TenantID: "tenant", ContentHash: "hash"})
	if err != nil {
		t.Fatalf("create duplicate: %v", err)
	}
	if created {
		t.Fatal("expected idempotent hit, got new task")
	}
	if second.ID != first.ID {
		t.Fatalf("expected existing task %s, got %s", first.ID, second.ID)
	}
}

func TestMachineIdempotencyIsKnowledgeBaseScoped(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()
	first, created, err := m.Create(ctx, Task{
		ID: "kb1-task", TenantID: "tenant", ContentHash: "same", Metadata: map[string]string{"kb_id": "kb1"},
	})
	if err != nil || !created {
		t.Fatalf("create first: created=%v err=%v", created, err)
	}
	second, created, err := m.Create(ctx, Task{
		ID: "kb2-task", TenantID: "tenant", ContentHash: "same", Metadata: map[string]string{"kb_id": "kb2"},
	})
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("different KB must create a distinct task: second=%+v created=%v err=%v", second, created, err)
	}
	duplicate, created, err := m.Create(ctx, Task{
		ID: "kb1-duplicate", TenantID: "tenant", ContentHash: "same", Metadata: map[string]string{"kb_id": "kb1"},
	})
	if err != nil || created || duplicate.ID != first.ID {
		t.Fatalf("same KB must deduplicate: duplicate=%+v created=%v err=%v", duplicate, created, err)
	}
}

func TestMachineTransition(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()
	if _, _, err := m.Create(ctx, Task{ID: "t1", TenantID: "tenant"}); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Transition(ctx, "t1", Fetched, ""); err != nil {
		t.Fatalf("pending->fetched: %v", err)
	}
	if _, err := m.Transition(ctx, "t1", Completed, ""); err == nil {
		t.Fatal("expected invalid transition error")
	}
}

func TestMachineReindexTransitions(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()
	if _, _, err := m.Create(ctx, Task{ID: "t1", TenantID: "tenant"}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []State{Fetched, Parsing, Parsed, Chunked, Embedded, Indexed, Completed} {
		if _, err := m.Transition(ctx, "t1", to, ""); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	if _, err := m.Transition(ctx, "t1", Reindexing, ""); err != nil {
		t.Fatalf("completed -> reindexing: %v", err)
	}
	// A rebuild advances straight into CHUNKED, keeping the document id.
	if _, err := m.Transition(ctx, "t1", Chunked, ""); err != nil {
		t.Fatalf("reindexing -> chunked: %v", err)
	}
	// Enqueue failure rolls the queued rebuild back to COMPLETED.
	if _, _, err := m.Create(ctx, Task{ID: "t2", TenantID: "tenant"}); err != nil {
		t.Fatal(err)
	}
	for _, to := range []State{Fetched, Parsing, Parsed, Chunked, Embedded, Indexed, Completed, Reindexing, Completed} {
		if _, err := m.Transition(ctx, "t2", to, ""); err != nil {
			t.Fatalf("t2 transition to %s: %v", to, err)
		}
	}
}

func TestMachineUpdateMetadata(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()
	if _, _, err := m.Create(ctx, Task{ID: "t1", TenantID: "tenant", Metadata: map[string]string{"kb_id": "kb1"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateMetadata(ctx, "t1", map[string]string{"markdown_key": "tenant/kb1/parsed/t1.md"}); err != nil {
		t.Fatalf("update metadata: %v", err)
	}
	task, ok, err := m.Get(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if task.Metadata["markdown_key"] != "tenant/kb1/parsed/t1.md" || task.Metadata["kb_id"] != "kb1" {
		t.Fatalf("metadata = %v", task.Metadata)
	}
	if err := m.UpdateMetadata(ctx, "missing", map[string]string{"a": "b"}); err != ErrTaskNotFound {
		t.Fatalf("missing task err = %v, want ErrTaskNotFound", err)
	}
}

func TestMachineDelete(t *testing.T) {
	m := NewMachine()
	ctx := context.Background()

	if _, _, err := m.Create(ctx, Task{ID: "t1", TenantID: "tenant", ContentHash: "hash-1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "t1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := m.Get(ctx, "t1"); err != nil || ok {
		t.Fatalf("task should be gone after delete: ok=%v err=%v", ok, err)
	}
	// Idempotency key must be released so the same hash can be re-ingested.
	if _, created, err := m.Create(ctx, Task{ID: "t2", TenantID: "tenant", ContentHash: "hash-1"}); err != nil || !created {
		t.Fatalf("re-create after delete should create new task: created=%v err=%v", created, err)
	}
}

func TestMachineDeleteNotFound(t *testing.T) {
	m := NewMachine()
	if err := m.Delete(context.Background(), "missing"); err != ErrTaskNotFound {
		t.Fatalf("expected ErrTaskNotFound, got %v", err)
	}
}
