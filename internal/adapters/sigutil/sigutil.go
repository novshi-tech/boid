// Package sigutil hosts the small signal-forwarding loop every harness
// adapter runs while its child process is alive. Phase 3-d (PR1) extracted
// it so the claude / codex / opencode / shell adapters do not all carry
// their own copy of the same select-loop boilerplate.
//
// The loop owns three concerns:
//
//  1. SIGUSR1 (the daemon's "stop this agent" out-of-band signal) →
//     forward SIGTERM to the child process. Marks the run as
//     stoppedByDaemon=true so the caller can normalise the resulting exit
//     code into 0.
//  2. SIGWINCH (terminal resize) → forward verbatim so PTY-backed children
//     redraw at the new width.
//  3. cmd.Wait() completion → exit code extraction with stop-signal
//     normalisation.
//
// Run() exposes a single entry point that drives the loop until the child
// exits. Callers fork the child themselves (each adapter has its own argv /
// env / Setsid handling) and just hand the *exec.Cmd to ForwardAndWait.
package sigutil

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

// ForwardAndWait runs the signal-forwarding select loop for a started
// *exec.Cmd. The returned exitCode is the normalised child exit (143 → 0
// when the daemon sent the stop signal). stoppedByDaemon is true when
// SIGUSR1 was observed during the run. label is used only to format any
// "wait" error so the upstream caller sees `wait claude: ...` etc.
//
// ForwardAndWait does NOT start the child — the caller has already done
// `cmd.Start()` and is responsible for closing whatever stdio plumbing
// surrounds the run (e.g. shell adapter's StdoutCaptureFile). This split
// keeps each adapter's setup distinct while sharing the wait/signal core.
func ForwardAndWait(cmd *exec.Cmd, label string) (exitCode int, stoppedByDaemon bool, err error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	for {
		select {
		case <-sigCh:
			stoppedByDaemon = true
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case <-winchCh:
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGWINCH)
			}
		case werr := <-done:
			if werr != nil {
				if ee, ok := werr.(*exec.ExitError); ok {
					exitCode = exitCodeFromExitError(ee)
				} else {
					return 0, stoppedByDaemon, fmt.Errorf("wait %s: %w", label, werr)
				}
			}
			if stoppedByDaemon {
				exitCode = 0
			}
			return exitCode, stoppedByDaemon, nil
		}
	}
}

// exitCodeFromExitError extracts a shell-convention exit code from a
// completed child's *exec.ExitError: 128+signal when the child was killed by
// a signal (e.g. SIGKILL → 137), matching how a real shell reports it and how
// the pre-cutover syscall.Exec path behaved. ee.ExitCode() alone returns -1
// for a signal-terminated child; a caller that does os.Exit(ee.ExitCode())
// truncates that -1 to 255, losing which signal actually killed the process
// (Opus review finding #3 on PR #735). Mirrors the identical convention in
// internal/dispatcher/runtime_local_linux.go's exitCode() and
// internal/sandbox/runner/runner_linux.go's commandExitCode(), which apply it
// on their own (different) child-process exit paths.
func exitCodeFromExitError(ee *exec.ExitError) int {
	if status, ok := ee.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return ee.ExitCode()
}
