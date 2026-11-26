package state

import (
	"context"
	"testing"
)

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
