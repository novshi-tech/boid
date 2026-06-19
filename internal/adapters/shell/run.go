package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/sigutil"
)

// Run forks the argv supplied via RunContext.Argv and forwards SIGUSR1 /
// SIGWINCH to the child the same way claude / codex / opencode adapters do.
// It is intentionally close in shape to internal/adapters/codex.Run — the
// helper extraction lives in a follow-up refactor so each adapter still
// reads independently.
//
// I/O resolution precedence mirrors the retired runExecArgv:
//
//  1. StdinBytes non-empty → pipe a *bytes.Reader as child stdin (host file
//     descriptor is not exposed to the child).
//  2. StdoutCaptureFile non-empty → create that host path and route stdout
//     into it (broker job-done reads from this file).
//  3. Otherwise → pass RunContext.Stdin / Stdout verbatim.
//
// Stderr always flows through RunContext.Stderr (or os.Stderr fallback) so
// runner-inner-child can stream diagnostics back to the daemon transcript.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	if len(rc.Argv) == 0 {
		return adapters.Result{}, errors.New("shell adapter: RunContext.Argv is empty")
	}

	cmd := exec.CommandContext(ctx, rc.Argv[0], rc.Argv[1:]...)
	cmd.Dir = rc.Workspace
	// Setsid mirrors the agent adapters so a daemon-driven group SIGUSR1 does
	// not reach the child directly; only our signal.Notify handler sees it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Env: inherit, overlay RunContext.Env. PWD is forced to Workspace so a
	// child that reads PWD instead of getcwd() sees the sandbox-side path.
	env := make([]string, 0, len(os.Environ())+len(rc.Env)+1)
	env = append(env, os.Environ()...)
	for k, v := range rc.Env {
		env = append(env, k+"="+v)
	}
	if rc.Workspace != "" {
		env = append(env, "PWD="+rc.Workspace)
	}
	cmd.Env = env

	// stdin / stdout routing.
	var captureFile *os.File
	switch {
	case len(rc.StdinBytes) > 0 && rc.StdoutCaptureFile != "":
		cmd.Stdin = bytes.NewReader(rc.StdinBytes)
		f, err := os.Create(rc.StdoutCaptureFile)
		if err != nil {
			return adapters.Result{}, fmt.Errorf("create stdout capture: %w", err)
		}
		captureFile = f
		cmd.Stdout = f
	case len(rc.StdinBytes) > 0:
		cmd.Stdin = bytes.NewReader(rc.StdinBytes)
		cmd.Stdout = rc.Stdout
	case rc.StdoutCaptureFile != "":
		cmd.Stdin = rc.Stdin
		f, err := os.Create(rc.StdoutCaptureFile)
		if err != nil {
			return adapters.Result{}, fmt.Errorf("create stdout capture: %w", err)
		}
		captureFile = f
		cmd.Stdout = f
	default:
		cmd.Stdin = rc.Stdin
		cmd.Stdout = rc.Stdout
	}
	cmd.Stderr = rc.Stderr

	if err := cmd.Start(); err != nil {
		if captureFile != nil {
			_ = captureFile.Close()
		}
		return adapters.Result{}, fmt.Errorf("start shell argv: %w", err)
	}

	exitCode, stoppedByDaemon, werr := sigutil.ForwardAndWait(cmd, "shell argv")
	if captureFile != nil {
		_ = captureFile.Close()
	}
	if werr != nil {
		return adapters.Result{}, werr
	}
	return adapters.Result{
		ExitCode:        exitCode,
		StoppedByDaemon: stoppedByDaemon,
	}, nil
}
