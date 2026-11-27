package identity

import (
	"context"
	"errors"
	"testing"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestKnowledgeBaseMemoryStoreLifecycle(t *testing.T) {
	store := seededStore(t)
	ctx := context.Background()
	if kb, ok, err := store.GetKnowledgeBase(ctx, "default", DefaultKnowledgeBaseID); err != nil || !ok || kb.Name == "" {
		t.Fatalf("default knowledge base: kb=%+v ok=%v err=%v", kb, ok, err)
	}

	created, err := store.CreateKnowledgeBase(ctx, KnowledgeBase{TenantID: "default", Name: "研发资料", Description: "旧描述"})
	if err != nil || created.ID == "" {
		t.Fatalf("create: kb=%+v err=%v", created, err)
	}
	if _, err := store.CreateKnowledgeBase(ctx, KnowledgeBase{TenantID: "default", Name: "研发资料"}); !errors.Is(err, ErrKnowledgeBaseExists) {
		t.Fatalf("case-insensitive duplicate err=%v", err)
	}
	if _, ok, _ := store.GetKnowledgeBase(ctx, "other", created.ID); ok {
		t.Fatal("cross-tenant knowledge base lookup must not succeed")
	}

	updated, err := store.UpdateKnowledgeBase(ctx, KnowledgeBase{
		TenantID: "default", ID: created.ID, Name: "研发知识库", Description: "新描述",
	})
	if err != nil || updated.Name != "研发知识库" || updated.Description != "新描述" {
		t.Fatalf("update: kb=%+v err=%v", updated, err)
	}
	items, err := store.ListKnowledgeBases(ctx, "default")
	if err != nil || len(items) != 2 {
		t.Fatalf("list: len=%d err=%v, want 2", len(items), err)
	}
	if err := store.DeleteKnowledgeBase(ctx, "default", DefaultKnowledgeBaseID); !errors.Is(err, ErrDefaultKnowledgeBase) {
		t.Fatalf("delete default err=%v", err)
	}
	if err := store.DeleteKnowledgeBase(ctx, "default", created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := store.GetKnowledgeBase(ctx, "default", created.ID); ok {
		t.Fatal("deleted knowledge base still exists")
	}
}

func TestKnowledgeBaseServiceTenantIsolationAndLazyDefault(t *testing.T) {
	svc := NewKnowledgeBaseService(NewMemoryStore())
	ctxA := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant-id", "tenant-a"))
	ctxB := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant-id", "tenant-b"))

	if _, err := svc.GetKnowledgeBase(ctxA, &pb.GetKnowledgeBaseRequest{Id: DefaultKnowledgeBaseID}); err != nil {
		t.Fatalf("lazy default: %v", err)
	}
	created, err := svc.CreateKnowledgeBase(ctxA, &pb.CreateKnowledgeBaseRequest{Name: "A 私有库"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.GetKnowledgeBase(ctxB, &pb.GetKnowledgeBaseRequest{Id: created.KnowledgeBase.Id}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-tenant get code=%v, want NotFound", status.Code(err))
	}
	listB, err := svc.ListKnowledgeBases(ctxB, &pb.ListKnowledgeBasesRequest{})
	if err != nil || len(listB.GetKnowledgeBases()) != 1 || listB.GetKnowledgeBases()[0].TenantId != "tenant-b" {
		t.Fatalf("tenant B list=%+v err=%v", listB, err)
	}
}
