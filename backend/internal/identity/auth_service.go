package identity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// AuthConfig controls token issuance and self-registration.
type AuthConfig struct {
	Secret            string
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
	AllowSelfRegister bool
}

func (c AuthConfig) accessTTL() time.Duration {
	if c.AccessTTL <= 0 {
		return 15 * time.Minute
	}
	return c.AccessTTL
}

func (c AuthConfig) refreshTTL() time.Duration {
	if c.RefreshTTL <= 0 {
		return 7 * 24 * time.Hour
	}
	return c.RefreshTTL
}

// AuthService implements the AuthService gRPC contract: register, login,
// token refresh, logout and token validation.
type AuthService struct {
	pb.UnimplementedAuthServiceServer
	store Store
	cfg   AuthConfig
}

func NewAuthService(store Store, cfg AuthConfig) *AuthService {
	return &AuthService{store: store, cfg: cfg}
}

func (s *AuthService) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if !s.cfg.AllowSelfRegister {
		return nil, status.Error(codes.PermissionDenied, ErrSelfRegisterOff.Error())
	}
	hash, err := HashPassword(req.GetPassword())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	u, err := s.store.CreateUser(ctx, User{
		TenantID:     req.GetTenantId(),
		Username:     req.GetUsername(),
		Email:        req.GetEmail(),
		PasswordHash: hash,
		Roles:        []string{RoleViewer},
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.RegisterResponse{User: userToPB(u)}, nil
}

func (s *AuthService) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	u, ok, err := s.store.GetUserByUsername(ctx, req.GetTenantId(), req.GetUsername())
	if err != nil {
		return nil, toStatus(err)
	}
	if !ok || !VerifyPassword(u.PasswordHash, req.GetPassword()) {
		return nil, status.Error(codes.Unauthenticated, ErrInvalidCreds.Error())
	}
	if !u.Active() {
		return nil, status.Error(codes.PermissionDenied, ErrUserDisabled.Error())
	}
	return s.issue(ctx, u)
}

func (s *AuthService) Refresh(ctx context.Context, req *pb.RefreshRequest) (*pb.LoginResponse, error) {
	p, err := tenant.ParseTypedToken(s.cfg.Secret, req.GetRefreshToken(), tenant.TokenTypeRefresh)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, ErrInvalidToken.Error())
	}
	u, ok, err := s.store.GetUser(ctx, p.TenantID, p.UserID)
	if err != nil {
		return nil, toStatus(err)
	}
	if !ok {
		return nil, status.Error(codes.Unauthenticated, ErrInvalidToken.Error())
	}
	if !u.Active() {
		return nil, status.Error(codes.PermissionDenied, ErrUserDisabled.Error())
	}
	return s.issue(ctx, u)
}

// Logout is a no-op for stateless JWTs; refresh tokens expire on their own.
// A production deployment would push the token id onto a revocation list.
func (s *AuthService) Logout(_ context.Context, _ *pb.LogoutRequest) (*pb.LogoutResponse, error) {
	return &pb.LogoutResponse{Success: true}, nil
}

func (s *AuthService) Validate(ctx context.Context, req *pb.ValidateRequest) (*pb.ValidateResponse, error) {
	p, err := tenant.ParseTypedToken(s.cfg.Secret, req.GetAccessToken(), tenant.TokenTypeAccess)
	if err != nil {
		return &pb.ValidateResponse{Valid: false}, nil
	}
	u, ok, err := s.store.GetUser(ctx, p.TenantID, p.UserID)
	if err != nil {
		return nil, toStatus(err)
	}
	if !ok || !u.Active() {
		return &pb.ValidateResponse{Valid: false}, nil
	}
	return &pb.ValidateResponse{Valid: true, User: userToPB(u)}, nil
}

func (s *AuthService) issue(ctx context.Context, u User) (*pb.LoginResponse, error) {
	perms := s.permissionsFor(ctx, u.Roles)
	p := tenant.Principal{
		TenantID:    u.TenantID,
		UserID:      u.ID,
		Roles:       u.Roles,
		Departments: u.Departments,
		Permissions: perms,
	}
	access, err := tenant.IssueToken(s.cfg.Secret, p, s.cfg.accessTTL())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	refresh, err := tenant.IssueRefreshToken(s.cfg.Secret, p, s.cfg.refreshTTL())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.LoginResponse{
		Tokens: &pb.TokenPair{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresIn:    int64(s.cfg.accessTTL().Seconds()),
		},
		User: userToPB(u),
	}, nil
}

func (s *AuthService) permissionsFor(ctx context.Context, roles []string) []string {
	defs := make([]Role, 0, len(roles))
	for _, name := range roles {
		if r, ok, _ := s.store.GetRole(ctx, name); ok {
			defs = append(defs, r)
		}
	}
	return ExpandPermissions(defs)
}

func userToPB(u User) *pb.UserInfo {
	return &pb.UserInfo{
		Id:          u.ID,
		TenantId:    u.TenantID,
		Username:    u.Username,
		Email:       u.Email,
		Roles:       u.Roles,
		Departments: u.Departments,
		Status:      u.Status,
		CreatedAt:   u.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   u.UpdatedAt.Format(time.RFC3339),
	}
}

func roleToPB(r Role) *pb.RoleInfo {
	return &pb.RoleInfo{
		Name:        r.Name,
		Permissions: r.Permissions,
		Builtin:     r.Builtin,
		Description: r.Description,
	}
}

func tenantOf(ctx context.Context, fallback string) string {
	// Only trust an explicit x-tenant-id propagated by the gateway (derived from
	// a verified JWT). Otherwise honour the request field, then default.
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("x-tenant-id"); len(vals) > 0 && strings.TrimSpace(vals[0]) != "" {
			return vals[0]
		}
	}
	if t := strings.TrimSpace(fallback); t != "" {
		return t
	}
	return "default"
}

func toStatus(err error) error {
	switch {
	case errors.Is(err, ErrUserNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrUserExists):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, ErrRoleNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrBuiltinRole):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrInvalidCreds):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
