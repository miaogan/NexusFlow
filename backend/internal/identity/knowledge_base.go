package identity

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const DefaultKnowledgeBaseID = "default"

var (
	ErrKnowledgeBaseNotFound = errors.New("knowledge base not found")
	ErrKnowledgeBaseExists   = errors.New("knowledge base already exists")
	ErrKnowledgeBaseName     = errors.New("knowledge base name is required")
	ErrDefaultKnowledgeBase  = errors.New("default knowledge base cannot be deleted")
)

// KnowledgeBase is a tenant-scoped namespace for files, tasks and retrieval.
type KnowledgeBase struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func defaultKnowledgeBase(tenantID string) KnowledgeBase {
	now := time.Now()
	return KnowledgeBase{
		ID: DefaultKnowledgeBaseID, TenantID: tenantID, Name: "默认知识库",
		Description: "系统默认知识库", CreatedAt: now, UpdatedAt: now,
	}
}

// KnowledgeBaseService owns tenant-scoped knowledge-base CRUD. Tenant identity
// is always taken from verified gRPC metadata, never from a request field.
type KnowledgeBaseService struct {
	pb.UnimplementedKnowledgeBaseServiceServer
	store Store
}

func NewKnowledgeBaseService(store Store) *KnowledgeBaseService {
	return &KnowledgeBaseService{store: store}
}

func (s *KnowledgeBaseService) CreateKnowledgeBase(ctx context.Context, req *pb.CreateKnowledgeBaseRequest) (*pb.KnowledgeBaseResponse, error) {
	kb, err := s.store.CreateKnowledgeBase(ctx, KnowledgeBase{
		TenantID: tenantOf(ctx, ""), Name: req.GetName(), Description: req.GetDescription(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb)}, nil
}

func (s *KnowledgeBaseService) GetKnowledgeBase(ctx context.Context, req *pb.GetKnowledgeBaseRequest) (*pb.KnowledgeBaseResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "knowledge base id is required")
	}
	tenantID := tenantOf(ctx, "")
	if id == DefaultKnowledgeBaseID {
		if err := s.ensureDefault(ctx, tenantID); err != nil {
			return nil, toStatus(err)
		}
	}
	kb, ok, err := s.store.GetKnowledgeBase(ctx, tenantID, id)
	if err != nil {
		return nil, toStatus(err)
	}
	if !ok {
		return nil, status.Error(codes.NotFound, ErrKnowledgeBaseNotFound.Error())
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb)}, nil
}

func (s *KnowledgeBaseService) ListKnowledgeBases(ctx context.Context, _ *pb.ListKnowledgeBasesRequest) (*pb.ListKnowledgeBasesResponse, error) {
	tenantID := tenantOf(ctx, "")
	if err := s.ensureDefault(ctx, tenantID); err != nil {
		return nil, toStatus(err)
	}
	items, err := s.store.ListKnowledgeBases(ctx, tenantID)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*pb.KnowledgeBaseInfo, 0, len(items))
	for _, kb := range items {
		out = append(out, knowledgeBaseToPB(kb))
	}
	return &pb.ListKnowledgeBasesResponse{KnowledgeBases: out}, nil
}

func (s *KnowledgeBaseService) UpdateKnowledgeBase(ctx context.Context, req *pb.UpdateKnowledgeBaseRequest) (*pb.KnowledgeBaseResponse, error) {
	if strings.TrimSpace(req.GetId()) == "" {
		return nil, status.Error(codes.InvalidArgument, "knowledge base id is required")
	}
	kb, err := s.store.UpdateKnowledgeBase(ctx, KnowledgeBase{
		ID: req.GetId(), TenantID: tenantOf(ctx, ""), Name: req.GetName(), Description: req.GetDescription(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb)}, nil
}

func (s *KnowledgeBaseService) DeleteKnowledgeBase(ctx context.Context, req *pb.DeleteKnowledgeBaseRequest) (*pb.DeleteKnowledgeBaseResponse, error) {
	if err := s.store.DeleteKnowledgeBase(ctx, tenantOf(ctx, ""), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DeleteKnowledgeBaseResponse{Success: true}, nil
}

func (s *KnowledgeBaseService) ensureDefault(ctx context.Context, tenantID string) error {
	_, err := s.store.CreateKnowledgeBase(ctx, defaultKnowledgeBase(tenantID))
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrKnowledgeBaseExists) {
		return err
	}
	_, ok, err := s.store.GetKnowledgeBase(ctx, tenantID, DefaultKnowledgeBaseID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	return nil
}

func knowledgeBaseToPB(kb KnowledgeBase) *pb.KnowledgeBaseInfo {
	return &pb.KnowledgeBaseInfo{
		Id: kb.ID, TenantId: kb.TenantID, Name: kb.Name, Description: kb.Description,
		CreatedAt: kb.CreatedAt.Format(time.RFC3339), UpdatedAt: kb.UpdatedAt.Format(time.RFC3339),
	}
}

func (m *MemoryStore) seedDefaultKnowledgeBaseLocked(tenantID string) {
	key := userKey(tenantID, DefaultKnowledgeBaseID)
	if _, ok := m.kbs[key]; ok {
		return
	}
	kb := defaultKnowledgeBase(tenantID)
	m.kbs[key] = &kb
	m.kbNames[nameKey(tenantID, kb.Name)] = kb.ID
}

func (m *MemoryStore) CreateKnowledgeBase(_ context.Context, kb KnowledgeBase) (KnowledgeBase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kb.TenantID = tenantOr(kb.TenantID)
	kb.Name = strings.TrimSpace(kb.Name)
	if kb.Name == "" {
		return KnowledgeBase{}, ErrKnowledgeBaseName
	}
	name := nameKey(kb.TenantID, kb.Name)
	if _, exists := m.kbNames[name]; exists {
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	if kb.ID == "" {
		kb.ID = uuid.NewString()
	}
	key := userKey(kb.TenantID, kb.ID)
	if _, exists := m.kbs[key]; exists {
		return KnowledgeBase{}, ErrKnowledgeBaseExists
	}
	now := time.Now()
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	kb.UpdatedAt = now
	m.kbs[key] = &kb
	m.kbNames[name] = kb.ID
	return kb, nil
}

func (m *MemoryStore) GetKnowledgeBase(_ context.Context, tenantID, id string) (KnowledgeBase, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	kb, ok := m.kbs[userKey(tenantOr(tenantID), id)]
	if !ok {
		return KnowledgeBase{}, false, nil
	}
	return *kb, true, nil
}

func (m *MemoryStore) ListKnowledgeBases(_ context.Context, tenantID string) ([]KnowledgeBase, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenantID = tenantOr(tenantID)
	out := make([]KnowledgeBase, 0)
	for _, kb := range m.kbs {
		if kb.TenantID == tenantID {
			out = append(out, *kb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryStore) UpdateKnowledgeBase(_ context.Context, patch KnowledgeBase) (KnowledgeBase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	tenantID := tenantOr(patch.TenantID)
	key := userKey(tenantID, patch.ID)
	existing, ok := m.kbs[key]
	if !ok {
		return KnowledgeBase{}, ErrKnowledgeBaseNotFound
	}
	if name := strings.TrimSpace(patch.Name); name != "" && !strings.EqualFold(name, existing.Name) {
		indexKey := nameKey(tenantID, name)
		if _, exists := m.kbNames[indexKey]; exists {
			return KnowledgeBase{}, ErrKnowledgeBaseExists
		}
		delete(m.kbNames, nameKey(tenantID, existing.Name))
		existing.Name = name
		m.kbNames[indexKey] = existing.ID
	} else if name != "" {
		existing.Name = name
	}
	existing.Description = patch.Description
	existing.UpdatedAt = time.Now()
	return *existing, nil
}

func (m *MemoryStore) DeleteKnowledgeBase(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == DefaultKnowledgeBaseID {
		return ErrDefaultKnowledgeBase
	}
	tenantID = tenantOr(tenantID)
	key := userKey(tenantID, id)
	kb, ok := m.kbs[key]
	if !ok {
		return ErrKnowledgeBaseNotFound
	}
	delete(m.kbNames, nameKey(tenantID, kb.Name))
	delete(m.kbs, key)
	return nil
}
