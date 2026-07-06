package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBroker_NeverWiresStdin verifies that the broker never connects
// caller-supplied stdin to the host process, even for a command whose
// (now-removed) legacy config used to opt in via AllowStdin/host_commands
// `stdin: true`. The host-command path always runs with stdin detached, so
// there is no per-command opt-in left to test.
func TestBroker_NeverWiresStdin(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"cat": {
			Name:            "cat",
			Path:            "/bin/cat",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "cat",
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "" {
		t.Fatalf("stdout = %q, want empty (no stdin wired, cat reads nothing)", resp.Stdout)
	}
}
