package connector

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const storeAdvisoryLock = 731005

var storeSchema = []string{
	`CREATE TABLE IF NOT EXISTS data_connectors (
		id               VARCHAR(64) PRIMARY KEY,
		tenant_id        VARCHAR(64) NOT NULL,
		kb_id            VARCHAR(64) NOT NULL,
		name             VARCHAR(128) NOT NULL,
		connector_type   VARCHAR(32) NOT NULL,
		config_encrypted TEXT NOT NULL,
		schedule_cron    VARCHAR(64) DEFAULT '',
		sync_cursor      JSONB DEFAULT '{}'::jsonb,
		status           VARCHAR(32) NOT NULL DEFAULT 'IDLE',
		last_sync_at     TIMESTAMPTZ,
		last_error       TEXT DEFAULT '',
		created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_connectors_tenant_kb
		ON data_connectors (tenant_id, kb_id)`,
	`CREATE TABLE IF NOT EXISTS connector_schedule_claims (
		connector_id VARCHAR(64) NOT NULL,
		run_minute   TIMESTAMPTZ NOT NULL,
		claimed_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (connector_id, run_minute)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_connector_claims_minute
		ON connector_schedule_claims (run_minute)`,
}

// NewStore builds the configured store. A broken DSN falls back to memory so
// ingestion still starts; connector definitions then live for one process.
func NewStore(ctx context.Context, driver, dsn string, cipher *Cipher) Store {
	if driver == "postgres" && dsn != "" {
		store, err := NewPostgresStore(ctx, dsn, cipher)
		if err == nil {
			return store
		}
		slog.Default().Warn("connector database unavailable, using memory", "error", err)
	}
	return NewMemoryStore(cipher)
}

// MemoryStore keeps connectors for one process (dev / tests).
type MemoryStore struct {
	mu     sync.RWMutex
	cipher *Cipher
	specs  map[string]Spec
	claims map[string]time.Time
}

func NewMemoryStore(cipher *Cipher) *MemoryStore {
	return &MemoryStore{
		cipher: cipher,
		specs:  make(map[string]Spec),
		claims: make(map[string]time.Time),
	}
}

func (s *MemoryStore) List(_ context.Context, tenantID, kbID string) ([]Spec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Spec, 0, len(s.specs))
	for _, spec := range s.specs {
		if spec.TenantID != tenantID {
			continue
		}
		if kbID != "" && spec.KBID != kbID {
			continue
		}
		out = append(out, spec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) ListAll(_ context.Context) ([]Spec, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Spec, 0, len(s.specs))
	for _, spec := range s.specs {
		out = append(out, spec.clone())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, tenantID, id string) (Spec, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	spec, ok := s.specs[id]
	if !ok || spec.TenantID != tenantID {
		return Spec{}, false, nil
	}
	return spec.clone(), true, nil
}

func (s *MemoryStore) Create(_ context.Context, spec Spec) (Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.specs[spec.ID]; exists {
		return Spec{}, fmt.Errorf("connector: %s already exists", spec.ID)
	}
	s.specs[spec.ID] = spec.clone()
	return spec.clone(), nil
}

func (s *MemoryStore) Update(_ context.Context, spec Spec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.specs[spec.ID]
	if !ok || current.TenantID != spec.TenantID {
		return ErrNotFound
	}
	s.specs[spec.ID] = spec.clone()
	return nil
}

func (s *MemoryStore) Delete(_ context.Context, tenantID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.specs[id]
	if !ok || spec.TenantID != tenantID {
		return false, nil
	}
	delete(s.specs, id)
	return true, nil
}

func (s *MemoryStore) ClaimSchedule(_ context.Context, connectorID string, minute time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := claimKey(connectorID, minute)
	if _, exists := s.claims[key]; exists {
		return false, nil
	}
	s.claims[key] = minute
	return true, nil
}

func (s *MemoryStore) PruneScheduleClaims(_ context.Context, before time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, minute := range s.claims {
		if minute.Before(before) {
			delete(s.claims, key)
		}
	}
	return nil
}

func claimKey(connectorID string, minute time.Time) string {
	return connectorID + "|" + minute.UTC().Format(time.RFC3339)
}

// PostgresStore persists connectors and cursors across replicas.
type PostgresStore struct {
	db     *sql.DB
	cipher *Cipher
}

func NewPostgresStore(ctx context.Context, dsn string, cipher *Cipher) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("connector: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("connector: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connector: ping postgres: %w", err)
	}
	if err := migrateStore(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &PostgresStore{db: db, cipher: cipher}, nil
}

func migrateStore(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("connector: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", storeAdvisoryLock); err != nil {
		return fmt.Errorf("connector: migration lock: %w", err)
	}
	for _, stmt := range storeSchema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("connector: migrate: %w", err)
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) List(ctx context.Context, tenantID, kbID string) ([]Spec, error) {
	query := `SELECT id, tenant_id, kb_id, name, connector_type, config_encrypted,
		schedule_cron, sync_cursor, status, last_sync_at, last_error, created_at, updated_at
		FROM data_connectors WHERE tenant_id = $1`
	args := []any{tenantID}
	if kbID != "" {
		query += " AND kb_id = $2"
		args = append(args, kbID)
	}
	query += " ORDER BY created_at DESC"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Spec
	for rows.Next() {
		spec, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListAll(ctx context.Context) ([]Spec, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, kb_id, name, connector_type,
		config_encrypted, schedule_cron, sync_cursor, status, last_sync_at, last_error,
		created_at, updated_at FROM data_connectors ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Spec
	for rows.Next() {
		spec, err := s.scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, spec)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (Spec, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, tenant_id, kb_id, name, connector_type,
		config_encrypted, schedule_cron, sync_cursor, status, last_sync_at, last_error,
		created_at, updated_at FROM data_connectors WHERE tenant_id = $1 AND id = $2`,
		tenantID, id)
	spec, err := s.scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Spec{}, false, nil
	}
	if err != nil {
		return Spec{}, false, err
	}
	return spec, true, nil
}

func (s *PostgresStore) Create(ctx context.Context, spec Spec) (Spec, error) {
	encoded, err := s.encodeConfig(spec.Config)
	if err != nil {
		return Spec{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO data_connectors (
		id, tenant_id, kb_id, name, connector_type, config_encrypted, schedule_cron,
		sync_cursor, status, last_error, created_at, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		spec.ID, spec.TenantID, spec.KBID, spec.Name, string(spec.Type), encoded,
		spec.ScheduleCron, mustJSON(spec.SyncCursor), spec.Status, spec.LastError,
		spec.CreatedAt, spec.UpdatedAt)
	if err != nil {
		return Spec{}, fmt.Errorf("connector: insert: %w", err)
	}
	return spec, nil
}

func (s *PostgresStore) Update(ctx context.Context, spec Spec) error {
	encoded, err := s.encodeConfig(spec.Config)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE data_connectors SET
		name = $3, config_encrypted = $4, schedule_cron = $5, sync_cursor = $6,
		status = $7, last_sync_at = $8, last_error = $9, updated_at = $10
		WHERE tenant_id = $1 AND id = $2`,
		spec.TenantID, spec.ID, spec.Name, encoded, spec.ScheduleCron,
		mustJSON(spec.SyncCursor), spec.Status, nullTime(spec.LastSyncAt),
		spec.LastError, spec.UpdatedAt)
	if err != nil {
		return fmt.Errorf("connector: update: %w", err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM data_connectors WHERE tenant_id = $1 AND id = $2", tenantID, id)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

func (s *PostgresStore) ClaimSchedule(ctx context.Context, connectorID string, minute time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO connector_schedule_claims (connector_id, run_minute)
		 VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		connectorID, minute.UTC())
	if err != nil {
		return false, fmt.Errorf("connector: claim schedule: %w", err)
	}
	affected, _ := result.RowsAffected()
	return affected > 0, nil
}

func (s *PostgresStore) PruneScheduleClaims(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"DELETE FROM connector_schedule_claims WHERE run_minute < $1", before.UTC())
	if err != nil {
		return fmt.Errorf("connector: prune schedule claims: %w", err)
	}
	return nil
}

func (s *PostgresStore) encodeConfig(cfg Config) (string, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("connector: encode config: %w", err)
	}
	return s.cipher.Encrypt(raw)
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func (s *PostgresStore) scan(row scanner) (Spec, error) {
	var (
		spec     Spec
		encoded  string
		cursor   []byte
		kind     string
		status   string
		lastSync sql.NullTime
	)
	if err := row.Scan(
		&spec.ID, &spec.TenantID, &spec.KBID, &spec.Name, &kind,
		&encoded, &spec.ScheduleCron, &cursor, &status, &lastSync,
		&spec.LastError, &spec.CreatedAt, &spec.UpdatedAt,
	); err != nil {
		return Spec{}, err
	}
	spec.Type = Type(kind)
	spec.Status = status
	if lastSync.Valid {
		spec.LastSyncAt = lastSync.Time
	}
	if len(cursor) > 0 {
		_ = json.Unmarshal(cursor, &spec.SyncCursor)
	}
	plain, err := s.cipher.Decrypt(encoded)
	if err != nil {
		return Spec{}, err
	}
	if err := json.Unmarshal(plain, &spec.Config); err != nil {
		return Spec{}, fmt.Errorf("connector: decode config: %w", err)
	}
	return spec, nil
}

func mustJSON(cursor map[string]string) []byte {
	if cursor == nil {
		return []byte("{}")
	}
	raw, err := json.Marshal(cursor)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s Spec) clone() Spec {
	out := s
	if s.Config.URLs != nil {
		out.Config.URLs = append([]string(nil), s.Config.URLs...)
	}
	if s.SyncCursor != nil {
		out.SyncCursor = make(map[string]string, len(s.SyncCursor))
		for key, value := range s.SyncCursor {
			out.SyncCursor[key] = value
		}
	}
	return out
}
