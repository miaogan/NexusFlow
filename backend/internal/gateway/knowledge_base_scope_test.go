package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingIngestionClient struct {
	upload    *pb.UploadFileRequest
	deleted   *pb.DeleteTaskRequest
	reindexed *pb.ReindexTaskRequest
	fileSize  int64
}

func (r *recordingIngestionClient) UploadFile(_ context.Context, req *pb.UploadFileRequest, _ ...grpc.CallOption) (*pb.UploadFileResponse, error) {
	r.upload = req
	return &pb.UploadFileResponse{TaskId: "task-1", Status: "PENDING", Created: true}, nil
}
func (r *recordingIngestionClient) GetTaskStatus(_ context.Context, req *pb.GetTaskStatusRequest, _ ...grpc.CallOption) (*pb.GetTaskStatusResponse, error) {
	return &pb.GetTaskStatusResponse{TaskId: req.GetTaskId(), FileSize: r.fileSize, KbId: req.GetKbId()}, nil
}
func (*recordingIngestionClient) ListTasks(context.Context, *pb.ListTasksRequest, ...grpc.CallOption) (*pb.ListTasksResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}
func (r *recordingIngestionClient) DeleteTask(_ context.Context, req *pb.DeleteTaskRequest, _ ...grpc.CallOption) (*pb.DeleteTaskResponse, error) {
	r.deleted = req
	return &pb.DeleteTaskResponse{Success: true, KbId: req.GetKbId(), ObjectDeleted: true, IndexDeleted: 7}, nil
}
func (r *recordingIngestionClient) RetryTask(context.Context, *pb.RetryTaskRequest, ...grpc.CallOption) (*pb.RetryTaskResponse, error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}

func (r *recordingIngestionClient) ReindexTask(_ context.Context, req *pb.ReindexTaskRequest, _ ...grpc.CallOption) (*pb.ReindexTaskResponse, error) {
	r.reindexed = req
	return &pb.ReindexTaskResponse{TaskId: req.GetTaskId(), Status: "REINDEXING", Message: "reindex enqueued"}, nil
}

type recordingQueryClient struct{ search *pb.SearchRequest }

func (r *recordingQueryClient) Search(_ context.Context, req *pb.SearchRequest, _ ...grpc.CallOption) (*pb.SearchResponse, error) {
	r.search = req
	return &pb.SearchResponse{}, nil
}
func (*recordingQueryClient) Answer(context.Context, *pb.AnswerRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.AnswerChunk], error) {
	return nil, status.Error(codes.Unimplemented, "unused")
}

func TestGatewayDeleteFileCascadesWithKnowledgeBaseScope(t *testing.T) {
	g := newIdentityGateway(t)
	ingestion := &recordingIngestionClient{}
	g.ingestion = ingestion
	h := g.Router()
	admin := loginToken(t, h, "admin", "admin123")

	w := call(t, h, http.MethodDelete, "/v1/files/task-1?kb_id=default", admin, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete file: %d %s", w.Code, w.Body.String())
	}
	if ingestion.deleted.GetTaskId() != "task-1" || ingestion.deleted.GetKbId() != "default" {
		t.Fatalf("delete request = %+v", ingestion.deleted)
	}
	for _, field := range []string{`"index_deleted":7`, `"object_deleted":true`, `"kb_id":"default"`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Errorf("response %s missing %s", w.Body.String(), field)
		}
	}
	if w := call(t, h, http.MethodDelete, "/v1/files/task-1", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("anonymous delete = %d, want 403", w.Code)
	}
}

func TestGatewayNormalizesAndOwnsKnowledgeBaseScope(t *testing.T) {
	g := newIdentityGateway(t)
	ingestion := &recordingIngestionClient{}
	query := &recordingQueryClient{}
	g.ingestion = ingestion
	g.query = query
	h := g.Router()
	admin := loginToken(t, h, "admin", "admin123")

	w := call(t, h, http.MethodPost, "/v1/files", admin, map[string]any{
		"file_name": "a.txt", "file_type": "txt", "metadata": map[string]string{"kb_id": "spoofed"},
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("upload default KB: %d %s", w.Code, w.Body.String())
	}
	if ingestion.upload.GetKbId() != "default" || ingestion.upload.GetMetadata()["kb_id"] != "default" {
		t.Fatalf("upload scope not normalized: %+v metadata=%v", ingestion.upload, ingestion.upload.GetMetadata())
	}

	w = call(t, h, http.MethodPost, "/v1/search", admin, map[string]any{
		"query": "test", "filters": map[string]string{
			"kb_id": "spoofed-kb", "tenant_id": "spoofed-tenant", "file_name": "a.txt",
		},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("search default KB: %d %s", w.Code, w.Body.String())
	}
	if query.search.GetKbId() != "default" || query.search.GetFilters()["kb_id"] != "default" || query.search.GetFilters()["tenant_id"] != "default" {
		t.Fatalf("search scope not enforced: %+v filters=%v", query.search, query.search.GetFilters())
	}
	if query.search.GetFilters()["file_name"] != "a.txt" {
		t.Fatalf("ordinary search filter lost: %v", query.search.GetFilters())
	}
}
