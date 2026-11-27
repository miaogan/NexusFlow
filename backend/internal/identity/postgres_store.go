package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresStore shares users and roles across the auth-service and user-service
// processes using the platform PostgreSQL database. Built-in roles are
// re-seeded on every start so permission catalogue upgrades roll out.
type PostgresStore struct {
	db *sql.DB
}

var _ Store = (*PostgresStore)(nil)

const userColumns = `id, tenant_id, username, email, password_hash,
	roles, departments, status, created_at, updated_at`

// identitySchemaLockKey serializes DDL across the auth-service and user-service
// when they start against the same database.
const identitySchemaLockKey int64 = 0x4e465f4944454e54 // "NF_IDENT"

var identitySchema = []string{
	`CREATE TABLE IF NOT EXISTS identity_users (
		id TEXT NOT NULL,
		tenant_id TEXT NOT NULL,
		username TEXT NOT NULL,
		email TEXT NOT NULL DEFAULT '',
		password_hash TEXT NOT NULL,
		roles JSONB NOT NULL DEFAULT '[]'::jsonb,
		departments JSONB NOT NULL DEFAULT '[]'::jsonb,
		status TEXT NOT NULL DEFAULT 'active',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (tenant_id, id)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS identity_users_tenant_username_idx
		ON identity_users (tenant_id, lower(username))`,
	`CREATE TABLE IF NOT EXISTS identity_roles (
		name TEXT PRIMARY KEY,
		permissions JSONB NOT NULL DEFAULT '[]'::jsonb,
		builtin BOOLEAN NOT NULL DEFAULT false,
		description TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS identity_knowledge_bases (
		id TEXT NOT NULL,
		tenant_id TEXT NOT NULL,
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (tenant_id, id)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS identity_knowledge_bases_tenant_name_idx
		ON identity_knowledge_bases (tenant_id, lower(name))`,
}

// NewPostgresStore opens the database, verifies connectivity and ensures the
// identity schema exists.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("identity: postgres DSN is empty")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("identity: open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("identity: ping postgres: %w", err)
	}
	store := &PostgresStore{db: db}
	if err := store.ensureSchema(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// ensureSchema applies the identity DDL exactly once per database. Concurrent
// start-ups (auth-service and user-service) are serialized with an advisory
// lock so CREATE TABLE IF NOT EXISTS does not race on the catalog.
func (s *PostgresStore) ensureSchema(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("identity: begin migrate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, identitySchemaLockKey); err != nil {
		return fmt.Errorf("identity: migrate lock: %w", err)
	}
	for _, stmt := range identitySchema {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("identity: migrate: %w", err)
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) Seed(opts SeedOptions) error {
	ctx := context.Background()
	if err := s.seedRoles(ctx); err != nil {
		return err
	}
	if _, err := s.CreateKnowledgeBase(ctx, defaultKnowledgeBase(tenantOr(opts.TenantID))); err != nil && !errors.Is(err, ErrKnowledgeBaseExists) {
		return err
	}
	return s.seedAdmin(ctx, opts)
}

// seedAdmin creates the first administrator when it is missing. The insert is
// guarded by ON CONFLICT DO NOTHING so repeated or concurrent seeding from both
// services still converges on exactly one account.
func (s *PostgresStore) seedAdmin(ctx context.Context, opts SeedOptions) error {
	if opts.AdminUsername == "" || opts.AdminPassword == "" {
		return nil
	}
	tenant := tenantOr(opts.TenantID)
	var exists int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM identity_users WHERE tenant_id = $1 AND lower(username) = lower($2)`,
		tenant, opts.AdminUsername,
	).Scan(&exists)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	hash, err := HashPassword(opts.AdminPassword)
	if err != nil {
		return err
	}
	roles, err := json.Marshal([]string{RoleAdmin})
	if err != nil {
		return err
	}
	departments, err := json.Marshal([]string{})
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO identity_users
		 (id, tenant_id, username, email, password_hash, roles, departments, status, created_at, updated_at)
		 VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $8)
		 ON CONFLICT DO NOTHING`,
		uuid.NewString(), tenant, strings.TrimSpace(opts.AdminUsername),
		hash, roles, departments, StatusActive, now,
	)
	return err
}

func (s *PostgresStore) seedRoles(ctx context.Context) error {
	for _, role := range BuiltinRoles() {
		perms, err := json.Marshal(role.Permissions)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(
			ctx,
			`INSERT INTO identity_roles (name, permissions, builtin, description)
			 VALUES ($1, $2, TRUE, $3)
			 ON CONFLICT (name) DO UPDATE
			 SET permissions = EXCLUDED.permissions,
			     description = EXCLUDED.description,
			     builtin = TRUE`,
			role.Name, perms, role.Description,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresStore) CreateUser(ctx context.Context, u User) (User, error) {
	u.TenantID = tenantOr(u.TenantID)
	u.Username = strings.TrimSpace(u.Username)
	if u.Username == "" {
		return User{}, ErrInvalidCreds
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
	if u.Departments == nil {
		u.Departments = []string{}
	}
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now

	roles, err := json.Marshal(u.Roles)
	if err != nil {
		return User{}, err
	}
	departments, err := json.Marshal(u.Departments)
	if err != nil {
		return User{}, err
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO identity_users
		 (id, tenant_id, username, email, password_hash, roles, departments, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		u.ID, u.TenantID, u.Username, u.Email, u.PasswordHash,
		roles, departments, u.Status, u.CreatedAt, u.UpdatedAt,
	)
	if isUniqueViolation(err) {
		return User{}, ErrUserExists
	}
	if err != nil {
		return User{}, err
	}
	return copyUser(u), nil
}

func (s *PostgresStore) GetUser(ctx context.Context, tenantID, id string) (User, bool, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+userColumns+` FROM identity_users WHERE tenant_id = $1 AND id = $2`,
		tenantOr(tenantID), id,
	)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (s *PostgresStore) GetUserByUsername(ctx context.Context, tenantID, username string) (User, bool, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT `+userColumns+` FROM identity_users
		 WHERE tenant_id = $1 AND lower(username) = lower($2)`,
		tenantOr(tenantID), strings.TrimSpace(username),
	)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	return u, true, nil
}

func (s *PostgresStore) ListUsers(ctx context.Context, tenantID, query string, limit, offset int) ([]User, int, error) {
	tenant := tenantOr(tenantID)
	where := `tenant_id = $1`
	args := []any{tenant}
	if q := strings.TrimSpace(query); q != "" {
		where += ` AND (username ILIKE $2 OR email ILIKE $2)`
		args = append(args, "%"+q+"%")
	}

	var total int
	if err := s.db.QueryRowContext(
		ctx, `SELECT COUNT(*) FROM identity_users WHERE `+where, args...,
	).Scan(&total); err != nil {
		return nil, 0, err
	}

	stmt := `SELECT ` + userColumns + ` FROM identity_users WHERE ` + where +
		` ORDER BY created_at ASC`
	if limit > 0 {
		stmt += fmt.Sprintf(" LIMIT %d OFFSET %d", limit, maxInt(offset, 0))
	} else if offset > 0 {
		stmt += fmt.Sprintf(" OFFSET %d", offset)
	}
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	users := make([]User, 0, total)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return users, total, nil
}

func (s *PostgresStore) UpdateUser(ctx context.Context, u User) (User, error) {
	tenant := tenantOr(u.TenantID)
	existing, ok, err := s.GetUser(ctx, tenant, u.ID)
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

	roles, err := json.Marshal(existing.Roles)
	if err != nil {
		return User{}, err
	}
	departments, err := json.Marshal(existing.Departments)
	if err != nil {
		return User{}, err
	}
	if _, err := s.db.ExecContext(
		ctx,
		`UPDATE identity_users
		 SET email = $1, roles = $2, departments = $3, status = $4,
		     password_hash = $5, updated_at = $6
		 WHERE tenant_id = $7 AND id = $8`,
		existing.Email, roles, departments, existing.Status,
		existing.PasswordHash, existing.UpdatedAt, tenant, existing.ID,
	); err != nil {
		return User{}, err
	}
	return copyUser(existing), nil
}

func (s *PostgresStore) DeleteUser(ctx context.Context, tenantID, id string) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM identity_users WHERE tenant_id = $1 AND id = $2`,
		tenantOr(tenantID), id,
	)
	if err != nil {
		return err
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (s *PostgresStore) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT name, permissions, builtin, description FROM identity_roles
		 ORDER BY builtin DESC, name ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Role, 0, 8)
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, role)
	}
	return out, rows.Err()
}

func (s *PostgresStore) GetRole(ctx context.Context, name string) (Role, bool, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT name, permissions, builtin, description FROM identity_roles WHERE name = $1`,
		name,
	)
	role, err := scanRole(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Role{}, false, nil
	}
	if err != nil {
		return Role{}, false, err
	}
	return role, true, nil
}

func (s *PostgresStore) UpsertRole(ctx context.Context, role Role) (Role, error) {
	role.Name = strings.TrimSpace(role.Name)
	if role.Name == "" {
		return Role{}, ErrRoleNotFound
	}
	if existing, ok, err := s.GetRole(ctx, role.Name); err != nil {
		return Role{}, err
	} else if ok && existing.Builtin {
		return Role{}, ErrBuiltinRole
	}
	role.Builtin = false
	perms, err := json.Marshal(role.Permissions)
	if err != nil {
		return Role{}, err
	}
	if _, err := s.db.ExecContext(
		ctx,
		`INSERT INTO identity_roles (name, permissions, builtin, description)
		 VALUES ($1, $2, FALSE, $3)
		 ON CONFLICT (name) DO UPDATE
		 SET permissions = EXCLUDED.permissions,
		     description = EXCLUDED.description
		 WHERE identity_roles.builtin = FALSE`,
		role.Name, perms, role.Description,
	); err != nil {
		return Role{}, err
	}
	return role, nil
}

func (s *PostgresStore) DeleteRole(ctx context.Context, name string) error {
	var builtin bool
	err := s.db.QueryRowContext(
		ctx, `SELECT builtin FROM identity_roles WHERE name = $1`, name,
	).Scan(&builtin)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRoleNotFound
	}
	if err != nil {
		return err
	}
	if builtin {
		return ErrBuiltinRole
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM identity_roles WHERE name = $1`, name)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(scanner rowScanner) (User, error) {
	var (
		u           User
		roles       []byte
		departments []byte
	)
	if err := scanner.Scan(
		&u.ID, &u.TenantID, &u.Username, &u.Email, &u.PasswordHash,
		&roles, &departments, &u.Status, &u.CreatedAt, &u.UpdatedAt,
	); err != nil {
		return User{}, err
	}
	if len(roles) > 0 {
		if err := json.Unmarshal(roles, &u.Roles); err != nil {
			return User{}, err
		}
	}
	if len(departments) > 0 {
		if err := json.Unmarshal(departments, &u.Departments); err != nil {
			return User{}, err
		}
	}
	return u, nil
}

func scanRole(scanner rowScanner) (Role, error) {
	var (
		role  Role
		perms []byte
	)
	if err := scanner.Scan(&role.Name, &perms, &role.Builtin, &role.Description); err != nil {
		return Role{}, err
	}
	if len(perms) > 0 {
		if err := json.Unmarshal(perms, &role.Permissions); err != nil {
			return Role{}, err
		}
	}
	return role, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
