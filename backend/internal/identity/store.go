package identity

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Store is the persistence boundary for users and roles. MemoryStore is the
// in-process implementation used by dev-all and tests; a shared backend
// (Redis/PostgreSQL) can be swapped in for multi-process deployments.
type Store interface {
	CreateUser(ctx context.Context, u User) (User, error)
	GetUser(ctx context.Context, tenantID, id string) (User, bool, error)
	GetUserByUsername(ctx context.Context, tenantID, username string) (User, bool, error)
	ListUsers(ctx context.Context, tenantID, query string, limit, offset int) ([]User, int, error)
	UpdateUser(ctx context.Context, u User) (User, error)
	DeleteUser(ctx context.Context, tenantID, id string) error

	ListRoles(ctx context.Context) ([]Role, error)
	GetRole(ctx context.Context, name string) (Role, bool, error)
	UpsertRole(ctx context.Context, r Role) (Role, error)
	DeleteRole(ctx context.Context, name string) error
}

// SeedOptions configures the startup seed of built-in roles and the first admin.
type SeedOptions struct {
	TenantID      string
	AdminUsername string
	AdminPassword string
}

// MemoryStore keeps users and roles in process memory.
type MemoryStore struct {
	mu        sync.RWMutex
	users     map[string]*User  // tenantID:id -> user
	usernames map[string]string // tenantID:username(lower) -> id
	roles     map[string]*Role  // name -> role
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:     make(map[string]*User),
		usernames: make(map[string]string),
		roles:     make(map[string]*Role),
	}
}

func userKey(tenantID, id string) string       { return tenantID + ":" + id }
func nameKey(tenantID, username string) string { return tenantID + ":" + strings.ToLower(username) }

func tenantOr(tenantID string) string {
	if strings.TrimSpace(tenantID) == "" {
		return "default"
	}
	return tenantID
}

// Seed applies built-in roles (idempotent, re-applied on every start) and
// creates the admin account when it does not yet exist.
func (m *MemoryStore) Seed(opts SeedOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range BuiltinRoles() {
		copied := r
		m.roles[r.Name] = &copied
	}
	if opts.AdminUsername == "" || opts.AdminPassword == "" {
		return nil
	}
	tenant := tenantOr(opts.TenantID)
	key := nameKey(tenant, opts.AdminUsername)
	if _, exists := m.usernames[key]; exists {
		return nil
	}
	hash, err := HashPassword(opts.AdminPassword)
	if err != nil {
		return err
	}
	now := time.Now()
	u := &User{
		ID:           uuid.NewString(),
		TenantID:     tenant,
		Username:     opts.AdminUsername,
		PasswordHash: hash,
		Roles:        []string{RoleAdmin},
		Departments:  []string{},
		Status:       StatusActive,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.users[userKey(tenant, u.ID)] = u
	m.usernames[key] = u.ID
	return nil
}

func (m *MemoryStore) CreateUser(_ context.Context, u User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u.TenantID = tenantOr(u.TenantID)
	u.Username = strings.TrimSpace(u.Username)
	if u.Username == "" {
		return User{}, ErrInvalidCreds
	}
	key := nameKey(u.TenantID, u.Username)
	if _, exists := m.usernames[key]; exists {
		return User{}, ErrUserExists
	}
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	if u.Status == "" {
		u.Status = StatusActive
	}
	if len(u.Roles) == 0 {
		u.Roles = []string{RoleViewer}
	}
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	copied := copyUser(u)
	m.users[userKey(u.TenantID, u.ID)] = &copied
	m.usernames[key] = u.ID
	return copied, nil
}

func (m *MemoryStore) GetUser(_ context.Context, tenantID, id string) (User, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	u, ok := m.users[userKey(tenantOr(tenantID), id)]
	if !ok {
		return User{}, false, nil
	}
	return copyUser(*u), true, nil
}

func (m *MemoryStore) GetUserByUsername(_ context.Context, tenantID, username string) (User, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenant := tenantOr(tenantID)
	id, ok := m.usernames[nameKey(tenant, username)]
	if !ok {
		return User{}, false, nil
	}
	u, ok := m.users[userKey(tenant, id)]
	if !ok {
		return User{}, false, nil
	}
	return copyUser(*u), true, nil
}

func (m *MemoryStore) ListUsers(_ context.Context, tenantID, query string, limit, offset int) ([]User, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tenant := tenantOr(tenantID)
	q := strings.ToLower(strings.TrimSpace(query))
	matched := make([]User, 0)
	for _, u := range m.users {
		if u.TenantID != tenant {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(u.Username), q) && !strings.Contains(strings.ToLower(u.Email), q) {
			continue
		}
		matched = append(matched, copyUser(*u))
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.Before(matched[j].CreatedAt) })

	total := len(matched)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

func (m *MemoryStore) UpdateUser(_ context.Context, u User) (User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	u.TenantID = tenantOr(u.TenantID)
	key := userKey(u.TenantID, u.ID)
	existing, ok := m.users[key]
	if !ok {
		return User{}, ErrUserNotFound
	}
	existing.Email = u.Email
	if len(u.Roles) > 0 {
		existing.Roles = append([]string(nil), u.Roles...)
	}
	if u.Departments != nil {
		existing.Departments = append([]string(nil), u.Departments...)
	}
	if u.Status != "" {
		existing.Status = u.Status
	}
	if u.PasswordHash != "" {
		existing.PasswordHash = u.PasswordHash
	}
	existing.UpdatedAt = time.Now()
	return copyUser(*existing), nil
}

func (m *MemoryStore) DeleteUser(_ context.Context, tenantID, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	tenant := tenantOr(tenantID)
	key := userKey(tenant, id)
	u, ok := m.users[key]
	if !ok {
		return ErrUserNotFound
	}
	delete(m.usernames, nameKey(tenant, u.Username))
	delete(m.users, key)
	return nil
}

func (m *MemoryStore) ListRoles(_ context.Context) ([]Role, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Role, 0, len(m.roles))
	for _, r := range m.roles {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *MemoryStore) GetRole(_ context.Context, name string) (Role, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.roles[name]
	if !ok {
		return Role{}, false, nil
	}
	return *r, true, nil
}

func (m *MemoryStore) UpsertRole(_ context.Context, r Role) (Role, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return Role{}, ErrRoleNotFound
	}
	if existing, ok := m.roles[r.Name]; ok && existing.Builtin {
		return Role{}, ErrBuiltinRole
	}
	copied := Role{
		Name:        r.Name,
		Permissions: append([]string(nil), r.Permissions...),
		Builtin:     false,
		Description: r.Description,
	}
	m.roles[r.Name] = &copied
	return copied, nil
}

func (m *MemoryStore) DeleteRole(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.roles[name]
	if !ok {
		return ErrRoleNotFound
	}
	if r.Builtin {
		return ErrBuiltinRole
	}
	delete(m.roles, name)
	return nil
}

// RolesByNames resolves role names to definitions, skipping unknown names.
func (m *MemoryStore) RolesByNames(names []string) []Role {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Role, 0, len(names))
	for _, n := range names {
		if r, ok := m.roles[n]; ok {
			out = append(out, *r)
		}
	}
	return out
}

func copyUser(u User) User {
	u.Roles = append([]string(nil), u.Roles...)
	u.Departments = append([]string(nil), u.Departments...)
	return u
}
