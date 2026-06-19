package opencode

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

// defaultPrompt mirrors the codex adapter: 1-turn smoke for Phase 3-c.
const defaultPrompt = "boid Phase 3-c smoke test: respond with one short line then exit."

// buildArgs constructs the argv for opencode.
// Format: `opencode run [-s <id> --continue] [-m M] <prompt>`.
//
// `opencode run` is the documented non-interactive entry. `-s/--session`
// selects an existing session id; `--continue` must be paired with `-s` to
// avoid opencode treating the id as a new title.
func buildArgs(sessionID, model, prompt string) []string {
	args := []string{"opencode", "run"}
	if sessionID != "" {
		args = append(args, "-s", sessionID, "--continue")
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Inherit the parent env, but strip PWD / OLDPWD so the daemon's
	// working dir does not leak into the sandbox. opencode's file picker
	// reads PWD and refuses to start if the path is not visible inside the
	// sandbox FS view. cmd.Dir is the source of truth for the workdir.
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
		return adapters.Result{}, fmt.Errorf("start opencode: %w", err)
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
					return adapters.Result{}, fmt.Errorf("wait opencode: %w", err)
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
