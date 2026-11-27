package feedback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore persists feedback in the shared Postgres database.
type PostgresStore struct {
	db *sql.DB
}

const feedbackColumns = `id, tenant_id, user_id, subject_id, trace_id, conversation_id,
	message_id, name, rating, comment, corrected_answer, query_text, answer_text,
	cited_chunk_ids, meta, created_at, updated_at`

var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS rag_feedback (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		subject_id TEXT NOT NULL,
		trace_id TEXT NOT NULL DEFAULT '',
		conversation_id TEXT NOT NULL DEFAULT '',
		message_id TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL,
		rating DOUBLE PRECISION NOT NULL DEFAULT 0,
		comment TEXT NOT NULL DEFAULT '',
		corrected_answer TEXT NOT NULL DEFAULT '',
		query_text TEXT NOT NULL DEFAULT '',
		answer_text TEXT NOT NULL DEFAULT '',
		cited_chunk_ids JSONB NOT NULL DEFAULT '[]'::jsonb,
		meta JSONB NOT NULL DEFAULT '{}'::jsonb,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (tenant_id, user_id, subject_id, name)
	)`,
	`CREATE INDEX IF NOT EXISTS rag_feedback_tenant_rating_idx
		ON rag_feedback (tenant_id, rating, updated_at DESC)`,
}

// NewPostgresStore opens the database, verifies connectivity and ensures the
// feedback schema exists.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("feedback: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("feedback: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("feedback: ping postgres: %w", err)
	}
	for _, stmt := range schemaStatements {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("feedback: migrate: %w", err)
		}
	}
	return &PostgresStore{db: db}, nil
}

func (s *PostgresStore) Upsert(ctx context.Context, e Entry) (Entry, error) {
	now := time.Now().UTC()
	// The primary key has no database default, so assign it here; concurrent
	// inserts of different subjects must not all claim the empty string.
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now

	cited, err := json.Marshal(nonNilStrings(e.CitedChunkIDs))
	if err != nil {
		return Entry{}, fmt.Errorf("feedback: marshal cited chunks: %w", err)
	}
	meta, err := json.Marshal(nonNilMap(e.Meta))
	if err != nil {
		return Entry{}, fmt.Errorf("feedback: marshal meta: %w", err)
	}

	const upsert = `INSERT INTO rag_feedback (` + feedbackColumns + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT (tenant_id, user_id, subject_id, name) DO UPDATE SET
			trace_id = EXCLUDED.trace_id,
			conversation_id = EXCLUDED.conversation_id,
			message_id = EXCLUDED.message_id,
			rating = EXCLUDED.rating,
			comment = EXCLUDED.comment,
			corrected_answer = EXCLUDED.corrected_answer,
			query_text = EXCLUDED.query_text,
			answer_text = EXCLUDED.answer_text,
			cited_chunk_ids = EXCLUDED.cited_chunk_ids,
			meta = EXCLUDED.meta,
			updated_at = now()
		RETURNING id, created_at, updated_at`

	row := s.db.QueryRowContext(ctx, upsert,
		e.ID, e.TenantID, e.UserID, e.SubjectID, e.TraceID, e.ConversationID,
		e.MessageID, e.Name, e.Rating, e.Comment, e.CorrectedAnswer, e.Query,
		e.Answer, cited, meta, e.CreatedAt, e.UpdatedAt,
	)
	if err := row.Scan(&e.ID, &e.CreatedAt, &e.UpdatedAt); err != nil {
		return Entry{}, fmt.Errorf("feedback: upsert: %w", err)
	}
	return e, nil
}

func (s *PostgresStore) List(ctx context.Context, f Filter) ([]Entry, error) {
	limit, offset := normalizePage(f.Limit, f.Offset)

	query := `SELECT ` + feedbackColumns + ` FROM rag_feedback WHERE tenant_id = $1`
	args := []any{f.TenantID}
	if f.Rating != nil {
		args = append(args, *f.Rating)
		query += fmt.Sprintf(" AND rating = $%d", len(args))
	}
	if f.NegativeOnly {
		query += " AND rating < 0"
	}
	args = append(args, limit, offset)
	query += fmt.Sprintf(" ORDER BY updated_at DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("feedback: list: %w", err)
	}
	defer rows.Close()

	out := make([]Entry, 0, limit)
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("feedback: scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("feedback: rows: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) Close() error { return s.db.Close() }

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEntry(row rowScanner) (Entry, error) {
	var (
		e     Entry
		cited []byte
		meta  []byte
	)
	if err := row.Scan(
		&e.ID, &e.TenantID, &e.UserID, &e.SubjectID, &e.TraceID, &e.ConversationID,
		&e.MessageID, &e.Name, &e.Rating, &e.Comment, &e.CorrectedAnswer, &e.Query,
		&e.Answer, &cited, &meta, &e.CreatedAt, &e.UpdatedAt,
	); err != nil {
		return Entry{}, err
	}
	_ = json.Unmarshal(cited, &e.CitedChunkIDs)
	_ = json.Unmarshal(meta, &e.Meta)
	return e, nil
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

func nonNilMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	return in
}
