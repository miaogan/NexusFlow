package processing

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
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

// imageClient returns one extracted figure alongside the Markdown.
type imageClient struct {
	mineru.MockClient
}

func (c *imageClient) PollDocument(
	ctx context.Context, jobID string,
) (mineru.Document, bool, error) {
	markdown, done, err := c.Poll(ctx, jobID)
	doc := mineru.Document{
		Markdown: markdown,
		Images:   map[string][]byte{"images/fig1.png": []byte("PNG-BYTES")},
	}
	return doc, done, err
}

var _ mineru.DocumentClient = (*imageClient)(nil)

// layoutClient returns MinerU's content_list.json alongside the Markdown.
type layoutClient struct {
	mineru.MockClient
}

func (c *layoutClient) PollDocument(
	ctx context.Context, jobID string,
) (mineru.Document, bool, error) {
	markdown, done, err := c.Poll(ctx, jobID)
	return mineru.Document{
		Markdown: markdown,
		Layout:   []byte(`[{"type":"text","page_idx":0}]`),
	}, done, err
}

var _ mineru.DocumentClient = (*layoutClient)(nil)

func TestWorkerPersistsLayoutJSON(t *testing.T) {
	machine := state.NewMachine()
	objects := newFakeObjects()
	pub := &fakePublisher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &layoutClient{MockClient: mineru.MockClient{Markdown: "# layout\n"}}
	worker := NewWorker(client, machine, pub, logger,
		10*time.Millisecond, 1, 1, "file.parsed", "task.dlq", "test", WithObjectStore(objects))

	ctx := context.Background()
	if _, _, err := machine.Create(ctx, state.Task{
		ID: "t1", TenantID: "default",
		Metadata: map[string]string{"file_name": "a.pdf", "file_type": "pdf", "kb_id": "kb1"},
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	msg := queue.Message{
		Topic: "file.uploaded", Key: "t1",
		Payload: []byte(`{"task_id":"t1","tenant_id":"default","file_url":"http://objects/x","file_type":"pdf"}`),
	}
	if err := worker.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	task, _, _ := machine.Get(ctx, "t1")
	key := "default/kb1/parsed/t1/layout.json"
	if task.Metadata["layout_key"] != key {
		t.Fatalf("layout_key = %q, want %q", task.Metadata["layout_key"], key)
	}
	if got := string(objects.data[key]); got != `[{"type":"text","page_idx":0}]` {
		t.Fatalf("persisted layout = %q", got)
	}
}

func TestWorkerPersistsImagesAndRewritesLinks(t *testing.T) {
	machine := state.NewMachine()
	objects := newFakeObjects()
	pub := &fakePublisher{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := &imageClient{MockClient: mineru.MockClient{
		Markdown: "# figure\n\n![arch](images/fig1.png)\n",
	}}
	worker := NewWorker(client, machine, pub, logger,
		10*time.Millisecond, 1, 1, "file.parsed", "task.dlq", "test", WithObjectStore(objects))

	ctx := context.Background()
	if _, _, err := machine.Create(ctx, state.Task{
		ID: "t1", TenantID: "default",
		Metadata: map[string]string{"file_name": "a.pdf", "file_type": "pdf", "kb_id": "kb1"},
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	msg := queue.Message{
		Topic: "file.uploaded", Key: "t1",
		Payload: []byte(`{"task_id":"t1","tenant_id":"default","file_url":"http://objects/x","file_type":"pdf"}`),
	}
	if err := worker.Handle(ctx, msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	imageKey := "default/kb1/parsed/t1/images/fig1.png"
	if string(objects.data[imageKey]) != "PNG-BYTES" {
		t.Fatalf("image not persisted, keys=%v", keysOf(objects.data))
	}
	markdown := string(objects.data["default/kb1/parsed/t1.md"])
	if !strings.Contains(markdown, "/v1/media?key=default%2Fkb1%2Fparsed%2Ft1%2Fimages%2Ffig1.png") {
		t.Fatalf("markdown image link not rewritten: %q", markdown)
	}
	if len(pub.messages) != 1 || !strings.Contains(string(pub.messages[0].Payload), "/v1/media?key=") {
		t.Fatalf("published markdown missing media link: %+v", pub.messages)
	}
}

func keysOf(data map[string][]byte) []string {
	out := make([]string, 0, len(data))
	for key := range data {
		out = append(out, key)
	}
	return out
}
