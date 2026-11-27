// Package metering tracks tenant resource consumption (stored bytes, file and
// query counts) and enforces the corresponding quotas.
package metering

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// KBUsage is the per-knowledge-base slice of a tenant's storage counters.
type KBUsage struct {
	KBID         string `json:"kb_id"`
	StorageBytes int64  `json:"storage_bytes"`
	FileCount    int64  `json:"file_count"`
}

// Usage is the current consumption snapshot for one tenant.
type Usage struct {
	TenantID       string    `json:"tenant_id"`
	StorageBytes   int64     `json:"storage_bytes"`
	FileCount      int64     `json:"file_count"`
	QueryCount     int64     `json:"query_count"`
	KnowledgeBases []KBUsage `json:"knowledge_bases"`
}

// Quota is the tenant allowance. A non-positive value means "unlimited", which
// keeps local development unconstrained while allowing production limits.
type Quota struct {
	MaxStorageBytes int64 `json:"max_storage_bytes"`
	MaxFiles        int64 `json:"max_files"`
	MaxQueries      int64 `json:"max_queries"`
}

// ErrQuotaExceeded is returned when an action would cross a configured quota.
var ErrQuotaExceeded = errors.New("quota exceeded")

// CheckUpload validates a new file against the storage and file-count quotas.
func (q Quota) CheckUpload(u Usage, addBytes int64) error {
	if addBytes < 0 {
		addBytes = 0
	}
	if q.MaxStorageBytes > 0 && u.StorageBytes+addBytes > q.MaxStorageBytes {
		return fmt.Errorf("%w: storage %d + %d > %d bytes",
			ErrQuotaExceeded, u.StorageBytes, addBytes, q.MaxStorageBytes)
	}
	if q.MaxFiles > 0 && u.FileCount+1 > q.MaxFiles {
		return fmt.Errorf("%w: files %d >= %d", ErrQuotaExceeded, u.FileCount, q.MaxFiles)
	}
	return nil
}

// CheckQuery validates one more retrieval/chat turn against the query quota.
func (q Quota) CheckQuery(u Usage) error {
	if q.MaxQueries > 0 && u.QueryCount >= q.MaxQueries {
		return fmt.Errorf("%w: queries %d >= %d", ErrQuotaExceeded, u.QueryCount, q.MaxQueries)
	}
	return nil
}

// Remaining reports how much of each quota is still available. Unlimited
// dimensions report -1.
func (q Quota) Remaining(u Usage) Quota {
	out := Quota{MaxStorageBytes: -1, MaxFiles: -1, MaxQueries: -1}
	if q.MaxStorageBytes > 0 {
		out.MaxStorageBytes = max64(q.MaxStorageBytes-u.StorageBytes, 0)
	}
	if q.MaxFiles > 0 {
		out.MaxFiles = max64(q.MaxFiles-u.FileCount, 0)
	}
	if q.MaxQueries > 0 {
		out.MaxQueries = max64(q.MaxQueries-u.QueryCount, 0)
	}
	return out
}

// Store is the metering persistence boundary shared by every gateway replica.
type Store interface {
	AddStorage(ctx context.Context, tenantID, kbID string, bytes, files int64) error
	AddQuery(ctx context.Context, tenantID string, count int64) error
	Usage(ctx context.Context, tenantID string) (Usage, error)
}

// NewStore selects the metering backend: memory (default) or redis.
func NewStore(driver, redisAddr string) (Store, error) {
	switch driver {
	case "redis":
		return NewRedisStore(redisAddr)
	default:
		return NewMemoryStore(), nil
	}
}

type counters struct {
	queries int64
	kbs     map[string]*kbCounters
}

type kbCounters struct {
	storage int64
	files   int64
}

// MemoryStore keeps counters in process memory; suitable for dev-all and tests.
type MemoryStore struct {
	mu      sync.RWMutex
	tenants map[string]*counters
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tenants: make(map[string]*counters)}
}

func (m *MemoryStore) countersLocked(tenantID string) *counters {
	c, ok := m.tenants[tenantID]
	if !ok {
		c = &counters{kbs: make(map[string]*kbCounters)}
		m.tenants[tenantID] = c
	}
	return c
}

func (m *MemoryStore) AddStorage(_ context.Context, tenantID, kbID string, bytes, files int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.countersLocked(tenantID)
	kb := c.kbs[kbID]
	if kb == nil {
		kb = &kbCounters{}
		c.kbs[kbID] = kb
	}
	// Clamp per knowledge base so an over-subtract in one namespace can never
	// drain another namespace's bytes; tenant totals are always re-derived.
	kb.storage = max64(kb.storage+bytes, 0)
	kb.files = max64(kb.files+files, 0)
	return nil
}

func (m *MemoryStore) AddQuery(_ context.Context, tenantID string, count int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	c := m.countersLocked(tenantID)
	c.queries = max64(c.queries+count, 0)
	return nil
}

func (m *MemoryStore) Usage(_ context.Context, tenantID string) (Usage, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := Usage{TenantID: tenantID, KnowledgeBases: []KBUsage{}}
	c, ok := m.tenants[tenantID]
	if !ok {
		return out, nil
	}
	out.QueryCount = c.queries
	for kbID, kb := range c.kbs {
		out.StorageBytes += kb.storage
		out.FileCount += kb.files
		out.KnowledgeBases = append(out.KnowledgeBases, KBUsage{
			KBID: kbID, StorageBytes: kb.storage, FileCount: kb.files,
		})
	}
	sort.Slice(out.KnowledgeBases, func(i, j int) bool {
		if out.KnowledgeBases[i].StorageBytes != out.KnowledgeBases[j].StorageBytes {
			return out.KnowledgeBases[i].StorageBytes > out.KnowledgeBases[j].StorageBytes
		}
		return out.KnowledgeBases[i].KBID < out.KnowledgeBases[j].KBID
	})
	return out, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
