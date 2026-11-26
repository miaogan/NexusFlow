package identity

import (
	"context"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newAuthSvc(t *testing.T, allowRegister bool) (*AuthService, *MemoryStore) {
	t.Helper()
	m := seededStore(t)
	return NewAuthService(m, AuthConfig{
		Secret:            "test-secret",
		AccessTTL:         5 * time.Minute,
		RefreshTTL:        24 * time.Hour,
		AllowSelfRegister: allowRegister,
	}), m
}

func TestAuthRegisterLoginRefresh(t *testing.T) {
	svc, _ := newAuthSvc(t, true)
	ctx := context.Background()

	reg, err := svc.Register(ctx, &pb.RegisterRequest{TenantId: "default", Username: "newbie", Password: "pw123456", Email: "n@ex.com"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.User.Roles[0] != RoleViewer {
		t.Errorf("new user roles = %v, want viewer", reg.User.Roles)
	}

	login, err := svc.Login(ctx, &pb.LoginRequest{TenantId: "default", Username: "newbie", Password: "pw123456"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if login.Tokens.AccessToken == "" || login.Tokens.RefreshToken == "" || login.Tokens.ExpiresIn != 300 {
		t.Fatalf("bad token pair: %+v", login.Tokens)
	}

	// Access token parses as access with viewer permissions.
	p, err := tenant.ParseTypedToken("test-secret", login.Tokens.AccessToken, tenant.TokenTypeAccess)
	if err != nil {
		t.Fatalf("parse access: %v", err)
	}
	if !tenant.HasPermission(p, "search:use") || tenant.HasPermission(p, "user:write") {
		t.Errorf("viewer permissions wrong: %v", p.Permissions)
	}

	// Refresh mints a new pair.
	ref, err := svc.Refresh(ctx, &pb.RefreshRequest{RefreshToken: login.Tokens.RefreshToken})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if ref.Tokens.AccessToken == "" {
		t.Error("refresh returned empty access token")
	}
}

func TestAuthRejectsWrongPasswordAndDisabled(t *testing.T) {
	svc, m := newAuthSvc(t, true)
	ctx := context.Background()

	if _, err := svc.Register(ctx, &pb.RegisterRequest{TenantId: "default", Username: "u1", Password: "pw123456"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := svc.Login(ctx, &pb.LoginRequest{TenantId: "default", Username: "u1", Password: "wrong"})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("wrong password code = %v, want Unauthenticated", status.Code(err))
	}

	u, _, _ := m.GetUserByUsername(ctx, "default", "u1")
	if _, err := m.UpdateUser(ctx, User{TenantID: "default", ID: u.ID, Status: StatusDisabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	_, err = svc.Login(ctx, &pb.LoginRequest{TenantId: "default", Username: "u1", Password: "pw123456"})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("disabled login code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestAuthSelfRegisterDisabled(t *testing.T) {
	svc, _ := newAuthSvc(t, false)
	_, err := svc.Register(context.Background(), &pb.RegisterRequest{TenantId: "default", Username: "x", Password: "pw123456"})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("self-register off code = %v, want PermissionDenied", status.Code(err))
	}
}

func TestAuthValidateAndRefreshRejectsAccessToken(t *testing.T) {
	svc, _ := newAuthSvc(t, true)
	ctx := context.Background()
	login, err := svc.Login(ctx, &pb.LoginRequest{TenantId: "default", Username: "admin", Password: "admin123"})
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	v, err := svc.Validate(ctx, &pb.ValidateRequest{AccessToken: login.Tokens.AccessToken})
	if err != nil || !v.Valid || v.User.Username != "admin" {
		t.Fatalf("validate: valid=%v err=%v", v.GetValid(), err)
	}

	// An access token must not be usable as a refresh token.
	if _, err := svc.Refresh(ctx, &pb.RefreshRequest{RefreshToken: login.Tokens.AccessToken}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("access-as-refresh code = %v, want Unauthenticated", status.Code(err))
	}
}

func TestAdminPermissionsAreWildcard(t *testing.T) {
	svc, _ := newAuthSvc(t, true)
	login, err := svc.Login(context.Background(), &pb.LoginRequest{TenantId: "default", Username: "admin", Password: "admin123"})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	p, err := tenant.ParseTypedToken("test-secret", login.Tokens.AccessToken, tenant.TokenTypeAccess)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !tenant.HasPermission(p, "user:write") || !tenant.HasPermission(p, "anything:else") {
		t.Errorf("admin should hold wildcard, got %v", p.Permissions)
	}
}
