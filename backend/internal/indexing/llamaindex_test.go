package indexing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestLlamaIndexIndexerSendsDocument(t *testing.T) {
	var got llamaIndexIndexRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/index" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"chunk_count":3,"indexed_count":3,"collection":"nexusflow_kb1"}`)
	}))
	defer server.Close()

	n, err := NewLlamaIndexIndexer(server.URL, time.Second).IndexMarkdown(
		context.Background(), "task-1", "# hello",
		map[string]string{"tenant_id": "t1", "kb_id": "kb1", "file_name": "a.md"},
	)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if n != 3 {
		t.Errorf("indexed count = %d, want 3", n)
	}
	if got.TaskID != "task-1" || got.Markdown != "# hello" || got.SplitStrategy != "markdown" {
		t.Errorf("request = %+v", got)
	}
	if got.Metadata["tenant_id"] != "t1" || got.Metadata["kb_id"] != "kb1" {
		t.Errorf("metadata = %v", got.Metadata)
	}
}

func TestLlamaIndexIndexerSurfacesErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "embedding unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if _, err := NewLlamaIndexIndexer(server.URL, time.Second).IndexMarkdown(
		context.Background(), "t", "md", nil,
	); err == nil {
		t.Fatal("expected an error for HTTP 503")
	}

	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":false,"error_msg":"no nodes produced"}`)
	}))
	defer failed.Close()
	if _, err := NewLlamaIndexIndexer(failed.URL, time.Second).IndexMarkdown(
		context.Background(), "t", "md", nil,
	); err == nil {
		t.Fatal("expected an error for success=false")
	}

	if _, err := NewLlamaIndexIndexer("", time.Second).IndexMarkdown(
		context.Background(), "t", "md", nil,
	); err == nil {
		t.Fatal("expected an error for an empty url")
	}
}
