package tenant

import "context"

// Principal is the authenticated caller propagated across every hop.
type Principal struct {
	TenantID    string
	UserID      string
	Roles       []string
	Departments []string
	Permissions []string
}

type ctxKey struct{}

func With(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func From(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// MustTenantID returns an empty string when no principal is attached.
func TenantID(ctx context.Context) string {
	p, _ := From(ctx)
	return p.TenantID
}

func HasRole(p Principal, role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// HasPermission reports whether the principal holds a permission key. The
// wildcard "*" grants everything.
func HasPermission(p Principal, perm string) bool {
	for _, x := range p.Permissions {
		if x == "*" || x == perm {
			return true
		}
	}
	return false
}
