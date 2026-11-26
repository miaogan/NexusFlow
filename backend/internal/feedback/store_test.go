package feedback

import (
	"context"
	"testing"
)

func TestMemoryStoreUpsertKeepsLatestRating(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	entry := Entry{
		TenantID:  "t1",
		UserID:    "u1",
		SubjectID: "msg-1",
		Name:      "user.rating",
		Rating:    -1,
	}

	first, err := store.Upsert(ctx, entry)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	entry.Rating = 1
	second, err := store.Upsert(ctx, entry)
	if err != nil {
		t.Fatalf("upsert again: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same id, got %q and %q", first.ID, second.ID)
	}

	items, err := store.List(ctx, Filter{TenantID: "t1"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].Rating != 1 {
		t.Fatalf("expected one latest entry, got %+v", items)
	}
}

func TestMemoryStoreFiltersByRatingAndTenant(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	negative := -1.0
	_ = mustUpsert(t, store, Entry{TenantID: "t1", UserID: "u1", SubjectID: "a", Name: "user.rating", Rating: -1})
	_ = mustUpsert(t, store, Entry{TenantID: "t1", UserID: "u1", SubjectID: "b", Name: "user.rating", Rating: 1})
	_ = mustUpsert(t, store, Entry{TenantID: "t2", UserID: "u1", SubjectID: "c", Name: "user.rating", Rating: -1})

	items, err := store.List(ctx, Filter{TenantID: "t1", Rating: &negative})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 1 || items[0].SubjectID != "a" {
		t.Fatalf("unexpected filtered entries: %+v", items)
	}
}

func mustUpsert(t *testing.T, store Store, e Entry) Entry {
	t.Helper()
	out, err := store.Upsert(context.Background(), e)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return out
}
