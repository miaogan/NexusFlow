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
