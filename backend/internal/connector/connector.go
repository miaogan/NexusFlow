// Package connector implements the external-data-source framework: a source
// discovers and pulls documents (web pages, object-store files, database rows),
// a cursor makes pulls incremental, and ingestion turns each document into a
// normal parse → index task.
package connector

import (
	"context"
	"errors"
	"time"
)

// Type identifies a built-in connector implementation.
type Type string

const (
	TypeWeb Type = "web_crawler"
	TypeS3  Type = "s3_bucket"
	TypeDB  Type = "db_query"
)

// Connector lifecycle states, aligned with data_connectors.status.
const (
	StatusIdle     = "IDLE"
	StatusSyncing  = "SYNCING"
	StatusError    = "ERROR"
	StatusDisabled = "DISABLED"
)

// ErrNotFound hides missing and cross-tenant connectors behind one error.
var ErrNotFound = errors.New("connector not found")

// Config is the per-type connector configuration. Secrets never leave the
// process: they are encrypted at rest and masked by the HTTP API.
type Config struct {
	// web_crawler: seed pages plus same-host link discovery.
	URLs         []string `json:"urls,omitempty"`
	MaxDepth     int      `json:"max_depth,omitempty"`
	MaxPages     int      `json:"max_pages,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`

	// s3_bucket: S3-compatible endpoint (MinIO, AWS, OSS...).
	Endpoint   string `json:"endpoint,omitempty"`
	Bucket     string `json:"bucket,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Region     string `json:"region,omitempty"`
	AccessKey  string `json:"access_key,omitempty"`
	SecretKey  string `json:"secret_key,omitempty"`
	UseSSL     bool   `json:"use_ssl,omitempty"`
	MaxObjects int    `json:"max_objects,omitempty"`

	// db_query: read-only SELECT serialized to one Markdown document per row.
	DSN       string `json:"dsn,omitempty"`
	SQL       string `json:"sql,omitempty"`
	IDColumn  string `json:"id_column,omitempty"`
	BatchSize int    `json:"batch_size,omitempty"`
	MaxRows   int    `json:"max_rows,omitempty"`
}

// Item is one discovered document ready for ingestion.
type Item struct {
	ExternalID string
	FileName   string
	FileType   string // pdf / docx / txt / md / html
	Content    []byte
}

// Batch is one page of discovery results plus the cursor for the next pull.
type Batch struct {
	Items      []Item
	NextCursor map[string]string
	// Done means the source has nothing after this batch in the current run.
	Done bool
}

// Spec is the persisted connector definition.
type Spec struct {
	ID           string
	TenantID     string
	KBID         string
	Name         string
	Type         Type
	Config       Config
	ScheduleCron string
	Status       string
	SyncCursor   map[string]string
	LastSyncAt   time.Time
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Source pulls documents of one connector type.
type Source interface {
	Type() Type
	Validate(cfg Config) error
	// Pull returns documents after cursor. Implementations must bound the
	// response (items, bytes and time) so one sync cannot run forever.
	Pull(ctx context.Context, cfg Config, cursor map[string]string) (Batch, error)
}

// Store persists connector definitions and sync cursors.
type Store interface {
	List(ctx context.Context, tenantID, kbID string) ([]Spec, error)
	Get(ctx context.Context, tenantID, id string) (Spec, bool, error)
	Create(ctx context.Context, spec Spec) (Spec, error)
	Update(ctx context.Context, spec Spec) error
	Delete(ctx context.Context, tenantID, id string) (bool, error)
}

// Result summarizes one sync run.
type Result struct {
	ConnectorID string   `json:"connector_id"`
	Created     int      `json:"created"`
	Skipped     int      `json:"skipped"`
	Failed      int      `json:"failed"`
	TaskIDs     []string `json:"task_ids"`
	Errors      []string `json:"errors,omitempty"`
}
