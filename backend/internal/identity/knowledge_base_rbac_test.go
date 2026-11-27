package identity

import (
	"context"
	"strings"
	"testing"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func kbCtx(tenantID, userID string, roles ...string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-tenant-id", tenantID,
		"x-user-id", userID,
		"x-roles", strings.Join(roles, ","),
	))
}

func createKBUser(t *testing.T, store *MemoryStore, username string, roles ...string) User {
	t.Helper()
	hash, err := HashPassword("pw123456")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := store.CreateUser(context.Background(), User{
		TenantID: "default", Username: username, PasswordHash: hash, Roles: roles,
	})
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

func TestKnowledgeBaseRoleResolution(t *testing.T) {
	store := seededStore(t)
	svc := NewKnowledgeBaseService(store)
	owner := createKBUser(t, store, "kbowner", RoleEditor)
	bob := createKBUser(t, store, "bob", RoleViewer)
	carol := createKBUser(t, store, "carol", RoleEditor)

	created, err := svc.CreateKnowledgeBase(kbCtx("default", owner.ID, RoleEditor),
		&pb.CreateKnowledgeBaseRequest{Name: "RBAC 库"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kbID := created.GetKnowledgeBase().GetId()
	if created.GetKnowledgeBase().GetRole() != KBRoleAdmin {
		t.Fatalf("creator role = %q, want %s", created.GetKnowledgeBase().GetRole(), KBRoleAdmin)
	}

	// Unbound users cannot see or open the namespace.
	list, err := svc.ListKnowledgeBases(kbCtx("default", bob.ID, RoleViewer), &pb.ListKnowledgeBasesRequest{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, kb := range list.GetKnowledgeBases() {
		if kb.GetId() == kbID {
			t.Fatal("unbound user must not see the namespace")
		}
	}
	if _, err := svc.GetKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.GetKnowledgeBaseRequest{Id: kbID}); status.Code(err) != codes.NotFound {
		t.Fatalf("unbound get code = %v, want NotFound", status.Code(err))
	}

	// Grant viewer, then role-based editor; explicit user binding wins.
	if _, err := svc.GrantKnowledgeBasePermission(kbCtx("default", owner.ID, RoleEditor), &pb.GrantKnowledgeBasePermissionRequest{
		KbId: kbID, SubjectType: KBPermissionSubjectUser, SubjectId: bob.ID, Permission: KBRoleViewer,
	}); err != nil {
		t.Fatalf("grant viewer: %v", err)
	}
	got, err := svc.GetKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.GetKnowledgeBaseRequest{Id: kbID})
	if err != nil || got.GetKnowledgeBase().GetRole() != KBRoleViewer {
		t.Fatalf("viewer role = %q err=%v", got.GetKnowledgeBase().GetRole(), err)
	}
	if _, err := svc.GrantKnowledgeBasePermission(kbCtx("default", owner.ID, RoleEditor), &pb.GrantKnowledgeBasePermissionRequest{
		KbId: kbID, SubjectType: KBPermissionSubjectRole, SubjectId: RoleEditor, Permission: KBRoleAdmin,
	}); err != nil {
		t.Fatalf("grant role admin: %v", err)
	}
	// Carol inherits admin through her global editor role.
	carolView, err := svc.GetKnowledgeBase(kbCtx("default", carol.ID, RoleEditor), &pb.GetKnowledgeBaseRequest{Id: kbID})
	if err != nil || carolView.GetKnowledgeBase().GetRole() != KBRoleAdmin {
		t.Fatalf("role binding = %q err=%v, want kb:admin", carolView.GetKnowledgeBase().GetRole(), err)
	}
	// Bob keeps his explicit viewer binding instead of inheriting role admin.
	bobView, err := svc.GetKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.GetKnowledgeBaseRequest{Id: kbID})
	if err != nil || bobView.GetKnowledgeBase().GetRole() != KBRoleViewer {
		t.Fatalf("explicit binding = %q err=%v, want kb:viewer", bobView.GetKnowledgeBase().GetRole(), err)
	}

	// viewer/editor cannot mutate the namespace; admin can.
	if _, err := svc.UpdateKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.UpdateKnowledgeBaseRequest{Id: kbID, Name: "x"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer update code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := svc.UpdateKnowledgeBase(kbCtx("default", owner.ID, RoleEditor), &pb.UpdateKnowledgeBaseRequest{Id: kbID, Name: "RBAC 库 v2"}); err != nil {
		t.Fatalf("admin update: %v", err)
	}
	if _, err := svc.RevokeKnowledgeBasePermission(kbCtx("default", owner.ID, RoleEditor), &pb.RevokeKnowledgeBasePermissionRequest{
		KbId: kbID, SubjectType: KBPermissionSubjectUser, SubjectId: bob.ID,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.GetKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.GetKnowledgeBaseRequest{Id: kbID}); status.Code(err) != codes.NotFound {
		t.Fatalf("after revoke code = %v, want NotFound", status.Code(err))
	}

	// tenant admin overrides missing bindings; default KB is viewer for members.
	admin, ok, err := store.GetUserByUsername(context.Background(), "default", "admin")
	if err != nil || !ok {
		t.Fatalf("admin lookup: ok=%v err=%v", ok, err)
	}
	adminView, err := svc.GetKnowledgeBase(kbCtx("default", admin.ID, RoleAdmin), &pb.GetKnowledgeBaseRequest{Id: kbID})
	if err != nil || adminView.GetKnowledgeBase().GetRole() != KBRoleAdmin {
		t.Fatalf("tenant admin role = %q err=%v", adminView.GetKnowledgeBase().GetRole(), err)
	}
	def, err := svc.GetKnowledgeBase(kbCtx("default", bob.ID, RoleViewer), &pb.GetKnowledgeBaseRequest{Id: DefaultKnowledgeBaseID})
	if err != nil || def.GetKnowledgeBase().GetRole() != KBRoleViewer {
		t.Fatalf("default kb role = %q err=%v", def.GetKnowledgeBase().GetRole(), err)
	}
}

func TestKnowledgeBasePermissionValidation(t *testing.T) {
	store := seededStore(t)
	svc := NewKnowledgeBaseService(store)
	owner := createKBUser(t, store, "owner2", RoleEditor)
	ctx := kbCtx("default", owner.ID, RoleEditor)
	created, err := svc.CreateKnowledgeBase(ctx, &pb.CreateKnowledgeBaseRequest{Name: "校验库"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	kbID := created.GetKnowledgeBase().GetId()

	cases := []struct {
		name string
		req  *pb.GrantKnowledgeBasePermissionRequest
	}{
		{"bad subject type", &pb.GrantKnowledgeBasePermissionRequest{KbId: kbID, SubjectType: "group", SubjectId: "x", Permission: KBRoleViewer}},
		{"bad role", &pb.GrantKnowledgeBasePermissionRequest{KbId: kbID, SubjectType: KBPermissionSubjectUser, SubjectId: owner.ID, Permission: "kb:owner"}},
		{"unknown user", &pb.GrantKnowledgeBasePermissionRequest{KbId: kbID, SubjectType: KBPermissionSubjectUser, SubjectId: "missing", Permission: KBRoleViewer}},
		{"unknown role", &pb.GrantKnowledgeBasePermissionRequest{KbId: kbID, SubjectType: KBPermissionSubjectRole, SubjectId: "ghost", Permission: KBRoleViewer}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.GrantKnowledgeBasePermission(ctx, tc.req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
			}
		})
	}
	if _, err := svc.ListKnowledgeBasePermissions(kbCtx("default", owner.ID, RoleEditor), &pb.ListKnowledgeBasePermissionsRequest{KbId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatalf("list missing kb code = %v, want NotFound", status.Code(err))
	}
}
