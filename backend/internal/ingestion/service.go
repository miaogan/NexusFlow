package ingestion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
}

func NewService(objects store.ObjectStore, machine state.Store, pub queue.Publisher, logger *slog.Logger, presignBase, topicUploaded string) *Service {
	return &Service{
		objects:       objects,
		machine:       machine,
		pub:           pub,
		logger:        logger,
		presignBase:   strings.TrimRight(presignBase, "/"),
		topicUploaded: topicUploaded,
	}
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

	principal := tenant.FromIncoming(ctx)
	taskID := uuid.NewString()
	// object_key 由 gateway 直传时指定（文件已落地对象存储），否则在此生成。
	objectKey := req.GetObjectKey()
	if objectKey == "" {
		objectKey = fmt.Sprintf("%s/%s/%s", principal.TenantID, taskID, req.GetFileName())
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
		Metadata:    req.GetMetadata(),
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
		Metadata:  req.GetMetadata(),
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

	s.logger.Info("upload registered", "task_id", task.ID, "tenant", task.TenantID, "file", req.GetFileName())
	return &pb.UploadFileResponse{
		TaskId:  task.ID,
		Status:  string(task.State),
		Message: "task accepted",
		Created: true,
	}, nil
}

// loadOwnedTask fetches a task and enforces tenant ownership. Missing tasks
// and tasks belonging to another tenant both surface as NotFound so task
// existence is never leaked across tenants.
func (s *Service) loadOwnedTask(ctx context.Context, id string) (state.Task, error) {
	principal := tenant.FromIncoming(ctx)
	task, ok, err := s.machine.Get(ctx, id)
	if err != nil {
		return state.Task{}, status.Error(codes.Internal, err.Error())
	}
	if !ok || task.TenantID != principal.TenantID {
		return state.Task{}, status.Error(codes.NotFound, "task not found")
	}
	return task, nil
}

func (s *Service) GetTaskStatus(ctx context.Context, req *pb.GetTaskStatusRequest) (*pb.GetTaskStatusResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId())
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
	}
}

// DeleteTask removes the task record and, best-effort, the underlying object.
// Index entries are left in place (see known limitation); deleting a task does
// not guarantee its chunks disappear from search results immediately.
func (s *Service) DeleteTask(ctx context.Context, req *pb.DeleteTaskRequest) (*pb.DeleteTaskResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId())
	if err != nil {
		return nil, err
	}
	if err := s.machine.Delete(ctx, task.ID); err != nil {
		if errors.Is(err, state.ErrTaskNotFound) {
			return nil, status.Error(codes.NotFound, "task not found")
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if objectKey := task.Metadata["object_key"]; objectKey != "" {
		if err := s.objects.Delete(ctx, objectKey); err != nil {
			s.logger.Warn("delete object failed", "task_id", task.ID, "object_key", objectKey, "error", err)
		}
	}
	s.logger.Info("task deleted", "task_id", task.ID, "tenant", task.TenantID)
	return &pb.DeleteTaskResponse{Success: true}, nil
}

// RetryTask re-enqueues a FAILED task by rolling it back to PENDING and
// re-publishing the original upload event. Only FAILED tasks are retryable.
func (s *Service) RetryTask(ctx context.Context, req *pb.RetryTaskRequest) (*pb.RetryTaskResponse, error) {
	task, err := s.loadOwnedTask(ctx, req.GetTaskId())
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
	tasks, total, err := s.machine.List(ctx, principal.TenantID, req.GetStatus(), limit, offset)
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
	}
}
