package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nexusflow/nexusflow/backend/internal/feedback"
	"github.com/nexusflow/nexusflow/backend/internal/identity"
	"github.com/nexusflow/nexusflow/backend/internal/metering"
	"github.com/nexusflow/nexusflow/backend/internal/platform/metrics"
	"github.com/nexusflow/nexusflow/backend/internal/platform/tracing"
	"github.com/nexusflow/nexusflow/backend/internal/revocation"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxUploadBytes = 100 << 20 // 100 MiB

// allowedUploadTypes mirrors the parsing stage's accepted formats. Validate
// before storing so rejected uploads never leave orphan objects behind.
var allowedUploadTypes = map[string]struct{}{
	"pdf": {}, "docx": {}, "txt": {}, "md": {}, "html": {},
}

type Gateway struct {
	ingestion      pb.IngestionServiceClient
	query          pb.QueryServiceClient
	sandbox        pb.SandboxExecutionServiceClient
	auth           pb.AuthServiceClient
	users          pb.UserServiceClient
	knowledgeBases pb.KnowledgeBaseServiceClient
	objects        store.ObjectStore
	logger         *slog.Logger
	secret         string
	allowAnon      bool
	tracer         tracing.Tracer
	// feedbackStore persists user ratings independently of tracing.
	feedbackStore feedback.Store
	// rag proxies chat/conversation calls to the Python RAG agent.
	rag *ragProxy
	// workflow proxies DAG validate/run calls to the workflow engine.
	workflow *workflowProxy
	// blacklist rejects revoked access tokens at the edge.
	blacklist revocation.Blacklist
	// allowedUploadHosts restricts presigned upload URLs; empty disables the check.
	allowedUploadHosts map[string]struct{}
	// metering records storage/query consumption and enforces the quota.
	metering metering.Store
	quota    metering.Quota
}

// Option configures a Gateway at construction time.
type Option func(*Gateway)

// WithAllowedUploadHosts restricts presigned upload URLs to the given hosts to
// mitigate SSRF. An empty list disables the check.
func WithAllowedUploadHosts(hosts []string) Option {
	return func(g *Gateway) {
		if len(hosts) == 0 {
			return
		}
		g.allowedUploadHosts = make(map[string]struct{}, len(hosts))
		for _, h := range hosts {
			g.allowedUploadHosts[h] = struct{}{}
		}
	}
}

// WithTracer attaches the observability tracer (feedback scores + trace viewer).
func WithTracer(t tracing.Tracer) Option {
	return func(g *Gateway) { g.tracer = t }
}

// WithRAGAgent enables the RAG chat endpoints by pointing the gateway at the
// Python rag-agent service. An empty URL leaves chat disabled.
func WithRAGAgent(rawURL string, serviceToken ...string) Option {
	return func(g *Gateway) { g.rag = newRAGProxy(rawURL, serviceToken...) }
}

// WithWorkflow enables the DAG validate/run proxy. The service token is
// attached to upstream calls so the engine can reject direct access.
func WithWorkflow(rawURL, serviceToken string) Option {
	return func(g *Gateway) { g.workflow = newWorkflowProxy(rawURL, serviceToken) }
}

// WithIdentity attaches the authentication and user-management gRPC clients.
func WithIdentity(auth pb.AuthServiceClient, users pb.UserServiceClient) Option {
	return func(g *Gateway) {
		g.auth = auth
		g.users = users
	}
}

// WithKnowledgeBases attaches the tenant-scoped knowledge-base client used by
// both management endpoints and file/retrieval authorization checks.
func WithKnowledgeBases(client pb.KnowledgeBaseServiceClient) Option {
	return func(g *Gateway) { g.knowledgeBases = client }
}

// WithFeedbackStore persists ratings and implicit feedback for review.
func WithFeedbackStore(s feedback.Store) Option {
	return func(g *Gateway) {
		if s != nil {
			g.feedbackStore = s
		}
	}
}

// WithMetering attaches the tenant resource meter and its quota policy.
func WithMetering(store metering.Store, quota metering.Quota) Option {
	return func(g *Gateway) {
		if store != nil {
			g.metering = store
		}
		g.quota = quota
	}
}

// WithTokenBlacklist enables access-token revocation checks on every request.
func WithTokenBlacklist(b revocation.Blacklist) Option {
	return func(g *Gateway) {
		if b != nil {
			g.blacklist = b
		}
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
		ingestion:     ingestion,
		query:         query,
		sandbox:       sandbox,
		objects:       objects,
		logger:        logger,
		secret:        secret,
		allowAnon:     allowAnon,
		tracer:        tracing.NoopTracer{},
		feedbackStore: feedback.NewMemoryStore(),
		metering:      metering.NewMemoryStore(),
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

	v1 := r.Group("/v1", Auth(g.secret, g.allowAnon, g.blacklist), RateLimit(50, 100))
	v1.POST("/auth/logout", g.logout)
	v1.POST("/files", RequirePermission("file:write"), g.uploadFile)
	// Cascade delete: object storage + retrieval index + task record.
	v1.DELETE("/files/:id", RequirePermission("file:delete"), g.deleteFile)
	// In-place index rebuild from the persisted parsed markdown.
	v1.POST("/files/:id/reindex", RequirePermission("task:write"), g.reindexFile)
	v1.GET("/tasks", g.listTasks)
	v1.GET("/tasks/:id", g.getTask)
	v1.DELETE("/tasks/:id", g.deleteTask)
	v1.POST("/tasks/:id/retry", g.retryTask)
	v1.POST("/search", RequirePermission("search:use"), g.search)
	// Tenant usage and quota snapshot for the console overview.
	v1.GET("/usage", RequirePermission("usage:read"), g.usage)
	// RAG chat: multi-turn Q&A over the retrieval corpus, scoped to the caller.
	v1.POST("/chat", RequirePermission("search:use"), g.chat)
	v1.POST("/chat/stream", RequirePermission("search:use"), g.chatStream)
	v1.GET("/conversations", RequirePermission("search:use"), g.listConversations)
	v1.GET("/conversations/:id", RequirePermission("search:use"), g.getConversation)
	v1.POST("/feedback", g.submitFeedback)
	v1.GET("/feedback", RequirePermission("user:read"), g.listFeedback)
	// Historical human corrections consumed by rag-agent before retrieval.
	v1.GET("/corrected-answers", RequirePermission("search:use"), g.listCorrectedAnswers)
	v1.GET("/traces", g.listTraces)
	v1.GET("/traces/:id", g.getTrace)
	// Custom SubAgent invocation (proxied to the hub /v1/call). Sandbox agents
	// additionally require sandbox:execute on their own execution request.
	v1.POST("/agents/:name/call", RequirePermission("search:use"), g.callAgent)
	// Human-in-the-loop approvals and audit (proxied to the Agent Hub).
	v1.GET("/approvals", RequirePermission("workflow:run"), g.listApprovals)
	v1.GET("/approvals/:id", RequirePermission("workflow:run"), g.getApproval)
	v1.POST("/approvals/:id/decide", RequirePermission("workflow:run"), g.decideApproval)
	v1.GET("/audit", RequirePermission("trace:read"), g.listAudit)
	v1.POST("/sandbox/execute", RequirePermission("sandbox:execute"), g.executeSandbox)
	// Workflow DAG validation and execution (proxied to the Go engine).
	v1.POST("/workflows/validate", RequirePermission("workflow:run"), g.workflowValidate)
	v1.POST("/workflows/run", RequirePermission("workflow:run"), g.workflowRun)
	// Persisted runs and human-in-the-loop approval.
	v1.GET("/workflows/runs", RequirePermission("workflow:run"), g.workflowRuns)
	v1.GET("/workflows/runs/:id", RequirePermission("workflow:run"), g.workflowRunDetail)
	v1.POST("/workflows/runs/:id/approve", RequirePermission("workflow:run"), g.workflowApprove)
	v1.POST("/workflows/runs/:id/resume", RequirePermission("workflow:run"), g.workflowResume)
	v1.GET("/me", g.me)

	// Knowledge-base management and namespace discovery.
	v1.GET("/knowledge-bases", RequirePermission("kb:read"), g.listKnowledgeBases)
	v1.POST("/knowledge-bases", RequirePermission("kb:write"), g.createKnowledgeBase)
	// Namespace management is authorized per KB inside the service (kb:admin),
	// so a kb-level admin works even without the tenant-wide kb:write permission.
	v1.PATCH("/knowledge-bases/:id", RequirePermission("kb:read"), g.updateKnowledgeBase)
	v1.DELETE("/knowledge-bases/:id", RequirePermission("kb:read"), g.deleteKnowledgeBase)
	// kb-scoped RBAC bindings (the service enforces kb:admin on the target KB).
	v1.GET("/knowledge-bases/:id/permissions", RequirePermission("kb:read"), g.listKnowledgeBasePermissions)
	v1.POST("/knowledge-bases/:id/permissions", RequirePermission("kb:read"), g.grantKnowledgeBasePermission)
	v1.DELETE("/knowledge-bases/:id/permissions", RequirePermission("kb:read"), g.revokeKnowledgeBasePermission)

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
	KBID         string            `json:"kb_id"`
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
	if !g.presignedURLAllowed(body.PresignedURL) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "presigned_url host not allowed"})
		return
	}
	kbID, role, ok := g.resolveKnowledgeBase(c, body.KBID)
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleEditor) {
		return
	}
	if !g.allowUpload(c, body.FileSize) {
		return
	}
	// Reserved metadata keys are server-owned: drop client-provided
	// object_key/file_url and derive them from explicit fields only, so a
	// caller cannot point deletes/retries at arbitrary objects.
	metadata := map[string]string{}
	for k, v := range body.Metadata {
		metadata[k] = v
	}
	delete(metadata, "object_key")
	delete(metadata, "file_url")
	delete(metadata, "kb_id")
	metadata["kb_id"] = kbID
	if body.PresignedURL != "" {
		metadata["file_url"] = body.PresignedURL
	}
	metadata["file_name"] = body.FileName
	metadata["file_type"] = body.FileType
	if body.FileSize > 0 {
		metadata["file_size"] = strconv.FormatInt(body.FileSize, 10)
	}

	ctx, p := g.callerContext(c)
	resp, err := g.ingestion.UploadFile(ctx, &pb.UploadFileRequest{
		FileName:     body.FileName,
		FileType:     body.FileType,
		FileSize:     body.FileSize,
		PresignedUrl: body.PresignedURL,
		Metadata:     metadata,
		KbId:         kbID,
	})
	if err != nil {
		g.logger.Error("upload failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	if resp.GetCreated() {
		g.recordUpload(c, kbID, body.FileSize)
	}
	c.JSON(http.StatusAccepted, gin.H{
		"task_id": resp.GetTaskId(),
		"status":  resp.GetStatus(),
		"message": resp.GetMessage(),
		"created": resp.GetCreated(),
	})
}

// uploadMultipart streams the file into object storage, computes a content
// hash for idempotency, then registers the ingestion task.
func (g *Gateway) uploadMultipart(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBytes)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file exceeds size limit"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing form field 'file': " + err.Error()})
		return
	}
	defer file.Close()

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(header.Filename), "."))
	if ext == "" {
		ext = "txt"
	}
	if _, ok := allowedUploadTypes[ext]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported file type: " + ext})
		return
	}
	if header.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty file"})
		return
	}

	kbID, role, ok := g.resolveKnowledgeBase(c, c.Request.PostForm.Get("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleEditor) {
		return
	}
	if !g.allowUpload(c, header.Size) {
		return
	}
	principal, _ := tenant.From(c.Request.Context())
	uploadID := uuid.NewString()
	objectKey := fmt.Sprintf("%s/%s/upload/%s/%s", principal.TenantID, kbID, uploadID, header.Filename)

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

	// User-provided metadata first; reserved keys are written last so a client
	// cannot spoof object_key / file_url / content_hash.
	metadata := map[string]string{}
	for key, vals := range c.Request.PostForm {
		if key == "file" || len(vals) == 0 {
			continue
		}
		metadata[key] = vals[0]
	}
	metadata["file_name"] = header.Filename
	metadata["file_type"] = ext
	metadata["file_size"] = strconv.FormatInt(header.Size, 10)
	metadata["content_hash"] = contentHash
	metadata["object_key"] = objectKey
	metadata["kb_id"] = kbID

	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.UploadFile(ctx, &pb.UploadFileRequest{
		FileName:    header.Filename,
		FileType:    ext,
		FileSize:    header.Size,
		ObjectKey:   objectKey,
		ContentHash: contentHash,
		Metadata:    metadata,
		KbId:        kbID,
	})
	if err != nil {
		g.logger.Error("upload register failed", "tenant", principal.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	body := gin.H{
		"task_id":      resp.GetTaskId(),
		"status":       resp.GetStatus(),
		"message":      resp.GetMessage(),
		"content_hash": contentHash,
		"created":      resp.GetCreated(),
	}
	if resp.GetCreated() {
		body["object_key"] = objectKey
		g.recordUpload(c, kbID, header.Size)
	} else if delErr := g.objects.Delete(ctx, objectKey); delErr != nil {
		// Content dedup matched an existing task: the object we just stored is
		// unreferenced, so remove it rather than leave an orphan.
		g.logger.Warn("delete orphan object failed", "key", objectKey, "error", delErr)
	}
	c.JSON(http.StatusAccepted, body)
}

// presignedURLAllowed validates a client-supplied upload URL against the host
// allowlist, preventing SSRF to arbitrary endpoints.
func (g *Gateway) presignedURLAllowed(raw string) bool {
	if raw == "" || len(g.allowedUploadHosts) == 0 {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	_, ok := g.allowedUploadHosts[u.Host]
	return ok
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
	kbID, role, ok := g.resolveKnowledgeBase(c, c.Query("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleViewer) {
		return
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.ListTasks(ctx, &pb.ListTasksRequest{
		Status: c.Query("status"),
		Limit:  int32(limit),
		Offset: int32(offset),
		KbId:   kbID,
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
			"kb_id":        t.GetKbId(),
		})
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "total": resp.GetTotal()})
}

func (g *Gateway) getTask(c *gin.Context) {
	kbID, role, ok := g.resolveKnowledgeBase(c, c.Query("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleViewer) {
		return
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: c.Param("id"), KbId: kbID})
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
		"kb_id":        resp.GetKbId(),
	})
}

func (g *Gateway) deleteTask(c *gin.Context) {
	kbID, role, ok := g.resolveKnowledgeBase(c, c.Query("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleEditor) {
		return
	}
	ctx, _ := g.callerContext(c)
	// Read the task first so the cascade delete can release its storage quota.
	var prior *pb.GetTaskStatusResponse
	if st, stErr := g.ingestion.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: c.Param("id"), KbId: kbID}); stErr == nil {
		prior = st
	}
	resp, err := g.ingestion.DeleteTask(ctx, &pb.DeleteTaskRequest{TaskId: c.Param("id"), KbId: kbID})
	if err != nil {
		code, msg := grpcToHTTP(err)
		c.JSON(code, gin.H{"error": msg})
		return
	}
	if prior != nil && resp.GetObjectDeleted() {
		g.releaseUpload(c, resp.GetKbId(), prior.GetFileSize())
	}
	c.JSON(http.StatusOK, gin.H{
		"success":        resp.GetSuccess(),
		"task_id":        c.Param("id"),
		"kb_id":          resp.GetKbId(),
		"object_deleted": resp.GetObjectDeleted(),
		"index_deleted":  resp.GetIndexDeleted(),
	})
}

// deleteFile exposes the same cascade cleanup on the file-oriented URL, which
// is what the console uses when removing an uploaded document.
func (g *Gateway) deleteFile(c *gin.Context) {
	g.deleteTask(c)
}

// reindexFile rebuilds a completed document's index in place without
// re-parsing the source file.
func (g *Gateway) reindexFile(c *gin.Context) {
	kbID, role, ok := g.resolveKnowledgeBase(c, c.Query("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleEditor) {
		return
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.ReindexTask(ctx, &pb.ReindexTaskRequest{TaskId: c.Param("id"), KbId: kbID})
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

func (g *Gateway) retryTask(c *gin.Context) {
	kbID, role, ok := g.resolveKnowledgeBase(c, c.Query("kb_id"))
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleEditor) {
		return
	}
	ctx, _ := g.callerContext(c)
	resp, err := g.ingestion.RetryTask(ctx, &pb.RetryTaskRequest{TaskId: c.Param("id"), KbId: kbID})
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
	kbID, role, ok := g.resolveKnowledgeBase(c, body.KBID)
	if !ok {
		return
	}
	if !requireKBRole(c, role, identity.KBRoleViewer) {
		return
	}
	if !g.allowQuery(c) {
		return
	}
	routes := make([]*pb.RecallRoute, 0, len(body.Routes))
	for _, r := range body.Routes {
		routes = append(routes, &pb.RecallRoute{Type: r.Type, TopK: r.TopK, FilterExpr: r.Filters})
	}

	filters := make(map[string]string, len(body.Filters)+2)
	for key, value := range body.Filters {
		if key == "kb_id" || key == "tenant_id" {
			continue
		}
		filters[key] = value
	}
	ctx, principal := g.callerContext(c)
	filters["kb_id"] = kbID
	filters["tenant_id"] = principal.TenantID
	resp, err := g.query.Search(ctx, &pb.SearchRequest{
		Query:       body.Query,
		Routes:      routes,
		RerankModel: body.Rerank,
		FinalTopN:   body.FinalTopN,
		KbId:        kbID,
		Filters:     filters,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	g.recordQuery(c)

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
	TraceID         string            `json:"trace_id"`
	MessageID       string            `json:"message_id"`
	ConversationID  string            `json:"conversation_id"`
	Name            string            `json:"name"`
	Value           *float64          `json:"value"`
	Comment         string            `json:"comment"`
	CorrectedAnswer string            `json:"corrected_answer"`
	Query           string            `json:"query"`
	Answer          string            `json:"answer"`
	CitedChunkIDs   []string          `json:"cited_chunk_ids"`
	Meta            map[string]string `json:"meta"`
}

// submitFeedback persists a user evaluation (rating or implicit signal) and,
// as a best-effort side channel, exports it as a trace score. Persistence is
// independent from tracing so feedback survives when tracing is a no-op.
func (g *Gateway) submitFeedback(c *gin.Context) {
	var body feedbackReq
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if body.TraceID == "" && body.MessageID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trace_id or message_id is required"})
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "user.rating"
	}
	value := 1.0
	if body.Value != nil {
		value = *body.Value
	} else if name == "user.rating" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "value is required for user.rating"})
		return
	}

	subject := body.MessageID
	if subject == "" {
		subject = body.TraceID
	}
	p, _ := tenant.From(c.Request.Context())

	entry := feedback.Entry{
		TenantID:        p.TenantID,
		UserID:          p.UserID,
		SubjectID:       subject,
		TraceID:         body.TraceID,
		ConversationID:  body.ConversationID,
		MessageID:       body.MessageID,
		Name:            name,
		Rating:          value,
		Comment:         body.Comment,
		CorrectedAnswer: body.CorrectedAnswer,
		Query:           body.Query,
		Answer:          body.Answer,
		CitedChunkIDs:   body.CitedChunkIDs,
		Meta:            body.Meta,
	}
	saved, err := g.feedbackStore.Upsert(c.Request.Context(), entry)
	if err != nil {
		g.logger.Error("persist feedback failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	if body.TraceID != "" {
		ctx, _ := g.callerContext(c)
		if err := g.tracer.AddScore(ctx, body.TraceID, name, value, body.Comment); err != nil {
			g.logger.Warn("export feedback score failed", "trace", body.TraceID, "error", err)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"id":         saved.ID,
		"subject_id": saved.SubjectID,
		"updated_at": saved.UpdatedAt,
	})
}

// listFeedback returns tenant-scoped feedback for review, e.g.
// GET /v1/feedback?rating=-1&limit=20.
func (g *Gateway) listFeedback(c *gin.Context) {
	p, _ := tenant.From(c.Request.Context())
	filter := feedback.Filter{TenantID: p.TenantID}
	if raw := c.Query("rating"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid rating"})
			return
		}
		filter.Rating = &v
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid limit"})
			return
		}
		filter.Limit = n
	}
	if raw := c.Query("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid offset"})
			return
		}
		filter.Offset = n
	}

	items, err := g.feedbackStore.List(c.Request.Context(), filter)
	if err != nil {
		g.logger.Error("list feedback failed", "tenant", p.TenantID, "error", err)
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"feedback": items, "count": len(items)})
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
