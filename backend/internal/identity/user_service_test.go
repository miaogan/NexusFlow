package identity

import (
	"context"
	"testing"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newUserSvc(t *testing.T) *UserService {
	t.Helper()
	return NewUserService(seededStore(t))
}

func TestUserServiceCRUD(t *testing.T) {
	svc := newUserSvc(t)
	ctx := context.Background()

	created, err := svc.CreateUser(ctx, &pb.CreateUserRequest{
		TenantId: "default", Username: "bob", Password: "pw123456", Email: "bob@ex.com",
		Roles: []string{RoleEditor},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.User.Roles[0] != RoleEditor {
		t.Errorf("roles = %v, want editor", created.User.Roles)
	}

	got, err := svc.GetUser(ctx, &pb.GetUserRequest{TenantId: "default", Id: created.User.Id})
	if err != nil || got.User.Username != "bob" {
		t.Fatalf("get: %v %v", got, err)
	}

	list, err := svc.ListUsers(ctx, &pb.ListUsersRequest{TenantId: "default"})
	if err != nil || list.Total != 2 { // admin + bob
		t.Fatalf("list total = %d err=%v, want 2", list.GetTotal(), err)
	}

	updated, err := svc.UpdateUser(ctx, &pb.UpdateUserRequest{
		TenantId: "default", Id: created.User.Id, Status: StatusDisabled, Roles: []string{RoleViewer},
	})
	if err != nil || updated.User.Status != StatusDisabled {
		t.Fatalf("update: %v %v", updated, err)
	}

	if _, err := svc.DeleteUser(ctx, &pb.DeleteUserRequest{TenantId: "default", Id: created.User.Id}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.GetUser(ctx, &pb.GetUserRequest{TenantId: "default", Id: created.User.Id}); status.Code(err) != codes.NotFound {
		t.Errorf("get deleted code = %v, want NotFound", status.Code(err))
	}
}

func TestUserServiceRejectsUnknownRole(t *testing.T) {
	svc := newUserSvc(t)
	_, err := svc.CreateUser(context.Background(), &pb.CreateUserRequest{
		TenantId: "default", Username: "x", Password: "pw123456", Roles: []string{"ghost"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("unknown role code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestRoleManagement(t *testing.T) {
	svc := newUserSvc(t)
	ctx := context.Background()

	roles, err := svc.ListRoles(ctx, &pb.ListRolesRequest{})
	if err != nil || len(roles.Roles) != 3 {
		t.Fatalf("list roles = %d err=%v, want 3", len(roles.GetRoles()), err)
	}

	created, err := svc.UpsertRole(ctx, &pb.UpsertRoleRequest{
		Name: "auditor", Permissions: []string{"trace:read", "user:read"}, Description: "审计员",
	})
	if err != nil {
		t.Fatalf("upsert role: %v", err)
	}
	if created.Role.Builtin {
		t.Error("custom role should not be builtin")
	}

	if _, err := svc.UpsertRole(ctx, &pb.UpsertRoleRequest{Name: "bad", Permissions: []string{"nope:key"}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid permission code = %v, want InvalidArgument", status.Code(err))
	}

	if _, err := svc.DeleteRole(ctx, &pb.DeleteRoleRequest{Name: RoleAdmin}); err == nil {
		t.Error("deleting builtin admin should fail")
	}
	if _, err := svc.DeleteRole(ctx, &pb.DeleteRoleRequest{Name: "auditor"}); err != nil {
		t.Errorf("delete custom role: %v", err)
	}

	perms, err := svc.ListPermissions(ctx, &pb.ListPermissionsRequest{})
	if err != nil || len(perms.Permissions) != len(Permissions) {
		t.Errorf("permissions len = %d err=%v, want %d", len(perms.GetPermissions()), err, len(Permissions))
	}
}
