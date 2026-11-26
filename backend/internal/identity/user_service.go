package identity

import (
	"context"
	"fmt"
	"strings"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UserService implements the UserService gRPC contract: tenant-scoped user CRUD
// plus global role and permission management.
type UserService struct {
	pb.UnimplementedUserServiceServer
	store Store
}

func NewUserService(store Store) *UserService {
	return &UserService{store: store}
}

func (s *UserService) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.UserResponse, error) {
	hash, err := HashPassword(req.GetPassword())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	roles := req.GetRoles()
	if len(roles) == 0 {
		roles = []string{RoleViewer}
	}
	if err := s.validateRoles(ctx, roles); err != nil {
		return nil, err
	}
	u, err := s.store.CreateUser(ctx, User{
		TenantID:     tenantOf(ctx, req.GetTenantId()),
		Username:     req.GetUsername(),
		Email:        req.GetEmail(),
		PasswordHash: hash,
		Roles:        roles,
		Departments:  req.GetDepartments(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.UserResponse{User: userToPB(u)}, nil
}

func (s *UserService) GetUser(ctx context.Context, req *pb.GetUserRequest) (*pb.UserResponse, error) {
	u, ok, err := s.store.GetUser(ctx, tenantOf(ctx, req.GetTenantId()), req.GetId())
	if err != nil {
		return nil, toStatus(err)
	}
	if !ok {
		return nil, status.Error(codes.NotFound, ErrUserNotFound.Error())
	}
	return &pb.UserResponse{User: userToPB(u)}, nil
}

func (s *UserService) ListUsers(ctx context.Context, req *pb.ListUsersRequest) (*pb.ListUsersResponse, error) {
	users, total, err := s.store.ListUsers(ctx, tenantOf(ctx, req.GetTenantId()), req.GetQuery(), int(req.GetLimit()), int(req.GetOffset()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*pb.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, userToPB(u))
	}
	return &pb.ListUsersResponse{Users: out, Total: int32(total)}, nil
}

func (s *UserService) UpdateUser(ctx context.Context, req *pb.UpdateUserRequest) (*pb.UserResponse, error) {
	if req.GetStatus() != "" && req.GetStatus() != StatusActive && req.GetStatus() != StatusDisabled {
		return nil, status.Error(codes.InvalidArgument, "status must be active or disabled")
	}
	if roles := req.GetRoles(); len(roles) > 0 {
		if err := s.validateRoles(ctx, roles); err != nil {
			return nil, err
		}
	}
	patch := User{
		ID:          req.GetId(),
		TenantID:    tenantOf(ctx, req.GetTenantId()),
		Email:       req.GetEmail(),
		Roles:       req.GetRoles(),
		Departments: req.GetDepartments(),
		Status:      req.GetStatus(),
	}
	if req.GetPassword() != "" {
		hash, err := HashPassword(req.GetPassword())
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		patch.PasswordHash = hash
	}
	u, err := s.store.UpdateUser(ctx, patch)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.UserResponse{User: userToPB(u)}, nil
}

func (s *UserService) DeleteUser(ctx context.Context, req *pb.DeleteUserRequest) (*pb.DeleteUserResponse, error) {
	if err := s.store.DeleteUser(ctx, tenantOf(ctx, req.GetTenantId()), req.GetId()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DeleteUserResponse{Success: true}, nil
}

func (s *UserService) ListRoles(ctx context.Context, _ *pb.ListRolesRequest) (*pb.ListRolesResponse, error) {
	roles, err := s.store.ListRoles(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	out := make([]*pb.RoleInfo, 0, len(roles))
	for _, r := range roles {
		out = append(out, roleToPB(r))
	}
	return &pb.ListRolesResponse{Roles: out}, nil
}

func (s *UserService) UpsertRole(ctx context.Context, req *pb.UpsertRoleRequest) (*pb.RoleResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "role name required")
	}
	if err := validatePermissions(req.GetPermissions()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	r, err := s.store.UpsertRole(ctx, Role{
		Name:        name,
		Permissions: req.GetPermissions(),
		Description: req.GetDescription(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.RoleResponse{Role: roleToPB(r)}, nil
}

func (s *UserService) DeleteRole(ctx context.Context, req *pb.DeleteRoleRequest) (*pb.DeleteRoleResponse, error) {
	if err := s.store.DeleteRole(ctx, req.GetName()); err != nil {
		return nil, toStatus(err)
	}
	return &pb.DeleteRoleResponse{Success: true}, nil
}

func (s *UserService) ListPermissions(_ context.Context, _ *pb.ListPermissionsRequest) (*pb.ListPermissionsResponse, error) {
	out := make([]*pb.PermissionInfo, 0, len(Permissions))
	for _, p := range Permissions {
		out = append(out, &pb.PermissionInfo{Key: p.Key, Description: p.Description})
	}
	return &pb.ListPermissionsResponse{Permissions: out}, nil
}

func (s *UserService) validateRoles(ctx context.Context, roles []string) error {
	for _, name := range roles {
		if _, ok, _ := s.store.GetRole(ctx, name); !ok {
			return status.Errorf(codes.InvalidArgument, "unknown role %q", name)
		}
	}
	return nil
}

func validatePermissions(perms []string) error {
	allowed := map[string]struct{}{Wildcard: {}}
	for _, p := range Permissions {
		allowed[p.Key] = struct{}{}
	}
	for _, p := range perms {
		if _, ok := allowed[p]; !ok {
			return fmt.Errorf("unknown permission %q", p)
		}
	}
	return nil
}
