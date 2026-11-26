package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/platform/tracing"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxUploadBytes = 100 << 20 // 100 MiB

type Gateway struct {
	ingestion pb.IngestionServiceClient
	query     pb.QueryServiceClient
	sandbox   pb.SandboxExecutionServiceClient
	auth      pb.AuthServiceClient
	users     pb.UserServiceClient
	objects   store.ObjectStore
	logger    *slog.Logger
	secret    string
	allowAnon bool
	tracer    tracing.Tracer
}

// Option configures a Gateway at construction time.
type Option func(*Gateway)

// WithTracer attaches the observability tracer (feedback scores + trace viewer).
func WithTracer(t tracing.Tracer) Option {
	return func(g *Gateway) { g.tracer = t }
}

// WithIdentity attaches the authentication and user-management gRPC clients.
func WithIdentity(auth pb.AuthServiceClient, users pb.UserServiceClient) Option {
	return func(g *Gateway) {
		g.auth = auth
		g.users = users
	}
}

func New(
	ingestion pb.IngestionServiceClient,
	query pb.QueryServiceClient,
	sandbox pb.SandboxExecutionServiceClient,
	objects store.ObjectStore,
	logger *slog.Logger,
	secret string,
	allowAnon bool,
	opts ...Option,
) *Gateway {
	g := &Gateway{
		ingestion: ingestion,
		query:     query,
		sandbox:   sandbox,
		objects:   objects,
		logger:    logger,
		secret:    secret,
		allowAnon: allowAnon,
		tracer:    tracing.NoopTracer{},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func (g *Gateway) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), Trace())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	// Public authentication endpoints: no bearer token required.
	pub := r.Group("/v1/auth")
	pub.POST("/login", g.login)
	pub.POST("/register", g.register)
	pub.POST("/refresh", g.refresh)

	v1 := r.Group("/v1", Auth(g.secret, g.allowAnon), RateLimit(50, 100))
	v1.POST("/auth/logout", g.logout)
	v1.POST("/files", g.uploadFile)
	v1.GET("/tasks", g.listTasks)
	v1.GET("/tasks/:id", g.getTask)
	v1.DELETE("/tasks/:id", g.deleteTask)
	v1.POST("/tasks/:id/retry", g.retryTask)
	v1.POST("/search", g.search)
	v1.POST("/feedback", g.submitFeedback)
	v1.GET("/traces", g.listTraces)
	v1.GET("/traces/:id", g.getTrace)
	v1.POST("/sandbox/execute", g.executeSandbox)
	v1.GET("/me", g.me)

	// User & RBAC management.
	v1.GET("/users", RequirePermission("user:read"), g.listUsers)
	v1.POST("/users", RequirePermission("user:write"), g.createUser)
	v1.GET("/users/:id", RequirePermission("user:read"), g.getUser)
	v1.PATCH("/users/:id", RequirePermission("user:write"), g.updateUser)
	v1.DELETE("/users/:id", RequirePermission("user:delete"), g.deleteUser)
	v1.GET("/roles", RequirePermission("role:read"), g.listRoles)
	v1.POST("/roles", RequirePermission("role:write"), g.upsertRole)
	v1.DELETE("/roles/:name", RequirePermission("role:write"), g.deleteRole)
	v1.GET("/permissions", RequirePermission("role:read"), g.listPermissions)

	return r
}

// publicContext propagates a trace id for unauthenticated endpoints without
// injecting a tenant principal (so the request body decides the tenant).
func (g *Gateway) publicContext(c *gin.Context) context.Context {
	ctx := c.Request.Context()
	id := c.GetHeader("x-trace-id")
	if id == "" {
		id = tracing.NewID()
	}
	ctx = tracing.ToGRPC(ctx, id)
	return tracing.WithTraceID(ctx, id)
}

func (g *Gateway) callerContext(c *gin.Context) (context.Context, tenant.Principal) {
	p, _ := tenant.From(c.Request.Context())
	ctx := tenant.ToOutgoing(c.Request.Context(), p)
	// Propagate (or seed) a trace id onto outgoing gRPC metadata so downstream
	// services continue the same trace.
	id := c.GetHeader("x-trace-id")
	if id == "" {
		id = tracing.NewID()
	}
	ctx = tracing.ToGRPC(ctx, id)
	ctx = tracing.WithTraceID(ctx, id)
	return ctx, p
}

type uploadReq struct {
	FileName     string            `json:"file_name" binding:"required"`
	FileType     string            `json:"file_type" binding:"required"`
	FileSize     int64             `json:"file_size"`
	PresignedURL string            `json:"presigned_url"`
	Metadata     map[string]string `json:"metadata"`
}

func (g *Gateway) uploadFile(c *gin.Context) {
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		g.uploadMultipart(c)
		return
	}
	g.uploadJSON(c)
}

// uploadJSON keeps the presigned-URL / metadata-only path for clients that
// upload bytes directly to object storage (e.g. browser S3 put).
func (g *Gateway) uploadJSON(c *gin.Context) {
	var body uploadReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	ctx, p := g.callerContext(c)
	resp, err := g.ingestion.UploadFile(ctx, &pb.UploadFileRequest{
		FileName:     body.FileName,
		FileType:     body.FileType,
		FileSize:     body.FileSize,
		PresignedUrl: body.PresignedURL,
		Metadata:     body.Metadata,
	})
	if err != nil {
		g.logger.Error("upload failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"task_id": resp.GetTaskId(),
		"status":  resp.GetStatus(),
		"message": resp.GetMessage(),
	})
}

// uploadMultipart streams the file into object storage, computes a content
// hash for idempotency, then registers the ingestion task.
func (g *Gateway) uploadMultipart(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing form field 'file': " + err.Error()})
		return
	}
	defer file.Close()

	principal, _ := tenant.From(c.Request.Context())
	uploadID := uuid.NewString()
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	if ext == "" {
		ext = "txt"
	}
	objectKey := fmt.Sprintf("%s/upload/%s/%s", principal.TenantID, uploadID, header.Filename)

	hasher := sha256.New()
	if err := g.objects.Put(
		c.Request.Context(),
		objectKey,
		io.TeeReader(file, hasher),
		header.Size,
		header.Header.Get("Content-Type"),
	); err != nil {
		g.logger.Error("store object failed", "tenant", principal.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "store object: " + err.Error()})
		return
	}
	contentHash := hex.EncodeToString(hasher.Sum(nil))

	metadata := map[string]string{
		"file_name":    header.Filename,
		"file_type":    ext,
		"file_size":    strconv.FormatInt(header.Size, 10),
		"content_hash": contentHash,
		"object_key":   objectKey,
	}
	for key, vals := range c.Request.PostForm {
		if key == "file" || len(vals) == 0 {
			continue
		}
		metadata[key] = vals[0]
	}

	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.UploadFile(ctx, &pb.UploadFileRequest{
		FileName:    header.Filename,
		FileType:    ext,
		FileSize:    header.Size,
		ObjectKey:   objectKey,
		ContentHash: contentHash,
		Metadata:    metadata,
	})
	if err != nil {
		g.logger.Error("upload register failed", "tenant", principal.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"task_id":      resp.GetTaskId(),
		"status":       resp.GetStatus(),
		"message":      resp.GetMessage(),
		"content_hash": contentHash,
		"object_key":   objectKey,
	})
}

func (g *Gateway) listTasks(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.ListTasks(ctx, &pb.ListTasksRequest{
		Status: c.Query("status"),
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	tasks := make([]gin.H, 0, len(resp.GetTasks()))
	for _, t := range resp.GetTasks() {
		tasks = append(tasks, gin.H{
			"task_id":      t.GetTaskId(),
			"status":       t.GetStatus(),
			"progress":     t.GetProgress(),
			"file_name":    t.GetFileName(),
			"file_type":    t.GetFileType(),
			"file_size":    t.GetFileSize(),
			"content_hash": t.GetContentHash(),
			"error_msg":    t.GetErrorMsg(),
			"created_at":   t.GetCreatedAt(),
			"updated_at":   t.GetUpdatedAt(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "total": resp.GetTotal()})
}

func (g *Gateway) getTask(c *gin.Context) {
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: c.Param("id")})
	if err != nil {
		code, msg := grpcToHTTP(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":      resp.GetTaskId(),
		"status":       resp.GetStatus(),
		"progress":     resp.GetProgress(),
		"error_msg":    resp.GetErrorMsg(),
		"content_hash": resp.GetContentHash(),
		"metadata":     resp.GetMetadata(),
		"file_name":    resp.GetFileName(),
		"file_type":    resp.GetFileType(),
		"file_size":    resp.GetFileSize(),
		"created_at":   resp.GetCreatedAt(),
		"updated_at":   resp.GetUpdatedAt(),
	})
}

func (g *Gateway) deleteTask(c *gin.Context) {
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.DeleteTask(ctx, &pb.DeleteTaskRequest{TaskId: c.Param("id")})
	if err != nil {
		code, msg := grpcToHTTP(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": resp.GetSuccess()})
}

func (g *Gateway) retryTask(c *gin.Context) {
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.RetryTask(ctx, &pb.RetryTaskRequest{TaskId: c.Param("id")})
	if err != nil {
		code, msg := grpcToHTTP(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"task_id": resp.GetTaskId(),
		"status":  resp.GetStatus(),
		"message": resp.GetMessage(),
	})
}

// grpcToHTTP maps gRPC status codes to sensible HTTP statuses and extracts a
// clean message, falling back to 502 for transport-level failures.
func grpcToHTTP(err error) (int, string) {
	if err == nil {
		return http.StatusOK, ""
	}
	msg := err.Error()
	if s, ok := status.FromError(err); ok {
		if m := s.Message(); m != "" {
			msg = m
		}
		switch s.Code() {
		case codes.NotFound:
			return http.StatusNotFound, msg
		case codes.FailedPrecondition, codes.InvalidArgument:
			return http.StatusBadRequest, msg
		case codes.PermissionDenied:
			return http.StatusForbidden, msg
		case codes.Unauthenticated:
			return http.StatusUnauthorized, msg
		}
	}
	return http.StatusBadGateway, msg
}

type routeReq struct {
	Type    string `json:"type"`
	TopK    int32  `json:"top_k"`
	Filters string `json:"filter_expr"`
}

type searchReq struct {
	Query     string            `json:"query" binding:"required"`
	Routes    []routeReq        `json:"routes"`
	Rerank    string            `json:"rerank_model"`
	FinalTopN int32             `json:"final_top_n"`
	KBID      string            `json:"kb_id"`
	Filters   map[string]string `json:"filters"`
}

func (g *Gateway) search(c *gin.Context) {
	var body searchReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	routes := make([]*pb.RecallRoute, 0, len(body.Routes))
	for _, r := range body.Routes {
		routes = append(routes, &pb.RecallRoute{Type: r.Type, TopK: r.TopK, FilterExpr: r.Filters})
	}

	ctx, _ := g.callerContext(c)
	resp, err := g.query.Search(ctx, &pb.SearchRequest{
		Query:       body.Query,
		Routes:      routes,
		RerankModel: body.Rerank,
		FinalTopN:   body.FinalTopN,
		KbId:        body.KBID,
		Filters:     body.Filters,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	chunks := make([]gin.H, 0, len(resp.GetChunks()))
	for _, ch := range resp.GetChunks() {
		chunks = append(chunks, gin.H{
			"chunk_id": ch.GetChunkId(),
			"content":  ch.GetContent(),
			"score":    ch.GetScore(),
			"meta":     ch.GetMeta(),
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"chunks":         chunks,
		"trace_id":       resp.GetTraceId(),
		"recall_count":   resp.GetRecallCount(),
		"rerank_applied": resp.GetRerankApplied() == 1,
	})
}

func (g *Gateway) memoryTracer() (*tracing.MemoryTracer, bool) {
	m, ok := g.tracer.(*tracing.MemoryTracer)
	return m, ok
}

type feedbackReq struct {
	TraceID string  `json:"trace_id" binding:"required"`
	Name    string  `json:"name"`
	Value   float64 `json:"value" binding:"required"`
	Comment string  `json:"comment"`
}

// submitFeedback records a user evaluation (赞/踩, or any numeric score) onto
// a trace. With MemoryTracer it is visible in the trace viewer; with
// LangfuseTracer it is exported as a score observation.
func (g *Gateway) submitFeedback(c *gin.Context) {
	var body feedbackReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	name := body.Name
	if name == "" {
		name = "user.rating"
	}
	ctx, _ := g.callerContext(c)
	if err := g.tracer.AddScore(ctx, body.TraceID, name, body.Value, body.Comment); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (g *Gateway) listTraces(c *gin.Context) {
	m, ok := g.memoryTracer()
	if !ok {
		c.JSON(http.StatusOK, gin.H{"traces": []any{}, "enabled": false})
		return
	}
	traces := m.Traces()
	const limit = 50
	if len(traces) > limit {
		traces = traces[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"traces": traces, "enabled": true})
}

func (g *Gateway) getTrace(c *gin.Context) {
	m, ok := g.memoryTracer()
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "trace viewer disabled (set TRACING_MEMORY=true or configure LANGFUSE)"})
		return
	}
	tr, ok := m.Trace(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "trace not found"})
		return
	}
	c.JSON(http.StatusOK, tr)
}

type sandboxReq struct {
	Code           string            `json:"code" binding:"required"`
	Runtime        string            `json:"runtime"`
	Packages       []string          `json:"packages"`
	EnvVars        map[string]string `json:"env_vars"`
	InputJSON      string            `json:"input_json"`
	TimeoutSeconds int32             `json:"timeout_seconds"`
}

func (g *Gateway) executeSandbox(c *gin.Context) {
	var body sandboxReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rt := pb.Runtime_PYTHON_311
	if r := strings.ToLower(strings.TrimSpace(body.Runtime)); r == "node" || r == "nodejs" || r == "node:18" {
		rt = pb.Runtime_NODE_18
	}
	if body.TimeoutSeconds <= 0 {
		body.TimeoutSeconds = 30
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.sandbox.Execute(ctx, &pb.CodeExecuteRequest{
		TaskId:         "web-playground",
		NodeId:         "playground",
		Runtime:        rt,
		Code:           body.Code,
		Packages:       body.Packages,
		EnvVars:        body.EnvVars,
		InputJson:      body.InputJSON,
		TimeoutSeconds: body.TimeoutSeconds,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":     resp.GetSuccess(),
		"output_json": resp.GetOutputJson(),
		"stdout":      resp.GetStdout(),
		"stderr":      resp.GetStderr(),
		"sandbox_id":  resp.GetSandboxId(),
		"error_msg":   resp.GetErrorMsg(),
	})
}

func (g *Gateway) me(c *gin.Context) {
	ctx, p := g.callerContext(c)
	resp := gin.H{
		"tenant_id":   p.TenantID,
		"user_id":     p.UserID,
		"roles":       p.Roles,
		"departments": p.Departments,
	}
	// Enrich with profile fields when the caller is a real (non-anonymous) user.
	if g.users != nil && p.UserID != "" && p.UserID != "anonymous" {
		if u, err := g.users.GetUser(ctx, &pb.GetUserRequest{Id: p.UserID}); err == nil && u.GetUser() != nil {
			resp["username"] = u.User.Username
			resp["email"] = u.User.Email
			resp["status"] = u.User.Status
		}
	}
	c.JSON(http.StatusOK, resp)
}
