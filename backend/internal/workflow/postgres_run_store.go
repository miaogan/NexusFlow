package workflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// runStoreAdvisoryLock serializes migrations across replicas so concurrent
// CREATE TABLE IF NOT EXISTS statements cannot race on the system catalog.
const runStoreAdvisoryLock = 731002

var runStoreSchema = []string{
	`CREATE TABLE IF NOT EXISTS workflow_executions (
		execution_id     VARCHAR(64) PRIMARY KEY,
		tenant_id        VARCHAR(64) NOT NULL,
		workflow_id      VARCHAR(64) NOT NULL,
		status           VARCHAR(32) NOT NULL,
		current_node_id  VARCHAR(64) DEFAULT '',
		context_snapshot JSONB NOT NULL DEFAULT '{}'::jsonb,
		triggered_by     VARCHAR(64) NOT NULL,
		started_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		finished_at      TIMESTAMPTZ,
		parent_run_id    VARCHAR(64) NOT NULL DEFAULT '',
		parent_node_id   VARCHAR(64) NOT NULL DEFAULT '',
		parent_index     INT
	)`,
	`ALTER TABLE workflow_executions
		ADD COLUMN IF NOT EXISTS parent_run_id VARCHAR(64) NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_executions
		ADD COLUMN IF NOT EXISTS parent_node_id VARCHAR(64) NOT NULL DEFAULT ''`,
	`ALTER TABLE workflow_executions
		ADD COLUMN IF NOT EXISTS parent_index INT`,
	`CREATE INDEX IF NOT EXISTS idx_wf_exec_tenant_status
		ON workflow_executions (tenant_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_wf_exec_parent
		ON workflow_executions (parent_run_id)`,
	`CREATE TABLE IF NOT EXISTS workflow_approvals (
		id              VARCHAR(64) PRIMARY KEY,
		tenant_id       VARCHAR(64) NOT NULL,
		execution_id    VARCHAR(64) NOT NULL
			REFERENCES workflow_executions(execution_id) ON DELETE CASCADE,
		node_id         VARCHAR(64) NOT NULL,
		assignee_role   VARCHAR(64) DEFAULT '',
		prompt_message  TEXT DEFAULT '',
		payload_preview JSONB DEFAULT '{}'::jsonb,
		decision        VARCHAR(32) NOT NULL DEFAULT 'PENDING',
		reviewer_id     VARCHAR(64) DEFAULT '',
		review_comment  TEXT DEFAULT '',
		expires_at      TIMESTAMPTZ,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		resolved_at     TIMESTAMPTZ
	)`,
	`CREATE INDEX IF NOT EXISTS idx_wf_approvals_pending
		ON workflow_approvals (tenant_id, decision, created_at DESC)`,
}

// PostgresRunStore persists workflow checkpoints and their human-approval
// tickets so suspended runs survive a restart and are visible to every replica.
type PostgresRunStore struct {
	db *sql.DB
}

// NewPostgresRunStore opens the database, verifies connectivity and applies the
// idempotent schema from docs/schema_enterprise_v1_1_to_1_5.sql.
func NewPostgresRunStore(ctx context.Context, dsn string) (*PostgresRunStore, error) {
	if dsn == "" {
		return nil, errors.New("workflow: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("workflow: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("workflow: ping postgres: %w", err)
	}
	if err := migrateRunStore(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &PostgresRunStore{db: db}, nil
}

func migrateRunStore(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("workflow: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", runStoreAdvisoryLock); err != nil {
		return fmt.Errorf("workflow: migration lock: %w", err)
	}
	for _, stmt := range runStoreSchema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("workflow: migrate run store: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workflow: commit migration: %w", err)
	}
	return nil
}

// NewRunStore builds the configured run store. A broken Postgres DSN must not
// stop the engine from starting, so failures fall back to the memory store and
// only lose cross-replica recovery.
func NewRunStore(ctx context.Context, driver, dsn string) (RunStore, func()) {
	if driver == "postgres" && dsn != "" {
		store, err := NewPostgresRunStore(ctx, dsn)
		if err == nil {
			return store, func() { _ = store.Close() }
		}
		slog.Default().Warn("workflow run store database unavailable, using memory", "error", err)
	}
	return NewMemoryRunStore(500), func() {}
}

// Close releases the connection pool.
func (s *PostgresRunStore) Close() error {
	return s.db.Close()
}

// Save upserts the run and keeps its approval ticket in sync. Errors are logged
// because RunStore cannot fail the request; the in-memory fallback keeps the
// single-process path working without a database.
func (s *PostgresRunStore) Save(state *RunState) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.save(ctx, state); err != nil {
		slog.Default().Error("workflow run store save failed", "run", state.ID, "error", err)
	}
}

func (s *PostgresRunStore) save(ctx context.Context, state *RunState) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveRunTx(ctx, tx, state); err != nil {
		return err
	}
	return tx.Commit()
}

func saveRunTx(ctx context.Context, tx *sql.Tx, state *RunState) error {
	snapshot, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode run snapshot: %w", err)
	}
	tenant := firstNonEmpty(state.Meta.TenantID, "default")
	workflowID := firstNonEmpty(state.DAG.Name, "default")
	triggeredBy := firstNonEmpty(state.Meta.UserID, "anonymous")
	updated := state.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	created := state.CreatedAt
	if created.IsZero() {
		created = updated
	}
	var finished any
	if isTerminal(state.Status) {
		finished = updated
	}
	var parentIndex any
	if state.ParentIndex != nil {
		parentIndex = *state.ParentIndex
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO workflow_executions (
			execution_id, tenant_id, workflow_id, status, current_node_id,
			context_snapshot, triggered_by, started_at, updated_at, finished_at,
			parent_run_id, parent_node_id, parent_index
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (execution_id) DO UPDATE SET
			tenant_id = EXCLUDED.tenant_id,
			workflow_id = EXCLUDED.workflow_id,
			status = EXCLUDED.status,
			current_node_id = EXCLUDED.current_node_id,
			context_snapshot = EXCLUDED.context_snapshot,
			triggered_by = EXCLUDED.triggered_by,
			updated_at = EXCLUDED.updated_at,
			finished_at = EXCLUDED.finished_at,
			parent_run_id = EXCLUDED.parent_run_id,
			parent_node_id = EXCLUDED.parent_node_id,
			parent_index = EXCLUDED.parent_index`,
		state.ID, tenant, workflowID, state.Status, state.PendingNode,
		snapshot, triggeredBy, created, updated, finished,
		state.ParentRunID, state.ParentNodeID, parentIndex,
	); err != nil {
		return fmt.Errorf("upsert workflow execution: %w", err)
	}

	if state.Status == string(RunWaitingApproval) && state.PendingNode != "" {
		if err := upsertApprovalTx(ctx, tx, state, tenant); err != nil {
			return err
		}
	}
	if isTerminal(state.Status) {
		if err := resolveApprovalsTx(ctx, tx, state); err != nil {
			return err
		}
	}
	return nil
}

func upsertApprovalTx(ctx context.Context, tx *sql.Tx, state *RunState, tenant string) error {
	preview, err := json.Marshal(map[string]any{
		"dag":          state.DAG.Name,
		"pending_node": state.PendingNode,
		"order":        state.Order,
		"output":       state.Output,
	})
	if err != nil {
		return fmt.Errorf("encode approval preview: %w", err)
	}
	approvalID := approvalIDFor(state.ID, state.PendingNode)
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO workflow_approvals (
			id, tenant_id, execution_id, node_id, prompt_message,
			payload_preview, decision, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,'PENDING',$7)
		ON CONFLICT (id) DO UPDATE SET
			prompt_message = EXCLUDED.prompt_message,
			payload_preview = EXCLUDED.payload_preview`,
		approvalID, tenant, state.ID, state.PendingNode,
		fmt.Sprintf("workflow %s paused at node %s", firstNonEmpty(state.DAG.Name, "default"), state.PendingNode),
		preview, state.UpdatedAt,
	); err != nil {
		return fmt.Errorf("upsert workflow approval: %w", err)
	}
	return nil
}

func resolveApprovalsTx(ctx context.Context, tx *sql.Tx, state *RunState) error {
	decision := "REJECTED"
	if state.Status == string(RunSucceeded) {
		decision = "APPROVED"
	}
	reviewer := firstNonEmpty(state.ApprovedBy, state.Meta.UserID, "anonymous")
	comment := firstNonEmpty(state.ApprovalReason, state.Error)
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE workflow_approvals SET
			decision = $2, reviewer_id = $3, review_comment = $4, resolved_at = $5
		WHERE execution_id = $1 AND decision = 'PENDING'`,
		state.ID, decision, reviewer, comment, state.UpdatedAt,
	); err != nil {
		return fmt.Errorf("resolve workflow approvals: %w", err)
	}
	return nil
}

// Get loads a checkpoint by run ID.
func (s *PostgresRunStore) Get(id string) (*RunState, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var (
		snapshot    []byte
		status      string
		pendingNode string
		updatedAt   time.Time
	)
	err := s.db.QueryRowContext(
		ctx,
		`SELECT context_snapshot, status, COALESCE(current_node_id, ''), updated_at
		 FROM workflow_executions WHERE execution_id = $1`,
		id,
	).Scan(&snapshot, &status, &pendingNode, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		slog.Default().Error("workflow run store get failed", "run", id, "error", err)
		return nil, false
	}
	state := &RunState{ID: id}
	if len(snapshot) > 0 {
		if err := json.Unmarshal(snapshot, state); err != nil {
			slog.Default().Error("decode run snapshot", "run", id, "error", err)
			return nil, false
		}
	}
	state.ID = id
	state.Status = status
	state.PendingNode = pendingNode
	state.UpdatedAt = updatedAt
	return state, true
}

// List returns the most recently updated top-level runs.
func (s *PostgresRunStore) List(limit int) []*RunState {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT execution_id, context_snapshot, status,
		        COALESCE(current_node_id, ''), updated_at
		 FROM workflow_executions
		 WHERE COALESCE(parent_run_id, '') = ''
		 ORDER BY updated_at DESC LIMIT $1`,
		limit,
	)
	if err != nil {
		slog.Default().Error("workflow run store list failed", "error", err)
		return nil
	}
	defer rows.Close()
	return scanRunStates(rows, limit)
}

// Children returns the loop sub-runs of a parent execution, newest first.
func (s *PostgresRunStore) Children(parentID string, limit int) []*RunState {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT execution_id, context_snapshot, status,
		        COALESCE(current_node_id, ''), updated_at
		 FROM workflow_executions
		 WHERE parent_run_id = $1
		 ORDER BY updated_at DESC LIMIT $2`,
		parentID, limit,
	)
	if err != nil {
		slog.Default().Error("workflow run store children failed", "parent", parentID, "error", err)
		return nil
	}
	defer rows.Close()
	return scanRunStates(rows, limit)
}

func scanRunStates(rows *sql.Rows, capacity int) []*RunState {
	out := make([]*RunState, 0, capacity)
	for rows.Next() {
		var (
			id          string
			snapshot    []byte
			status      string
			pendingNode string
			updatedAt   time.Time
		)
		if err := rows.Scan(&id, &snapshot, &status, &pendingNode, &updatedAt); err != nil {
			slog.Default().Error("workflow run store scan failed", "error", err)
			continue
		}
		state := &RunState{ID: id}
		if len(snapshot) > 0 {
			if err := json.Unmarshal(snapshot, state); err != nil {
				slog.Default().Error("decode run snapshot", "run", id, "error", err)
				continue
			}
		}
		state.ID = id
		state.Status = status
		state.PendingNode = pendingNode
		state.UpdatedAt = updatedAt
		out = append(out, state)
	}
	if err := rows.Err(); err != nil {
		slog.Default().Error("workflow run store rows failed", "error", err)
	}
	return out
}

// Transition atomically applies a status change under a row lock, so two
// replicas cannot approve the same suspended run twice.
func (s *PostgresRunStore) Transition(
	id string, from RunStatus, apply func(*RunState) error,
) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		snapshot []byte
		status   string
	)
	err = tx.QueryRowContext(
		ctx,
		`SELECT context_snapshot, status FROM workflow_executions
		 WHERE execution_id = $1 FOR UPDATE`,
		id,
	).Scan(&snapshot, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if RunStatus(status) != from {
		return false, nil
	}
	state := &RunState{ID: id}
	if len(snapshot) > 0 {
		if err := json.Unmarshal(snapshot, state); err != nil {
			return false, fmt.Errorf("decode run snapshot: %w", err)
		}
	}
	state.ID = id
	if err := apply(state); err != nil {
		return false, err
	}
	if err := saveRunTx(ctx, tx, state); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// approvalIDFor derives a stable ticket ID so retries are idempotent and fit
// the VARCHAR(64) primary key.
func approvalIDFor(runID, nodeID string) string {
	sum := sha256.Sum256([]byte(runID + "|" + nodeID))
	return "wfa-" + hex.EncodeToString(sum[:8])
}

func isTerminal(status string) bool {
	switch RunStatus(status) {
	case RunSucceeded, RunFailed, RunRejected:
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
