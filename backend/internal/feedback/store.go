// Package feedback persists user evaluations (ratings and implicit signals)
// for RAG review. It is intentionally independent from the tracing backends so
// feedback survives restarts even when tracing is a no-op.
package feedback

import (
	"context"
	"time"
)

// Entry is a single feedback record.
type Entry struct {
	ID              string            `json:"id"`
	TenantID        string            `json:"tenant_id"`
	UserID          string            `json:"user_id"`
	SubjectID       string            `json:"subject_id"`
	TraceID         string            `json:"trace_id,omitempty"`
	ConversationID  string            `json:"conversation_id,omitempty"`
	MessageID       string            `json:"message_id,omitempty"`
	Name            string            `json:"name"`
	Rating          float64           `json:"rating"`
	Comment         string            `json:"comment,omitempty"`
	CorrectedAnswer string            `json:"corrected_answer,omitempty"`
	Query           string            `json:"query,omitempty"`
	Answer          string            `json:"answer,omitempty"`
	CitedChunkIDs   []string          `json:"cited_chunk_ids,omitempty"`
	Meta            map[string]string `json:"meta,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

// Filter narrows a feedback listing. TenantID is always required.
type Filter struct {
	TenantID string
	Rating   *float64
	// NegativeOnly keeps ratings below zero (used by retrieval down-ranking).
	NegativeOnly bool
	Limit        int
	Offset       int
}

// Store persists feedback. Implementations must be safe for concurrent use.
type Store interface {
	Upsert(ctx context.Context, e Entry) (Entry, error)
	List(ctx context.Context, f Filter) ([]Entry, error)
	Close() error
}

// NewStore selects a backend: memory (default) or postgres.
func NewStore(driver, dsn string) (Store, error) {
	switch driver {
	case "", "memory":
		return NewMemoryStore(), nil
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return NewPostgresStore(ctx, dsn)
	default:
		return nil, &ConfigError{Driver: driver}
	}
}

// ConfigError reports an unsupported feedback backend.
type ConfigError struct{ Driver string }

func (e *ConfigError) Error() string {
	return "unsupported feedback driver: " + e.Driver
}
