package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"

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
// Signal handling: we run sigutil.ForwardAndWait but deliberately do NOT set
// Setsid. The claude / codex / opencode adapters do set Setsid because they
// need to intercept the daemon's runtime-pgrp SIGUSR1 broadcast and translate
// it into a SIGTERM the agent CLI can drain (jsonl flush, etc). The shell
// adapter cannot follow suit: PTY-attached hook scripts in the E2E suite
// (hook-attach-smoke) open /dev/tty directly, and Setsid detaches the child
// from the controlling terminal — the open() then fails with ENXIO and breaks
// the hook before it can speak. The shrunken byte-equivalent runExecArgv
// path PR1 retired never set Setsid for the same reason.
//
// Trade-off without Setsid: if `boid agent shell` ever exposes a SIGUSR1-
// based daemon stop to an interactive bash session, the SIGUSR1 will also
// reach bash directly (bash's default disposition is "terminate"). The race
// is harmless in practice because sigutil's exit-code normalisation maps any
// stop-signal exit to 0 with StoppedByDaemon=true regardless of whether bash
// died from our forwarded SIGTERM or from the racing SIGUSR1; the boid side
// reads "session paused" the same way. Hook / exec runtimes observe no
// behaviour change either way — the daemon never sends SIGUSR1 to them so
// the forwarding loop simply idles until cmd.Wait() returns.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	if len(rc.Argv) == 0 {
		return adapters.Result{}, errors.New("shell adapter: RunContext.Argv is empty")
	}

	cmd := exec.CommandContext(ctx, rc.Argv[0], rc.Argv[1:]...)
	cmd.Dir = rc.Workspace
	cmd.Env = envSlice(rc.Env)
	// Deliberately no SysProcAttr.Setsid: see the package-level Run() comment
	// for why PTY-attached hook scripts open /dev/tty and need the controlling
	// terminal preserved through the fork.

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
