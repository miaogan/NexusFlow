package query

import "testing"

func TestFuseReciprocalRank(t *testing.T) {
	list1 := []Chunk{{ID: "a", Content: "A"}, {ID: "b", Content: "B"}}
	list2 := []Chunk{{ID: "b", Content: "B"}, {ID: "c", Content: "C"}}

	fused := Fuse([][]Chunk{list1, list2}, 60)
	if len(fused) != 3 {
		t.Fatalf("expected 3 unique chunks, got %d", len(fused))
	}
	if fused[0].ID != "b" {
		t.Fatalf("expected b first (appears in both lists), got %s", fused[0].ID)
	}
	if fused[1].ID != "a" || fused[2].ID != "c" {
		t.Fatalf("unexpected order: %s, %s", fused[1].ID, fused[2].ID)
	}
}

func TestFuseDedupesWithinList(t *testing.T) {
	list := []Chunk{{ID: "a", Content: "A"}, {ID: "a", Content: "A"}}
	fused := Fuse([][]Chunk{list}, 60)
	if len(fused) != 1 {
		t.Fatalf("expected duplicate removed, got %d", len(fused))
	}
}

func TestFuseFallsBackToContentHash(t *testing.T) {
	list1 := []Chunk{{Content: "same content"}}
	list2 := []Chunk{{Content: "same content"}}
	fused := Fuse([][]Chunk{list1, list2}, 60)
	if len(fused) != 1 {
		t.Fatalf("expected content-hash dedupe, got %d", len(fused))
	}
}
