package workflow

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestPostgresRunStoreIntegration exercises workflow_executions and
// workflow_approvals against a real database. It is skipped unless
// POSTGRES_TEST_DSN points at a disposable PostgreSQL instance:
//
//	POSTGRES_TEST_DSN=postgresql://nexusflow:nexusflow@127.0.0.1:5432/nexusflow \
//	    go test ./internal/workflow -run Postgres
func TestPostgresRunStoreIntegration(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set POSTGRES_TEST_DSN to run the PostgresRunStore integration test")
	}
	ctx := context.Background()
	store, err := NewPostgresRunStore(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	runID := "run-test-" + time.Now().Format("20060102150405.000000000")
	state := &RunState{
		ID:          runID,
		DAG:         DAG{Name: "integration"},
		Input:       map[string]any{"q": "hello"},
		Meta:        Meta{TenantID: "t1", UserID: "u1", TraceID: "tr-1"},
		Status:      string(RunWaitingApproval),
		PendingNode: "gate",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	store.Save(state)

	loaded, ok := store.Get(runID)
	if !ok || loaded.Status != string(RunWaitingApproval) || loaded.PendingNode != "gate" {
		t.Fatalf("loaded = %+v ok=%v", loaded, ok)
	}

	// Loop sub-runs are persisted with parent linkage and excluded from the
	// top-level listing.
	childID := runID + ":loop:0"
	index := 0
	child := &RunState{
		ID:           childID,
		DAG:          DAG{Name: "inner"},
		Meta:         Meta{TenantID: "t1", UserID: "u1"},
		Status:       string(RunSucceeded),
		ParentRunID:  runID,
		ParentNodeID: "loop",
		ParentIndex:  &index,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	store.Save(child)
	children := store.Children(runID, 10)
	if len(children) != 1 || children[0].ID != childID ||
		children[0].ParentNodeID != "loop" || children[0].ParentIndex == nil {
		t.Fatalf("children = %+v", children)
	}
	for _, run := range store.List(500) {
		if run.ID == childID {
			t.Fatalf("sub-run %s leaked into the top-level list", childID)
		}
	}

	// Two competing decisions: only the first transition may win.
	var resumed *RunState
	claimed, err := store.Transition(runID, RunWaitingApproval, func(st *RunState) error {
		st.Status = string(RunRunning)
		st.ApprovedBy = "reviewer"
		st.UpdatedAt = time.Now().UTC()
		resumed = st
		return nil
	})
	if err != nil || !claimed {
		t.Fatalf("first transition claimed=%v err=%v", claimed, err)
	}
	again, err := store.Transition(runID, RunWaitingApproval, func(st *RunState) error {
		st.Status = string(RunRunning)
		return nil
	})
	if err != nil {
		t.Fatalf("second transition: %v", err)
	}
	if again {
		t.Fatal("second transition must lose the race")
	}

	resumed.Status = string(RunSucceeded)
	resumed.ApprovedBy = "reviewer"
	resumed.ApprovalReason = "ok"
	resumed.UpdatedAt = time.Now().UTC()
	store.Save(resumed)

	var decision string
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT decision FROM workflow_approvals WHERE execution_id = $1",
		runID,
	).Scan(&decision); err != nil {
		t.Fatalf("read approval ticket: %v", err)
	}
	if decision != "APPROVED" {
		t.Fatalf("approval decision = %q, want APPROVED", decision)
	}

	for _, id := range []string{childID, runID} {
		if _, err := store.db.ExecContext(
			ctx, "DELETE FROM workflow_executions WHERE execution_id = $1", id,
		); err != nil {
			t.Fatalf("cleanup %s: %v", id, err)
		}
	}
}
