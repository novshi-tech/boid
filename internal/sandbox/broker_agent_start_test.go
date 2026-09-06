package sandbox_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// BoidOpAgentStart requires the SAME card-context precondition as
// BoidOpCardContext — a card-command launcher job's token carries
// CardID/CardRequestID — and (like BoidOpTaskCreate) resolves/authorizes
// its target project against the caller's token before ever reaching the
// executor.

func testAgentStartBoidPolicy() map[string]sandbox.BuiltinPolicy {
	return map[string]sandbox.BuiltinPolicy{
		"boid": {AllowedOps: map[string]struct{}{
			string(sandbox.BoidOpAgentStart): {},
		}},
	}
}

func TestBroker_BoidAgentStart_NoCardContext_RejectedBeforeExecutor(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testAgentStartBoidPolicy(), sandbox.TokenContext{
		JobID:      "job-1",
		ProjectID:  "proj-1",
		ProjectDir: projectDir,
		// CardID/CardRequestID deliberately left empty.
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"},
	})

	if resp.ExitCode != 1 || !strings.Contains(resp.Stderr, "card context") {
		t.Fatalf("expected a clear no-card-context rejection, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should never be reached when the token has no card context, calls=%d", len(exec.calls))
	}
}

func TestBroker_BoidAgentStart_DisallowedByPolicy_Rejected(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	// No "boid" builtin policy registered at all.
	token := broker.Register(map[string]sandbox.CommandDef{}, nil, sandbox.TokenContext{
		JobID:         "job-1",
		ProjectID:     "proj-1",
		ProjectDir:    projectDir,
		CardID:        "card-1",
		CardRequestID: "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"},
	})

	if resp.ExitCode != 1 {
		t.Fatalf("expected rejection without the boid builtin policy, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%d", len(exec.calls))
	}
}

// TestBroker_BoidAgentStart_DefaultsProjectToLauncherOwn confirms an empty
// ProjectID on the request falls back to the launcher job's own project —
// the common case, since a card command normally starts a session for its
// own workspace project.
func TestBroker_BoidAgentStart_DefaultsProjectToLauncherOwn(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testAgentStartBoidPolicy(), sandbox.TokenContext{
		JobID:         "job-1",
		ProjectID:     "proj-1",
		ProjectDir:    projectDir,
		CardID:        "card-1",
		CardRequestID: "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].ProjectID != "proj-1" {
		t.Fatalf("executor received project_id = %q, want proj-1", exec.calls[0].ProjectID)
	}
}

// TestBroker_BoidAgentStart_ResolvesProjectRef mirrors
// TestBroker_BoidTaskCreate_ResolvesProjectRef: an explicit project ref is
// resolved through ProjectResolver before reaching the executor.
func TestBroker_BoidAgentStart_ResolvesProjectRef(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{
		BoidExecutor: exec,
		ProjectResolver: func(ref string) (string, error) {
			if ref == "mera-ui" {
				return "p2", nil
			}
			return ref, nil
		},
	}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testAgentStartBoidPolicy(), sandbox.TokenContext{
		JobID:             "job-1",
		ProjectID:         "p1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"p1", "p2"},
		ProjectDir:        projectDir,
		CardID:            "card-1",
		CardRequestID:     "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude", ProjectID: "mera-ui"},
	})
	if resp.ExitCode != 0 {
		t.Fatalf("name-based project ref should be accepted after resolution, exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 1 {
		t.Fatalf("executor calls = %d, want 1", len(exec.calls))
	}
	if exec.calls[0].ProjectID != "p2" {
		t.Fatalf("executor received project_id = %q, want resolved %q", exec.calls[0].ProjectID, "p2")
	}
}

// TestBroker_BoidAgentStart_RejectsProjectOutsideWorkspace: a resolved
// project id outside the token's AllowedProjectIDs is rejected before the
// executor ever sees it — same workspace-boundary enforcement as task_create.
func TestBroker_BoidAgentStart_RejectsProjectOutsideWorkspace(t *testing.T) {
	exec := &fakeBoidExecutor{}
	broker := &sandbox.Broker{BoidExecutor: exec}
	projectDir := t.TempDir()
	token := broker.Register(map[string]sandbox.CommandDef{}, testAgentStartBoidPolicy(), sandbox.TokenContext{
		JobID:             "job-1",
		ProjectID:         "p1",
		WorkspaceID:       "ws-1",
		AllowedProjectIDs: []string{"p1"},
		ProjectDir:        projectDir,
		CardID:            "card-1",
		CardRequestID:     "req-1",
	})

	resp := broker.Handle(&sandbox.ExecRequest{
		Command: "boid",
		Cwd:     projectDir,
		Token:   token,
		Boid:    &sandbox.BoidRequest{Op: sandbox.BoidOpAgentStart, HarnessType: "claude", ProjectID: "p-outside"},
	})
	if resp.ExitCode != 1 {
		t.Fatalf("expected rejection for out-of-workspace project, got exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(exec.calls) != 0 {
		t.Fatalf("executor should not be reached, calls=%d", len(exec.calls))
	}
}
