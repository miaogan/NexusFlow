package apikey

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore persists API keys in the shared Postgres database. The DDL
// mirrors api_keys in docs/schema_enterprise_v1_1_to_1_5.sql.
type PostgresStore struct {
	db *sql.DB
}

const keyColumns = `id, tenant_id, name, key_prefix, key_hash, allowed_kb_ids, scopes,
	rate_limit_rpm, status, expires_at, last_used_at, created_by, created_at`

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS api_keys (
		id             VARCHAR(64) PRIMARY KEY,
		tenant_id      VARCHAR(64) NOT NULL,
		name           VARCHAR(128) NOT NULL,
		key_prefix     VARCHAR(16) NOT NULL,
		key_hash       VARCHAR(128) NOT NULL UNIQUE,
		allowed_kb_ids JSONB DEFAULT '[]'::jsonb,
		scopes         JSONB DEFAULT '["chat:completions", "search:use"]'::jsonb,
		rate_limit_rpm INT NOT NULL DEFAULT 60,
		status         VARCHAR(16) NOT NULL DEFAULT 'ACTIVE',
		expires_at     TIMESTAMPTZ,
		last_used_at   TIMESTAMPTZ,
		created_by     VARCHAR(64) NOT NULL,
		created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_api_keys_tenant
		ON api_keys (tenant_id, status)`,
}

// NewPostgresStore opens the database, verifies connectivity and ensures the
// api_keys schema exists.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("apikey: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("apikey: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apikey: ping postgres: %w", err)
	}
	for _, stmt := range schemaStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("apikey: migrate: %w", err)
		}
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Create(ctx context.Context, k Key) (Key, error) {
	if k.Status == "" {
		k.Status = StatusActive
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	allowed, err := json.Marshal(nonNil(k.AllowedKBIDs))
	if err != nil {
		return Key{}, fmt.Errorf("apikey: marshal allowed ids: %w", err)
	}
	scopes, err := json.Marshal(nonNil(k.Scopes))
	if err != nil {
		return Key{}, fmt.Errorf("apikey: marshal scopes: %w", err)
	}

	const insert = `INSERT INTO api_keys (` + keyColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING created_at`
	row := s.db.QueryRowContext(ctx, insert,
		k.ID, k.TenantID, k.Name, k.KeyPrefix, k.KeyHash, allowed, scopes,
		k.RateLimitRPM, k.Status, k.ExpiresAt, k.LastUsedAt, k.CreatedBy, k.CreatedAt,
	)
	if err := row.Scan(&k.CreatedAt); err != nil {
		if isUniqueViolation(err) {
			return Key{}, ErrDuplicate
		}
		return Key{}, fmt.Errorf("apikey: insert: %w", err)
	}
	return k, nil
}

func (s *PostgresStore) List(ctx context.Context, tenantID string) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("apikey: list: %w", err)
	}
	defer rows.Close()

	out := make([]Key, 0)
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("apikey: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("apikey: rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (Key, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("apikey: get: %w", err)
	}
	return k, nil
}

func (s *PostgresStore) Revoke(ctx context.Context, tenantID, id string) (Key, error) {
	row := s.db.QueryRowContext(ctx,
		`UPDATE api_keys SET status = $3 WHERE tenant_id = $1 AND id = $2
		 RETURNING `+keyColumns,
		tenantID, id, StatusRevoked,
	)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("apikey: revoke: %w", err)
	}
	return k, nil
}

func (s *PostgresStore) FindByHash(ctx context.Context, hash string) (Key, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+keyColumns+` FROM api_keys WHERE key_hash = $1`, hash,
	)
	k, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("apikey: find by hash: %w", err)
	}
	return k, nil
}

func (s *PostgresStore) Touch(ctx context.Context, tenantID, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE api_keys SET last_used_at = $3 WHERE tenant_id = $1 AND id = $2`,
		tenantID, id, at,
	)
	if err != nil {
		return fmt.Errorf("apikey: touch: %w", err)
	}
	return nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

func scanKey(row interface{ Scan(...any) error }) (Key, error) {
	var (
		k       Key
		allowed []byte
		scopes  []byte
		expires sql.NullTime
		last    sql.NullTime
	)
	if err := row.Scan(
		&k.ID, &k.TenantID, &k.Name, &k.KeyPrefix, &k.KeyHash, &allowed, &scopes,
		&k.RateLimitRPM, &k.Status, &expires, &last, &k.CreatedBy, &k.CreatedAt,
	); err != nil {
		return Key{}, err
	}
	_ = json.Unmarshal(allowed, &k.AllowedKBIDs)
	_ = json.Unmarshal(scopes, &k.Scopes)
	if k.AllowedKBIDs == nil {
		k.AllowedKBIDs = []string{}
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	if expires.Valid {
		k.ExpiresAt = &expires.Time
	}
	if last.Valid {
		k.LastUsedAt = &last.Time
	}
	return k, nil
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// isUniqueViolation checks the pgx SQLSTATE for unique_violation (23505).
func isUniqueViolation(err error) bool {
	type sqlStater interface{ SQLState() string }
	var st sqlStater
	if errors.As(err, &st) {
		return st.SQLState() == "23505"
	}
	return false
}
