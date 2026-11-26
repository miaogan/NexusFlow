package identity

import "testing"

func TestNewStorePostgresRequiresDSN(t *testing.T) {
	// The postgres driver must fail fast on an empty DSN instead of silently
	// falling back to an in-memory store.
	if _, err := NewStore("postgres", "", ""); err == nil {
		t.Fatal("expected an error for empty postgres DSN")
	}
}

func TestNewStoreDefaultsToMemory(t *testing.T) {
	store, err := NewStore("", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := store.(*MemoryStore); !ok {
		t.Fatalf("expected MemoryStore, got %T", store)
	}
}
