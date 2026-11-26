package identity

import (
	"context"
	"testing"
)

func seededStore(t *testing.T) *MemoryStore {
	t.Helper()
	m := NewMemoryStore()
	if err := m.Seed(SeedOptions{TenantID: "default", AdminUsername: "admin", AdminPassword: "admin123"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return m
}

func TestSeedCreatesBuiltinRolesAndAdmin(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()

	roles, err := m.ListRoles(ctx)
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	if len(roles) != 3 {
		t.Fatalf("want 3 builtin roles, got %d", len(roles))
	}
	for _, name := range []string{RoleAdmin, RoleEditor, RoleViewer} {
		if _, ok, _ := m.GetRole(ctx, name); !ok {
			t.Errorf("missing builtin role %q", name)
		}
	}

	admin, ok, err := m.GetUserByUsername(ctx, "default", "admin")
	if err != nil || !ok {
		t.Fatalf("admin not seeded: ok=%v err=%v", ok, err)
	}
	if len(admin.Roles) != 1 || admin.Roles[0] != RoleAdmin {
		t.Errorf("admin roles = %v, want [admin]", admin.Roles)
	}
	if !VerifyPassword(admin.PasswordHash, "admin123") {
		t.Error("admin password hash does not verify")
	}
	if VerifyPassword(admin.PasswordHash, "wrong") {
		t.Error("wrong password unexpectedly verified")
	}
}

func TestCreateUserUniquenessAndDefaults(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()

	hash, _ := HashPassword("secret")
	u, err := m.CreateUser(ctx, User{TenantID: "default", Username: "alice", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID == "" {
		t.Error("expected generated id")
	}
	if u.Status != StatusActive {
		t.Errorf("status = %q, want active", u.Status)
	}
	if len(u.Roles) != 1 || u.Roles[0] != RoleViewer {
		t.Errorf("default roles = %v, want [viewer]", u.Roles)
	}

	if _, err := m.CreateUser(ctx, User{TenantID: "default", Username: "Alice", PasswordHash: hash}); err != ErrUserExists {
		t.Errorf("duplicate username err = %v, want ErrUserExists (case-insensitive)", err)
	}
	// Same username in a different tenant is allowed.
	if _, err := m.CreateUser(ctx, User{TenantID: "acme", Username: "alice", PasswordHash: hash}); err != nil {
		t.Errorf("cross-tenant username should be allowed: %v", err)
	}
}

func TestListUsersFilterAndPaging(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()
	hash, _ := HashPassword("x")

	for _, n := range []string{"bob", "carol", "dave"} {
		if _, err := m.CreateUser(ctx, User{TenantID: "default", Username: n, Email: n + "@ex.com", PasswordHash: hash}); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}

	all, total, err := m.ListUsers(ctx, "default", "", 0, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 4 || len(all) != 4 { // admin + 3
		t.Errorf("total=%d len=%d, want 4", total, len(all))
	}

	filtered, _, _ := m.ListUsers(ctx, "default", "car", 0, 0)
	if len(filtered) != 1 || filtered[0].Username != "carol" {
		t.Errorf("filter car -> %v, want [carol]", filtered)
	}

	paged, _, _ := m.ListUsers(ctx, "default", "", 2, 1)
	if len(paged) != 2 {
		t.Errorf("paged len=%d, want 2", len(paged))
	}
}

func TestUpdateUserPartial(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()
	hash, _ := HashPassword("x")
	u, _ := m.CreateUser(ctx, User{TenantID: "default", Username: "eve", Email: "eve@old", PasswordHash: hash})

	updated, err := m.UpdateUser(ctx, User{
		TenantID: "default",
		ID:       u.ID,
		Email:    "eve@new",
		Roles:    []string{RoleEditor},
		Status:   StatusDisabled,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Email != "eve@new" || updated.Status != StatusDisabled {
		t.Errorf("update not applied: %+v", updated)
	}
	if len(updated.Roles) != 1 || updated.Roles[0] != RoleEditor {
		t.Errorf("roles = %v, want [editor]", updated.Roles)
	}
	if !VerifyPassword(updated.PasswordHash, "x") {
		t.Error("password should be unchanged when empty")
	}
}

func TestDeleteUser(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()
	hash, _ := HashPassword("x")
	u, _ := m.CreateUser(ctx, User{TenantID: "default", Username: "frank", PasswordHash: hash})

	if err := m.DeleteUser(ctx, "default", u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := m.GetUser(ctx, "default", u.ID); ok {
		t.Error("user should be gone")
	}
	// Username index cleared, so it can be recreated.
	if _, err := m.CreateUser(ctx, User{TenantID: "default", Username: "frank", PasswordHash: hash}); err != nil {
		t.Errorf("recreate after delete: %v", err)
	}
	if err := m.DeleteUser(ctx, "default", "missing"); err != ErrUserNotFound {
		t.Errorf("delete missing err = %v, want ErrUserNotFound", err)
	}
}

func TestRoleGuards(t *testing.T) {
	m := seededStore(t)
	ctx := context.Background()

	if _, err := m.UpsertRole(ctx, Role{Name: RoleAdmin, Permissions: []string{"file:read"}}); err != ErrBuiltinRole {
		t.Errorf("upsert builtin err = %v, want ErrBuiltinRole", err)
	}
	if err := m.DeleteRole(ctx, RoleViewer); err != ErrBuiltinRole {
		t.Errorf("delete builtin err = %v, want ErrBuiltinRole", err)
	}

	created, err := m.UpsertRole(ctx, Role{Name: "auditor", Permissions: []string{"trace:read"}, Description: "审计"})
	if err != nil {
		t.Fatalf("create custom role: %v", err)
	}
	if created.Builtin {
		t.Error("custom role must not be builtin")
	}
	if err := m.DeleteRole(ctx, "auditor"); err != nil {
		t.Errorf("delete custom role: %v", err)
	}
	if err := m.DeleteRole(ctx, "nope"); err != ErrRoleNotFound {
		t.Errorf("delete missing role err = %v, want ErrRoleNotFound", err)
	}
}

func TestExpandPermissions(t *testing.T) {
	m := seededStore(t)
	adminRoles := m.RolesByNames([]string{RoleAdmin})
	if got := ExpandPermissions(adminRoles); len(got) != 1 || got[0] != Wildcard {
		t.Errorf("admin expand = %v, want [*]", got)
	}

	editorRoles := m.RolesByNames([]string{RoleEditor, "unknown"})
	got := ExpandPermissions(editorRoles)
	found := false
	for _, p := range got {
		if p == "file:write" {
			found = true
		}
	}
	if !found {
		t.Errorf("editor should include file:write, got %v", got)
	}
}
