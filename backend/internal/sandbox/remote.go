package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	pathpkg "path"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

const (
	// defaultPythonImage / defaultNodeImage are used when the caller passes a
	// bare runtime ("python" / "node") instead of a fully-qualified image. The
	// image must ship a shell so the server can stage execd into it.
	defaultPythonImage = "python:3.12"
	defaultNodeImage   = "node:18"
	// remoteCreateTimeout bounds sandbox provisioning plus the readiness wait.
	remoteCreateTimeout = 2 * time.Minute
)

// RemoteBackend maps the sandbox contract onto the official OpenSandbox Go SDK.
//
// It relies on the SDK for the whole lifecycle (create + wait-for-ready, execd
// command execution with SSE streaming, file transfer through execd, and hard
// deletion). The control plane authenticates with OPEN-SANDBOX-API-KEY, which
// the SDK derives from ConnectionConfig.APIKey.
type RemoteBackend struct {
	Config      opensandbox.ConnectionConfig
	PythonImage string
	NodeImage   string
}

// NewRemoteBackend configures the SDK against baseURL (http(s)://host:port,
// optional /v1 suffix) and the server API key.
func NewRemoteBackend(baseURL, token string) *RemoteBackend {
	return NewRemoteBackendWithImages(baseURL, token, defaultPythonImage, defaultNodeImage)
}

// NewRemoteBackendWithImages allows overriding the runtime images so operators
// can point at an internal registry or a pinned base image.
func NewRemoteBackendWithImages(baseURL, token, pythonImage, nodeImage string) *RemoteBackend {
	if strings.TrimSpace(pythonImage) == "" {
		pythonImage = defaultPythonImage
	}
	if strings.TrimSpace(nodeImage) == "" {
		nodeImage = defaultNodeImage
	}
	return &RemoteBackend{
		Config:      connectionConfig(baseURL, token),
		PythonImage: pythonImage,
		NodeImage:   nodeImage,
	}
}

// connectionConfig splits the configured URL into the SDK's domain/protocol
// pair. The SDK always appends the /v1 lifecycle prefix itself.
func connectionConfig(baseURL, token string) opensandbox.ConnectionConfig {
	cfg := opensandbox.ConnectionConfig{APIKey: token, Protocol: "http"}
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return cfg
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return cfg
	}
	cfg.Domain = u.Host
	if u.Scheme != "" {
		cfg.Protocol = u.Scheme
	}
	return cfg
}

func (b *RemoteBackend) imageFor(runtime string) string {
	rt := strings.ToLower(strings.TrimSpace(runtime))
	switch {
	case rt == "":
		return b.PythonImage
	case strings.HasPrefix(rt, "node"):
		// A versioned runtime such as "node:20" is already a valid image tag.
		if strings.Contains(rt, ":") {
			return rt
		}
		return b.NodeImage
	case strings.HasPrefix(rt, "python"):
		if strings.Contains(rt, ":") {
			return rt
		}
		return b.PythonImage
	default:
		return rt
	}
}

// egressPolicy converts the allowlist into a deny-by-default policy. An empty
// allowlist keeps the server default instead of silently blocking egress.
func egressPolicy(allow []string) *opensandbox.NetworkPolicy {
	rules := make([]opensandbox.NetworkRule, 0, len(allow))
	for _, target := range allow {
		if target = strings.TrimSpace(target); target != "" {
			rules = append(rules, opensandbox.NetworkRule{Action: "allow", Target: target})
		}
	}
	if len(rules) == 0 {
		return nil
	}
	return &opensandbox.NetworkPolicy{DefaultAction: "deny", Egress: rules}
}

func ttlSeconds(ttl time.Duration) int {
	seconds := int(ttl.Seconds())
	if seconds <= 0 {
		seconds = 300 // the execution contract always enforces a TTL
	}
	return seconds
}

func (b *RemoteBackend) Create(ctx context.Context, spec Spec) (Instance, error) {
	if b.Config.Domain == "" {
		return nil, fmt.Errorf("opensandbox: server url is empty")
	}
	createCtx, cancel := context.WithTimeout(ctx, remoteCreateTimeout)
	defer cancel()

	opts := opensandbox.SandboxCreateOptions{
		Image:          b.imageFor(spec.Runtime),
		Env:            spec.Env,
		TimeoutSeconds: ttlPtr(ttlSeconds(spec.TTL)),
		NetworkPolicy:  egressPolicy(spec.EgressAllow),
	}
	// Packages are not installed at create time; the runtime image is expected
	// to preinstall them, matching the local backend's behaviour.
	sbx, err := opensandbox.CreateSandbox(createCtx, b.Config, opts)
	if err != nil {
		return nil, fmt.Errorf("opensandbox create: %w", err)
	}
	return &remoteInstance{sbx: sbx, runtime: spec.Runtime}, nil
}

func ttlPtr(seconds int) *int { return &seconds }

type remoteInstance struct {
	sbx     *opensandbox.Sandbox
	runtime string

	mu        sync.Mutex
	destroyed bool
}

func (i *remoteInstance) ID() string { return i.sbx.ID() }

func (i *remoteInstance) WriteFile(ctx context.Context, path string, data []byte) error {
	err := i.sbx.UploadFile(ctx, bytes.NewReader(data), opensandbox.UploadFileOptions{
		FileName: pathpkg.Base(path),
		Metadata: opensandbox.FileMetadata{Path: path},
	})
	if err != nil {
		return fmt.Errorf("opensandbox write %s: %w", path, err)
	}
	return nil
}

func (i *remoteInstance) ReadFile(ctx context.Context, path string) ([]byte, error) {
	rc, err := i.sbx.DownloadFile(ctx, path, "")
	if err != nil {
		return nil, fmt.Errorf("opensandbox read %s: %w", path, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("opensandbox read %s: %w", path, err)
	}
	return data, nil
}

// RunCode uploads the source to entryFile and runs it through execd, mirroring
// the local backend's contract (code is a file, args are passed verbatim).
func (i *remoteInstance) RunCode(ctx context.Context, code, entryFile string, args []string) (RunResult, error) {
	if err := i.WriteFile(ctx, entryFile, []byte(code)); err != nil {
		return RunResult{}, err
	}
	interpreter := "python3"
	if strings.HasPrefix(strings.ToLower(i.runtime), "node") {
		interpreter = "node"
	}
	argv := append([]string{interpreter, entryFile}, args...)

	// execd requires Command (shell form); Argv alone is rejected by the server
	// validator. Every element is quoted so user-supplied args cannot inject
	// shell syntax.
	req := opensandbox.RunCommandRequest{Command: shellJoin(argv)}
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining > 0 {
			req.Timeout = remaining.Milliseconds()
		}
	}

	exec, err := i.sbx.RunCommandWithOpts(ctx, req, nil)
	res := RunResult{}
	if exec != nil {
		res.Stdout = exec.Text()
		res.Stderr = outputText(exec.Stderr)
	}
	if exec != nil && exec.Error != nil {
		if code, ok := exitCode(exec); ok {
			return res, fmt.Errorf("code exited non-zero: exit code %d: %s", code, strings.TrimSpace(res.Stderr))
		}
		return res, fmt.Errorf("code execution failed: %s %s", exec.Error.Name, exec.Error.Value)
	}
	if err != nil {
		return res, fmt.Errorf("opensandbox exec: %w", err)
	}
	return res, nil
}

func (i *remoteInstance) Destroy(ctx context.Context) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.destroyed {
		return nil
	}
	i.destroyed = true
	if err := i.sbx.Kill(ctx); err != nil {
		return fmt.Errorf("opensandbox delete: %w", err)
	}
	return nil
}

// shellJoin renders argv as a POSIX shell command with every element quoted, so
// values like "/tmp/input.json" cannot be interpreted as shell syntax.
func shellJoin(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

// outputText joins streamed stderr fragments, inserting a newline only when a
// fragment ends without one (execd emits line-oriented events).
func outputText(messages []opensandbox.OutputMessage) string {
	var b strings.Builder
	for idx, msg := range messages {
		if idx > 0 {
			prev := messages[idx-1].Text
			if !strings.HasSuffix(prev, "\n") && !strings.HasSuffix(prev, "\r") {
				b.WriteByte('\n')
			}
		}
		b.WriteString(msg.Text)
	}
	return b.String()
}

func exitCode(exec *opensandbox.Execution) (int, bool) {
	if exec.Error != nil && exec.Error.ExitCode != nil {
		return *exec.Error.ExitCode, true
	}
	if exec.ExitCode != nil {
		return *exec.ExitCode, true
	}
	return 0, false
}
