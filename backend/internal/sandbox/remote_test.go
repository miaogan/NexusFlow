package sandbox

import (
	"testing"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

func TestConnectionConfigParsesServerURL(t *testing.T) {
	cases := []struct {
		raw      string
		domain   string
		protocol string
	}{
		{"http://opensandbox:8080", "opensandbox:8080", "http"},
		{"https://sandbox.example.com/v1", "sandbox.example.com", "https"},
		{"opensandbox.nexusflow.svc:8080", "opensandbox.nexusflow.svc:8080", "http"},
		{"", "", "http"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg := connectionConfig(tc.raw, "secret")
			if cfg.Domain != tc.domain || cfg.Protocol != tc.protocol {
				t.Fatalf("connectionConfig(%q) = %s/%s, want %s/%s",
					tc.raw, cfg.Domain, cfg.Protocol, tc.domain, tc.protocol)
			}
			if cfg.APIKey != "secret" {
				t.Fatalf("api key = %q", cfg.APIKey)
			}
		})
	}
}

func TestImageForRuntime(t *testing.T) {
	backend := NewRemoteBackendWithImages("http://localhost:8080", "k", "python:3.12", "node:18")
	cases := map[string]string{
		"":            "python:3.12",
		"python":      "python:3.12",
		"python:3.11": "python:3.11",
		"node":        "node:18",
		"node:20":     "node:20",
		"custom:tag":  "custom:tag",
	}
	for runtime, want := range cases {
		if got := backend.imageFor(runtime); got != want {
			t.Errorf("imageFor(%q) = %q, want %q", runtime, got, want)
		}
	}
}

func TestEgressPolicyIsDenyByDefaultOnlyWhenScoped(t *testing.T) {
	if policy := egressPolicy(nil); policy != nil {
		t.Fatalf("empty allowlist must keep server default, got %+v", policy)
	}
	policy := egressPolicy([]string{" api.example.com ", ""})
	if policy == nil || policy.DefaultAction != "deny" || len(policy.Egress) != 1 {
		t.Fatalf("policy = %+v", policy)
	}
	rule := policy.Egress[0]
	if rule.Action != "allow" || rule.Target != "api.example.com" {
		t.Fatalf("rule = %+v", rule)
	}
}

func TestTTLSecondsAlwaysPositive(t *testing.T) {
	if got := ttlSeconds(0); got != 300 {
		t.Fatalf("default ttl = %d, want 300", got)
	}
	if got := ttlSeconds(90 * time.Second); got != 90 {
		t.Fatalf("ttl = %d, want 90", got)
	}
}

func TestShellJoinQuotesEveryArgument(t *testing.T) {
	got := shellJoin([]string{"python3", "/tmp/main.py", "/tmp/input.json"})
	if want := "'python3' '/tmp/main.py' '/tmp/input.json'"; got != want {
		t.Fatalf("shellJoin = %q, want %q", got, want)
	}
	if got := shellJoin([]string{"sh", "a'b"}); got != `'sh' 'a'\''b'` {
		t.Fatalf("quote escaping = %q", got)
	}
}

func TestOutputTextJoinsFragments(t *testing.T) {
	got := outputText([]opensandbox.OutputMessage{
		{Text: "line1"},
		{Text: "line2\n"},
		{Text: "tail"},
	})
	if got != "line1\nline2\ntail" {
		t.Fatalf("outputText = %q", got)
	}
	if got := outputText(nil); got != "" {
		t.Fatalf("empty output = %q", got)
	}
}

func TestExitCodePrefersExecutionError(t *testing.T) {
	code := 2
	exec := &opensandbox.Execution{
		Error:    &opensandbox.ExecutionError{ExitCode: &code},
		ExitCode: intPtr(0),
	}
	if got, ok := exitCode(exec); !ok || got != 2 {
		t.Fatalf("exitCode = %d/%v, want 2/true", got, ok)
	}
	if _, ok := exitCode(&opensandbox.Execution{}); ok {
		t.Fatal("missing exit code should report false")
	}
}

func intPtr(v int) *int { return &v }
