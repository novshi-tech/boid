package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBroker_RejectRule_NonStreaming verifies that a matching RejectRule
// blocks the call with the exact "host_commands.<name>: rejected: <reason>"
// stderr message, ahead of the generic CheckPolicy gate.
func TestBroker_RejectRule_NonStreaming(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/usr/bin/gh": {
			Name:            "gh",
			Path:            "/usr/bin/gh",
			AllowedPatterns: []string{"*"},
			RejectRules: []sandbox.RejectRule{
				{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
			},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/gh",
		Args:    []string{"pr", "create", "--body-file", "/tmp/x"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	want := "host_commands.gh: rejected: sandbox paths are not visible on the host"
	if resp.Stderr != want {
		t.Fatalf("stderr = %q, want %q", resp.Stderr, want)
	}
}

// TestBroker_RejectRule_NonMatchingStillPasses verifies that a RejectRule
// which does not match the invocation leaves CheckPolicy in charge as before.
func TestBroker_RejectRule_NonMatchingStillPasses(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/echo": {
			Name:            "echo",
			Path:            "/bin/echo",
			AllowedPatterns: []string{"*"},
			RejectRules: []sandbox.RejectRule{
				{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
			},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
		Token:   token,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
}

// TestBroker_RejectRule_PrecedesArgumentsNotAllowed verifies that a reject
// rule takes precedence over the generic "arguments not allowed" message: an
// invocation that both fails CheckPolicy and matches a reject rule must
// surface the actionable reject message, not the generic one.
func TestBroker_RejectRule_PrecedesArgumentsNotAllowed(t *testing.T) {
	broker := &sandbox.Broker{}
	token := broker.Register(map[string]sandbox.CommandDef{
		"/usr/bin/gh": {
			Name: "gh",
			Path: "/usr/bin/gh",
			// No AllowedPatterns/AllowedSubcommands: everything would
			// normally fail CheckPolicy with "arguments not allowed".
			RejectRules: []sandbox.RejectRule{
				{Match: "*--web*", Reason: "no browser in the sandbox"},
			},
		},
	}, nil, testCtx)

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "/usr/bin/gh",
		Args:    []string{"pr", "create", "--web"},
		Token:   token,
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	want := "host_commands.gh: rejected: no browser in the sandbox"
	if resp.Stderr != want {
		t.Fatalf("stderr = %q, want %q (reject rule must take precedence)", resp.Stderr, want)
	}
}
