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
)

// Run forks the argv supplied via RunContext.Argv. The body is intentionally
// byte-equivalent with the retired runExecArgv in internal/sandbox/runner
// (see git history of runner_linux.go before PR #594): a single exec.Cmd
// built from the spec, stdio routed per StdinBytes / StdoutCaptureFile
// precedence, env taken straight from RunContext.Env (no PWD injection,
// no host inherit), and a synchronous cmd.Run() to drive it to completion.
//
// SIGUSR1 → SIGTERM forwarding, SIGWINCH passthrough, and Setsid are
// deliberately NOT wired in this PR. The legacy runExecArgv path the
// shell adapter replaces wired none of them — hook scripts have no use
// for SIGUSR1 (NotifyTask.StopAgent only signals agents), and adding a
// process-group split via Setsid breaks the bash broker handshake (an
// early PR #594 iteration regressed E2E builtin-task-create that way).
// The follow-up PR that lands `boid agent shell` as a first-class
// session entry point will reintroduce signal forwarding via sigutil at
// that boundary, where it actually matters.
//
// I/O resolution precedence mirrors the retired runExecArgv:
//
//  1. StdinBytes non-empty → pipe a *bytes.Reader as child stdin (host
//     file descriptor is not exposed to the child).
//  2. StdoutCaptureFile non-empty → create that host path and route stdout
//     into it (broker job-done reads from this file).
//  3. Otherwise → pass RunContext.Stdin / Stdout verbatim.
//
// Stderr always flows through RunContext.Stderr.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	if len(rc.Argv) == 0 {
		return adapters.Result{}, errors.New("shell adapter: RunContext.Argv is empty")
	}

	cmd := exec.CommandContext(ctx, rc.Argv[0], rc.Argv[1:]...)
	cmd.Dir = rc.Workspace
	cmd.Env = envSlice(rc.Env)

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

	err := cmd.Run()
	if captureFile != nil {
		_ = captureFile.Close()
	}

	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return adapters.Result{}, fmt.Errorf("run shell argv: %w", err)
		}
	}
	return adapters.Result{ExitCode: exitCode}, nil
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
