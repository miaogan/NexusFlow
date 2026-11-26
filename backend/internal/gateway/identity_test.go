package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/identity"
	"github.com/nexusflow/nexusflow/backend/internal/platform/log"
	"github.com/nexusflow/nexusflow/backend/internal/revocation"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// newIdentityGateway wires a Gateway to real Auth/User gRPC services running
// over an in-memory bufconn listener, so routing, JWT auth and RBAC are all
// exercised end to end.
func newIdentityGateway(t *testing.T) *Gateway {
	t.Helper()

	store := identity.NewMemoryStore()
	if err := store.Seed(identity.SeedOptions{TenantID: "default", AdminUsername: "admin", AdminPassword: "admin123"}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	blacklist := revocation.NewMemoryBlacklist()
	pb.RegisterAuthServiceServer(srv, identity.NewAuthService(store, identity.AuthConfig{
		Secret: "test-secret", AccessTTL: 5 * time.Minute, RefreshTTL: time.Hour, AllowSelfRegister: true,
	}, identity.WithRevocationList(blacklist)))
	pb.RegisterUserServiceServer(srv, identity.NewUserService(store))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return New(nil, nil, nil, nil, log.New("gateway-test"), "test-secret", true,
		WithIdentity(pb.NewAuthServiceClient(conn), pb.NewUserServiceClient(conn)),
		WithTokenBlacklist(blacklist),
	)
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func loginToken(t *testing.T, h http.Handler, username, password string) string {
	t.Helper()
	w := call(t, h, http.MethodPost, "/v1/auth/login", "", map[string]string{"username": username, "password": password})
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: status=%d body=%s", username, w.Code, w.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.AccessToken == "" {
		t.Fatalf("login response missing token: %v %s", err, w.Body.String())
	}
	return resp.AccessToken
}

func TestIdentityAnonymousCannotManageUsers(t *testing.T) {
	h := newIdentityGateway(t).Router()
	if w := call(t, h, http.MethodGet, "/v1/users", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("anonymous GET /v1/users = %d, want 403", w.Code)
	}
}

func TestIdentityLogoutRevokesAccessToken(t *testing.T) {
	h := newIdentityGateway(t).Router()
	token := loginToken(t, h, "admin", "admin123")

	if w := call(t, h, http.MethodGet, "/v1/me", token, nil); w.Code != http.StatusOK {
		t.Fatalf("me before logout: %d %s", w.Code, w.Body.String())
	}
	if w := call(t, h, http.MethodPost, "/v1/auth/logout", token, nil); w.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	// The same access token must be rejected once it is on the blacklist.
	if w := call(t, h, http.MethodGet, "/v1/me", token, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", w.Code)
	}
}

func TestIdentityAdminLoginAndUserCRUD(t *testing.T) {
	h := newIdentityGateway(t).Router()
	admin := loginToken(t, h, "admin", "admin123")

	// /v1/me carries the profile from user-service.
	w := call(t, h, http.MethodGet, "/v1/me", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("me: %d %s", w.Code, w.Body.String())
	}
	var me struct {
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me.Username != "admin" || len(me.Roles) == 0 || me.Roles[0] != "admin" {
		t.Errorf("me = %+v, want username=admin role=admin", me)
	}

	// Create an editor user.
	w = call(t, h, http.MethodPost, "/v1/users", admin, map[string]any{
		"username": "bob", "password": "bob12345", "roles": []string{"editor"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create user: %d %s", w.Code, w.Body.String())
	}

	// List reflects admin + bob.
	w = call(t, h, http.MethodGet, "/v1/users", admin, nil)
	var list struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if list.Total != 2 {
		t.Errorf("user total = %d, want 2", list.Total)
	}

	// Roles endpoint returns the three built-ins.
	w = call(t, h, http.MethodGet, "/v1/roles", admin, nil)
	var roles struct {
		Roles []struct {
			Name string `json:"name"`
		} `json:"roles"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &roles)
	if len(roles.Roles) != 3 {
		t.Errorf("roles = %d, want 3", len(roles.Roles))
	}
}

func TestIdentityEditorReadButCannotWrite(t *testing.T) {
	h := newIdentityGateway(t).Router()
	admin := loginToken(t, h, "admin", "admin123")
	call(t, h, http.MethodPost, "/v1/users", admin, map[string]any{
		"username": "bob", "password": "bob12345", "roles": []string{"editor"},
	})

	editor := loginToken(t, h, "bob", "bob12345")
	if w := call(t, h, http.MethodGet, "/v1/users", editor, nil); w.Code != http.StatusOK {
		t.Errorf("editor GET /v1/users = %d, want 200", w.Code)
	}
	if w := call(t, h, http.MethodPost, "/v1/users", editor, map[string]any{
		"username": "x", "password": "xxxxxxxx",
	}); w.Code != http.StatusForbidden {
		t.Errorf("editor POST /v1/users = %d, want 403", w.Code)
	}
}

func TestIdentityAuthErrorMapping(t *testing.T) {
	h := newIdentityGateway(t).Router()

	if w := call(t, h, http.MethodPost, "/v1/auth/login", "", map[string]string{
		"username": "admin", "password": "wrong",
	}); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d, want 401", w.Code)
	}

	// Self-registration assigns the viewer role.
	w := call(t, h, http.MethodPost, "/v1/auth/register", "", map[string]string{
		"username": "carol", "password": "carol123",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	var reg struct {
		User struct {
			Roles []string `json:"roles"`
		} `json:"user"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &reg)
	if len(reg.User.Roles) != 1 || reg.User.Roles[0] != "viewer" {
		t.Errorf("registered roles = %v, want [viewer]", reg.User.Roles)
	}

	// Deleting a built-in role is rejected (422 FailedPrecondition).
	admin := loginToken(t, h, "admin", "admin123")
	if w := call(t, h, http.MethodDelete, "/v1/roles/admin", admin, nil); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("delete builtin role = %d, want 422", w.Code)
	}
}
