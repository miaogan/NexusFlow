package tenant

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"
)

// FromIncoming rebuilds a Principal from gRPC metadata propagated by the
// gateway. Fallbacks keep local development frictionless.
func FromIncoming(ctx context.Context) Principal {
	p := Principal{TenantID: "default", UserID: "anonymous"}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return p
	}
	if v := first(md.Get("x-tenant-id")); v != "" {
		p.TenantID = v
	}
	if v := first(md.Get("x-user-id")); v != "" {
		p.UserID = v
	}
	if v := first(md.Get("x-departments")); v != "" {
		p.Departments = strings.Split(v, ",")
	}
	if v := first(md.Get("x-roles")); v != "" {
		p.Roles = strings.Split(v, ",")
	}
	if v := first(md.Get("x-permissions")); v != "" {
		p.Permissions = strings.Split(v, ",")
	}
	return p
}

func first(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// ToOutgoing propagates the principal to downstream gRPC calls.
func ToOutgoing(ctx context.Context, p Principal) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"x-tenant-id", p.TenantID,
		"x-user-id", p.UserID,
		"x-departments", strings.Join(p.Departments, ","),
		"x-roles", strings.Join(p.Roles, ","),
		"x-permissions", strings.Join(p.Permissions, ","),
	)
}
