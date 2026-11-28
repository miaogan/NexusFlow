package apikey

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore keeps API keys in process memory. It is the default backend for
// local development and tests; restarting the gateway invalidates every key.
type MemoryStore struct {
	mu    sync.RWMutex
	byID  map[string]Key
	byKey map[string]string // key_hash -> id
}

// NewMemoryStore returns an empty in-memory key store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		byID:  make(map[string]Key),
		byKey: make(map[string]string),
	}
}

func (s *MemoryStore) Create(_ context.Context, k Key) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k.ID == "" {
		k.ID = uuid.NewString()
	}
	if k.Status == "" {
		k.Status = StatusActive
	}
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	if _, exists := s.byKey[k.KeyHash]; exists {
		return Key{}, ErrDuplicate
	}
	s.byID[k.ID] = cloneKey(k)
	s.byKey[k.KeyHash] = k.ID
	return cloneKey(k), nil
}

func (s *MemoryStore) List(_ context.Context, tenantID string) ([]Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, 0, len(s.byID))
	for _, k := range s.byID {
		if k.TenantID == tenantID {
			out = append(out, cloneKey(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, tenantID, id string) (Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return Key{}, ErrNotFound
	}
	return cloneKey(k), nil
}

func (s *MemoryStore) Revoke(_ context.Context, tenantID, id string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return Key{}, ErrNotFound
	}
	k.Status = StatusRevoked
	s.byID[id] = k
	return cloneKey(k), nil
}

func (s *MemoryStore) FindByHash(_ context.Context, hash string) (Key, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byKey[hash]
	if !ok {
		return Key{}, ErrNotFound
	}
	k, ok := s.byID[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	return cloneKey(k), nil
}

func (s *MemoryStore) Touch(_ context.Context, tenantID, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok || k.TenantID != tenantID {
		return ErrNotFound
	}
	stamp := at
	k.LastUsedAt = &stamp
	s.byID[id] = k
	return nil
}

func (s *MemoryStore) Close() error { return nil }

func cloneKey(k Key) Key {
	k.AllowedKBIDs = append([]string{}, k.AllowedKBIDs...)
	k.Scopes = append([]string{}, k.Scopes...)
	if k.ExpiresAt != nil {
		expiry := *k.ExpiresAt
		k.ExpiresAt = &expiry
	}
	if k.LastUsedAt != nil {
		last := *k.LastUsedAt
		k.LastUsedAt = &last
	}
	return k
}
