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

// taskBootstrapPrompt is sent as the first user turn when opencode is
// launched for a task hook (rc.TaskID != ""). Same role as the codex
// adapter's bootstrap text (see internal/adapters/codex/run.go) — point
// the agent at ~/.claude/skills/boid-task/SKILL.md via its read-file tool
// and remind it to call boid task notify --done/--fail before exiting.
//
// Kept literal-identical to the codex bootstrap on purpose so both harnesses
// see the exact same first-turn instructions. The duplication is cheap
// (one constant per adapter) and avoids introducing a shared package just
// to hold a string.
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

// selectPrompt picks the first user turn handed to opencode. Same shape as
// the codex adapter's selectPrompt — hook (rc.TaskID != "") always gets the
// bootstrap prompt, session jobs receive UserAnswer (empty string means no
// positional prompt for the TUI).
func selectPrompt(isSession bool, userAnswer string) string {
	if !isSession {
		return taskBootstrapPrompt
	}
	return userAnswer
}

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

	// --dangerously-skip-permissions auto-approves tool calls that are not
	// explicitly denied — required for the task hook bootstrap to work:
	// opencode's Read tool fails to expand "~" (literal "/tmp/.../~/.boid/..."
	// not found), retries with the absolute path "/home/$USER/.boid/...",
	// and then hits the external_directory permission gate which auto-rejects
	// without this flag. The agent is already inside the boid sandbox, so the
	// opencode-side permission layer adds no isolation.
	args := []string{"opencode", "run", "--dangerously-skip-permissions"}
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
	prompt := selectPrompt(interactive, rc.UserAnswer)
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
