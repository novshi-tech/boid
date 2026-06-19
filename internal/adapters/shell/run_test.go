package shell_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestRun_StopSignalNormalisesExit covers the SIGUSR1 → SIGTERM → ExitCode=0
// path: a daemon-driven stop must surface as a success exit so the awaiting
// task settles as paused, not failed.
func TestRun_StopSignalNormalisesExit(t *testing.T) {
	a := shell.New()
	stdout := &bytes.Buffer{}
	// Drive the SIGUSR1 from a goroutine 50ms after Run starts so the
	// `sleep` child is alive when the signal arrives.
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGUSR1)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// `exec sleep` so the trapped sleep is the direct child of exec.Cmd —
	// otherwise the intermediate /bin/sh blocks on its waitpid and the
	// daemon's SIGTERM has to wait for the sleep to finish or for the ctx
	// deadline to fire (whichever wins).
	res, err := a.Run(ctx, adapters.RunContext{
		Argv:   []string{"/bin/sh", "-c", "exec sleep 5"},
		Stdout: stdout,
		Stderr: os.Stderr,
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}
	if !res.StoppedByDaemon {
		t.Errorf("StoppedByDaemon = false, want true")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (stop-normalised)", res.ExitCode)
	}
}
