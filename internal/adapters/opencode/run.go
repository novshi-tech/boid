package opencode

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
// Mirrors the codex adapter's no-op fallback. Interactive mode never reaches
// it (no prompt arg is appended to the TUI argv).
const defaultPrompt = "boid opencode non-interactive smoke fallback: respond with one short line then exit."

// buildArgs constructs the argv for opencode.
//
// Two modes, picked by the caller:
//
//   - interactive == true (boid agent opencode session): `opencode [project]`
//     launches the TUI. opencode treats the first positional as the project
//     root; we pass rc.Workspace so the TUI's file picker opens on the
//     correct directory inside the sandbox. The PTY is already allocated
//     by the dispatcher so opencode inherits the user's terminal.
//   - interactive == false (task hook path, legacy non-interactive entry):
//     `opencode run [-m M] <prompt>` is the documented one-prompt entry
//     point. Same scope-out story as codex: this path is kept functional but
//     task hook integration is out of scope for the multi-harness-production
//     plan. Session-id resume was removed alongside the claude --resume
//     path: every dispatch is a fresh opencode run.
func buildArgs(interactive bool, workspace, model, prompt string) []string {
	if interactive {
		args := []string{"opencode"}
		if workspace != "" {
			args = append(args, workspace)
		}
		if model != "" {
			args = append(args, "-m", model)
		}
		return args
	}

	args := []string{"opencode", "run"}
	if model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, prompt)
	return args
}

// Run forks opencode. Interactive vs non-interactive is keyed off rc.TaskID:
// session jobs (JobKindSession) carry no task and are user-initiated, so
// they land in interactive TUI mode; hook jobs carry a BOID_TASK_ID and
// fall through to non-interactive `opencode run`. Mirrors the codex adapter
// and how the claude adapter discriminates JobKindSession from JobKindHook
// via rc.TaskID == "".
//
// Other responsibilities mirror the claude / codex adapters: signal
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

	args := buildArgs(interactive, rc.Workspace, rc.Model, prompt)

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

	exitCode, stoppedByDaemon, werr := sigutil.ForwardAndWait(cmd, "opencode")
	if werr != nil {
		return adapters.Result{}, werr
	}
	return adapters.Result{
		ExitCode:        exitCode,
		StoppedByDaemon: stoppedByDaemon,
	}, nil
}
