package processing

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nexusflow/nexusflow/backend/internal/mineru"
	"github.com/nexusflow/nexusflow/backend/internal/queue"
	"github.com/nexusflow/nexusflow/backend/internal/state"
	"github.com/nexusflow/nexusflow/backend/internal/store"
)

type fakeObjects struct{ data map[string][]byte }

func newFakeObjects() *fakeObjects { return &fakeObjects{data: make(map[string][]byte)} }

func (f *fakeObjects) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	raw, _ := io.ReadAll(r)
	f.data[key] = raw
	return nil
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	raw, ok := f.data[key]
	if !ok {
		return nil, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	return nil
}

func (f *fakeObjects) URL(key string) string { return "http://objects/" + key }

type fakePublisher struct{ messages []queue.Message }

func (f *fakePublisher) Publish(_ context.Context, msg queue.Message) error {
	f.messages = append(f.messages, msg)
	return nil
}

func TestWorkerPersistsParsedMarkdown(t *testing.T) {
	machine := state.NewMachine()
	objects := newFakeObjects()
	pub := &fakePublisher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(&mineru.MockClient{Markdown: "# parsed markdown"}, machine, pub, logger,
		10*time.Millisecond, 1, 1, "file.parsed", "task.dlq", "test", WithObjectStore(objects))

	ctx := context.Background()
	_, _, err := machine.Create(ctx, state.Task{
		ID: "t1", TenantID: "default",
		Metadata: map[string]string{"file_name": "a.md", "file_type": "md", "kb_id": "kb1"},
	})
	if err != nil {
		t.Fatalf("seed task: %v", err)
	}
	msg := queue.Message{
		Topic: "file.uploaded", Key: "t1",
		Payload: []byte(`{"task_id":"t1","tenant_id":"default","file_url":"http://objects/x","file_type":"md","file_name":"a.md"}`),
	}
	if err := worker.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	task, ok, err := machine.Get(ctx, "t1")
	if err != nil || !ok {
		t.Fatalf("get task: ok=%v err=%v", ok, err)
	}
	key := "default/kb1/parsed/t1.md"
	if task.Metadata["markdown_key"] != key {
		t.Fatalf("markdown_key = %q, want %q", task.Metadata["markdown_key"], key)
	}
	if got := string(objects.data[key]); got != "# parsed markdown" {
		t.Fatalf("persisted markdown = %q", got)
	}
	if task.State != state.Parsed {
		t.Fatalf("state = %v, want PARSED", task.State)
	}
	if len(pub.messages) != 1 || pub.messages[0].Topic != "file.parsed" {
		t.Fatalf("published messages = %+v", pub.messages)
	}
}

// A storage outage must not block indexing: the parsed event still goes out.
func TestWorkerToleratesMarkdownPersistFailure(t *testing.T) {
	machine := state.NewMachine()
	pub := &fakePublisher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := NewWorker(&mineru.MockClient{Markdown: "# md"}, machine, pub, logger,
		10*time.Millisecond, 1, 1, "file.parsed", "task.dlq", "test",
		WithObjectStore(&failingObjects{}))

	ctx := context.Background()
	if _, _, err := machine.Create(ctx, state.Task{ID: "t1", TenantID: "default"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	msg := queue.Message{Topic: "file.uploaded", Key: "t1",
		Payload: []byte(`{"task_id":"t1","tenant_id":"default","file_url":"x","file_type":"md"}`)}
	if err := worker.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}
	task, _, _ := machine.Get(ctx, "t1")
	if task.State != state.Parsed || len(pub.messages) != 1 {
		t.Fatalf("state=%v messages=%d", task.State, len(pub.messages))
	}
	if task.Metadata["markdown_key"] != "" {
		t.Fatalf("unexpected markdown_key on persist failure: %q", task.Metadata["markdown_key"])
	}
}

type failingObjects struct{}

func (*failingObjects) Put(context.Context, string, io.Reader, int64, string) error {
	return io.ErrClosedPipe
}
func (*failingObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, io.ErrClosedPipe
}
func (*failingObjects) Delete(context.Context, string) error { return nil }
func (*failingObjects) URL(string) string                    { return "" }

var _ store.ObjectStore = (*failingObjects)(nil)
var _ store.ObjectStore = (*fakeObjects)(nil)
