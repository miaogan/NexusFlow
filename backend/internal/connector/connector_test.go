package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSource struct {
	kind    Type
	batches []Batch
	err     error
	calls   int
	cursors []map[string]string
}

func (f *fakeSource) Type() Type            { return f.kind }
func (f *fakeSource) Validate(Config) error { return nil }
func (f *fakeSource) Pull(_ context.Context, _ Config, cursor map[string]string) (Batch, error) {
	f.calls++
	f.cursors = append(f.cursors, cursor)
	if f.err != nil {
		return Batch{}, f.err
	}
	if len(f.batches) == 0 {
		return Batch{Done: true}, nil
	}
	batch := f.batches[0]
	f.batches = f.batches[1:]
	return batch, nil
}

type fakeRegistrar struct {
	existing map[string]bool
	tasks    []string
	err      error
}

func (r *fakeRegistrar) RegisterContent(
	_ context.Context,
	_, _, _, externalID, _, _ string, _ []byte,
) (string, bool, error) {
	if r.err != nil {
		return "", false, r.err
	}
	task := "task-" + externalID
	if r.existing[externalID] {
		return task, false, nil
	}
	r.tasks = append(r.tasks, task)
	return task, true, nil
}

func newTestService(t *testing.T, source Source, registrar Registrar) (*Service, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore(NewCipher("test-secret"))
	svc := NewService(store, registrar, slog.New(slog.NewTextHandler(io.Discard, nil)), source)
	return svc, store
}

func TestServiceSyncCreatesAndSkipsDocuments(t *testing.T) {
	source := &fakeSource{kind: TypeWeb, batches: []Batch{{
		Items: []Item{
			{ExternalID: "a", FileName: "a.html", FileType: "html", Content: []byte("<p>a</p>")},
			{ExternalID: "b", FileName: "b.html", FileType: "html", Content: []byte("<p>b</p>")},
		},
		NextCursor: map[string]string{"seen": `["a","b"]`},
		Done:       true,
	}}}
	reg := &fakeRegistrar{existing: map[string]bool{"b": true}}
	svc, store := newTestService(t, source, reg)

	spec, err := svc.Create(context.Background(), "t1", Spec{
		Name: "docs", KBID: "kb1", Type: TypeWeb, Config: Config{URLs: []string{"http://example.com"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	result, err := svc.Sync(context.Background(), "t1", spec.ID)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if result.Created != 1 || result.Skipped != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	saved, ok, _ := store.Get(context.Background(), "t1", spec.ID)
	if !ok || saved.Status != StatusIdle || saved.LastSyncAt.IsZero() {
		t.Fatalf("saved = %+v ok=%v", saved, ok)
	}
	if saved.SyncCursor["seen"] == "" {
		t.Fatalf("cursor not persisted: %+v", saved.SyncCursor)
	}
}

func TestServiceSyncMarksPullFailure(t *testing.T) {
	source := &fakeSource{kind: TypeWeb, err: errors.New("upstream down")}
	svc, store := newTestService(t, source, &fakeRegistrar{})
	spec, err := svc.Create(context.Background(), "t1", Spec{
		Name: "docs", KBID: "kb1", Type: TypeWeb, Config: Config{URLs: []string{"http://example.com"}},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := svc.Sync(context.Background(), "t1", spec.ID); err == nil {
		t.Fatal("expected sync error")
	}
	saved, _, _ := store.Get(context.Background(), "t1", spec.ID)
	if saved.Status != StatusError || !strings.Contains(saved.LastError, "upstream down") {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestServiceCreateValidatesTypeAndName(t *testing.T) {
	svc, _ := newTestService(t, &fakeSource{kind: TypeWeb}, &fakeRegistrar{})
	if _, err := svc.Create(context.Background(), "t1", Spec{Name: "", KBID: "kb", Type: TypeWeb}); err == nil {
		t.Fatal("expected empty name to fail")
	}
	if _, err := svc.Create(context.Background(), "t1", Spec{Name: "x", KBID: "kb", Type: TypeS3}); err == nil {
		t.Fatal("expected unsupported type to fail")
	}
}

func TestWebSourceCrawlsLinksAndHonoursCursor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><a href="/next">next</a></html>`))
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain text"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	source := WebSource{}
	cfg := Config{URLs: []string{server.URL}, MaxDepth: 1, MaxPages: 5, AllowPrivate: true}
	batch, err := source.Pull(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(batch.Items) != 2 {
		t.Fatalf("items = %d, want 2 (%+v)", len(batch.Items), batch.Items)
	}
	for _, item := range batch.Items {
		if strings.HasSuffix(item.FileName, ".html.html") {
			t.Fatalf("file name has a doubled extension: %q", item.FileName)
		}
	}
	next, err := source.Pull(context.Background(), cfg, batch.NextCursor)
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if len(next.Items) != 0 {
		t.Fatalf("cursor should skip seen pages, got %d", len(next.Items))
	}

	var seen []string
	if err := json.Unmarshal([]byte(batch.NextCursor["seen"]), &seen); err != nil || len(seen) != 2 {
		t.Fatalf("seen cursor = %q err=%v", batch.NextCursor["seen"], err)
	}
}

func TestCipherRoundTripAndPlaintext(t *testing.T) {
	cipher := NewCipher("secret")
	encoded, err := cipher.Encrypt([]byte(`{"bucket":"b"}`))
	if err != nil || !strings.HasPrefix(encoded, "v1:") {
		t.Fatalf("encrypt = %q err=%v", encoded, err)
	}
	plain, err := cipher.Decrypt(encoded)
	if err != nil || string(plain) != `{"bucket":"b"}` {
		t.Fatalf("decrypt = %q err=%v", plain, err)
	}
	if _, err := NewCipher("").Decrypt("v1:abc"); err == nil {
		t.Fatal("encrypted value without key must fail")
	}
}

func TestDBSourceRejectsWrites(t *testing.T) {
	source := DBSource{}
	if err := source.Validate(Config{DSN: "postgres://x", SQL: "DELETE FROM users"}); err == nil {
		t.Fatal("expected write SQL to be rejected")
	}
	if err := source.Validate(Config{DSN: "postgres://x", SQL: "SELECT * FROM users"}); err != nil {
		t.Fatalf("select rejected: %v", err)
	}
}
