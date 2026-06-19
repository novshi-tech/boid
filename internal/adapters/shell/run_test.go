package shell_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/shell"
)

// TestRun_SimpleExitCode covers the happy path: shell adapter execs the
// supplied argv, the child runs to completion, and Run() returns the
// observed exit code without StoppedByDaemon.
func TestRun_SimpleExitCode(t *testing.T) {
	a := shell.New()
	stdout := &bytes.Buffer{}
	res, err := a.Run(context.Background(), adapters.RunContext{
		Argv:   []string{"/bin/sh", "-c", "echo hello && exit 7"},
		Stdout: stdout,
		Stderr: stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.StoppedByDaemon {
		t.Errorf("StoppedByDaemon = true, want false")
	}
	if got := strings.TrimSpace(stdout.String()); got != "hello" {
		t.Errorf("stdout = %q, want %q", got, "hello")
	}
}

// TestRun_StdinBytes verifies that StdinBytes is piped to the child instead
// of RunContext.Stdin — `cat` echoes whatever it receives.
func TestRun_StdinBytes(t *testing.T) {
	a := shell.New()
	stdout := &bytes.Buffer{}
	res, err := a.Run(context.Background(), adapters.RunContext{
		Argv:       []string{"/bin/cat"},
		StdinBytes: []byte("piped-input"),
		Stdout:     stdout,
		Stderr:     os.Stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := stdout.String(); got != "piped-input" {
		t.Errorf("stdout = %q, want %q", got, "piped-input")
	}
}

// TestRun_StdoutCaptureFile verifies that StdoutCaptureFile, when set,
// receives the child's stdout instead of RunContext.Stdout. The broker
// job-done callback's "payload patch → stdout capture" fallback chain
// relies on this routing.
func TestRun_StdoutCaptureFile(t *testing.T) {
	a := shell.New()
	dir := t.TempDir()
	capPath := filepath.Join(dir, "stdout.cap")
	stdout := &bytes.Buffer{}
	res, err := a.Run(context.Background(), adapters.RunContext{
		Argv:              []string{"/bin/sh", "-c", "printf captured-bytes"},
		StdoutCaptureFile: capPath,
		Stdout:            stdout, // should NOT receive anything
		Stderr:            os.Stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if stdout.Len() != 0 {
		t.Errorf("Stdout buffer received %d bytes, want 0 (capture file should have intercepted)", stdout.Len())
	}
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if string(data) != "captured-bytes" {
		t.Errorf("capture file = %q, want %q", string(data), "captured-bytes")
	}
}

// TestRun_EmptyArgv guards the invariant that an empty Argv is a programmer
// error. The runner-inner-child relies on this so dispatch failures do not
// silently fork /bin/sh-equivalents.
func TestRun_EmptyArgv(t *testing.T) {
	a := shell.New()
	_, err := a.Run(context.Background(), adapters.RunContext{
		Argv: nil,
	})
	if err == nil {
		t.Fatal("Run with empty Argv returned nil error, want non-nil")
	}
}

// Phase 3-d PR1: the SIGUSR1 → SIGTERM normalisation test was removed
// alongside the sigutil.ForwardAndWait wiring. The shell adapter's body
// is now byte-equivalent with the retired runExecArgv, which never
// listened for SIGUSR1 either (NotifyTask.StopAgent only signals agent
// runtimes — claude / codex / opencode). When `boid agent shell` lands
// as a first-class session entry point in a follow-up PR, the signal
// forwarding loop and a corresponding StoppedByDaemon test will return
// at that boundary.
