package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RemoteBackend adapts the OpenSandbox Server control-plane API.
//
// The endpoint paths below mirror the lifecycle used by the Go SDK in the
// design doc (chapter 9/11). If your OpenSandbox version exposes different
// paths, adjust this adapter only; the rest of the system depends on the
// Backend/Instance interfaces.
type RemoteBackend struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewRemoteBackend(baseURL, token string) *RemoteBackend {
	return &RemoteBackend{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

func (b *RemoteBackend) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("opensandbox %s %s: status %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (b *RemoteBackend) Create(ctx context.Context, spec Spec) (Instance, error) {
	body := map[string]any{
		"runtime":      spec.Runtime,
		"packages":     spec.Packages,
		"env":          spec.Env,
		"egress_allow": spec.EgressAllow,
		"ttl_seconds":  int64(spec.TTL.Seconds()),
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := b.do(ctx, http.MethodPost, "/v1/sandboxes", body, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("opensandbox: empty sandbox id")
	}
	return &remoteInstance{id: out.ID, backend: b}, nil
}

type remoteInstance struct {
	id      string
	backend *RemoteBackend
}

func (i *remoteInstance) ID() string { return i.id }

func (i *remoteInstance) fp(path string) string {
	return "/v1/sandboxes/" + url.PathEscape(i.id) + "/files?path=" + url.QueryEscape(path)
}

func (i *remoteInstance) WriteFile(ctx context.Context, path string, data []byte) error {
	body := map[string]string{"path": path, "content": string(data)}
	return i.backend.do(ctx, http.MethodPut, "/v1/sandboxes/"+url.PathEscape(i.id)+"/files", body, nil)
}

func (i *remoteInstance) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var out struct {
		Content string `json:"content"`
	}
	if err := i.backend.do(ctx, http.MethodGet, i.fp(path), nil, &out); err != nil {
		return nil, err
	}
	return []byte(out.Content), nil
}

func (i *remoteInstance) RunCode(ctx context.Context, code, entryFile string, args []string) (RunResult, error) {
	body := map[string]any{"code": code, "entry_file": entryFile, "args": args}
	var out struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	}
	if err := i.backend.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(i.id)+"/exec", body, &out); err != nil {
		return RunResult{Stdout: out.Stdout, Stderr: out.Stderr}, err
	}
	return RunResult{Stdout: out.Stdout, Stderr: out.Stderr}, nil
}

func (i *remoteInstance) Destroy(ctx context.Context) error {
	return i.backend.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(i.id), nil, nil)
}
