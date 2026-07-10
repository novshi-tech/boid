package sigutil

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// sigutil.ForwardAndWait is the shared signal/exit-code core every harness
// adapter (claude / codex / opencode / shell) runs while its child is alive.
// The shell adapter exercises it end-to-end, but per-package coverage does
// not attribute cross-package hits, so these tests drive it directly to lock
// the contract in one place.

// TestForwardAndWait_ExitCode covers the plain-completion path: the child runs
// to a non-zero exit with no stop signal, and ForwardAndWait surfaces the raw
// exit code with StoppedByDaemon=false.
func TestForwardAndWait_ExitCode(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	code, stopped, err := ForwardAndWait(cmd, "sh")
	if err != nil {
		t.Fatalf("ForwardAndWait: %v", err)
	}
	if code != 7 {
		t.Errorf("exitCode = %d, want 7", code)
	}
	if stopped {
		t.Errorf("stoppedByDaemon = true, want false")
	}
}

// TestForwardAndWait_Success pins exit code 0 for a clean completion — the
// common case, kept distinct from the non-zero path so a regression that
// mangles the zero branch (e.g. an off-by-one in the ExitError extraction)
// is caught.
func TestForwardAndWait_Success(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	code, stopped, err := ForwardAndWait(cmd, "true")
	if err != nil {
		t.Fatalf("ForwardAndWait: %v", err)
	}
	if code != 0 {
		t.Errorf("exitCode = %d, want 0", code)
	}
	if stopped {
		t.Errorf("stoppedByDaemon = true, want false")
	}
}

// TestForwardAndWait_StopSignalNormalisesExit is the daemon-stop contract:
// SIGUSR1 to the parent while the child is alive → child SIGTERM → exit
// normalised to 0 with StoppedByDaemon=true. This is what lets a daemon-driven
// stop surface as "paused", not "failed".
//
// We deliver SIGUSR1 to our own PID (not the child's) so only ForwardAndWait's
// notify loop observes it and forwards SIGTERM to the child. Setsid puts the
// child in its own session/pgrp as an extra guard that the signal never
// reaches it directly.
func TestForwardAndWait_StopSignalNormalisesExit(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Fire the stop signal shortly after ForwardAndWait registers its notify
	// handler (signal.Notify runs at function entry, well before 150ms).
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = syscall.Kill(os.Getpid(), syscall.SIGUSR1)
	}()

	code, stopped, err := ForwardAndWait(cmd, "sleep")
	if err != nil {
		t.Fatalf("ForwardAndWait: %v", err)
	}
	if !stopped {
		t.Errorf("stoppedByDaemon = false, want true")
	}
	if code != 0 {
		t.Errorf("exitCode = %d, want 0 (normalised)", code)
	}
}

// TestForwardAndWait_SignalExitUsesShellConvention is the Opus review finding
// #3 regression guard (PR #735): a child killed by a real signal (not the
// daemon's SIGUSR1 stop path) must surface as 128+signal (bash convention,
// e.g. SIGKILL → 137), not exec.ExitError.ExitCode()'s bare -1 — which a
// caller doing os.Exit(-1) would truncate to 255, losing which signal killed
// the process.
func TestForwardAndWait_SignalExitUsesShellConvention(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = cmd.Process.Kill() // SIGKILL, delivered directly to the child (not via our own PID's SIGUSR1 path)
	}()

	code, stopped, err := ForwardAndWait(cmd, "sleep")
	if err != nil {
		t.Fatalf("ForwardAndWait: %v", err)
	}
	if stopped {
		t.Errorf("stoppedByDaemon = true, want false (this was a direct SIGKILL, not the daemon stop path)")
	}
	const wantCode = 128 + int(syscall.SIGKILL)
	if code != wantCode {
		t.Errorf("exitCode = %d, want %d (128+SIGKILL, shell convention)", code, wantCode)
	}
}
