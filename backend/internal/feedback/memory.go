package feedback

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore keeps feedback in process memory. It backs dev-all and unit
// tests; it is not durable across restarts.
type MemoryStore struct {
	mu    sync.Mutex
	items map[string]Entry
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{items: make(map[string]Entry)}
}

func (s *MemoryStore) Upsert(_ context.Context, e Entry) (Entry, error) {
	now := time.Now().UTC()
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	e.UpdatedAt = now

	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.items[memoryKey(e)]; ok {
		e.ID = prev.ID
		e.CreatedAt = prev.CreatedAt
	} else if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	s.items[memoryKey(e)] = e
	return e, nil
}

func (s *MemoryStore) List(_ context.Context, f Filter) ([]Entry, error) {
	limit, offset := normalizePage(f.Limit, f.Offset)

	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.items))
	for _, e := range s.items {
		if e.TenantID != f.TenantID {
			continue
		}
		if f.Rating != nil && e.Rating != *f.Rating {
			continue
		}
		if f.NegativeOnly && e.Rating >= 0 {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if offset >= len(out) {
		return []Entry{}, nil
	}
	out = out[offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *MemoryStore) Close() error { return nil }

func memoryKey(e Entry) string {
	return e.TenantID + "\x00" + e.UserID + "\x00" + e.SubjectID + "\x00" + e.Name
}

func normalizePage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}
