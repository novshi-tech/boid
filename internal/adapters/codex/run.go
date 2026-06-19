package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
)

// defaultPrompt is sent when the caller does not supply one. Phase 3-c
// targets a 1-turn smoke test, not real work, so a no-op exit suffices.
const defaultPrompt = "boid Phase 3-c smoke test: respond with one short line then exit."

// buildArgs constructs the argv handed to exec.Cmd for codex.
// Format: `codex exec [resume <id>] [--model M] [-c sandbox_mode=...] <prompt>`.
//
//   - `exec` is the non-interactive subcommand (we still PTY-allocate at the
//     dispatcher layer, but `codex exec` is the documented one-prompt entry).
//   - `--skip-git-repo-check` so codex does not refuse outside a git repo.
//   - `--dangerously-bypass-approvals-and-sandbox` because the agent is
//     already inside the boid sandbox; codex's own confirm/sandbox layer
//     would prompt the user for every shell command otherwise.
func buildArgs(sessionID, model, prompt string) []string {
	args := []string{"codex", "exec",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
	}
	if sessionID != "" {
		// `codex exec resume <id> <prompt>` is the resume form.
		args = []string{"codex", "exec", "resume", sessionID,
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
		}
	}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, prompt)
	return args
}

// Run is the Phase 3-c entry point. Minimum implementation: argv +
// SIGUSR1→child SIGTERM forwarding + SIGWINCH passthrough + exit code
// normalisation. No session persistence, no payload_patch capture.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	prompt := rc.UserAnswer
	if prompt == "" {
		prompt = defaultPrompt
	}

	args := buildArgs(rc.SessionID, rc.Model, prompt)

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = rc.Workspace
	cmd.Stdin = rc.Stdin
	cmd.Stdout = rc.Stdout
	cmd.Stderr = rc.Stderr
	// Setsid: child gets its own session/pgrp so the daemon's group SIGUSR1
	// never reaches the codex child directly — only our signal.Notify sees
	// it. Mirrors the claude adapter.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Env: parent env + RunContext overlay. Strip PWD / OLDPWD so the
	// daemon's working dir does not leak into the sandbox (some CLIs read
	// PWD instead of getcwd() and trip on a path that is not bound inside
	// the sandbox FS view). cmd.Dir is the source of truth for the workdir.
	env := make([]string, 0, len(os.Environ())+len(rc.Env)+1)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PWD=") || strings.HasPrefix(e, "OLDPWD=") {
			continue
		}
		env = append(env, e)
	}
	for k, v := range rc.Env {
		env = append(env, k+"="+v)
	}
	if rc.Workspace != "" {
		env = append(env, "PWD="+rc.Workspace)
	}
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return adapters.Result{}, fmt.Errorf("start codex: %w", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	winchCh := make(chan os.Signal, 1)
	signal.Notify(winchCh, syscall.SIGWINCH)
	defer signal.Stop(winchCh)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	stoppedByDaemon := false
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
		case err := <-done:
			exitCode := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					exitCode = ee.ExitCode()
				} else {
					return adapters.Result{}, fmt.Errorf("wait codex: %w", err)
				}
			}
			if stoppedByDaemon {
				exitCode = 0
			}
			return adapters.Result{
				ExitCode:        exitCode,
				StoppedByDaemon: stoppedByDaemon,
			}, nil
		}
	}
}
