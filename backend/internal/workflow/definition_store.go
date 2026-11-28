package workflow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Vec2 is a canvas position for one node. Definitions persist the authored
// layout so reopening a workflow restores the same arrangement.
type Vec2 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

const (
	DefinitionDraft     = "draft"
	DefinitionPublished = "published"
)

// Definition is a saved, reusable workflow: the DAG plus console metadata.
// Published definitions are locked; edits require converting them to a draft.
type Definition struct {
	ID          string          `json:"id"`
	TenantID    string          `json:"tenant_id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Category    string          `json:"category,omitempty"`
	DAG         DAG             `json:"dag"`
	Layout      map[string]Vec2 `json:"layout,omitempty"`
	Status      string          `json:"status"`
	Version     int             `json:"version"`
	PublishedAt *time.Time      `json:"published_at,omitempty"`
	CreatedBy   string          `json:"created_by,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// DefinitionVersion is an immutable published snapshot used for rollback.
type DefinitionVersion struct {
	DefinitionID string          `json:"definition_id"`
	TenantID     string          `json:"tenant_id"`
	Version      int             `json:"version"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Category     string          `json:"category,omitempty"`
	DAG          DAG             `json:"dag"`
	Layout       map[string]Vec2 `json:"layout,omitempty"`
	CreatedBy    string          `json:"created_by,omitempty"`
	PublishedAt  time.Time       `json:"published_at"`
}

// DefinitionStore persists workflow definitions and publish snapshots per tenant.
type DefinitionStore interface {
	List(ctx context.Context, tenantID string) ([]*Definition, error)
	Get(ctx context.Context, tenantID, id string) (*Definition, bool, error)
	Save(ctx context.Context, def *Definition) error
	Delete(ctx context.Context, tenantID, id string) (bool, error)
	ListVersions(ctx context.Context, tenantID, definitionID string) ([]*DefinitionVersion, error)
	SaveVersion(ctx context.Context, version *DefinitionVersion) error
}

// NewDefinitionID returns a collision-resistant workflow definition ID.
func NewDefinitionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("wf-%d", time.Now().UnixNano())
	}
	return "wf-" + hex.EncodeToString(buf)
}

// DefinitionStoreFor reuses the run store's database connection when it can
// provide a definition store, otherwise keeps definitions in memory.
func DefinitionStoreFor(runStore RunStore) DefinitionStore {
	if provider, ok := runStore.(definitionStoreProvider); ok {
		return provider.Definitions()
	}
	return NewMemoryDefinitionStore()
}

// definitionStoreProvider is implemented by stores that share their database
// connection with workflow definitions.
type definitionStoreProvider interface {
	Definitions() DefinitionStore
}

// MemoryDefinitionStore keeps definitions in process memory (single replica).
type MemoryDefinitionStore struct {
	mu       sync.RWMutex
	items    map[string]map[string]*Definition
	versions map[string]map[string][]*DefinitionVersion
}

func NewMemoryDefinitionStore() *MemoryDefinitionStore {
	return &MemoryDefinitionStore{
		items:    make(map[string]map[string]*Definition),
		versions: make(map[string]map[string][]*DefinitionVersion),
	}
}

func (s *MemoryDefinitionStore) List(_ context.Context, tenantID string) ([]*Definition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	byID := s.items[tenantID]
	out := make([]*Definition, 0, len(byID))
	for _, def := range byID {
		out = append(out, cloneDefinition(def))
	}
	sortDefinitions(out)
	return out, nil
}

func (s *MemoryDefinitionStore) Get(_ context.Context, tenantID, id string) (*Definition, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	def, ok := s.items[tenantID][id]
	if !ok {
		return nil, false, nil
	}
	return cloneDefinition(def), true, nil
}

func (s *MemoryDefinitionStore) Save(_ context.Context, def *Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if def.Status == "" {
		def.Status = DefinitionDraft
	}
	if s.items[def.TenantID] == nil {
		s.items[def.TenantID] = make(map[string]*Definition)
	}
	s.items[def.TenantID][def.ID] = cloneDefinition(def)
	return nil
}

func (s *MemoryDefinitionStore) Delete(_ context.Context, tenantID, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[tenantID][id]; !ok {
		return false, nil
	}
	delete(s.items[tenantID], id)
	delete(s.versions[tenantID], id)
	return true, nil
}

func (s *MemoryDefinitionStore) ListVersions(_ context.Context, tenantID, definitionID string) ([]*DefinitionVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored := s.versions[tenantID][definitionID]
	out := make([]*DefinitionVersion, 0, len(stored))
	for _, version := range stored {
		out = append(out, cloneDefinitionVersion(version))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

func (s *MemoryDefinitionStore) SaveVersion(_ context.Context, version *DefinitionVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[version.TenantID] == nil {
		s.versions[version.TenantID] = make(map[string][]*DefinitionVersion)
	}
	existing := s.versions[version.TenantID][version.DefinitionID]
	for i, item := range existing {
		if item.Version == version.Version {
			existing[i] = cloneDefinitionVersion(version)
			return nil
		}
	}
	s.versions[version.TenantID][version.DefinitionID] = append(existing, cloneDefinitionVersion(version))
	return nil
}

// ---------- Postgres ----------

// postgresDefinitionStore shares the run store's connection pool.
type postgresDefinitionStore struct {
	db *sql.DB
}

// Definitions exposes the workflow-definition view of the same database.
func (s *PostgresRunStore) Definitions() DefinitionStore {
	return &postgresDefinitionStore{db: s.db}
}

func (s *postgresDefinitionStore) List(ctx context.Context, tenantID string) ([]*Definition, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, tenant_id, name, description, category, dag, layout,
		        COALESCE(status, 'draft'), COALESCE(version, 0), published_at,
		        created_by, created_at, updated_at
		 FROM workflow_definitions WHERE tenant_id = $1
		 ORDER BY updated_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workflow definitions: %w", err)
	}
	defer rows.Close()
	out := make([]*Definition, 0)
	for rows.Next() {
		def, err := scanDefinition(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow definitions: %w", err)
	}
	return out, nil
}

func (s *postgresDefinitionStore) Get(ctx context.Context, tenantID, id string) (*Definition, bool, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, tenant_id, name, description, category, dag, layout,
		        COALESCE(status, 'draft'), COALESCE(version, 0), published_at,
		        created_by, created_at, updated_at
		 FROM workflow_definitions WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	def, err := scanDefinition(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return def, true, nil
}

func (s *postgresDefinitionStore) Save(ctx context.Context, def *Definition) error {
	dag, err := json.Marshal(def.DAG)
	if err != nil {
		return fmt.Errorf("encode workflow dag: %w", err)
	}
	layout, err := json.Marshal(def.Layout)
	if err != nil {
		return fmt.Errorf("encode workflow layout: %w", err)
	}
	status := def.Status
	if status == "" {
		status = DefinitionDraft
		def.Status = status
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO workflow_definitions (
			id, tenant_id, name, description, category, dag, layout,
			status, version, published_at, created_by, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			category = EXCLUDED.category,
			dag = EXCLUDED.dag,
			layout = EXCLUDED.layout,
			status = EXCLUDED.status,
			version = EXCLUDED.version,
			published_at = EXCLUDED.published_at,
			updated_at = EXCLUDED.updated_at
		WHERE workflow_definitions.tenant_id = EXCLUDED.tenant_id`,
		def.ID, def.TenantID, def.Name, def.Description, def.Category,
		dag, layout, status, def.Version, def.PublishedAt,
		def.CreatedBy, def.CreatedAt, def.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert workflow definition: %w", err)
	}
	return nil
}

func (s *postgresDefinitionStore) Delete(ctx context.Context, tenantID, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM workflow_definition_versions WHERE tenant_id = $1 AND definition_id = $2`,
		tenantID, id,
	); err != nil {
		return false, fmt.Errorf("delete workflow versions: %w", err)
	}
	res, err := tx.ExecContext(
		ctx,
		`DELETE FROM workflow_definitions WHERE tenant_id = $1 AND id = $2`,
		tenantID, id,
	)
	if err != nil {
		return false, fmt.Errorf("delete workflow definition: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return affected > 0, nil
}

func (s *postgresDefinitionStore) ListVersions(ctx context.Context, tenantID, definitionID string) ([]*DefinitionVersion, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT definition_id, tenant_id, version, name, description, category,
		        dag, layout, created_by, published_at
		 FROM workflow_definition_versions
		 WHERE tenant_id = $1 AND definition_id = $2
		 ORDER BY version DESC`,
		tenantID, definitionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list workflow versions: %w", err)
	}
	defer rows.Close()
	out := make([]*DefinitionVersion, 0)
	for rows.Next() {
		version, err := scanDefinitionVersion(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, version)
	}
	return out, rows.Err()
}

func (s *postgresDefinitionStore) SaveVersion(ctx context.Context, version *DefinitionVersion) error {
	dag, err := json.Marshal(version.DAG)
	if err != nil {
		return fmt.Errorf("encode workflow version dag: %w", err)
	}
	layout, err := json.Marshal(version.Layout)
	if err != nil {
		return fmt.Errorf("encode workflow version layout: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO workflow_definition_versions (
			definition_id, tenant_id, version, name, description, category,
			dag, layout, created_by, published_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (definition_id, version) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description,
			category = EXCLUDED.category,
			dag = EXCLUDED.dag,
			layout = EXCLUDED.layout,
			published_at = EXCLUDED.published_at`,
		version.DefinitionID, version.TenantID, version.Version, version.Name,
		version.Description, version.Category, dag, layout,
		version.CreatedBy, version.PublishedAt,
	)
	if err != nil {
		return fmt.Errorf("save workflow version: %w", err)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner func(dest ...any) error

func scanDefinition(scan scanner) (*Definition, error) {
	var (
		def         Definition
		dagRaw      []byte
		layoutRaw   []byte
		publishedAt sql.NullTime
	)
	if err := scan(
		&def.ID, &def.TenantID, &def.Name, &def.Description, &def.Category,
		&dagRaw, &layoutRaw, &def.Status, &def.Version, &publishedAt,
		&def.CreatedBy, &def.CreatedAt, &def.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if def.Status == "" {
		def.Status = DefinitionDraft
	}
	if publishedAt.Valid {
		value := publishedAt.Time
		def.PublishedAt = &value
	}
	if len(dagRaw) > 0 {
		if err := json.Unmarshal(dagRaw, &def.DAG); err != nil {
			return nil, fmt.Errorf("decode workflow dag: %w", err)
		}
	}
	if len(layoutRaw) > 0 {
		if err := json.Unmarshal(layoutRaw, &def.Layout); err != nil {
			return nil, fmt.Errorf("decode workflow layout: %w", err)
		}
	}
	return &def, nil
}

func scanDefinitionVersion(scan scanner) (*DefinitionVersion, error) {
	var (
		version   DefinitionVersion
		dagRaw    []byte
		layoutRaw []byte
	)
	if err := scan(
		&version.DefinitionID, &version.TenantID, &version.Version, &version.Name,
		&version.Description, &version.Category, &dagRaw, &layoutRaw,
		&version.CreatedBy, &version.PublishedAt,
	); err != nil {
		return nil, err
	}
	if len(dagRaw) > 0 {
		if err := json.Unmarshal(dagRaw, &version.DAG); err != nil {
			return nil, fmt.Errorf("decode workflow version dag: %w", err)
		}
	}
	if len(layoutRaw) > 0 {
		if err := json.Unmarshal(layoutRaw, &version.Layout); err != nil {
			return nil, fmt.Errorf("decode workflow version layout: %w", err)
		}
	}
	return &version, nil
}

func cloneDefinition(def *Definition) *Definition {
	clone := *def
	clone.DAG.Nodes = append([]NodeSpec(nil), def.DAG.Nodes...)
	clone.DAG.Edges = append([]Edge(nil), def.DAG.Edges...)
	if def.Layout != nil {
		clone.Layout = make(map[string]Vec2, len(def.Layout))
		for id, pos := range def.Layout {
			clone.Layout[id] = pos
		}
	}
	return &clone
}

func cloneDefinitionVersion(version *DefinitionVersion) *DefinitionVersion {
	clone := *version
	clone.DAG.Nodes = append([]NodeSpec(nil), version.DAG.Nodes...)
	clone.DAG.Edges = append([]Edge(nil), version.DAG.Edges...)
	if version.Layout != nil {
		clone.Layout = make(map[string]Vec2, len(version.Layout))
		for id, pos := range version.Layout {
			clone.Layout[id] = pos
		}
	}
	return &clone
}

func sortDefinitions(defs []*Definition) {
	// Newest first, matching the run list ordering.
	for i := 1; i < len(defs); i++ {
		for j := i; j > 0 && defs[j].UpdatedAt.After(defs[j-1].UpdatedAt); j-- {
			defs[j], defs[j-1] = defs[j-1], defs[j]
		}
	}
}
