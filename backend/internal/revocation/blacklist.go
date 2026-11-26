// Package revocation maintains the JWT revocation list (token blacklist).
//
// Tokens carry a jti claim; logging out records that id here until the token's
// natural expiry, and every authentication check rejects revoked ids. A Redis
// backend shares the list across the gateway and auth-service processes; the
// in-memory backend keeps the same behaviour within a single process, which is
// the degraded path when Redis is unavailable.
package revocation

import (
	"context"
	"sync"
	"time"
)

// Blacklist stores revoked token ids until they expire.
type Blacklist interface {
	// Revoke records tokenID as invalid for ttl.
	Revoke(ctx context.Context, tokenID string, ttl time.Duration) error
	// Revoked reports whether tokenID has been revoked.
	Revoked(ctx context.Context, tokenID string) (bool, error)
	// Close releases backend resources.
	Close() error
}

// Check reports whether tokenID is revoked. A nil blacklist and empty ids are
// treated as not revoked. Store errors fail open so a Redis outage cannot lock
// every caller out of the platform.
func Check(ctx context.Context, b Blacklist, tokenID string) bool {
	if b == nil || tokenID == "" {
		return false
	}
	revoked, err := b.Revoked(ctx, tokenID)
	return err == nil && revoked
}

// MemoryBlacklist keeps revocations in-process. It is the dev default and the
// fallback when Redis is unreachable.
type MemoryBlacklist struct {
	mu      sync.Mutex
	expires map[string]time.Time
	now     func() time.Time
}

func NewMemoryBlacklist() *MemoryBlacklist {
	return &MemoryBlacklist{expires: make(map[string]time.Time), now: time.Now}
}

func (m *MemoryBlacklist) Revoke(_ context.Context, tokenID string, ttl time.Duration) error {
	if tokenID == "" || ttl <= 0 {
		return nil
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	// Opportunistically drop expired entries so the map cannot grow unbounded.
	if len(m.expires) >= 64 {
		for id, exp := range m.expires {
			if now.After(exp) {
				delete(m.expires, id)
			}
		}
	}
	m.expires[tokenID] = now.Add(ttl)
	return nil
}

func (m *MemoryBlacklist) Revoked(_ context.Context, tokenID string) (bool, error) {
	if tokenID == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.expires[tokenID]
	if !ok {
		return false, nil
	}
	if m.now().After(exp) {
		delete(m.expires, tokenID)
		return false, nil
	}
	return true, nil
}

func (m *MemoryBlacklist) Close() error { return nil }

// New selects the blacklist backend: redis | memory (anything else falls back
// to memory). When Redis cannot be reached the returned list is an in-memory
// implementation together with the connection error so the caller can warn.
func New(driver, redisAddr string) (Blacklist, error) {
	if driver == "redis" {
		b, err := NewRedisBlacklist(redisAddr)
		if err != nil {
			return NewMemoryBlacklist(), err
		}
		return b, nil
	}
	return NewMemoryBlacklist(), nil
}
