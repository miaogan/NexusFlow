package revocation

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestMemoryBlacklistRevokeAndExpire(t *testing.T) {
	b := NewMemoryBlacklist()
	ctx := context.Background()

	if Check(ctx, b, "") {
		t.Error("empty token id must not be treated as revoked")
	}
	if err := b.Revoke(ctx, "", time.Minute); err != nil {
		t.Fatalf("revoke empty id: %v", err)
	}
	if Check(ctx, b, "tok-1") {
		t.Error("token revoked before Revoke was called")
	}
	if err := b.Revoke(ctx, "tok-1", time.Minute); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if !Check(ctx, b, "tok-1") {
		t.Error("token not revoked after Revoke")
	}

	// Entries expire with the token, so the list self-prunes.
	b.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if Check(ctx, b, "tok-1") {
		t.Error("expired revocation still reported as active")
	}
}

func TestMemoryBlacklistIgnoresExpiredTTL(t *testing.T) {
	b := NewMemoryBlacklist()
	if err := b.Revoke(context.Background(), "tok-2", 0); err != nil {
		t.Fatalf("revoke with zero ttl: %v", err)
	}
	if Check(context.Background(), b, "tok-2") {
		t.Error("already-expired token should not be stored")
	}
}

func TestNewSelectsBackend(t *testing.T) {
	for _, driver := range []string{"", "memory", "unknown"} {
		b, err := New(driver, "")
		if err != nil {
			t.Fatalf("New(%q): %v", driver, err)
		}
		if _, ok := b.(*MemoryBlacklist); !ok {
			t.Errorf("New(%q) = %T, want *MemoryBlacklist", driver, b)
		}
	}

	// An unreachable Redis degrades to the in-memory list and reports why.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	b, err := New("redis", addr)
	if err == nil {
		t.Fatal("expected connection error for unreachable redis")
	}
	if _, ok := b.(*MemoryBlacklist); !ok {
		t.Errorf("fallback = %T, want *MemoryBlacklist", b)
	}
}
