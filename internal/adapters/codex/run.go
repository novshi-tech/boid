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

// taskBootstrapPrompt is sent as the first user turn when codex is launched
// for a task hook (rc.TaskID != ""). It tells the agent to read the canonical
// task skill manual + boid task context yaml files, then run the task and
// emit boid task notify --done/--fail before exiting.
//
// codex has no slash command / skill loader mechanism, so the claude pattern
// of passing "/boid-task" as positional does not apply. Instead we point the
// agent at the skill file via its read-file tool — the SKILL.md is bind
// mounted into ~/.boid/skills/boid-task/ by Bindings() below.
//
// codex also has no --append-system-prompt equivalent, so the lifecycle
// reminder ("call boid task notify before exiting") is collapsed into the
// same user prompt instead of being delivered as a separate system message.
const taskBootstrapPrompt = `You are a boid task agent running inside a sandboxed environment.

Step 1: Read the skill manual at ~/.claude/skills/boid-task/SKILL.md with your
read-file tool. That file is the single source of truth for how this task
should be handled — it tells you whether you are in supervisor or executor
mode based on environment.yaml ` + "`readonly`" + `, and how to use boid task notify /
boid task ask.

Step 2: Read the task context files under ~/.boid/context/ as instructed by
the skill manual:
  - task.yaml         (id, title, behavior, status)
  - instructions.yaml (the LAST element is the active instruction)
  - environment.yaml  (readonly, network, host_commands)
  - payload.yaml      (existing artifacts, prior child results)

Step 3: Perform the task. Use $BOID_TASK_ID whenever you call boid task
notify or boid task ask.

Step 4: Before terminating, you MUST call EXACTLY ONE of:
  boid task notify "$BOID_TASK_ID" --message "<short>" --done "<achievement>"
  boid task notify "$BOID_TASK_ID" --message "<short>" --fail "<reason>"
For mid-flight user questions, use the blocking RPC:
  ANSWER=$(boid task ask "<question>")
  # The answer arrives on stdout; the call returns and you continue.
  # Do NOT use boid task notify --ask (vestigial).

Failure to call notify --done or --fail leaves the task stuck in ` + "`executing`" + `
forever. The daemon SIGTERMs your runtime after notify.`

// selectPrompt picks the first user turn handed to codex.
//
//   - isSession == false (hook job, rc.TaskID != ""): always taskBootstrapPrompt.
//     Hook jobs do not carry a UserAnswer (the field is reserved for
//     `boid agent <harness> --instruction` session bootstrap text), but we
//     ignore it unconditionally to keep hook behaviour deterministic.
//   - isSession == true + UserAnswer == "": empty string. Session TUI mode
//     receives no positional prompt — the user drives the harness directly.
//   - isSession == true + UserAnswer != "": the UserAnswer text is passed
//     verbatim as the first turn (the `--instruction` flag plumbing).
//
// Mirrors the shape of internal/adapters/claude/run.go selectPrompt; the
// codex bootstrap text replaces claude's "/boid-task" slash command.
func selectPrompt(isSession bool, userAnswer string) string {
	if !isSession {
		return taskBootstrapPrompt
	}
	return userAnswer
}

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
//     `codex exec [-m M] <prompt>` is the documented one-prompt entry point.
//     This path is left functional for hooks that still target codex, but
//     the multi-harness-production plan calls out that `boid task`
//     integration via the hook path is out of scope; the prompt-driven flow
//     continues to work as a thin smoke-test surface. Session-id resume was
//     removed alongside the claude --resume path: every dispatch is a fresh
//     codex run.
//
// Flags:
//
//   - `--dangerously-bypass-approvals-and-sandbox` (both modes): the agent
//     is already inside the boid sandbox; codex's own confirm / sandbox
//     layer would prompt the user for every shell command otherwise.
//   - `--skip-git-repo-check` (exec mode only): lets codex run outside a
//     git repo; boid's sandbox bind-mounts arbitrary workspaces, not all
//     of them are repos. As of codex-cli 0.141.0 this flag lives on the
//     `exec` subcommand only — passing it at the top level (TUI mode)
//     errors out with "unexpected argument", so interactive argv omits it.
func buildArgs(interactive bool, model, prompt string) []string {
	if interactive {
		args := []string{"codex",
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
	prompt := selectPrompt(interactive, rc.UserAnswer)
	args := buildArgs(interactive, rc.Model, prompt)

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
