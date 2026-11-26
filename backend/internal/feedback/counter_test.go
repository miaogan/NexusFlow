package feedback

import (
	"context"
	"testing"
)

func TestNegativeCounterAggregatesCitedChunks(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	_ = mustUpsert(t, store, Entry{
		TenantID: "t1", UserID: "u1", SubjectID: "m1", Name: "user.rating", Rating: -1,
		CitedChunkIDs: []string{"c1", "c2"},
	})
	_ = mustUpsert(t, store, Entry{
		TenantID: "t1", UserID: "u2", SubjectID: "m2", Name: "user.rating", Rating: -1,
		CitedChunkIDs: []string{"c1", "c1"}, // duplicate ids count once per record
	})
	_ = mustUpsert(t, store, Entry{
		TenantID: "t1", UserID: "u3", SubjectID: "m3", Name: "user.rating", Rating: 1,
		CitedChunkIDs: []string{"c1"},
	})
	_ = mustUpsert(t, store, Entry{
		TenantID: "t2", UserID: "u1", SubjectID: "m1", Name: "user.rating", Rating: -1,
		CitedChunkIDs: []string{"c1"},
	})

	counts, err := NewNegativeCounter(store).NegativeCounts(ctx, "t1", []string{"c1", "c2", "c3"})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts["c1"] != 2 {
		t.Errorf("c1 negative count = %d, want 2", counts["c1"])
	}
	if counts["c2"] != 1 {
		t.Errorf("c2 negative count = %d, want 1", counts["c2"])
	}
	if _, ok := counts["c3"]; ok {
		t.Errorf("c3 has no feedback but got %d", counts["c3"])
	}
}

func TestNegativeCounterIgnoresUnrequestedChunks(t *testing.T) {
	store := NewMemoryStore()
	_ = mustUpsert(t, store, Entry{
		TenantID: "t1", UserID: "u1", SubjectID: "m1", Name: "user.rating", Rating: -1,
		CitedChunkIDs: []string{"other"},
	})

	counts, err := NewNegativeCounter(store).NegativeCounts(context.Background(), "t1", []string{"c1"})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if len(counts) != 0 {
		t.Errorf("counts = %v, want empty", counts)
	}
}
