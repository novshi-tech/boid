package codex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/sigutil"
)

// defaultPrompt is sent when the task hook path forgets to supply a prompt.
// A no-op fallback line keeps non-interactive codex from hanging on empty
// input. Interactive mode (boid agent codex) never reaches this fallback —
// the user drives the TUI, so no prompt is appended at all.
const defaultPrompt = "boid codex non-interactive smoke fallback: respond with one short line then exit."

// buildArgs constructs the argv handed to exec.Cmd for codex.
//
// Two modes, picked by the caller:
//
//   - interactive == true (boid agent codex session): `codex` (no
//     sub-command) launches the TUI. The PTY is already allocated by the
//     dispatcher so codex inherits the user's terminal and the user drives
//     the session interactively. No prompt is appended — the TUI handles
//     input itself. This is the `boid agent <harness>` entry point.
//   - interactive == false (task hook path, legacy non-interactive entry):
//     `codex exec [resume <id>] [-m M] <prompt>` is the documented
//     one-prompt entry point. This path is left functional for hooks that
//     still target codex, but the multi-harness-production plan calls out
//     that `boid task` integration via the hook path is out of scope; the
//     prompt-driven flow continues to work as a thin smoke-test surface.
//
// Common flags:
//
//   - `--skip-git-repo-check` lets codex run outside a git repo; boid's
//     sandbox bind-mounts arbitrary workspaces, not all of them are repos.
//   - `--dangerously-bypass-approvals-and-sandbox` because the agent is
//     already inside the boid sandbox; codex's own confirm / sandbox layer
//     would prompt the user for every shell command otherwise.
func buildArgs(interactive bool, sessionID, model, prompt string) []string {
	if interactive {
		args := []string{"codex",
			"--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox",
		}
		if model != "" {
			args = append(args, "-m", model)
		}
		return args
	}

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

// Run forks codex. Interactive vs non-interactive is keyed off rc.TaskID:
// session jobs (JobKindSession) carry no task and are user-initiated, so
// they land in interactive TUI mode; hook jobs carry a BOID_TASK_ID and
// fall through to non-interactive `codex exec`. This mirrors how the
// claude adapter discriminates JobKindSession from JobKindHook via
// rc.TaskID == "".
//
// Other responsibilities mirror the claude / opencode adapters: signal
// forwarding via sigutil, exit code normalisation for daemon-initiated
// stops, PWD strip on the child env, and cmd.Dir as the source of truth
// for the workdir.
//
// Session persistence and payload_patch.json writes are deliberately NOT
// wired here — see docs/plans/multi-harness-production.md for the explicit
// non-goals (interactive sessions are run-and-done, no resume yet).
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	interactive := rc.TaskID == ""

	prompt := rc.UserAnswer
	if !interactive && prompt == "" {
		prompt = defaultPrompt
	}

	args := buildArgs(interactive, rc.SessionID, rc.Model, prompt)

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

	exitCode, stoppedByDaemon, werr := sigutil.ForwardAndWait(cmd, "codex")
	if werr != nil {
		return adapters.Result{}, werr
	}
	return adapters.Result{
		ExitCode:        exitCode,
		StoppedByDaemon: stoppedByDaemon,
	}, nil
}
