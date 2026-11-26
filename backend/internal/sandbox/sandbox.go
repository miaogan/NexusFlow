package sandbox

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// Spec describes an isolated execution environment.
type Spec struct {
	Runtime     string
	Packages    []string
	Env         map[string]string
	TTL         time.Duration
	EgressAllow []string
}

type RunResult struct {
	Stdout string
	Stderr string
}

// Instance is a live sandbox. Every implementation must be safe to Destroy
// more than once.
type Instance interface {
	ID() string
	WriteFile(ctx context.Context, path string, data []byte) error
	RunCode(ctx context.Context, code, entryFile string, args []string) (RunResult, error)
	ReadFile(ctx context.Context, path string) ([]byte, error)
	Destroy(ctx context.Context) error
}

// Backend creates sandboxes. LocalBackend is used in development and
// RemoteBackend maps to OpenSandbox Server in production.
type Backend interface {
	Create(ctx context.Context, spec Spec) (Instance, error)
}

type ExecuteRequest struct {
	TaskID    string
	NodeID    string
	Runtime   string
	Code      string
	Packages  []string
	Env       map[string]string
	InputJSON string
	Timeout   time.Duration
}

type ExecuteResponse struct {
	Success    bool
	OutputJSON string
	Stdout     string
	Stderr     string
	SandboxID  string
	ErrorMsg   string
}

type Service struct {
	backend    Backend
	logger     *slog.Logger
	defaultTTL time.Duration
}

func NewService(backend Backend, logger *slog.Logger) *Service {
	return &Service{backend: backend, logger: logger, defaultTTL: 5 * time.Minute}
}

// Execute implements the guardrails from chapter 9/11: mandatory TTL,
// unconditional destroy and file-based input/output.
func (s *Service) Execute(ctx context.Context, req ExecuteRequest) ExecuteResponse {
	ttl := req.Timeout
	if ttl <= 0 {
		ttl = s.defaultTTL
	}

	inst, err := s.backend.Create(ctx, Spec{
		Runtime:  req.Runtime,
		Packages: req.Packages,
		Env:      req.Env,
		TTL:      ttl,
	})
	if err != nil {
		return ExecuteResponse{ErrorMsg: "create sandbox: " + err.Error()}
	}
	// Unconditional cleanup: never leak a sandbox even on panic.
	defer func() {
		destroyCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := inst.Destroy(destroyCtx); err != nil {
			s.logger.Warn("sandbox destroy failed", "sandbox_id", inst.ID(), "error", err)
		}
	}()

	resp := ExecuteResponse{SandboxID: inst.ID()}

	if req.InputJSON != "" {
		if err := inst.WriteFile(ctx, "/tmp/input.json", []byte(req.InputJSON)); err != nil {
			resp.ErrorMsg = "write input: " + err.Error()
			return resp
		}
	}

	execCtx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()

	res, runErr := inst.RunCode(execCtx, req.Code, "/tmp/main.py", []string{"/tmp/input.json"})
	resp.Stdout = res.Stdout
	resp.Stderr = res.Stderr
	if runErr != nil {
		resp.ErrorMsg = runErr.Error()
		return resp
	}

	out, readErr := inst.ReadFile(execCtx, "/tmp/output.json")
	if readErr != nil || !json.Valid(out) {
		resp.OutputJSON = res.Stdout
	} else {
		resp.OutputJSON = string(out)
	}
	resp.Success = true
	return resp
}
