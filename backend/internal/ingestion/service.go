package ingestion

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

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
	objectKey := fmt.Sprintf("%s/%s/%s", principal.TenantID, taskID, req.GetFileName())

	contentHash := ""
	if req.GetMetadata() != nil {
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
		return nil, status.Error(codes.Unavailable, "enqueue task: "+err.Error())
	}

	s.logger.Info("upload registered", "task_id", task.ID, "tenant", task.TenantID, "file", req.GetFileName())
	return &pb.UploadFileResponse{
		TaskId:  task.ID,
		Status:  string(task.State),
		Message: "task accepted",
	}, nil
}

func (s *Service) GetTaskStatus(ctx context.Context, req *pb.GetTaskStatusRequest) (*pb.GetTaskStatusResponse, error) {
	task, ok, err := s.machine.Get(ctx, req.GetTaskId())
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return &pb.GetTaskStatusResponse{
		TaskId:   task.ID,
		Status:   string(task.State),
		Progress: task.Progress,
		ErrorMsg: task.ErrorMsg,
	}, nil
}
