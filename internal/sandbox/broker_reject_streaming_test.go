//go:build linux

package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBroker_RejectRule_Streaming mirrors TestBroker_RejectRule_NonStreaming
// for the streaming exec path (broker_streaming_linux.go), verifying the
// shared gate produces identical behavior on both call sites.
func TestBroker_RejectRule_Streaming(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"/usr/bin/gh": {
				Name:            "gh",
				Path:            "/usr/bin/gh",
				AllowedPatterns: []string{"*"},
				RejectRules: []sandbox.RejectRule{
					{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
				},
			},
		},
		testCtx,
	)

	stdout, stderr, exitCode := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "/usr/bin/gh",
		Args:    []string{"pr", "create", "--body-file", "/tmp/x"},
		Token:   token,
	})
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	want := "host_commands.gh: rejected: sandbox paths are not visible on the host"
	if stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want empty", stdout)
	}
}

// TestBroker_RejectRule_StreamingNonMatchingStillPasses is the streaming
// counterpart of TestBroker_RejectRule_NonMatchingStillPasses.
func TestBroker_RejectRule_StreamingNonMatchingStillPasses(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"/bin/echo": {
				Name:            "echo",
				Path:            "/bin/echo",
				AllowedPatterns: []string{"*"},
				RejectRules: []sandbox.RejectRule{
					{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
				},
			},
		},
		testCtx,
	)

	stdout, _, exitCode := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if stdout == "" {
		t.Fatalf("expected echo output, got empty stdout")
	}
}
