package sandbox

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewService(NewLocalBackend(t.TempDir()), logger)
}

func TestServiceExecutesCodeAndCollectsOutput(t *testing.T) {
	svc := newTestService(t)
	code := `
import json
with open("/tmp/input.json", encoding="utf-8") as fh:
    data = json.load(fh)
with open("/tmp/output.json", "w", encoding="utf-8") as fh:
    json.dump({"echo": data["value"]}, fh)
`
	resp := svc.Execute(context.Background(), ExecuteRequest{
		TaskID:    "t1",
		NodeID:    "n1",
		Runtime:   "python:3.11",
		Code:      code,
		InputJSON: `{"value":"hello"}`,
		Timeout:   15 * time.Second,
	})

	if !resp.Success {
		t.Fatalf("execute failed: %s (stderr=%s)", resp.ErrorMsg, resp.Stderr)
	}
	if !strings.Contains(resp.OutputJSON, "hello") {
		t.Fatalf("expected echoed value, got %q", resp.OutputJSON)
	}
	if resp.SandboxID == "" {
		t.Fatal("expected sandbox id")
	}
}

func TestServiceCapturesFailure(t *testing.T) {
	svc := newTestService(t)
	resp := svc.Execute(context.Background(), ExecuteRequest{
		Runtime: "python:3.11",
		Code:    "raise SystemExit(3)",
		Timeout: 15 * time.Second,
	})
	if resp.Success {
		t.Fatal("expected failure for non-zero exit")
	}
}
