package identity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const DefaultKnowledgeBaseID = "default"

// Knowledge-base scoped roles. They are stored verbatim in kb_permissions
// (see docs/schema_enterprise_v1_1_to_1_5.sql).
const (
	KBRoleAdmin  = "kb:admin"
	KBRoleEditor = "kb:editor"
	KBRoleViewer = "kb:viewer"

	KBPermissionSubjectUser = "user"
	KBPermissionSubjectRole = "role"

	KBStatusActive   = "ACTIVE"
	KBStatusArchived = "ARCHIVED"
	KBStatusDeleting = "DELETING"
)

var (
	ErrKnowledgeBaseNotFound   = errors.New("knowledge base not found")
	ErrKnowledgeBaseExists     = errors.New("knowledge base already exists")
	ErrKnowledgeBaseName       = errors.New("knowledge base name is required")
	ErrDefaultKnowledgeBase    = errors.New("default knowledge base cannot be deleted")
	ErrKnowledgeBaseRole       = errors.New("invalid knowledge base role")
	ErrKnowledgeBaseSubject    = errors.New("invalid knowledge base subject")
	ErrKnowledgeBaseReferenced = errors.New("knowledge base is referenced by other knowledge bases")
)

// KnowledgeBase is a tenant-scoped namespace for files, tasks and retrieval.
// Columns mirror the enterprise DDL (knowledge_bases). SourceKBID non-empty
// marks a reference namespace: vectors live in the source KB and are reused
// without re-uploading or re-embedding.
type KnowledgeBase struct {
	ID             string         `json:"id"`
	TenantID       string         `json:"tenant_id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	EmbeddingModel string         `json:"embedding_model"`
	ChunkStrategy  map[string]any `json:"chunk_strategy,omitempty"`
	SourceKBID     string         `json:"source_kb_id,omitempty"`
	Status         string         `json:"status"`
	CreatedBy      string         `json:"created_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// KnowledgeBasePermission binds a user or role to one knowledge base with a
// single kb:* role. Explicit user bindings take precedence over role bindings.
type KnowledgeBasePermission struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	KBID        string    `json:"kb_id"`
	SubjectType string    `json:"subject_type"`
	SubjectID   string    `json:"subject_id"`
	Permission  string    `json:"permission"`
	CreatedAt   time.Time `json:"created_at"`
}

func defaultChunkStrategy() map[string]any {
	return map[string]any{"type": "semantic", "chunk_size": 512, "overlap": 64}
}

func defaultKnowledgeBase(tenantID string) KnowledgeBase {
	now := time.Now()
	return KnowledgeBase{
		ID: DefaultKnowledgeBaseID, TenantID: tenantID, Name: "默认知识库",
		Description: "系统默认知识库", EmbeddingModel: "default",
		ChunkStrategy: defaultChunkStrategy(), Status: KBStatusActive,
		CreatedBy: "system", CreatedAt: now, UpdatedAt: now,
	}
}

// roleRank orders the three kb roles; unknown roles rank 0 (no access).
func roleRank(role string) int {
	switch role {
	case KBRoleAdmin:
		return 3
	case KBRoleEditor:
		return 2
	case KBRoleViewer:
		return 1
	default:
		return 0
	}
}

func strongerRole(a, b string) string {
	if roleRank(b) > roleRank(a) {
		return b
	}
	return a
}

// RoleAtLeast reports whether role grants at least the minimum knowledge-base
// role. An empty or unknown role never satisfies a requirement.
func RoleAtLeast(role, minimum string) bool {
	return roleRank(role) >= roleRank(minimum) && roleRank(role) > 0
}

func validKBRole(role string) bool { return roleRank(role) > 0 }

// KnowledgeBaseService owns tenant-scoped knowledge-base CRUD plus kb-scoped
// RBAC. Tenant identity is always taken from verified gRPC metadata.
type KnowledgeBaseService struct {
	pb.UnimplementedKnowledgeBaseServiceServer
	store Store
}

func NewKnowledgeBaseService(store Store) *KnowledgeBaseService {
	return &KnowledgeBaseService{store: store}
}

func (s *KnowledgeBaseService) CreateKnowledgeBase(ctx context.Context, req *pb.CreateKnowledgeBaseRequest) (*pb.KnowledgeBaseResponse, error) {
	p := tenant.FromIncoming(ctx)
	createdBy := p.UserID
	if createdBy == "" {
		createdBy = "system"
	}
	tenantID := tenantOf(ctx, "")
	sourceID := strings.TrimSpace(req.GetSourceKbId())
	if sourceID != "" {
		if err := s.validateReferenceSource(ctx, tenantID, sourceID); err != nil {
			return nil, err
		}
	}
	kb, err := s.store.CreateKnowledgeBase(ctx, KnowledgeBase{
		TenantID: tenantID, Name: req.GetName(), Description: req.GetDescription(),
		EmbeddingModel: "default", ChunkStrategy: defaultChunkStrategy(),
		SourceKBID: sourceID, Status: KBStatusActive, CreatedBy: createdBy,
	})
	if err != nil {
		return nil, toStatus(err)
	}
	// The creator becomes the first kb admin so the new namespace is manageable
	// even without a tenant-wide admin role.
	if p.UserID != "" {
		if _, err := s.store.UpsertKnowledgeBasePermission(ctx, KnowledgeBasePermission{
			TenantID: kb.TenantID, KBID: kb.ID, SubjectType: KBPermissionSubjectUser,
			SubjectID: p.UserID, Permission: KBRoleAdmin,
		}); err != nil {
			return nil, toStatus(err)
		}
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb, KBRoleAdmin)}, nil
}

// validateReferenceSource ensures a reference KB points at an owned KB the
// caller can actually read, and forbids chaining references.
func (s *KnowledgeBaseService) validateReferenceSource(ctx context.Context, tenantID, sourceID string) error {
	if sourceID == DefaultKnowledgeBaseID {
		if err := s.ensureDefault(ctx, tenantID); err != nil {
			return toStatus(err)
		}
	}
	src, ok, err := s.store.GetKnowledgeBase(ctx, tenantID, sourceID)
	if err != nil {
		return toStatus(err)
	}
	if !ok {
		return status.Errorf(codes.InvalidArgument, "source knowledge base %q not found", sourceID)
	}
	if src.SourceKBID != "" {
		return status.Error(codes.InvalidArgument, "cannot reference another reference knowledge base")
	}
	role, err := s.effectiveRole(ctx, tenantID, sourceID)
	if err != nil {
		return toStatus(err)
	}
	if !RoleAtLeast(role, KBRoleViewer) {
		return status.Error(codes.PermissionDenied, "read access to the source knowledge base is required")
	}
	return nil
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
	role, err := s.effectiveRole(ctx, tenantID, id)
	if err != nil {
		return nil, toStatus(err)
	}
	if role == "" {
		// Existence is not leaked to callers without access to this namespace.
		return nil, status.Error(codes.NotFound, ErrKnowledgeBaseNotFound.Error())
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb, role)}, nil
}

// ResolveKnowledgeBase returns routing metadata without a kb-role check. The
// gateway calls it only for authenticated API keys after enforcing their
// knowledge-base allow-list, because API keys are not identity users and
// therefore have no kb role to resolve.
func (s *KnowledgeBaseService) ResolveKnowledgeBase(ctx context.Context, req *pb.ResolveKnowledgeBaseRequest) (*pb.ResolveKnowledgeBaseResponse, error) {
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
	return &pb.ResolveKnowledgeBaseResponse{
		Id: kb.ID, Name: kb.Name, SourceKbId: kb.SourceKBID,
	}, nil
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
		role, err := s.effectiveRole(ctx, tenantID, kb.ID)
		if err != nil {
			return nil, toStatus(err)
		}
		if role == "" {
			continue // namespaces the caller cannot access stay invisible
		}
		out = append(out, knowledgeBaseToPB(kb, role))
	}
	return &pb.ListKnowledgeBasesResponse{KnowledgeBases: out}, nil
}

func (s *KnowledgeBaseService) UpdateKnowledgeBase(ctx context.Context, req *pb.UpdateKnowledgeBaseRequest) (*pb.KnowledgeBaseResponse, error) {
	id := strings.TrimSpace(req.GetId())
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "knowledge base id is required")
	}
	tenantID := tenantOf(ctx, "")
	if err := s.requireRole(ctx, tenantID, id, KBRoleAdmin); err != nil {
		return nil, err
	}
	kb, err := s.store.UpdateKnowledgeBase(ctx, KnowledgeBase{
		ID: id, TenantID: tenantID, Name: req.GetName(), Description: req.GetDescription(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.KnowledgeBaseResponse{KnowledgeBase: knowledgeBaseToPB(kb, KBRoleAdmin)}, nil
}

func (s *KnowledgeBaseService) DeleteKnowledgeBase(ctx context.Context, req *pb.DeleteKnowledgeBaseRequest) (*pb.DeleteKnowledgeBaseResponse, error) {
	tenantID := tenantOf(ctx, "")
	if err := s.requireRole(ctx, tenantID, req.GetId(), KBRoleAdmin); err != nil {
		return nil, err
	}
	items, err := s.store.ListKnowledgeBases(ctx, tenantID)
	if err != nil {
		return nil, toStatus(err)
	}
	for _, kb := range items {
		if kb.SourceKBID == req.GetId() {
			return nil, status.Errorf(
				codes.FailedPrecondition,
				"%s: %q still references it", ErrKnowledgeBaseReferenced.Error(), kb.Name,
			)
		}
	}
	if err := s.store.DeleteKnowledgeBase(ctx, tenantID, req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DeleteKnowledgeBaseResponse{Success: true}, nil
}

// ---------- kb-scoped permission management ----------

func (s *KnowledgeBaseService) GrantKnowledgeBasePermission(ctx context.Context, req *pb.GrantKnowledgeBasePermissionRequest) (*pb.KnowledgeBasePermissionResponse, error) {
	tenantID := tenantOf(ctx, "")
	subjectType := strings.TrimSpace(req.GetSubjectType())
	subjectID := strings.TrimSpace(req.GetSubjectId())
	role := strings.TrimSpace(req.GetPermission())
	if subjectType != KBPermissionSubjectUser && subjectType != KBPermissionSubjectRole {
		return nil, status.Error(codes.InvalidArgument, ErrKnowledgeBaseSubject.Error())
	}
	if subjectID == "" {
		return nil, status.Error(codes.InvalidArgument, "subject_id is required")
	}
	if !validKBRole(role) {
		return nil, status.Error(codes.InvalidArgument, ErrKnowledgeBaseRole.Error())
	}
	if err := s.requireRole(ctx, tenantID, req.GetKbId(), KBRoleAdmin); err != nil {
		return nil, err
	}
	switch subjectType {
	case KBPermissionSubjectRole:
		if _, ok, err := s.store.GetRole(ctx, subjectID); err != nil {
			return nil, toStatus(err)
		} else if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown role %q", subjectID)
		}
	case KBPermissionSubjectUser:
		if _, ok, err := s.store.GetUser(ctx, tenantID, subjectID); err != nil {
			return nil, toStatus(err)
		} else if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "unknown user %q", subjectID)
		}
	}
	perm, err := s.store.UpsertKnowledgeBasePermission(ctx, KnowledgeBasePermission{
		TenantID: tenantID, KBID: req.GetKbId(), SubjectType: subjectType,
		SubjectID: subjectID, Permission: role,
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.KnowledgeBasePermissionResponse{Permission: permissionToPB(perm)}, nil
}

func (s *KnowledgeBaseService) RevokeKnowledgeBasePermission(ctx context.Context, req *pb.RevokeKnowledgeBasePermissionRequest) (*pb.DeleteKnowledgeBaseResponse, error) {
	tenantID := tenantOf(ctx, "")
	if err := s.requireRole(ctx, tenantID, req.GetKbId(), KBRoleAdmin); err != nil {
		return nil, err
	}
	if err := s.store.DeleteKnowledgeBasePermission(ctx, tenantID, req.GetKbId(), req.GetSubjectType(), req.GetSubjectId()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DeleteKnowledgeBaseResponse{Success: true}, nil
}

func (s *KnowledgeBaseService) ListKnowledgeBasePermissions(ctx context.Context, req *pb.ListKnowledgeBasePermissionsRequest) (*pb.ListKnowledgeBasePermissionsResponse, error) {
	tenantID := tenantOf(ctx, "")
	if err := s.requireRole(ctx, tenantID, req.GetKbId(), KBRoleAdmin); err != nil {
		return nil, err
	}
	perms, err := s.store.ListKnowledgeBasePermissions(ctx, tenantID, req.GetKbId())
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*pb.KnowledgeBasePermissionInfo, 0, len(perms))
	for _, perm := range perms {
		out = append(out, permissionToPB(perm))
	}
	return &pb.ListKnowledgeBasePermissionsResponse{Permissions: out}, nil
}

// effectiveRole resolves the caller's role for one knowledge base:
// explicit user binding > role bindings > tenant admin > default-KB viewer.
func (s *KnowledgeBaseService) effectiveRole(ctx context.Context, tenantID, kbID string) (string, error) {
	p := tenant.FromIncoming(ctx)
	if slices.Contains(p.Roles, RoleAdmin) || slices.Contains(p.Permissions, Wildcard) {
		return KBRoleAdmin, nil
	}
	if p.UserID == "" {
		return "", nil
	}
	perms, err := s.store.ListKnowledgeBasePermissions(ctx, tenantID, kbID)
	if err != nil {
		return "", err
	}
	explicit := ""
	for _, perm := range perms {
		if perm.SubjectType == KBPermissionSubjectUser && perm.SubjectID == p.UserID {
			explicit = strongerRole(explicit, perm.Permission)
		}
	}
	if explicit != "" {
		return explicit, nil
	}
	roleBased := ""
	for _, role := range p.Roles {
		for _, perm := range perms {
			if perm.SubjectType == KBPermissionSubjectRole && perm.SubjectID == role {
				roleBased = strongerRole(roleBased, perm.Permission)
			}
		}
	}
	if roleBased != "" {
		return roleBased, nil
	}
	if kbID == DefaultKnowledgeBaseID {
		return KBRoleViewer, nil
	}
	return "", nil
}

// requireRole enforces a minimum kb role inside the service, so callers that
// bypass the gateway still cannot mutate or inspect permissions.
func (s *KnowledgeBaseService) requireRole(ctx context.Context, tenantID, kbID, minimum string) error {
	if kbID == "" {
		return status.Error(codes.InvalidArgument, "knowledge base id is required")
	}
	if _, ok, err := s.store.GetKnowledgeBase(ctx, tenantID, kbID); err != nil {
		return toStatus(err)
	} else if !ok {
		return status.Error(codes.NotFound, ErrKnowledgeBaseNotFound.Error())
	}
	role, err := s.effectiveRole(ctx, tenantID, kbID)
	if err != nil {
		return toStatus(err)
	}
	if roleRank(role) < roleRank(minimum) {
		return status.Errorf(codes.PermissionDenied, "%s required", minimum)
	}
	return nil
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

func knowledgeBaseToPB(kb KnowledgeBase, role string) *pb.KnowledgeBaseInfo {
	statusValue := kb.Status
	if statusValue == "" {
		statusValue = KBStatusActive
	}
	return &pb.KnowledgeBaseInfo{
		Id: kb.ID, TenantId: kb.TenantID, Name: kb.Name, Description: kb.Description,
		CreatedAt: kb.CreatedAt.Format(time.RFC3339), UpdatedAt: kb.UpdatedAt.Format(time.RFC3339),
		Role: role, Status: statusValue, CreatedBy: kb.CreatedBy, EmbeddingModel: kb.EmbeddingModel,
		SourceKbId: kb.SourceKBID, ReadOnly: kb.SourceKBID != "",
	}
}

func permissionToPB(perm KnowledgeBasePermission) *pb.KnowledgeBasePermissionInfo {
	return &pb.KnowledgeBasePermissionInfo{
		Id: perm.ID, TenantId: perm.TenantID, KbId: perm.KBID,
		SubjectType: perm.SubjectType, SubjectId: perm.SubjectID,
		Permission: perm.Permission, CreatedAt: perm.CreatedAt.Format(time.RFC3339),
	}
}

// ---------- MemoryStore implementation ----------

func kbNameKey(tenantID, name string) string { return tenantID + "\x1f" + name }

func (m *MemoryStore) seedDefaultKnowledgeBaseLocked(tenantID string) {
	key := userKey(tenantID, DefaultKnowledgeBaseID)
	if _, ok := m.kbs[key]; ok {
		return
	}
	kb := defaultKnowledgeBase(tenantID)
	m.kbs[key] = &kb
	m.kbNames[kbNameKey(tenantID, kb.Name)] = kb.ID
}

func (m *MemoryStore) CreateKnowledgeBase(_ context.Context, kb KnowledgeBase) (KnowledgeBase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kb.TenantID = tenantOr(kb.TenantID)
	kb.Name = strings.TrimSpace(kb.Name)
	if kb.Name == "" {
		return KnowledgeBase{}, ErrKnowledgeBaseName
	}
	name := kbNameKey(kb.TenantID, kb.Name)
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
	if kb.Status == "" {
		kb.Status = KBStatusActive
	}
	if kb.EmbeddingModel == "" {
		kb.EmbeddingModel = "default"
	}
	if kb.ChunkStrategy == nil {
		kb.ChunkStrategy = defaultChunkStrategy()
	}
	if kb.CreatedBy == "" {
		kb.CreatedBy = "system"
	}
	kb.UpdatedAt = now
	m.kbs[key] = &kb
	m.kbNames[name] = kb.ID
	return copyKnowledgeBase(kb), nil
}

func (m *MemoryStore) GetKnowledgeBase(_ context.Context, tenantID, id string) (KnowledgeBase, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	kb, ok := m.kbs[userKey(tenantOr(tenantID), id)]
	if !ok {
		return KnowledgeBase{}, false, nil
	}
	return copyKnowledgeBase(*kb), true, nil
}

func (m *MemoryStore) ListKnowledgeBases(_ context.Context, tenantID string) ([]KnowledgeBase, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenantID = tenantOr(tenantID)
	out := make([]KnowledgeBase, 0)
	for _, kb := range m.kbs {
		if kb.TenantID == tenantID {
			out = append(out, copyKnowledgeBase(*kb))
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
	if name := strings.TrimSpace(patch.Name); name != "" && name != existing.Name {
		indexKey := kbNameKey(tenantID, name)
		if _, exists := m.kbNames[indexKey]; exists {
			return KnowledgeBase{}, ErrKnowledgeBaseExists
		}
		delete(m.kbNames, kbNameKey(tenantID, existing.Name))
		existing.Name = name
		m.kbNames[indexKey] = existing.ID
	}
	existing.Description = patch.Description
	existing.UpdatedAt = time.Now()
	return copyKnowledgeBase(*existing), nil
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
	delete(m.kbNames, kbNameKey(tenantID, kb.Name))
	delete(m.kbs, key)
	for permKey, perm := range m.kbPerms {
		if perm.TenantID == tenantID && perm.KBID == id {
			delete(m.kbPerms, permKey)
		}
	}
	return nil
}

func permissionKey(tenantID, kbID, subjectType, subjectID string) string {
	return tenantID + "\x1f" + kbID + "\x1f" + subjectType + "\x1f" + subjectID
}

func (m *MemoryStore) UpsertKnowledgeBasePermission(_ context.Context, perm KnowledgeBasePermission) (KnowledgeBasePermission, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	perm.TenantID = tenantOr(perm.TenantID)
	key := permissionKey(perm.TenantID, perm.KBID, perm.SubjectType, perm.SubjectID)
	if existing, ok := m.kbPerms[key]; ok {
		existing.Permission = perm.Permission
		return *existing, nil
	}
	perm.ID = uuid.NewString()
	perm.CreatedAt = time.Now()
	copied := perm
	m.kbPerms[key] = &copied
	return copied, nil
}

func (m *MemoryStore) DeleteKnowledgeBasePermission(_ context.Context, tenantID, kbID, subjectType, subjectID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.kbPerms, permissionKey(tenantOr(tenantID), kbID, subjectType, subjectID))
	return nil
}

func (m *MemoryStore) ListKnowledgeBasePermissions(_ context.Context, tenantID, kbID string) ([]KnowledgeBasePermission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenantID = tenantOr(tenantID)
	out := make([]KnowledgeBasePermission, 0)
	for _, perm := range m.kbPerms {
		if perm.TenantID == tenantID && perm.KBID == kbID {
			out = append(out, *perm)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubjectType != out[j].SubjectType {
			return out[i].SubjectType < out[j].SubjectType
		}
		return out[i].SubjectID < out[j].SubjectID
	})
	return out, nil
}

func copyKnowledgeBase(kb KnowledgeBase) KnowledgeBase {
	if kb.ChunkStrategy != nil {
		strategy := make(map[string]any, len(kb.ChunkStrategy))
		for key, value := range kb.ChunkStrategy {
			strategy[key] = value
		}
		kb.ChunkStrategy = strategy
	}
	return kb
}

// marshalChunkStrategy renders the JSONB column value.
func marshalChunkStrategy(strategy map[string]any) ([]byte, error) {
	if len(strategy) == 0 {
		strategy = defaultChunkStrategy()
	}
	return json.Marshal(strategy)
}
