package ingestion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	"github.com/nexusflow/nexusflow/backend/internal/tenant"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

// IndexCleaner removes a document's chunks and graph nodes from the retrieval
// index. *indexing.LlamaIndexIndexer implements it in production deployments.
type IndexCleaner interface {
	DeleteDocument(ctx context.Context, docID, kbID, tenantID string) (int, error)
}

// Option configures a Service at construction time.
type Option func(*Service)

// WithIndexCleaner enables cascade deletion of a document's vectors and graph
// nodes when the file/task is removed.
func WithIndexCleaner(cleaner IndexCleaner) Option {
	return func(s *Service) { s.cleaner = cleaner }
}

// WithParsedTopic configures the topic used to re-enqueue a persisted parsed
// document for in-place reindexing.
func WithParsedTopic(topic string) Option {
	return func(s *Service) { s.parsedTopic = topic }
}

// Service implements the ingestion contract: register uploads, persist
// metadata and hand the task to the queue.
type Service struct {
	pb.UnimplementedIngestionServiceServer
	objects       store.ObjectStore
	machine       state.Store
	pub           queue.Publisher
	logger        *slog.Logger
	presignBase   string
	topicUploaded string
	parsedTopic   string
	cleaner       IndexCleaner
}

func NewService(objects store.ObjectStore, machine state.Store, pub queue.Publisher, logger *slog.Logger, presignBase, topicUploaded string, opts ...Option) *Service {
	s := &Service{
		objects:       objects,
		machine:       machine,
		pub:           pub,
		logger:        logger,
		presignBase:   strings.TrimRight(presignBase, "/"),
		topicUploaded: topicUploaded,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

type uploadedEvent struct {
	TaskID    string            `json:"task_id"`
	TenantID  string            `json:"tenant_id"`
	FileURL   string            `json:"file_url"`
	FileType  string            `json:"file_type"`
	FileName  string            `json:"file_name"`
	ObjectKey string            `json:"object_key"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

func validFileType(t string) bool {
	switch t {
	case "pdf", "docx", "txt", "md", "html":
		return true
	default:
		return false
	}
}

func (s *Service) UploadFile(ctx context.Context, req *pb.UploadFileRequest) (*pb.UploadFileResponse, error) {
	if strings.TrimSpace(req.GetFileName()) == "" {
		return nil, status.Error(codes.InvalidArgument, "file_name is required")
	}
	if !validFileType(req.GetFileType()) {
		return nil, status.Error(codes.InvalidArgument, "unsupported file_type")
	}

	metadata := make(map[string]string, len(req.GetMetadata())+1)
	for key, value := range req.GetMetadata() {
		metadata[key] = value
	}
	kbID := strings.TrimSpace(req.GetKbId())
	if kbID == "" {
		kbID = strings.TrimSpace(metadata["kb_id"])
	}
	if kbID == "" {
		kbID = "default"
	}
	metadata["kb_id"] = kbID

	principal := tenant.FromIncoming(ctx)
	taskID := uuid.NewString()
	// object_key 由 gateway 直传时指定（文件已落地对象存储），否则在此生成。
	objectKey := req.GetObjectKey()
	if objectKey == "" {
		objectKey = fmt.Sprintf("%s/%s/%s/%s", principal.TenantID, kbID, taskID, req.GetFileName())
	}

	contentHash := req.GetContentHash()
	if contentHash == "" && req.GetMetadata() != nil {
		contentHash = req.GetMetadata()["content_hash"]
	}

	task, created, err := s.machine.Create(ctx, state.Task{
		ID:          taskID,
		TenantID:    principal.TenantID,
		State:       state.Pending,
		ContentHash: contentHash,
		Metadata:    metadata,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !created {
		return &pb.UploadFileResponse{
			TaskId:  task.ID,
			Status:  string(task.State),
			Message: "duplicate content, returning existing task",
			Created: false,
		}, nil
	}

	fileURL := req.GetPresignedUrl()
	if fileURL == "" {
		fileURL = s.presignBase + "/" + objectKey
	}

	evt, _ := json.Marshal(uploadedEvent{
		TaskID:    task.ID,
		TenantID:  task.TenantID,
		FileURL:   fileURL,
		FileType:  req.GetFileType(),
		FileName:  req.GetFileName(),
		ObjectKey: objectKey,
		Metadata:  metadata,
	})
	if err := s.pub.Publish(ctx, queue.Message{
		Topic:   s.topicUploaded,
		Key:     task.ID,
		Payload: evt,
		Headers: map[string]string{"tenant_id": task.TenantID},
	}); err != nil {
		// Roll back so the task does not linger in PENDING forever.
		if _, terr := s.machine.Transition(ctx, task.ID, state.Failed, "enqueue: "+err.Error()); terr != nil {
			s.logger.Warn("rollback to FAILED failed", "task_id", task.ID, "error", terr)
		}
		return nil, status.Error(codes.Unavailable, "enqueue task: "+err.Error())
	}

	s.logger.Info("upload registered", "task_id", task.ID, "tenant", task.TenantID, "kb_id", kbID, "file", req.GetFileName())
	return &pb.UploadFileResponse{
		TaskId:  task.ID,
		Status:  string(task.State),
		Message: "task accepted",
		Created: true,
	}, nil
}

// RegisterContent persists connector-fetched bytes as a normal document task.
// The SHA-256 content hash makes repeated syncs idempotent per tenant and
// knowledge base, so unchanged external documents are skipped.
func (s *Service) RegisterContent(
	ctx context.Context,
	tenantID, kbID, connectorID, externalID, fileName, fileType string,
	content []byte,
) (string, bool, error) {
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(kbID) == "" {
		return "", false, fmt.Errorf("connector register: tenant_id and kb_id are required")
	}
	if !validFileType(fileType) {
		return "", false, fmt.Errorf("connector register: unsupported file_type %q", fileType)
	}
	if len(content) == 0 {
		return "", false, fmt.Errorf("connector register: empty content")
	}
	name := connectorFileName(fileName, externalID, fileType)
	digest := sha256.Sum256(content)
	contentHash := hex.EncodeToString(digest[:])
	taskID := uuid.NewString()
	objectKey := fmt.Sprintf("%s/%s/connector/%s/%s/%s", tenantID, kbID, connectorID, taskID, name)
	metadata := map[string]string{
		"kb_id":        kbID,
		"file_name":    name,
		"file_type":    fileType,
		"file_size":    strconv.Itoa(len(content)),
		"content_hash": contentHash,
		"connector_id": connectorID,
		"external_id":  externalID,
		"source":       "connector",
	}
	task, created, err := s.machine.Create(ctx, state.Task{
		ID:          taskID,
		TenantID:    tenantID,
		State:       state.Pending,
		ContentHash: contentHash,
		Metadata:    metadata,
	})
	if err != nil {
		return "", false, err
	}
	if !created {
		return task.ID, false, nil
	}
	if err := s.objects.Put(ctx, objectKey, bytes.NewReader(content), int64(len(content)), contentTypeForFile(fileType)); err != nil {
		if _, terr := s.machine.Transition(ctx, task.ID, state.Failed, "store connector content: "+err.Error()); terr != nil {
			s.logger.Warn("rollback connector task failed", "task_id", task.ID, "error", terr)
		}
		return "", false, err
	}
	evt, _ := json.Marshal(uploadedEvent{
		TaskID:    task.ID,
		TenantID:  tenantID,
		FileURL:   s.presignBase + "/" + objectKey,
		FileType:  fileType,
		FileName:  name,
		ObjectKey: objectKey,
		Metadata:  metadata,
	})
	if err := s.pub.Publish(ctx, queue.Message{
		Topic:   s.topicUploaded,
		Key:     task.ID,
		Payload: evt,
		Headers: map[string]string{"tenant_id": tenantID},
	}); err != nil {
		if _, terr := s.machine.Transition(ctx, task.ID, state.Failed, "enqueue: "+err.Error()); terr != nil {
			s.logger.Warn("rollback connector task failed", "task_id", task.ID, "error", terr)
		}
		return "", false, err
	}
	s.logger.Info("connector document registered", "task_id", task.ID, "tenant", tenantID,
		"kb_id", kbID, "connector_id", connectorID, "external_id", externalID)
	return task.ID, true, nil
}

// connectorFileName keeps one path-safe segment with the declared extension.
func connectorFileName(name, externalID, fileType string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = externalID
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if ext != fileType {
		name = strings.TrimSuffix(name, path.Ext(name)) + "." + fileType
	}
	name = strings.Trim(name, ". ")
	if name == "" || name == ".." {
		name = "document." + fileType
	}
	if len(name) > 120 {
		name = name[:120-len(fileType)-1] + "." + fileType
	}
	return name
}

func contentTypeForFile(fileType string) string {
	switch fileType {
	case "pdf":
		return "application/pdf"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "html":
		return "text/html; charset=utf-8"
	case "md":
		return "text/markdown; charset=utf-8"
	default:
		return "text/plain; charset=utf-8"
	}
}

func taskKnowledgeBaseID(task state.Task) string {
	if id := strings.TrimSpace(task.Metadata["kb_id"]); id != "" {
		return id
	}
	return "default"
}

// loadOwnedTask enforces tenant and knowledge-base ownership. Missing and
// out-of-scope tasks both surface as NotFound to avoid leaking their existence.
func (s *Service) loadOwnedTask(ctx context.Context, id, kbID string) (state.Task, error) {
	principal := tenant.FromIncoming(ctx)
	task, ok, err := s.machine.Get(ctx, id)
	if err != nil {
		return state.Task{}, status.Error(codes.Internal, err.Error())
	}
	if !ok || task.TenantID != principal.TenantID {
		return state.Task{}, status.Error(codes.NotFound, "task not found")
	}
	if kbID != "" && taskKnowledgeBaseID(task) != kbID {
		return state.Task{}, status.Error(codes.NotFound, "task not found")
	}
	return task, nil
}

func (s *Service) GetTaskStatus(ctx context.Context, req *pb.GetTaskStatusRequest) (*pb.GetTaskStatusResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId(), req.GetKbId())
	if err != nil {
		return nil, err
	}
	return toTaskStatus(task), nil
}

func toTaskStatus(t state.Task) *pb.GetTaskStatusResponse {
	meta := t.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	fileSize, _ := strconv.ParseInt(meta["file_size"], 10, 64)
	contentHash := t.ContentHash
	if contentHash == "" {
		contentHash = meta["content_hash"]
	}
	return &pb.GetTaskStatusResponse{
		TaskId:      t.ID,
		Status:      string(t.State),
		Progress:    t.Progress,
		ErrorMsg:    t.ErrorMsg,
		ContentHash: contentHash,
		Metadata:    meta,
		FileName:    meta["file_name"],
		FileType:    meta["file_type"],
		FileSize:    fileSize,
		CreatedAt:   t.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   t.UpdatedAt.Format(time.RFC3339),
		KbId:        taskKnowledgeBaseID(t),
	}
}

// DeleteTask performs the full lifecycle cleanup for a document: it removes the
// retrieval-side vectors and graph nodes, deletes the stored object and finally
// drops the task record. Index cleanup runs first and is idempotent; if it
// fails the task is kept so the operator can retry instead of leaving orphan
// vectors behind.
func (s *Service) DeleteTask(ctx context.Context, req *pb.DeleteTaskRequest) (*pb.DeleteTaskResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId(), req.GetKbId())
	if err != nil {
		return nil, err
	}
	kbID := taskKnowledgeBaseID(task)

	indexDeleted := 0
	if s.cleaner != nil {
		n, cleanupErr := s.cleaner.DeleteDocument(ctx, task.ID, kbID, task.TenantID)
		if cleanupErr != nil {
			s.logger.Error("delete index failed", "task_id", task.ID, "kb_id", kbID, "error", cleanupErr)
			return nil, status.Error(codes.Unavailable, "delete index: "+cleanupErr.Error())
		}
		indexDeleted = n
	}

	if err := s.machine.Delete(ctx, task.ID); err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			return nil, status.Error(codes.NotFound, "task not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	objectDeleted := false
	if objectKey := task.Metadata["object_key"]; objectKey != "" {
		if err := s.objects.Delete(ctx, objectKey); err != nil {
			s.logger.Warn("delete object failed", "task_id", task.ID, "object_key", objectKey, "error", err)
		} else {
			objectDeleted = true
		}
	}
	// Derived parse artifacts (markdown + extracted images) belong to the
	// document and must not outlive it.
	for _, key := range parsedArtifactKeys(task.Metadata) {
		if err := s.objects.Delete(ctx, key); err != nil {
			s.logger.Warn("delete parsed artifact failed", "task_id", task.ID, "key", key, "error", err)
		}
	}
	s.logger.Info("task deleted", "task_id", task.ID, "tenant", task.TenantID,
		"kb_id", kbID, "index_deleted", indexDeleted, "object_deleted", objectDeleted)
	return &pb.DeleteTaskResponse{
		Success:       true,
		KbId:          kbID,
		ObjectDeleted: objectDeleted,
		IndexDeleted:  int32(indexDeleted),
	}, nil
}

// RetryTask re-enqueues a FAILED task by rolling it back to PENDING and
// re-publishing the original upload event. Only FAILED tasks are retryable.
func (s *Service) RetryTask(ctx context.Context, req *pb.RetryTaskRequest) (*pb.RetryTaskResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId(), req.GetKbId())
	if err != nil {
		return nil, err
	}
	if task.State != state.Failed {
		return nil, status.Error(codes.FailedPrecondition, "only FAILED tasks can be retried")
	}
	if _, err := s.machine.Transition(ctx, task.ID, state.Pending, ""); err != nil {
		if errors.Is(err, state.ErrInvalidState) {
			return nil, status.Error(codes.FailedPrecondition, "task is not retryable")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}

	objectKey := task.Metadata["object_key"]
	fileURL := task.Metadata["file_url"]
	if fileURL == "" && objectKey != "" {
		fileURL = s.presignBase + "/" + objectKey
	}
	evt, _ := json.Marshal(uploadedEvent{
		TaskID:    task.ID,
		TenantID:  task.TenantID,
		FileURL:   fileURL,
		FileType:  task.Metadata["file_type"],
		FileName:  task.Metadata["file_name"],
		ObjectKey: objectKey,
		Metadata:  task.Metadata,
	})
	if err := s.pub.Publish(ctx, queue.Message{
		Topic:   s.topicUploaded,
		Key:     task.ID,
		Payload: evt,
		Headers: map[string]string{"tenant_id": task.TenantID},
	}); err != nil {
		// Roll back to FAILED so the operator can retry once the broker recovers.
		if _, terr := s.machine.Transition(ctx, task.ID, state.Failed, "enqueue retry: "+err.Error()); terr != nil {
			s.logger.Warn("rollback to FAILED failed", "task_id", task.ID, "error", terr)
		}
		return nil, status.Error(codes.Unavailable, "enqueue retry: "+err.Error())
	}
	s.logger.Info("task retry scheduled", "task_id", task.ID, "tenant", task.TenantID)
	return &pb.RetryTaskResponse{TaskId: task.ID, Status: string(state.Pending), Message: "retry enqueued"}, nil
}

// ReindexTask rebuilds a completed document's index in place from the persisted
// parsed markdown, without re-parsing the source file.
func (s *Service) ReindexTask(ctx context.Context, req *pb.ReindexTaskRequest) (*pb.ReindexTaskResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId(), req.GetKbId())
	if err != nil {
		return nil, err
	}
	if task.State != state.Completed {
		return nil, status.Error(codes.FailedPrecondition, "only COMPLETED tasks can be reindexed")
	}
	if s.parsedTopic == "" {
		return nil, status.Error(codes.FailedPrecondition, "reindex is not configured (missing parsed topic)")
	}
	key := strings.TrimSpace(task.Metadata["markdown_key"])
	if key == "" {
		return nil, status.Error(codes.FailedPrecondition, "parsed markdown is unavailable; re-upload the file")
	}
	rc, err := s.objects.Get(ctx, key)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "read parsed markdown: "+err.Error())
	}
	raw, readErr := io.ReadAll(rc)
	_ = rc.Close()
	if readErr != nil {
		return nil, status.Error(codes.Unavailable, "read parsed markdown: "+readErr.Error())
	}
	if strings.TrimSpace(string(raw)) == "" {
		return nil, status.Error(codes.FailedPrecondition, "parsed markdown is empty")
	}

	if _, err := s.machine.Transition(ctx, task.ID, state.Reindexing, ""); err != nil {
		if errors.Is(err, state.ErrInvalidState) {
			return nil, status.Error(codes.FailedPrecondition, "task is not reindexable")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	evt, _ := json.Marshal(map[string]string{
		"task_id": task.ID, "tenant_id": task.TenantID, "markdown": string(raw),
	})
	if err := s.pub.Publish(ctx, queue.Message{
		Topic:   s.parsedTopic,
		Key:     task.ID,
		Payload: evt,
		Headers: map[string]string{"tenant_id": task.TenantID},
	}); err != nil {
		// Keep the document usable: roll back to COMPLETED so a later reindex
		// attempt is still allowed.
		if _, terr := s.machine.Transition(ctx, task.ID, state.Completed, ""); terr != nil {
			s.logger.Warn("rollback reindex state failed", "task_id", task.ID, "error", terr)
		}
		return nil, status.Error(codes.Unavailable, "enqueue reindex: "+err.Error())
	}
	s.logger.Info("task reindex scheduled", "task_id", task.ID, "kb_id", taskKnowledgeBaseID(task))
	return &pb.ReindexTaskResponse{
		TaskId: task.ID, Status: string(state.Reindexing), Message: "reindex enqueued",
	}, nil
}

func (s *Service) ListTasks(ctx context.Context, req *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	principal := tenant.FromIncoming(ctx)
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	offset := int(req.GetOffset())
	if offset < 0 {
		offset = 0
	}
	var tasks []state.Task
	var total int
	var err error
	if req.GetKbId() == "" {
		tasks, total, err = s.machine.List(ctx, principal.TenantID, req.GetStatus(), limit, offset)
	} else {
		var all []state.Task
		all, _, err = s.machine.List(ctx, principal.TenantID, req.GetStatus(), 0, 0)
		if err == nil {
			filtered := make([]state.Task, 0, len(all))
			for _, task := range all {
				if taskKnowledgeBaseID(task) == req.GetKbId() {
					filtered = append(filtered, task)
				}
			}
			total = len(filtered)
			if offset > total {
				offset = total
			}
			end := offset + limit
			if limit <= 0 || end > total {
				end = total
			}
			tasks = filtered[offset:end]
		}
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := make([]*pb.TaskSummary, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toTaskSummary(t))
	}
	return &pb.ListTasksResponse{Tasks: out, Total: int32(total)}, nil
}

func toTaskSummary(t state.Task) *pb.TaskSummary {
	meta := t.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	fileSize, _ := strconv.ParseInt(meta["file_size"], 10, 64)
	return &pb.TaskSummary{
		TaskId:      t.ID,
		Status:      string(t.State),
		Progress:    t.Progress,
		FileName:    meta["file_name"],
		FileType:    meta["file_type"],
		FileSize:    fileSize,
		ContentHash: t.ContentHash,
		ErrorMsg:    t.ErrorMsg,
		CreatedAt:   t.CreatedAt.Format(time.RFC3339),
		UpdatedAt:   t.UpdatedAt.Format(time.RFC3339),
		KbId:        taskKnowledgeBaseID(t),
	}
}

// parsedArtifactKeys lists derived files created by the parser for one task:
// the persisted markdown, the layout JSON and every extracted image.
func parsedArtifactKeys(meta map[string]string) []string {
	var keys []string
	if key := strings.TrimSpace(meta["markdown_key"]); key != "" {
		keys = append(keys, key)
	}
	if key := strings.TrimSpace(meta["layout_key"]); key != "" {
		keys = append(keys, key)
	}
	for _, key := range strings.Split(meta["media_keys"], ",") {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}
