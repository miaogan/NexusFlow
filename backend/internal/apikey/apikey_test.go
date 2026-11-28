package apikey

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/tenant"
)

func newTestKey(tenant, name string, plain string) Key {
	return Key{
		ID:           "key-" + name,
		TenantID:     tenant,
		Name:         name,
		KeyPrefix:    plain[:12],
		KeyHash:      Hash(plain),
		AllowedKBIDs: []string{"kb-a"},
		Scopes:       []string{ScopeChatCompletions, ScopeSearchUse},
		RateLimitRPM: 60,
		Status:       StatusActive,
		CreatedBy:    "tester",
	}
}

func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()

	plain, prefix, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if !strings.HasPrefix(plain, Prefix) || len(prefix) != 12 {
		t.Fatalf("plain=%q prefix=%q", plain, prefix)
	}

	created, err := store.Create(ctx, newTestKey("t1", "ci", plain))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Status != StatusActive {
		t.Fatalf("status = %q", created.Status)
	}

	found, err := store.FindByHash(ctx, Hash(plain))
	if err != nil || found.ID != created.ID {
		t.Fatalf("FindByHash = %+v, %v", found, err)
	}
	if _, err := store.FindByHash(ctx, Hash("nf-sk-missing")); err != ErrNotFound {
		t.Fatalf("missing key error = %v", err)
	}

	items, err := store.List(ctx, "t1")
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %v, %v", items, err)
	}
	if other, _ := store.List(ctx, "t2"); len(other) != 0 {
		t.Fatalf("tenant isolation broken: %v", other)
	}

	if _, err := store.Get(ctx, "t2", created.ID); err != ErrNotFound {
		t.Fatalf("cross-tenant get error = %v", err)
	}

	now := time.Now().UTC()
	if err := store.Touch(ctx, "t1", created.ID, now); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	touched, _ := store.Get(ctx, "t1", created.ID)
	if touched.LastUsedAt == nil || !touched.LastUsedAt.Equal(now) {
		t.Fatalf("LastUsedAt = %v", touched.LastUsedAt)
	}

	revoked, err := store.Revoke(ctx, "t1", created.ID)
	if err != nil || revoked.Status != StatusRevoked {
		t.Fatalf("Revoke = %+v, %v", revoked, err)
	}
	if _, err := store.Revoke(ctx, "t1", "missing"); err != ErrNotFound {
		t.Fatalf("revoke missing error = %v", err)
	}
}

func TestValidateScopes(t *testing.T) {
	defaults, err := ValidateScopes(nil)
	if err != nil || len(defaults) != 2 {
		t.Fatalf("defaults = %v, %v", defaults, err)
	}
	scopes, err := ValidateScopes([]string{" search:use ", "search:use"})
	if err != nil || len(scopes) != 1 || scopes[0] != ScopeSearchUse {
		t.Fatalf("scopes = %v, %v", scopes, err)
	}
	if _, err := ValidateScopes([]string{"admin:all"}); err != ErrInvalidScope {
		t.Fatalf("invalid scope error = %v", err)
	}
}

func TestKeyHelpers(t *testing.T) {
	expiry := time.Now().Add(-time.Minute)
	key := Key{ID: "k1", AllowedKBIDs: []string{"kb-a"}, Scopes: []string{ScopeChatCompletions}}
	if !key.AllowsKB("kb-a") || key.AllowsKB("kb-b") {
		t.Fatal("allowed kb matching failed")
	}
	key.AllowedKBIDs = nil
	if !key.AllowsKB("kb-b") {
		t.Fatal("empty allow-list should grant all kbs")
	}
	key.ExpiresAt = &expiry
	if !key.Expired(time.Now()) {
		t.Fatal("expired key not detected")
	}

	p := key.Principal()
	if p.UserID != "apikey:k1" || !tenant.HasPermission(p, ScopeChatCompletions) {
		t.Fatalf("principal = %+v", p)
	}
	if !tenant.HasPermission(p, ScopeSearchUse) {
		t.Fatalf("chat:completions should imply search:use: %+v", p.Permissions)
	}
}
