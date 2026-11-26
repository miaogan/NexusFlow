package gateway

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------- Auth (public) ----------

type loginReq struct {
	TenantID string `json:"tenant_id"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type registerReq struct {
	TenantID string `json:"tenant_id"`
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
	Email    string `json:"email"`
}

type refreshReq struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

func (g *Gateway) login(c *gin.Context) {
	if !g.requireAuth(c) {
		return
	}
	var body loginReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := g.auth.Login(g.publicContext(c), &pb.LoginRequest{
		TenantId: body.TenantID, Username: body.Username, Password: body.Password,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, loginBody(res))
}

func (g *Gateway) register(c *gin.Context) {
	if !g.requireAuth(c) {
		return
	}
	var body registerReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := g.auth.Register(g.publicContext(c), &pb.RegisterRequest{
		TenantId: body.TenantID, Username: body.Username, Password: body.Password, Email: body.Email,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": res.User})
}

func (g *Gateway) refresh(c *gin.Context) {
	if !g.requireAuth(c) {
		return
	}
	var body refreshReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	res, err := g.auth.Refresh(g.publicContext(c), &pb.RefreshRequest{RefreshToken: body.RefreshToken})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, loginBody(res))
}

func (g *Gateway) logout(c *gin.Context) {
	if !g.requireAuth(c) {
		return
	}
	var body refreshReq
	_ = c.ShouldBindJSON(&body) // body optional: logout is stateless
	ctx, _ := g.callerContext(c)
	if _, err := g.auth.Logout(ctx, &pb.LogoutRequest{RefreshToken: body.RefreshToken}); err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func loginBody(res *pb.LoginResponse) gin.H {
	h := gin.H{}
	if res.GetTokens() != nil {
		h["access_token"] = res.Tokens.AccessToken
		h["refresh_token"] = res.Tokens.RefreshToken
		h["expires_in"] = res.Tokens.ExpiresIn
	}
	if res.GetUser() != nil {
		h["user"] = res.User
	}
	return h
}

func (g *Gateway) requireAuth(c *gin.Context) bool {
	if g.auth == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth service not configured"})
		return false
	}
	return true
}

func (g *Gateway) requireUsers(c *gin.Context) bool {
	if g.users == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service not configured"})
		return false
	}
	return true
}

// ---------- Users ----------

type createUserReq struct {
	Username    string   `json:"username" binding:"required"`
	Password    string   `json:"password" binding:"required"`
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	Departments []string `json:"departments"`
}

type updateUserReq struct {
	Email       string   `json:"email"`
	Roles       []string `json:"roles"`
	Departments []string `json:"departments"`
	Status      string   `json:"status"`
	Password    string   `json:"password"`
}

func (g *Gateway) listUsers(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	ctx, _ := g.callerContext(c)
	res, err := g.users.ListUsers(ctx, &pb.ListUsersRequest{
		Query: c.Query("query"), Limit: int32(limit), Offset: int32(offset),
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": res.Users, "total": res.Total})
}

func (g *Gateway) createUser(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	var body createUserReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.CreateUser(ctx, &pb.CreateUserRequest{
		Username: body.Username, Password: body.Password, Email: body.Email,
		Roles: body.Roles, Departments: body.Departments,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": res.User})
}

func (g *Gateway) getUser(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.GetUser(ctx, &pb.GetUserRequest{Id: c.Param("id")})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": res.User})
}

func (g *Gateway) updateUser(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	var body updateUserReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.UpdateUser(ctx, &pb.UpdateUserRequest{
		Id: c.Param("id"), Email: body.Email, Roles: body.Roles,
		Departments: body.Departments, Status: body.Status, Password: body.Password,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": res.User})
}

func (g *Gateway) deleteUser(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	ctx, _ := g.callerContext(c)
	if _, err := g.users.DeleteUser(ctx, &pb.DeleteUserRequest{Id: c.Param("id")}); err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// ---------- Roles & permissions ----------

type upsertRoleReq struct {
	Name        string   `json:"name" binding:"required"`
	Permissions []string `json:"permissions"`
	Description string   `json:"description"`
}

func (g *Gateway) listRoles(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.ListRoles(ctx, &pb.ListRolesRequest{})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"roles": res.Roles})
}

func (g *Gateway) upsertRole(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	var body upsertRoleReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.UpsertRole(ctx, &pb.UpsertRoleRequest{
		Name: body.Name, Permissions: body.Permissions, Description: body.Description,
	})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"role": res.Role})
}

func (g *Gateway) deleteRole(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	ctx, _ := g.callerContext(c)
	if _, err := g.users.DeleteRole(ctx, &pb.DeleteRoleRequest{Name: c.Param("name")}); err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}

func (g *Gateway) listPermissions(c *gin.Context) {
	if !g.requireUsers(c) {
		return
	}
	ctx, _ := g.callerContext(c)
	res, err := g.users.ListPermissions(ctx, &pb.ListPermissionsRequest{})
	if err != nil {
		writeIdentityError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"permissions": res.Permissions})
}

func writeIdentityError(c *gin.Context, err error) {
	st, ok := status.FromError(err)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	code := http.StatusInternalServerError
	switch st.Code() {
	case codes.Unauthenticated:
		code = http.StatusUnauthorized
	case codes.PermissionDenied:
		code = http.StatusForbidden
	case codes.NotFound:
		code = http.StatusNotFound
	case codes.AlreadyExists:
		code = http.StatusConflict
	case codes.InvalidArgument:
		code = http.StatusBadRequest
	case codes.FailedPrecondition:
		code = http.StatusUnprocessableEntity
	}
	c.JSON(code, gin.H{"error": st.Message()})
}
