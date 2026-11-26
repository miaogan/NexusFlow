package identity

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// RedisStore shares users and roles across the auth-service and user-service
// processes. Values are stored as JSON strings keyed by tenant/id.
type RedisStore struct {
	rdb *redis.Client
}

func NewRedisStore(addr string) (*RedisStore, error) {
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		return nil, err
	}
	return &RedisStore{rdb: rdb}, nil
}

func (r *RedisStore) userKey(tenant, id string) string { return "identity:user:" + tenant + ":" + id }
func (r *RedisStore) nameKey(tenant, name string) string {
	return "identity:username:" + tenant + ":" + strings.ToLower(name)
}
func (r *RedisStore) roleKey(name string) string { return "identity:role:" + name }

func (r *RedisStore) scan(ctx context.Context, pattern string) ([]string, error) {
	keys := make([]string, 0, 32)
	iter := r.rdb.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

func (r *RedisStore) Seed(opts SeedOptions) error {
	ctx := context.Background()
	for _, role := range BuiltinRoles() {
		if err := r.putRole(ctx, role); err != nil {
			return err
		}
	}
	if opts.AdminUsername == "" || opts.AdminPassword == "" {
		return nil
	}
	tenant := tenantOr(opts.TenantID)
	exists, err := r.rdb.Exists(ctx, r.nameKey(tenant, opts.AdminUsername)).Result()
	if err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	hash, err := HashPassword(opts.AdminPassword)
	if err != nil {
		return err
	}
	now := time.Now()
	u := User{
		ID: uuid.NewString(), TenantID: tenant, Username: opts.AdminUsername,
		PasswordHash: hash, Roles: []string{RoleAdmin}, Departments: []string{},
		Status: StatusActive, CreatedAt: now, UpdatedAt: now,
	}
	_, err = r.CreateUser(ctx, u)
	return err
}

func (r *RedisStore) CreateUser(ctx context.Context, u User) (User, error) {
	u.TenantID = tenantOr(u.TenantID)
	u.Username = strings.TrimSpace(u.Username)
	if u.Username == "" {
		return User{}, ErrInvalidCreds
	}
	exists, err := r.rdb.Exists(ctx, r.nameKey(u.TenantID, u.Username)).Result()
	if err != nil {
		return User{}, err
	}
	if exists > 0 {
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

	payload, err := json.Marshal(u)
	if err != nil {
		return User{}, err
	}
	pipe := r.rdb.TxPipeline()
	pipe.Set(ctx, r.userKey(u.TenantID, u.ID), payload, 0)
	pipe.Set(ctx, r.nameKey(u.TenantID, u.Username), u.ID, 0)
	if _, err := pipe.Exec(ctx); err != nil {
		return User{}, err
	}
	return u, nil
}

func (r *RedisStore) GetUser(ctx context.Context, tenantID, id string) (User, bool, error) {
	raw, err := r.rdb.Get(ctx, r.userKey(tenantOr(tenantID), id)).Bytes()
	if err == redis.Nil {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (r *RedisStore) GetUserByUsername(ctx context.Context, tenantID, username string) (User, bool, error) {
	tenant := tenantOr(tenantID)
	id, err := r.rdb.Get(ctx, r.nameKey(tenant, username)).Result()
	if err == redis.Nil {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return r.GetUser(ctx, tenant, id)
}

func (r *RedisStore) ListUsers(ctx context.Context, tenantID, query string, limit, offset int) ([]User, int, error) {
	tenant := tenantOr(tenantID)
	keys, err := r.scan(ctx, "identity:user:"+tenant+":*")
	if err != nil {
		return nil, 0, err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	matched := make([]User, 0, len(keys))
	for _, key := range keys {
		raw, err := r.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var u User
		if json.Unmarshal(raw, &u) != nil {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(u.Username), q) && !strings.Contains(strings.ToLower(u.Email), q) {
			continue
		}
		matched = append(matched, u)
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

func (r *RedisStore) UpdateUser(ctx context.Context, u User) (User, error) {
	u.TenantID = tenantOr(u.TenantID)
	existing, ok, err := r.GetUser(ctx, u.TenantID, u.ID)
	if err != nil {
		return User{}, err
	}
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

	payload, err := json.Marshal(existing)
	if err != nil {
		return User{}, err
	}
	if err := r.rdb.Set(ctx, r.userKey(existing.TenantID, existing.ID), payload, 0).Err(); err != nil {
		return User{}, err
	}
	return existing, nil
}

func (r *RedisStore) DeleteUser(ctx context.Context, tenantID, id string) error {
	tenant := tenantOr(tenantID)
	u, ok, err := r.GetUser(ctx, tenant, id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUserNotFound
	}
	pipe := r.rdb.TxPipeline()
	pipe.Del(ctx, r.userKey(tenant, id))
	pipe.Del(ctx, r.nameKey(tenant, u.Username))
	_, err = pipe.Exec(ctx)
	return err
}

func (r *RedisStore) ListRoles(ctx context.Context) ([]Role, error) {
	keys, err := r.scan(ctx, "identity:role:*")
	if err != nil {
		return nil, err
	}
	out := make([]Role, 0, len(keys))
	for _, key := range keys {
		raw, err := r.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var role Role
		if json.Unmarshal(raw, &role) == nil {
			out = append(out, role)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Builtin != out[j].Builtin {
			return out[i].Builtin
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (r *RedisStore) GetRole(ctx context.Context, name string) (Role, bool, error) {
	raw, err := r.rdb.Get(ctx, r.roleKey(name)).Bytes()
	if err == redis.Nil {
		return Role{}, false, nil
	}
	if err != nil {
		return Role{}, false, err
	}
	var role Role
	if err := json.Unmarshal(raw, &role); err != nil {
		return Role{}, false, err
	}
	return role, true, nil
}

func (r *RedisStore) UpsertRole(ctx context.Context, role Role) (Role, error) {
	role.Name = strings.TrimSpace(role.Name)
	if role.Name == "" {
		return Role{}, ErrRoleNotFound
	}
	if existing, ok, _ := r.GetRole(ctx, role.Name); ok && existing.Builtin {
		return Role{}, ErrBuiltinRole
	}
	role.Builtin = false
	if err := r.putRole(ctx, role); err != nil {
		return Role{}, err
	}
	return role, nil
}

func (r *RedisStore) DeleteRole(ctx context.Context, name string) error {
	role, ok, err := r.GetRole(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return ErrRoleNotFound
	}
	if role.Builtin {
		return ErrBuiltinRole
	}
	return r.rdb.Del(ctx, r.roleKey(name)).Err()
}

func (r *RedisStore) putRole(ctx context.Context, role Role) error {
	payload, err := json.Marshal(role)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, r.roleKey(role.Name), payload, 0).Err()
}

// NewStore selects the identity store implementation from configuration.
// redisAddr is used for the redis driver; dsn is used for the postgres driver.
func NewStore(driver, redisAddr, dsn string) (Store, error) {
	switch driver {
	case "redis":
		return NewRedisStore(redisAddr)
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return NewPostgresStore(ctx, dsn)
	default:
		return NewMemoryStore(), nil
	}
}
