package ingestion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
	pb "github.com/nexusflow/nexusflow/backend/pb/vectorization/v1"
)

type fakePublisher struct {
	mu       sync.Mutex
	messages []queue.Message
}

func (f *fakePublisher) Publish(_ context.Context, msg queue.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return nil
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakePublisher) lastPayload() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(f.messages[len(f.messages)-1].Payload, &out)
	return out
}

type fakeObjectStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	deleted []string
}

func newFakeObjectStore() *fakeObjectStore {
	return &fakeObjectStore{objects: make(map[string][]byte)}
}

func (f *fakeObjectStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	raw, _ := io.ReadAll(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = raw
	return nil
}

func (f *fakeObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.objects[key]
	if !ok {
		return nil, &notFoundError{}
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (f *fakeObjectStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	f.deleted = append(f.deleted, key)
	return nil
}

func (f *fakeObjectStore) URL(key string) string { return "http://objects/" + key }

func (f *fakeObjectStore) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

type notFoundError struct{}

func (*notFoundError) Error() string { return "object not found" }

func newTestService(t *testing.T) (*Service, *state.Machine, *fakePublisher, *fakeObjectStore) {
	t.Helper()
	m := state.NewMachine()
	pub := &fakePublisher{}
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, pub, logger, "http://objects", "file.uploaded")
	return svc, m, pub, objects
}

func ctxWithTenant(tenantID string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-tenant-id", tenantID))
}

func seedTask(t *testing.T, m *state.Machine, id, tenantID string, meta map[string]string) {
	t.Helper()
	_, created, err := m.Create(context.Background(), state.Task{
		ID:          id,
		TenantID:    tenantID,
		ContentHash: meta["content_hash"],
		Metadata:    meta,
	})
	if err != nil || !created {
		t.Fatalf("seed task %s: created=%v err=%v", id, created, err)
	}
}

func TestGetTaskStatusRich(t *testing.T) {
	svc, m, _, _ := newTestService(t)
	meta := map[string]string{
		"file_name":    "report.md",
		"file_type":    "md",
		"file_size":    "2048",
		"content_hash": "abc123",
		"object_key":   "default/upload/x/report.md",
		"source":       "web-console",
	}
	seedTask(t, m, "t1", "default", meta)

	resp, err := svc.GetTaskStatus(context.Background(), &pb.GetTaskStatusRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if resp.GetFileName() != "report.md" || resp.GetFileType() != "md" || resp.GetFileSize() != 2048 {
		t.Fatalf("file fields mismatch: %+v", resp)
	}
	if resp.GetContentHash() != "abc123" {
		t.Fatalf("expected content_hash abc123, got %q", resp.GetContentHash())
	}
	if got := resp.GetMetadata()["source"]; got != "web-console" {
		t.Fatalf("metadata source mismatch: %q", got)
	}
	if resp.GetCreatedAt() == "" || resp.GetUpdatedAt() == "" {
		t.Fatal("expected timestamps to be populated")
	}
}

func TestDeleteTask(t *testing.T) {
	svc, m, _, objects := newTestService(t)
	meta := map[string]string{"file_name": "a.md", "object_key": "default/upload/x/a.md", "content_hash": "h1"}
	seedTask(t, m, "t1", "default", meta)
	_ = objects.Put(context.Background(), "default/upload/x/a.md", bytes.NewReader([]byte("hi")), 2, "text/markdown")

	resp, err := svc.DeleteTask(context.Background(), &pb.DeleteTaskRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !resp.GetSuccess() {
		t.Fatal("expected success=true")
	}
	if _, ok, _ := m.Get(context.Background(), "t1"); ok {
		t.Fatal("task should be gone after delete")
	}
	if keys := objects.deletedKeys(); len(keys) != 1 || keys[0] != "default/upload/x/a.md" {
		t.Fatalf("expected object deleted once, got %v", keys)
	}
	// Idempotency key released: same hash can create a new task.
	if _, created, err := m.Create(context.Background(), state.Task{ID: "t2", TenantID: "default", ContentHash: "h1"}); err != nil || !created {
		t.Fatalf("re-create after delete should succeed: created=%v err=%v", created, err)
	}
}

func TestDeleteTaskNotFound(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	_, err := svc.DeleteTask(context.Background(), &pb.DeleteTaskRequest{TaskId: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestDeleteTaskCrossTenant(t *testing.T) {
	svc, m, _, _ := newTestService(t)
	seedTask(t, m, "t1", "default", map[string]string{"file_name": "a.md"})

	_, err := svc.DeleteTask(ctxWithTenant("other"), &pb.DeleteTaskRequest{TaskId: "t1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound for cross-tenant delete, got %v", err)
	}
	// Task must still exist for its real owner.
	if _, ok, _ := m.Get(context.Background(), "t1"); !ok {
		t.Fatal("task must survive cross-tenant delete attempt")
	}
}

func TestRetryTask(t *testing.T) {
	svc, m, pub, _ := newTestService(t)
	meta := map[string]string{
		"file_name":  "b.md",
		"file_type":  "md",
		"object_key": "default/upload/x/b.md",
	}
	seedTask(t, m, "t1", "default", meta)
	if _, err := m.Transition(context.Background(), "t1", state.Failed, "boom"); err != nil {
		t.Fatalf("transition to failed: %v", err)
	}

	resp, err := svc.RetryTask(context.Background(), &pb.RetryTaskRequest{TaskId: "t1"})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if resp.GetStatus() != string(state.Pending) {
		t.Fatalf("expected status PENDING, got %q", resp.GetStatus())
	}
	task, ok, _ := m.Get(context.Background(), "t1")
	if !ok {
		t.Fatal("task missing")
	}
	if task.State != state.Pending {
		t.Fatalf("expected state PENDING, got %s", task.State)
	}
	if task.ErrorMsg != "" {
		t.Fatalf("expected error cleared, got %q", task.ErrorMsg)
	}
	if pub.count() != 1 {
		t.Fatalf("expected 1 published message, got %d", pub.count())
	}
	payload := pub.lastPayload()
	if payload["task_id"] != "t1" {
		t.Fatalf("published task_id mismatch: %v", payload["task_id"])
	}
	if payload["file_url"] != "http://objects/default/upload/x/b.md" {
		t.Fatalf("rebuilt file_url mismatch: %v", payload["file_url"])
	}
	if payload["file_type"] != "md" || payload["file_name"] != "b.md" {
		t.Fatalf("published file fields mismatch: %v", payload)
	}
}

func TestRetryTaskNotFailed(t *testing.T) {
	svc, m, pub, _ := newTestService(t)
	seedTask(t, m, "t1", "default", map[string]string{"file_name": "c.md"})
	// Task is PENDING, not FAILED.
	_, err := svc.RetryTask(context.Background(), &pb.RetryTaskRequest{TaskId: "t1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
	if pub.count() != 0 {
		t.Fatalf("expected no publish on rejected retry, got %d", pub.count())
	}
}

type failPublisher struct{ err error }

func (f failPublisher) Publish(context.Context, queue.Message) error { return f.err }

// When enqueue fails, the task must roll back to FAILED instead of lingering in
// PENDING forever.
func TestUploadFile_RollsBackOnPublishFailure(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, failPublisher{err: errors.New("broker down")}, logger, "http://objects", "file.uploaded")
	ctx := ctxWithTenant("t1")

	_, err := svc.UploadFile(ctx, &pb.UploadFileRequest{
		FileName: "a.txt", FileType: "txt", FileSize: 3,
		ObjectKey: "t1/a.txt", ContentHash: "hash-rollback",
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}

	tasks, total, err := m.List(ctx, "t1", "", 10, 0)
	if err != nil || total != 1 || len(tasks) != 1 {
		t.Fatalf("tasks=%d err=%v, want 1", total, err)
	}
	if tasks[0].State != state.Failed {
		t.Fatalf("state = %s, want FAILED after rollback", tasks[0].State)
	}
}

func TestUploadAndTaskAccessAreKnowledgeBaseScoped(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	ctx := ctxWithTenant("default")
	first, err := svc.UploadFile(ctx, &pb.UploadFileRequest{
		FileName: "same.txt", FileType: "txt", ContentHash: "same-hash", KbId: "kb1",
	})
	if err != nil || !first.GetCreated() {
		t.Fatalf("first upload: resp=%+v err=%v", first, err)
	}
	second, err := svc.UploadFile(ctx, &pb.UploadFileRequest{
		FileName: "same.txt", FileType: "txt", ContentHash: "same-hash", KbId: "kb2",
	})
	if err != nil || !second.GetCreated() || second.GetTaskId() == first.GetTaskId() {
		t.Fatalf("different KB must not deduplicate: resp=%+v err=%v", second, err)
	}
	duplicate, err := svc.UploadFile(ctx, &pb.UploadFileRequest{
		FileName: "same.txt", FileType: "txt", ContentHash: "same-hash", KbId: "kb1",
	})
	if err != nil || duplicate.GetCreated() || duplicate.GetTaskId() != first.GetTaskId() {
		t.Fatalf("same KB must deduplicate: resp=%+v err=%v", duplicate, err)
	}

	list, err := svc.ListTasks(ctx, &pb.ListTasksRequest{KbId: "kb1"})
	if err != nil || list.GetTotal() != 1 || len(list.GetTasks()) != 1 || list.GetTasks()[0].GetKbId() != "kb1" {
		t.Fatalf("KB-filtered list=%+v err=%v", list, err)
	}
	if _, err := svc.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: second.GetTaskId(), KbId: "kb1"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-KB task access code=%v, want NotFound", status.Code(err))
	}
	statusResp, err := svc.GetTaskStatus(ctx, &pb.GetTaskStatusRequest{TaskId: second.GetTaskId(), KbId: "kb2"})
	if err != nil || statusResp.GetKbId() != "kb2" || statusResp.GetMetadata()["kb_id"] != "kb2" {
		t.Fatalf("owned task status=%+v err=%v", statusResp, err)
	}
}

type fakeCleaner struct {
	mu      sync.Mutex
	calls   []cleanerCall
	deleted int
	err     error
}

type cleanerCall struct{ docID, kbID string }

func (f *fakeCleaner) DeleteDocument(_ context.Context, docID, kbID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cleanerCall{docID: docID, kbID: kbID})
	return f.deleted, f.err
}

func (f *fakeCleaner) lastCall() cleanerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return cleanerCall{}
	}
	return f.calls[len(f.calls)-1]
}

// DeleteTask must cascade through the retrieval index, the object store and the
// task record, and report what was removed.
func TestDeleteTaskCascadesIndexCleanup(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleaner := &fakeCleaner{deleted: 4}
	svc := NewService(objects, m, &fakePublisher{}, logger, "http://objects", "file.uploaded",
		WithIndexCleaner(cleaner))

	meta := map[string]string{
		"file_name": "a.md", "object_key": "default/kb1/upload/x/a.md",
		"content_hash": "h1", "kb_id": "kb1",
	}
	seedTask(t, m, "t1", "default", meta)
	if err := objects.Put(context.Background(), meta["object_key"], bytes.NewReader([]byte("hi")), 2, "text/markdown"); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	resp, err := svc.DeleteTask(context.Background(), &pb.DeleteTaskRequest{TaskId: "t1", KbId: "kb1"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !resp.GetSuccess() || resp.GetKbId() != "kb1" || resp.GetIndexDeleted() != 4 || !resp.GetObjectDeleted() {
		t.Fatalf("delete response = %+v", resp)
	}
	if call := cleaner.lastCall(); call.docID != "t1" || call.kbID != "kb1" {
		t.Fatalf("cleaner call = %+v, want t1/kb1", call)
	}
	if _, ok, _ := m.Get(context.Background(), "t1"); ok {
		t.Fatal("task should be gone after cascade delete")
	}
	if keys := objects.deletedKeys(); len(keys) != 1 || keys[0] != meta["object_key"] {
		t.Fatalf("object deletion = %v", keys)
	}
}

// Derived parse artifacts (markdown + extracted images) must not outlive the
// document that produced them.
func TestDeleteTaskRemovesParsedArtifacts(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, &fakePublisher{}, logger, "http://objects", "file.uploaded")

	meta := map[string]string{
		"file_name": "a.pdf", "object_key": "default/kb1/upload/x/a.pdf",
		"kb_id": "kb1", "markdown_key": "default/kb1/parsed/t1.md",
		"media_keys": "default/kb1/parsed/t1/images/a.png,default/kb1/parsed/t1/images/b.png",
	}
	seedTask(t, m, "t1", "default", meta)
	for _, key := range []string{
		meta["object_key"], meta["markdown_key"],
		"default/kb1/parsed/t1/images/a.png", "default/kb1/parsed/t1/images/b.png",
	} {
		if err := objects.Put(context.Background(), key, bytes.NewReader([]byte("x")), 1, "application/octet-stream"); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}

	if _, err := svc.DeleteTask(context.Background(), &pb.DeleteTaskRequest{TaskId: "t1", KbId: "kb1"}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got := map[string]bool{}
	for _, key := range objects.deletedKeys() {
		got[key] = true
	}
	for _, key := range []string{
		meta["object_key"], meta["markdown_key"],
		"default/kb1/parsed/t1/images/a.png", "default/kb1/parsed/t1/images/b.png",
	} {
		if !got[key] {
			t.Fatalf("expected %s to be deleted, got %v", key, got)
		}
	}
}

// A failed index cleanup must abort the delete so vectors never outlive their
// task record; the caller can retry once the index service recovers.
func TestDeleteTaskKeepsTaskWhenIndexCleanupFails(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleaner := &fakeCleaner{err: errors.New("qdrant down")}
	svc := NewService(objects, m, &fakePublisher{}, logger, "http://objects", "file.uploaded",
		WithIndexCleaner(cleaner))

	meta := map[string]string{"file_name": "b.md", "object_key": "default/upload/x/b.md", "kb_id": "kb1"}
	seedTask(t, m, "t1", "default", meta)

	_, err := svc.DeleteTask(context.Background(), &pb.DeleteTaskRequest{TaskId: "t1", KbId: "kb1"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", status.Code(err))
	}
	if _, ok, _ := m.Get(context.Background(), "t1"); !ok {
		t.Fatal("task must survive a failed index cleanup")
	}
	if keys := objects.deletedKeys(); len(keys) != 0 {
		t.Fatalf("object must not be deleted before vectors are cleaned: %v", keys)
	}
}

func advanceToCompleted(t *testing.T, m *state.Machine, id string) {
	t.Helper()
	for _, to := range []state.State{state.Fetched, state.Parsing, state.Parsed, state.Chunked, state.Embedded, state.Indexed, state.Completed} {
		if _, err := m.Transition(context.Background(), id, to, ""); err != nil {
			t.Fatalf("transition %s: %v", to, err)
		}
	}
}

func TestReindexTaskRequeuesPersistedMarkdown(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	pub := &fakePublisher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, pub, logger, "http://objects", "file.uploaded",
		WithParsedTopic("file.parsed"))

	meta := map[string]string{
		"file_name": "a.md", "file_type": "md", "kb_id": "kb1",
		"markdown_key": "default/kb1/parsed/t1.md", "content_hash": "h1",
	}
	seedTask(t, m, "t1", "default", meta)
	advanceToCompleted(t, m, "t1")
	if err := objects.Put(context.Background(), meta["markdown_key"],
		bytes.NewReader([]byte("# parsed content")), 16, "text/markdown"); err != nil {
		t.Fatalf("seed markdown: %v", err)
	}

	resp, err := svc.ReindexTask(context.Background(), &pb.ReindexTaskRequest{TaskId: "t1", KbId: "kb1"})
	if err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if resp.GetStatus() != string(state.Reindexing) {
		t.Fatalf("status = %q, want REINDEXING", resp.GetStatus())
	}
	task, ok, _ := m.Get(context.Background(), "t1")
	if !ok || task.State != state.Reindexing {
		t.Fatalf("task state = %v, want REINDEXING", task.State)
	}
	if pub.count() != 1 {
		t.Fatalf("published %d messages, want 1", pub.count())
	}
	msg := pub.messages[0]
	if msg.Topic != "file.parsed" {
		t.Fatalf("topic = %q, want file.parsed", msg.Topic)
	}
	var payload map[string]string
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["markdown"] != "# parsed content" || payload["task_id"] != "t1" {
		t.Fatalf("payload = %v", payload)
	}
}

func TestReindexTaskGuards(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, &fakePublisher{}, logger, "http://objects", "file.uploaded",
		WithParsedTopic("file.parsed"))

	// Not completed yet.
	seedTask(t, m, "running", "default", map[string]string{
		"file_name": "a.md", "kb_id": "kb1", "markdown_key": "k",
	})
	if _, err := svc.ReindexTask(context.Background(), &pb.ReindexTaskRequest{TaskId: "running"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("non-completed code = %v, want FailedPrecondition", status.Code(err))
	}

	// Completed but the parsed markdown was never persisted.
	seedTask(t, m, "no-md", "default", map[string]string{"file_name": "b.md", "kb_id": "kb1"})
	advanceToCompleted(t, m, "no-md")
	if _, err := svc.ReindexTask(context.Background(), &pb.ReindexTaskRequest{TaskId: "no-md"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("missing markdown code = %v, want FailedPrecondition", status.Code(err))
	}

	// Cross-KB access stays hidden.
	seedTask(t, m, "kb-task", "default", map[string]string{
		"file_name": "c.md", "kb_id": "kb1", "markdown_key": "default/kb1/parsed/kb-task.md",
	})
	advanceToCompleted(t, m, "kb-task")
	if _, err := svc.ReindexTask(context.Background(), &pb.ReindexTaskRequest{TaskId: "kb-task", KbId: "kb2"}); status.Code(err) != codes.NotFound {
		t.Fatalf("cross-kb reindex code = %v, want NotFound", status.Code(err))
	}
}

func TestReindexTaskRollsBackWhenEnqueueFails(t *testing.T) {
	m := state.NewMachine()
	objects := newFakeObjectStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := NewService(objects, m, failPublisher{err: errors.New("broker down")}, logger,
		"http://objects", "file.uploaded", WithParsedTopic("file.parsed"))

	key := "default/kb1/parsed/t1.md"
	seedTask(t, m, "t1", "default", map[string]string{
		"file_name": "a.md", "kb_id": "kb1", "markdown_key": key,
	})
	advanceToCompleted(t, m, "t1")
	if err := objects.Put(context.Background(), key, bytes.NewReader([]byte("md")), 2, "text/markdown"); err != nil {
		t.Fatalf("seed markdown: %v", err)
	}

	if _, err := svc.ReindexTask(context.Background(), &pb.ReindexTaskRequest{TaskId: "t1"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("enqueue failure code = %v, want Unavailable", status.Code(err))
	}
	task, _, _ := m.Get(context.Background(), "t1")
	if task.State != state.Completed {
		t.Fatalf("state = %v, want COMPLETED after rollback", task.State)
	}
}

// Ensure the fake object store satisfies the interface at compile time.
var _ store.ObjectStore = (*fakeObjectStore)(nil)
