package mineru

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestHTTPClientV1Tasks(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join("default", "upload", "task", "report.pdf")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, key)), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("# source document\n")
	if err := os.WriteFile(filepath.Join(root, key), content, 0o600); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	entry, err := zw.Create("result/full.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("# parsed markdown\n")); err != nil {
		t.Fatal(err)
	}
	figure, err := zw.Create("result/images/fig1.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := figure.Write([]byte("PNG-BYTES")); err != nil {
		t.Fatal(err)
	}
	layout, err := zw.Create("result/full_content_list.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := layout.Write([]byte(`[{"type":"text","page_idx":0}]`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	var polls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing API authorization on %s", r.URL.Path)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks":
			var body struct {
				Files []struct {
					Name  string `json:"file_name"`
					Bytes int64  `json:"bytes"`
				} `json:"files"`
				Tier    string   `json:"tier"`
				Formats []string `json:"output_formats"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if len(body.Files) != 1 || body.Files[0].Name != "report.pdf" || body.Files[0].Bytes != int64(len(content)) {
				t.Errorf("unexpected file manifest: %+v", body.Files)
			}
			if body.Tier != "standard" || len(body.Formats) != 1 || body.Formats[0] != "default" {
				t.Errorf("unexpected task options: %+v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id": "mineru-job",
				"files": []map[string]any{{
					"file_id": "file-id",
					"upload": map[string]any{
						"url":     server.URL + "/upload/file-id",
						"headers": map[string]string{"X-Upload-Token": "upload-secret"},
					},
				}},
			})
		case r.Method == http.MethodPut && r.URL.Path == "/upload/file-id":
			if r.Header.Get("X-Upload-Token") != "upload-secret" {
				t.Error("upload headers were not forwarded")
			}
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(raw, content) {
				t.Errorf("unexpected upload: %q, %v", raw, err)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"task_id": "mineru-job", "file_id": "file-id", "status": "pending"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/mineru-job":
			if polls.Add(1) == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"task_id": "mineru-job", "status": "running"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id": "mineru-job",
				"status":  "completed",
				"files": []map[string]any{{
					"file_id": "file-id", "status": "completed", "result_url": server.URL + "/result.zip",
				}},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/result.zip":
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, 0, 1)
	client.APIKey = "secret"
	client.Tier = "standard"
	client.ObjectDir = root
	client.ObjectBase = "http://objects/nexusflow"
	jobID, err := client.Submit(context.Background(), "nexusflow-task", "http://objects/nexusflow/default/upload/task/report.pdf", "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if jobID != "mineru-job" {
		t.Fatalf("job ID = %q", jobID)
	}
	if _, done, err := client.Poll(context.Background(), jobID); err != nil || done {
		t.Fatalf("first poll: done=%v err=%v", done, err)
	}
	doc, done, err := client.PollDocument(context.Background(), jobID)
	if err != nil || !done || doc.Markdown != "# parsed markdown\n" {
		t.Fatalf("second poll: doc=%+v done=%v err=%v", doc, done, err)
	}
	if got := string(doc.Images["images/fig1.png"]); got != "PNG-BYTES" {
		t.Fatalf("extracted image = %q, want PNG-BYTES (all=%v)", got, doc.Images)
	}
	if got := string(doc.Layout); got != `[{"type":"text","page_idx":0}]` {
		t.Fatalf("extracted layout = %q", got)
	}
}

func TestHTTPClientTextPassthrough(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default", "note.md"), []byte("# plain text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewHTTPClient("http://unused", 0, 1)
	client.ObjectDir = root
	client.ObjectBase = "http://objects/nexusflow"
	jobID, err := client.Submit(context.Background(), "text-task", "http://objects/nexusflow/default/note.md", "md")
	if err != nil {
		t.Fatal(err)
	}
	markdown, done, err := client.Poll(context.Background(), jobID)
	if err != nil || !done || markdown != "# plain text\n" {
		t.Fatalf("text passthrough: markdown=%q done=%v err=%v", markdown, done, err)
	}
}

func TestOpenObjectRejectsOutsideURLsAndTraversal(t *testing.T) {
	client := NewHTTPClient("http://mineru", 0, 1)
	client.ObjectDir = t.TempDir()
	client.ObjectBase = "http://objects/nexusflow"
	for _, fileURL := range []string{
		"http://other/nexusflow/default/a.pdf",
		"http://objects/other/default/a.pdf",
		"http://objects/nexusflow/../secret.pdf",
		"http://objects/nexusflow/default/%2E%2E/secret.pdf",
	} {
		file, _, _, err := client.openObject(fileURL)
		if file != nil {
			file.Close()
		}
		if err == nil {
			t.Errorf("openObject(%q) unexpectedly succeeded", fileURL)
		}
	}
}
