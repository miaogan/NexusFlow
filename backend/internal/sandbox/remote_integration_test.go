package sandbox

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRemoteBackendIntegration exercises the full OpenSandbox path against a
// live server: create + wait-ready, file upload, execd execution, output
// download and hard deletion. It is skipped unless OPENSANDBOX_TEST_URL points
// at a running server, e.g.:
//
//	OPENSANDBOX_TEST_URL=http://127.0.0.1:28080 \
//	OPENSANDBOX_TEST_TOKEN=test-api-key \
//	OPENSANDBOX_TEST_IMAGE=opensandbox/code-interpreter:latest \
//	    go test ./internal/sandbox -run Remote -v
func TestRemoteBackendIntegration(t *testing.T) {
	baseURL := os.Getenv("OPENSANDBOX_TEST_URL")
	if baseURL == "" {
		t.Skip("set OPENSANDBOX_TEST_URL to run the OpenSandbox integration test")
	}
	image := os.Getenv("OPENSANDBOX_TEST_IMAGE")
	if image == "" {
		image = "opensandbox/code-interpreter:latest"
	}
	backend := NewRemoteBackendWithImages(baseURL, os.Getenv("OPENSANDBOX_TEST_TOKEN"), image, image)
	// The sandbox endpoint is only reachable from inside the server's network,
	// so route execd traffic through the lifecycle server proxy.
	backend.Config.UseServerProxy = true
	svc := NewService(backend, slog.New(slog.NewTextHandler(io.Discard, nil)))

	code := `
import json
with open("/tmp/input.json", encoding="utf-8") as fh:
    data = json.load(fh)
with open("/tmp/output.json", "w", encoding="utf-8") as fh:
    json.dump({"echo": data["value"]}, fh)
print("stdout from sandbox")
`
	resp := svc.Execute(context.Background(), ExecuteRequest{
		TaskID:    "it-task",
		NodeID:    "it-node",
		Runtime:   "python",
		Code:      code,
		InputJSON: `{"value":"hello-opensandbox"}`,
		Timeout:   3 * time.Minute,
	})
	if !resp.Success {
		t.Fatalf("execute failed: %s (stdout=%q stderr=%q)", resp.ErrorMsg, resp.Stdout, resp.Stderr)
	}
	if resp.SandboxID == "" {
		t.Fatal("expected a sandbox id")
	}
	if !strings.Contains(resp.OutputJSON, "hello-opensandbox") {
		t.Fatalf("output json = %q, want the echoed value", resp.OutputJSON)
	}
	if !strings.Contains(resp.Stdout, "stdout from sandbox") {
		t.Fatalf("stdout = %q, want the printed line", resp.Stdout)
	}
}
