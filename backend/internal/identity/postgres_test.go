package identity

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestPostgresStoreIntegration exercises the IDENTITY_DRIVER=postgres path
// (auto-migration, built-in role seeding and seeded admin) against a real
// PostgreSQL instance. It is skipped unless POSTGRES_TEST_DSN points at a
// disposable database, e.g.:
//
//	POSTGRES_TEST_DSN=postgresql://nexusflow:nexusflow@127.0.0.1:5432/nexusflow \
//	    go test ./internal/identity -run Postgres
func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set POSTGRES_TEST_DSN to run the PostgresStore integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// A unique tenant keeps parallel runs (and repeated runs against the same
	// database) from clobbering each other.
	tenant := fmt.Sprintf("test-tenant-pg-%d", time.Now().UnixNano())

	// auth-service and user-service start at the same time, so migration and
	// seeding must converge instead of failing on schema or unique conflicts.
	stores := make([]*PostgresStore, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = NewPostgresStore(ctx, dsn)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("open store #%d: %v", i, err)
		}
	}
	st := stores[0]

	seed := SeedOptions{TenantID: tenant, AdminUsername: "pgadmin", AdminPassword: "pgadmin123"}
	if err := st.Seed(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, other := range stores {
		wg.Add(1)
		go func(s *PostgresStore) {
			defer wg.Done()
			if err := s.Seed(seed); err != nil {
				t.Errorf("repeat seed: %v", err)
			}
		}(other)
	}
	wg.Wait()

	// Both services build the store through identity.NewStore; cover that path
	// too so IDENTITY_DRIVER=postgres wiring stays verified.
	factory, err := NewStore("postgres", "", dsn)
	if err != nil {
		t.Fatalf("NewStore(postgres): %v", err)
	}
	if err := factory.Seed(seed); err != nil {
		t.Fatalf("seed via NewStore: %v", err)
	}

	// Exactly one admin exists and its credentials work.
	admin, ok, err := st.GetUserByUsername(ctx, tenant, "PGADMIN")
	if err != nil || !ok {
		t.Fatalf("seeded admin missing: ok=%v err=%v", ok, err)
	}
	if !VerifyPassword(admin.PasswordHash, seed.AdminPassword) {
		t.Error("seeded admin password does not verify")
	}
	if len(admin.Roles) != 1 || admin.Roles[0] != RoleAdmin {
		t.Errorf("seeded admin roles = %v, want [%s]", admin.Roles, RoleAdmin)
	}
	if admin.Status != StatusActive {
		t.Errorf("seeded admin status = %q, want %q", admin.Status, StatusActive)
	}
	if users, total, err := st.ListUsers(ctx, tenant, "", 0, 0); err != nil || total != 1 {
		t.Fatalf("list after seed: total=%d err=%v, want 1 (%d users)", total, err, len(users))
	}

	// Built-in roles are re-applied on every start and cannot be removed.
	roles, err := st.ListRoles(ctx)
	if err != nil || len(roles) < len(BuiltinRoles()) {
		t.Fatalf("roles=%d err=%v, want >=%d", len(roles), err, len(BuiltinRoles()))
	}
	adminRole, ok, err := st.GetRole(ctx, RoleAdmin)
	if err != nil || !ok || !adminRole.Builtin {
		t.Fatalf("built-in admin role: %+v ok=%v err=%v", adminRole, ok, err)
	}
	if err := st.DeleteRole(ctx, RoleAdmin); err != ErrBuiltinRole {
		t.Errorf("delete builtin err = %v, want ErrBuiltinRole", err)
	}

	// User lifecycle against the shared tables.
	hash, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u, err := st.CreateUser(ctx, User{TenantID: tenant, Username: "pguser", PasswordHash: hash})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateUser(ctx, User{TenantID: tenant, Username: "PGUSER", PasswordHash: hash}); err != ErrUserExists {
		t.Errorf("duplicate err = %v, want ErrUserExists", err)
	}
	got, ok, err := st.GetUser(ctx, tenant, u.ID)
	if err != nil || !ok || got.Username != "pguser" {
		t.Fatalf("get by id: ok=%v err=%v user=%+v", ok, err, got)
	}
	updated, err := st.UpdateUser(ctx, User{
		TenantID: tenant, ID: u.ID, Status: StatusDisabled, Roles: []string{RoleEditor},
	})
	if err != nil || updated.Status != StatusDisabled {
		t.Fatalf("update: %+v err=%v", updated, err)
	}
	if err := st.DeleteUser(ctx, tenant, u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := st.GetUser(ctx, tenant, u.ID); ok {
		t.Error("user still present after delete")
	}

	// Keep the database clean for repeated runs.
	if err := st.DeleteUser(ctx, tenant, admin.ID); err != nil {
		t.Errorf("cleanup admin: %v", err)
	}
}
