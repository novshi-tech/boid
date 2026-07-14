package shell_test

import (
	"bytes"
	"context"
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

// TestRun_StopSignalNormalisesExit asserts that when the parent process
// receives SIGUSR1 while a shell-adapter child is alive, sigutil.ForwardAndWait
// translates that into a child SIGTERM, normalises the resulting exit (143)
// into 0, and sets Result.StoppedByDaemon=true. This is the same contract
// the claude / codex / opencode adapters honour — an interactive
// `boid exec -- bash` relies on it so a daemon-driven stop surfaces as
// paused, not failed.
//
// We fork /bin/sleep directly (not via a shell) so the SIGTERM the adapter
// forwards reaches a process whose default disposition is "terminate" with
// no shell wait-loop in between. Setsid on the cmd places sleep in its own
// session/pgrp so the SIGUSR1 we deliver to ourselves never reaches sleep
// directly — only the adapter's sigutil loop sees it and forwards SIGTERM
// to sleep's PID.
func TestRun_StopSignalNormalisesExit(t *testing.T) {
	a := shell.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Schedule the SIGUSR1 to fire shortly after Run() forks the child.
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-time.After(150 * time.Millisecond):
			_ = syscall.Kill(os.Getpid(), syscall.SIGUSR1)
		case <-ctx.Done():
		}
	}()

	res, err := a.Run(ctx, adapters.RunContext{
		Argv:   []string{"/bin/sleep", "30"},
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
	})
	<-done
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.StoppedByDaemon {
		t.Errorf("StoppedByDaemon = false, want true")
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (normalised)", res.ExitCode)
	}
}
