package metering

import (
	"context"
	"errors"
	"testing"
)

func TestMemoryStoreAggregatesAndFloors(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	if err := store.AddStorage(ctx, "t1", "kb1", 100, 1); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := store.AddStorage(ctx, "t1", "kb2", 50, 1); err != nil {
		t.Fatalf("add kb2: %v", err)
	}
	if err := store.AddQuery(ctx, "t1", 3); err != nil {
		t.Fatalf("add query: %v", err)
	}
	// Deleting a file subtracts; counters never go negative.
	if err := store.AddStorage(ctx, "t1", "kb1", -100, -1); err != nil {
		t.Fatalf("subtract: %v", err)
	}
	if err := store.AddStorage(ctx, "t1", "kb1", -999, -1); err != nil {
		t.Fatalf("over-subtract: %v", err)
	}

	usage, err := store.Usage(ctx, "t1")
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.StorageBytes != 50 || usage.FileCount != 1 || usage.QueryCount != 3 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(usage.KnowledgeBases) != 2 || usage.KnowledgeBases[0].KBID != "kb2" {
		t.Fatalf("kb breakdown = %+v", usage.KnowledgeBases)
	}
	empty, err := store.Usage(ctx, "missing")
	if err != nil || empty.StorageBytes != 0 || len(empty.KnowledgeBases) != 0 {
		t.Fatalf("empty usage = %+v err=%v", empty, err)
	}
}

func TestQuotaChecks(t *testing.T) {
	quota := Quota{MaxStorageBytes: 100, MaxFiles: 2, MaxQueries: 1}
	usage := Usage{StorageBytes: 90, FileCount: 1, QueryCount: 0}

	if err := quota.CheckUpload(usage, 10); err != nil {
		t.Fatalf("exactly at quota should pass: %v", err)
	}
	if err := quota.CheckUpload(usage, 11); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("storage overflow err = %v", err)
	}
	if err := quota.CheckUpload(Usage{FileCount: 2}, 1); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("file overflow err = %v", err)
	}
	if err := quota.CheckQuery(usage); err != nil {
		t.Fatalf("first query should pass: %v", err)
	}
	if err := quota.CheckQuery(Usage{QueryCount: 1}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("query overflow err = %v", err)
	}

	unlimited := Quota{}
	if err := unlimited.CheckUpload(Usage{StorageBytes: 1 << 40, FileCount: 1 << 20}, 1<<40); err != nil {
		t.Fatalf("unlimited quota must pass: %v", err)
	}
	if got := unlimited.Remaining(usage); got.MaxStorageBytes != -1 || got.MaxFiles != -1 || got.MaxQueries != -1 {
		t.Fatalf("unlimited remaining = %+v", got)
	}
	remaining := quota.Remaining(usage)
	if remaining.MaxStorageBytes != 10 || remaining.MaxFiles != 1 || remaining.MaxQueries != 1 {
		t.Fatalf("remaining = %+v", remaining)
	}
}
