package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/sigutil"
)

// Run forks the argv supplied via RunContext.Argv and drives it through the
// shared sigutil.ForwardAndWait loop so every harness adapter (claude / codex
// / opencode / shell) reaches the daemon's "stop this job" out-of-band signal
// the same way.
//
// I/O resolution precedence (StdinBytes / StdoutCaptureFile / Stdin / Stdout)
// mirrors the retired runExecArgv path the shell adapter replaced in PR1:
//
//  1. StdinBytes non-empty → pipe a *bytes.Reader as child stdin (host file
//     descriptor is not exposed to the child).
//  2. StdoutCaptureFile non-empty → create that host path and route stdout
//     into it (broker job-done reads from this file).
//  3. Otherwise → pass RunContext.Stdin / Stdout verbatim.
//
// Stderr always flows through RunContext.Stderr.
//
// Signal handling matches the claude/codex/opencode adapters:
//
//  - Setsid=true places the child in its own session/pgrp so the daemon's
//    runtime-pgrp SIGUSR1 broadcast does NOT reach the child directly. Only
//    our sigutil.ForwardAndWait loop sees the SIGUSR1 and translates it into
//    a child SIGTERM. (Without Setsid, bash would terminate immediately on
//    SIGUSR1 since its default disposition is "terminate", racing the
//    forwarding loop.)
//  - sigutil.ForwardAndWait owns the wait + signal forward + exit code
//    normalisation (143 → 0 when StoppedByDaemon).
//
// The shell adapter has no graceful-stop concept of its own (unlike a claude
// session which flushes the jsonl on SIGTERM), but the forwarding path stays
// uniformly wired so `boid agent shell` interactive sessions resize their PTY
// (SIGWINCH passthrough) and surface a clean StoppedByDaemon=true exit when
// the daemon kills the run. Hook / exec callers do not observe a behaviour
// change because the daemon never sends SIGUSR1 to those runtimes — the
// forwarding loop simply idles until cmd.Wait() returns.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	if len(rc.Argv) == 0 {
		return adapters.Result{}, errors.New("shell adapter: RunContext.Argv is empty")
	}

	cmd := exec.CommandContext(ctx, rc.Argv[0], rc.Argv[1:]...)
	cmd.Dir = rc.Workspace
	cmd.Env = envSlice(rc.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

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

	exitCode, stoppedByDaemon, werr := sigutil.ForwardAndWait(cmd, "shell")
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

// envSlice mirrors internal/sandbox/runner.envSlice: deterministic
// KEY=VALUE slice in sorted key order, with no PWD injection and no
// inheritance from os.Environ() (the runner-inner-child has already
// pivoted into the sandbox root, so leaking host paths through duplicate
// keys would shadow spec.Env at the child's first getenv()).
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
