package identity

import (
	"context"
	"os"
	"testing"
)

// TestRedisStore exercises the shared store. It is skipped unless a Redis
// instance is provided, e.g.:
//
//	REDIS_TEST_ADDR=127.0.0.1:6379 go test ./internal/identity -run Redis
func TestRedisStore(t *testing.T) {
	addr := os.Getenv("REDIS_TEST_ADDR")
	if addr == "" {
		t.Skip("set REDIS_TEST_ADDR to run the RedisStore integration test")
	}
	ctx := context.Background()

	st, err := NewRedisStore(addr)
	if err != nil {
		t.Fatalf("connect redis: %v", err)
	}

	// Use an isolated tenant so the run does not clobber other data.
	const tenant = "test-tenant-redis"
	if err := st.Seed(SeedOptions{TenantID: tenant, AdminUsername: "radmin", AdminPassword: "radmin123"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	hash, _ := HashPassword("pw")
	u, err := st.CreateUser(ctx, User{TenantID: tenant, Username: "ruser", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateUser(ctx, User{TenantID: tenant, Username: "RUSER", PasswordHash: hash}); err != ErrUserExists {
		t.Errorf("duplicate err = %v, want ErrUserExists", err)
	}

	got, ok, err := st.GetUserByUsername(ctx, tenant, "ruser")
	if err != nil || !ok || got.ID != u.ID {
		t.Fatalf("get by username: ok=%v err=%v", ok, err)
	}

	users, total, err := st.ListUsers(ctx, tenant, "", 0, 0)
	if err != nil || total != 2 { // seeded admin + ruser
		t.Fatalf("list total=%d err=%v, want 2", total, err)
	}
	_ = users

	updated, err := st.UpdateUser(ctx, User{TenantID: tenant, ID: u.ID, Status: StatusDisabled, Roles: []string{RoleEditor}})
	if err != nil || updated.Status != StatusDisabled {
		t.Fatalf("update: %+v %v", updated, err)
	}

	if err := st.DeleteUser(ctx, tenant, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := st.GetUser(ctx, tenant, u.ID); ok {
		t.Error("user still present after delete")
	}

	roles, err := st.ListRoles(ctx)
	if err != nil || len(roles) < 3 {
		t.Fatalf("roles=%d err=%v, want >=3", len(roles), err)
	}
	if err := st.DeleteRole(ctx, RoleAdmin); err != ErrBuiltinRole {
		t.Errorf("delete builtin err = %v, want ErrBuiltinRole", err)
	}
}
