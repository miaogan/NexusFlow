// Package apikey implements tenant-scoped API keys for the external
// OpenAI-compatible endpoint. Keys are stored as SHA-256 hashes; the plaintext
// value is only returned once at creation time.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

const (
	// Prefix marks a bearer token as a NexusFlow API key instead of a JWT.
	Prefix = "nf-sk-"

	StatusActive  = "ACTIVE"
	StatusRevoked = "REVOKED"

	// ScopeChatCompletions allows the OpenAI-compatible chat endpoint.
	ScopeChatCompletions = "chat:completions"
	// ScopeSearchUse allows retrieval-backed routes (search/chat/media).
	ScopeSearchUse = "search:use"
)

// AllowedScopes is the fixed catalogue accepted at issuance time.
var AllowedScopes = []string{ScopeChatCompletions, ScopeSearchUse}

var (
	// ErrNotFound is returned when a key does not exist for the tenant.
	ErrNotFound = errors.New("api key not found")
	// ErrInvalidScope is returned for a scope outside AllowedScopes.
	ErrInvalidScope = errors.New("invalid api key scope")
	// ErrDuplicate is returned when a key hash already exists.
	ErrDuplicate = errors.New("api key already exists")
)

// Key is a tenant-scoped credential. KeyHash is never serialized.
type Key struct {
	ID           string     `json:"id"`
	TenantID     string     `json:"tenant_id"`
	Name         string     `json:"name"`
	KeyPrefix    string     `json:"key_prefix"`
	KeyHash      string     `json:"-"`
	AllowedKBIDs []string   `json:"allowed_kb_ids"`
	Scopes       []string   `json:"scopes"`
	RateLimitRPM int        `json:"rate_limit_rpm"`
	Status       string     `json:"status"`
	ExpiresAt    *time.Time `json:"expires_at"`
	LastUsedAt   *time.Time `json:"last_used_at"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
}

// Expired reports whether the key is past its optional expiry.
func (k Key) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && now.After(*k.ExpiresAt)
}

// AllowsKB reports whether the key may access a knowledge base. An empty
// allow-list means every knowledge base in the tenant.
func (k Key) AllowsKB(kbID string) bool {
	if len(k.AllowedKBIDs) == 0 {
		return true
	}
	for _, id := range k.AllowedKBIDs {
		if id == kbID {
			return true
		}
	}
	return false
}

// Principal converts the key into the caller identity propagated to services.
// chat:completions implies search:use because the RAG pipeline retrieves
// through the same gateway.
func (k Key) Principal() tenant.Principal {
	perms := append([]string(nil), k.Scopes...)
	if hasScope(perms, ScopeChatCompletions) && !hasScope(perms, ScopeSearchUse) {
		perms = append(perms, ScopeSearchUse)
	}
	return tenant.Principal{
		TenantID:    k.TenantID,
		UserID:      "apikey:" + k.ID,
		Roles:       []string{"api"},
		Permissions: perms,
	}
}

func hasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if scope == want {
			return true
		}
	}
	return false
}

// LooksLike reports whether a raw bearer token is an API key.
func LooksLike(raw string) bool {
	return strings.HasPrefix(raw, Prefix)
}

// NewSecret generates a plaintext key plus its display prefix.
func NewSecret() (plain, displayPrefix string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = Prefix + hex.EncodeToString(buf)
	displayPrefix = plain
	if len(displayPrefix) > 12 {
		displayPrefix = displayPrefix[:12]
	}
	return plain, displayPrefix, nil
}

// Hash returns the hex-encoded SHA-256 digest persisted in api_keys.key_hash.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ValidateScopes normalizes the requested scopes and rejects unknown values.
// An empty request falls back to both default scopes.
func ValidateScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return append([]string(nil), AllowedScopes...), nil
	}
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if !knownScope(scope) {
			return nil, ErrInvalidScope
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if len(out) == 0 {
		return append([]string(nil), AllowedScopes...), nil
	}
	return out, nil
}

func knownScope(scope string) bool {
	for _, allowed := range AllowedScopes {
		if scope == allowed {
			return true
		}
	}
	return false
}

// NormalizeKBIDs trims and de-duplicates a knowledge-base allow-list.
func NormalizeKBIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// Store persists API keys. Implementations must be safe for concurrent use.
type Store interface {
	Create(ctx context.Context, k Key) (Key, error)
	List(ctx context.Context, tenantID string) ([]Key, error)
	Get(ctx context.Context, tenantID, id string) (Key, error)
	Revoke(ctx context.Context, tenantID, id string) (Key, error)
	FindByHash(ctx context.Context, hash string) (Key, error)
	Touch(ctx context.Context, tenantID, id string, at time.Time) error
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

// ConfigError reports an unsupported API-key backend.
type ConfigError struct{ Driver string }

func (e *ConfigError) Error() string {
	return "unsupported api key driver: " + e.Driver
}

type ctxKey struct{}

// With attaches the authenticated API key to a request context.
func With(ctx context.Context, k Key) context.Context {
	return context.WithValue(ctx, ctxKey{}, k)
}

// From returns the API key attached by the auth middleware.
func From(ctx context.Context) (Key, bool) {
	k, ok := ctx.Value(ctxKey{}).(Key)
	return k, ok
}
