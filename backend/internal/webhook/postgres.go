package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore persists subscriptions and delivery logs in the shared
// Postgres database. DDL mirrors docs/schema_enterprise_v1_1_to_1_5.sql.
type PostgresStore struct {
	db *sql.DB
}

const subscriptionColumns = `id, tenant_id, name, target_url, secret_encrypted, events,
	is_active, created_at, updated_at`

const deliveryColumns = `id, tenant_id, webhook_id, event_type, payload, attempt_count,
	http_status, response_body, status, next_retry_at, created_at`

var storeSchema = []string{
	`CREATE TABLE IF NOT EXISTS webhooks (
		id               VARCHAR(64) PRIMARY KEY,
		tenant_id        VARCHAR(64) NOT NULL,
		name             VARCHAR(128) NOT NULL,
		target_url       TEXT NOT NULL,
		secret_encrypted TEXT NOT NULL,
		events           JSONB NOT NULL DEFAULT '["task.completed", "task.failed"]'::jsonb,
		is_active        BOOLEAN NOT NULL DEFAULT TRUE,
		created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_webhooks_tenant_active
		ON webhooks (tenant_id, is_active)`,
	`CREATE TABLE IF NOT EXISTS webhook_deliveries (
		id            VARCHAR(64) PRIMARY KEY,
		tenant_id     VARCHAR(64) NOT NULL,
		webhook_id    VARCHAR(64) NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
		event_type    VARCHAR(64) NOT NULL,
		payload       JSONB NOT NULL,
		attempt_count INT NOT NULL DEFAULT 1,
		http_status   INT DEFAULT 0,
		response_body TEXT DEFAULT '',
		status        VARCHAR(16) NOT NULL DEFAULT 'PENDING',
		next_retry_at TIMESTAMPTZ,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_retry
		ON webhook_deliveries (status, next_retry_at)
		WHERE status = 'PENDING'`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_tenant
		ON webhook_deliveries (tenant_id, webhook_id, created_at DESC)`,
}

// NewPostgresStore opens the database, verifies connectivity and ensures the
// webhook schema exists.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("webhook: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("webhook: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("webhook: ping postgres: %w", err)
	}
	for _, stmt := range storeSchema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("webhook: migrate: %w", err)
		}
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) CreateSubscription(ctx context.Context, sub Subscription) (Subscription, error) {
	now := time.Now().UTC()
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = now
	}
	sub.UpdatedAt = now
	events, err := json.Marshal(nonNilStrings(sub.Events))
	if err != nil {
		return Subscription{}, fmt.Errorf("webhook: marshal events: %w", err)
	}

	const insert = `INSERT INTO webhooks (` + subscriptionColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at, updated_at`
	row := s.db.QueryRowContext(ctx, insert,
		sub.ID, sub.TenantID, sub.Name, sub.TargetURL, sub.SecretEncrypted,
		events, sub.IsActive, sub.CreatedAt, sub.UpdatedAt,
	)
	if err := row.Scan(&sub.CreatedAt, &sub.UpdatedAt); err != nil {
		return Subscription{}, fmt.Errorf("webhook: insert: %w", err)
	}
	return sub, nil
}

func (s *PostgresStore) ListSubscriptions(ctx context.Context, tenantID string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subscriptionColumns+` FROM webhooks WHERE tenant_id = $1 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("webhook: list subscriptions: %w", err)
	}
	defer rows.Close()
	return scanSubscriptions(rows)
}

func (s *PostgresStore) GetSubscription(ctx context.Context, tenantID, id string) (Subscription, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM webhooks WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	sub, err := scanSubscription(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	if err != nil {
		return Subscription{}, fmt.Errorf("webhook: get subscription: %w", err)
	}
	return sub, nil
}

func (s *PostgresStore) DeleteSubscription(ctx context.Context, tenantID, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM webhooks WHERE tenant_id = $1 AND id = $2`, tenantID, id,
	)
	if err != nil {
		return fmt.Errorf("webhook: delete subscription: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("webhook: delete subscription: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) ListActive(ctx context.Context, tenantID string) ([]Subscription, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subscriptionColumns+` FROM webhooks
		 WHERE tenant_id = $1 AND is_active = TRUE ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("webhook: list active: %w", err)
	}
	defer rows.Close()
	return scanSubscriptions(rows)
}

func (s *PostgresStore) CreateDelivery(ctx context.Context, d Delivery) (Delivery, error) {
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	if d.Status == "" {
		d.Status = StatusPending
	}
	const insert = `INSERT INTO webhook_deliveries (` + deliveryColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING created_at`
	row := s.db.QueryRowContext(ctx, insert,
		d.ID, d.TenantID, d.WebhookID, d.EventType, []byte(d.Payload), d.AttemptCount,
		d.HTTPStatus, d.ResponseBody, d.Status, d.NextRetryAt, d.CreatedAt,
	)
	if err := row.Scan(&d.CreatedAt); err != nil {
		return Delivery{}, fmt.Errorf("webhook: insert delivery: %w", err)
	}
	return d, nil
}

func (s *PostgresStore) UpdateDelivery(ctx context.Context, d Delivery) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE webhook_deliveries SET
			attempt_count = $3, http_status = $4, response_body = $5,
			status = $6, next_retry_at = $7
		 WHERE id = $1 AND tenant_id = $2`,
		d.ID, d.TenantID, d.AttemptCount, d.HTTPStatus, d.ResponseBody,
		d.Status, d.NextRetryAt,
	)
	if err != nil {
		return fmt.Errorf("webhook: update delivery: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListDeliveries(ctx context.Context, tenantID, webhookID string, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries
		 WHERE tenant_id = $1 AND webhook_id = $2
		 ORDER BY created_at DESC LIMIT $3`,
		tenantID, webhookID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("webhook: list deliveries: %w", err)
	}
	defer rows.Close()

	out := make([]Delivery, 0, limit)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("webhook: scan delivery: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhook: rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSubscription(row rowScanner) (Subscription, error) {
	var (
		sub    Subscription
		events []byte
	)
	if err := row.Scan(
		&sub.ID, &sub.TenantID, &sub.Name, &sub.TargetURL, &sub.SecretEncrypted,
		&events, &sub.IsActive, &sub.CreatedAt, &sub.UpdatedAt,
	); err != nil {
		return Subscription{}, err
	}
	_ = json.Unmarshal(events, &sub.Events)
	if sub.Events == nil {
		sub.Events = []string{}
	}
	return sub, nil
}

func scanSubscriptions(rows *sql.Rows) ([]Subscription, error) {
	out := make([]Subscription, 0)
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("webhook: scan subscription: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webhook: rows: %w", err)
	}
	return out, nil
}

func scanDelivery(row rowScanner) (Delivery, error) {
	var (
		d       Delivery
		payload []byte
		next    sql.NullTime
	)
	if err := row.Scan(
		&d.ID, &d.TenantID, &d.WebhookID, &d.EventType, &payload, &d.AttemptCount,
		&d.HTTPStatus, &d.ResponseBody, &d.Status, &next, &d.CreatedAt,
	); err != nil {
		return Delivery{}, err
	}
	d.Payload = append([]byte{}, payload...)
	if next.Valid {
		d.NextRetryAt = &next.Time
	}
	return d, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}
