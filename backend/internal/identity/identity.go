package identity

import (
	"errors"
	"sort"
	"time"
)

// User status values.
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Built-in role names.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

var (
	ErrUserNotFound     = errors.New("user not found")
	ErrUserExists       = errors.New("user already exists")
	ErrInvalidCreds     = errors.New("invalid username or password")
	ErrUserDisabled     = errors.New("user disabled")
	ErrRoleNotFound     = errors.New("role not found")
	ErrRoleExists       = errors.New("role already exists")
	ErrBuiltinRole      = errors.New("built-in role cannot be modified")
	ErrRoleInUse        = errors.New("role is assigned to users")
	ErrPermissionDenied = errors.New("permission denied")
	ErrSelfRegisterOff  = errors.New("self registration disabled")
	ErrInvalidToken     = errors.New("invalid token")
)

// User is a tenant-scoped account. Roles reference role names in the global
// role table; permissions are expanded from those roles at login time.
type User struct {
	ID           string    `json:"id"`
	TenantID     string    `json:"tenant_id"`
	Username     string    `json:"username"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	Roles        []string  `json:"roles"`
	Departments  []string  `json:"departments"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Active reports whether the account may authenticate.
func (u User) Active() bool { return u.Status == "" || u.Status == StatusActive }

// Role is a named set of permission keys. Built-in roles cannot be edited or
// deleted; custom roles can.
type Role struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions"`
	Builtin     bool     `json:"builtin"`
	Description string   `json:"description"`
}

// Permission is a single capability key such as "user:write".
type Permission struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// Wildcard grants every permission.
const Wildcard = "*"

// Permissions is the fixed catalogue of capability keys. Roles are composed
// from these; the catalogue itself is not user-editable.
var Permissions = []Permission{
	{"user:read", "查看用户"},
	{"user:write", "创建与修改用户"},
	{"user:delete", "删除用户"},
	{"role:read", "查看角色"},
	{"role:write", "管理角色与权限"},
	{"file:read", "查看文件"},
	{"file:write", "上传文件"},
	{"file:delete", "删除文件"},
	{"kb:read", "查看知识库"},
	{"kb:write", "创建、修改与删除知识库"},
	{"task:read", "查看任务"},
	{"task:write", "操作任务（重试/删除）"},
	{"usage:read", "查看租户用量与配额"},
	{"search:use", "使用检索"},
	{"sandbox:execute", "执行沙箱代码"},
	{"workflow:run", "运行工作流"},
	{"trace:read", "查看链路追踪"},
	{"apikey:write", "签发与吊销 API Key"},
	{Wildcard, "全量权限"},
}

// BuiltinRoles seeds the three default roles. They are re-applied on startup so
// upgrades to the permission catalogue roll out automatically.
func BuiltinRoles() []Role {
	return []Role{
		{
			Name:        RoleAdmin,
			Permissions: []string{Wildcard},
			Builtin:     true,
			Description: "平台管理员，拥有全部权限",
		},
		{
			Name: RoleEditor,
			Permissions: []string{
				"file:read", "file:write", "file:delete", "kb:read", "kb:write",
				"task:read", "task:write",
				"search:use", "sandbox:execute", "workflow:run",
				"trace:read", "user:read", "role:read", "usage:read",
			},
			Builtin:     true,
			Description: "内容编辑者，可管理文件、任务并执行检索/沙箱/工作流",
		},
		{
			Name: RoleViewer,
			Permissions: []string{
				"file:read", "kb:read", "task:read", "search:use", "trace:read", "usage:read",
			},
			Builtin:     true,
			Description: "只读访客",
		},
	}
}

// ExpandPermissions resolves a set of role names to a deduplicated permission
// list. Unknown roles contribute nothing; the wildcard collapses the list to
// just ["*"].
func ExpandPermissions(roles []Role) []string {
	seen := map[string]struct{}{}
	for _, r := range roles {
		for _, p := range r.Permissions {
			if p == Wildcard {
				return []string{Wildcard}
			}
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
