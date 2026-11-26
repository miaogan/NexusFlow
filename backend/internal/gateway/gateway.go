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
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

const maxUploadBytes = 100 << 20 // 100 MiB

type Gateway struct {
	ingestion pb.IngestionServiceClient
	query     pb.QueryServiceClient
	sandbox   pb.SandboxExecutionServiceClient
	objects   store.ObjectStore
	logger    *slog.Logger
	secret    string
	allowAnon bool
}

func New(
	ingestion pb.IngestionServiceClient,
	query pb.QueryServiceClient,
	sandbox pb.SandboxExecutionServiceClient,
	objects store.ObjectStore,
	logger *slog.Logger,
	secret string,
	allowAnon bool,
) *Gateway {
	return &Gateway{
		ingestion: ingestion,
		query:     query,
		sandbox:   sandbox,
		objects:   objects,
		logger:    logger,
		secret:    secret,
		allowAnon: allowAnon,
	}
}

func (g *Gateway) Router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), Trace())

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", gin.WrapH(metrics.Handler()))

	v1 := r.Group("/v1", Auth(g.secret, g.allowAnon), RateLimit(50, 100))
	v1.POST("/files", g.uploadFile)
	v1.GET("/tasks", g.listTasks)
	v1.GET("/tasks/:id", g.getTask)
	v1.POST("/search", g.search)
	v1.POST("/sandbox/execute", g.executeSandbox)
	v1.GET("/me", g.me)

	return r
}

func (g *Gateway) callerContext(c *gin.Context) (context.Context, tenant.Principal) {
	p, _ := tenant.From(c.Request.Context())
	return tenant.ToOutgoing(c.Request.Context(), p), p
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
		FileName:     header.Filename,
		FileType:     ext,
		FileSize:     header.Size,
		ObjectKey:    objectKey,
		ContentHash:  contentHash,
		Metadata:     metadata,
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
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":   resp.GetTaskId(),
		"status":    resp.GetStatus(),
		"progress":  resp.GetProgress(),
		"error_msg": resp.GetErrorMsg(),
	})
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
		"success":    resp.GetSuccess(),
		"output_json": resp.GetOutputJson(),
		"stdout":     resp.GetStdout(),
		"stderr":     resp.GetStderr(),
		"sandbox_id": resp.GetSandboxId(),
		"error_msg":  resp.GetErrorMsg(),
	})
}

func (g *Gateway) me(c *gin.Context) {
	_, p := g.callerContext(c)
	c.JSON(http.StatusOK, gin.H{
		"tenant_id":   p.TenantID,
		"user_id":     p.UserID,
		"roles":       p.Roles,
		"departments": p.Departments,
	})
}
