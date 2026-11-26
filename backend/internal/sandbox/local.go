package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LocalBackend executes user code in a throwaway directory on the local host.
// It exists so the pipeline can be exercised without an OpenSandbox cluster.
// It is NOT a security boundary: never enable it for untrusted production
// traffic, use RemoteBackend there instead.
type LocalBackend struct {
	WorkDir string
}

func NewLocalBackend(workDir string) *LocalBackend {
	return &LocalBackend{WorkDir: workDir}
}

type localInstance struct {
	id      string
	dir     string
	runtime string
	env     map[string]string
}

func (b *LocalBackend) Create(_ context.Context, spec Spec) (Instance, error) {
	if err := os.MkdirAll(b.WorkDir, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(b.WorkDir, "sbx-*")
	if err != nil {
		return nil, err
	}
	return &localInstance{
		id:      filepath.Base(dir),
		dir:     dir,
		runtime: spec.Runtime,
		env:     spec.Env,
	}, nil
}

func (i *localInstance) ID() string { return i.id }

func (i *localInstance) resolve(p string) (string, error) {
	clean := filepath.Clean("/" + p)
	full := filepath.Join(i.dir, clean)
	if !strings.HasPrefix(full, i.dir+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes sandbox", p)
	}
	return full, nil
}

func (i *localInstance) WriteFile(_ context.Context, path string, data []byte) error {
	full, err := i.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

func (i *localInstance) ReadFile(_ context.Context, path string) ([]byte, error) {
	full, err := i.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (i *localInstance) RunCode(ctx context.Context, code, entryFile string, args []string) (RunResult, error) {
	full, err := i.resolve(entryFile)
	if err != nil {
		return RunResult{}, err
	}

	// The execution contract uses /tmp/input.json and /tmp/output.json inside
	// the sandbox. The local backend has no mount namespace, so it rewrites
	// those contract paths to the temp directory before running. Only the
	// contract paths are rewritten; this backend is for development only.
	inputPath, _ := i.resolve("/tmp/input.json")
	outputPath, _ := i.resolve("/tmp/output.json")
	rewritten := strings.ReplaceAll(code, "/tmp/input.json", inputPath)
	rewritten = strings.ReplaceAll(rewritten, "/tmp/output.json", outputPath)
	rewritten = strings.ReplaceAll(rewritten, "/tmp/main.py", full)

	if err := os.WriteFile(full, []byte(rewritten), 0o644); err != nil {
		return RunResult{}, err
	}

	var bin string
	switch {
	case strings.HasPrefix(i.runtime, "node"):
		bin = "node"
	default:
		bin = "python3"
	}
	cmdArgs := append([]string{full}, args...)
	cmd := exec.CommandContext(ctx, bin, cmdArgs...)
	cmd.Dir = i.dir
	cmd.Env = os.Environ()
	for k, v := range i.env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); ok {
			return res, fmt.Errorf("code exited non-zero: %v: %s", runErr, strings.TrimSpace(stderr.String()))
		}
		return res, runErr
	}
	return res, nil
}

func (i *localInstance) Destroy(_ context.Context) error {
	return os.RemoveAll(i.dir)
}
