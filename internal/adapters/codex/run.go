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

// harnessCLI is the binary name Run execs. Used by the PATH fail-fast check
// below.
const harnessCLI = "codex"

// lookPath resolves harnessCLI on the sandbox PATH; overridable for tests.
var lookPath = exec.LookPath

// missingCLIError builds the fail-fast error Run returns when harnessCLI is
// not on the sandbox PATH. See internal/adapters/claude/run.go's
// missingCLIError for the full rationale. slug comes from
// rc.Env["BOID_WORKSPACE_SLUG"] and falls back to "default"; cause is
// wrapped with %w so errors.Is(err, exec.ErrNotFound) still holds.
//
// Kept literal-identical to the other two adapters' copies on purpose: this is
// one message with three call sites, and a divergence here would show up as an
// operator being told different things depending on which harness they
// happened to run.
func missingCLIError(slug string, cause error) error {
	if slug == "" {
		slug = "default"
	}
	return fmt.Errorf(
		"%s CLI not found in workspace $HOME.\n"+
			"workspace ごとの $HOME (named volume) に harness CLI をインストールする必要があり、それを行うのが workspace の init.sh です。\n"+
			"init.sh は daemon が保持しているので、ホスト側の ~/.config/boid/workspaces/%s/init.sh を編集しても反映されません。\n"+
			"  現在の内容:   boid workspace get-init-script %s\n"+
			"  ファイルから: boid workspace set-init-script %s -f init.sh\n"+
			"  直接編集:     boid workspace edit-init-script %s\n"+
			"init.sh に %s のインストールコマンドを書くと、次回 dispatch 時に自動セットアップされます。\n"+
			"例: `curl -fsSL https://claude.ai/install.sh | bash` (実際のインストール方法はハーネスによる)。\n"+
			"shebang 行は無視される (boid は init.sh を exec せず bash に渡す)。\n"+
			"詳細: docs/ja/guide/workspace-home.md の init.sh 節を参照。\n"+
			"(lookup error: %w)",
		harnessCLI, slug, slug, slug, slug, harnessCLI, cause,
	)
}

// taskBootstrapPrompt is sent as the first user turn when codex is launched
// for a task hook (rc.TaskID != ""). It tells the agent to read the canonical
// task skill manual, then pull the task context via the `boid task ...`
// broker RPCs, then run the task and emit boid task notify --done/--fail
// before exiting.
//
// The claude pattern of passing "/boid-task" as a positional slash command
// does not apply to codex, so this points the agent at the skill FILE via its
// read-file tool instead, at ~/.claude/skills/boid-task/SKILL.md — the one
// path all three harnesses have, which is why the instruction can be worded
// identically for each.
//
// ~/.claude/skills is not a path codex DISCOVERS skills at (its own scan
// covers $HOME/.agents/skills, the repo-relative .agents/skills, and
// /etc/codex/skills) — that does not matter for this prompt, since a
// read-file instruction only needs the file to exist. internal/dispatcher's
// skillLinks symlinks every skill into every skillDiscoveryRoots entry
// (.agents/skills included), so codex also reaches this manual by
// discovery.
//
// Bindings() below declares nothing — the skills are baked into the runner
// image and mounted by the dispatcher, not by this adapter.
//
// codex also has no --append-system-prompt equivalent, so the lifecycle
// reminder ("call boid task notify before exiting") is collapsed into the
// same user prompt instead of being delivered as a separate system message.
const taskBootstrapPrompt = `You are a boid task agent running inside a sandboxed environment.

Step 1: Read the skill manual at ~/.claude/skills/boid-task/SKILL.md with your
read-file tool. That file is the single source of truth for how this task
should be handled — it tells you whether you are in supervisor or executor
mode, and how to use boid task notify / boid task ask.

Step 2: Fetch the task context via the boid CLI (broker RPC):
  boid task current       -> id, title, behavior, status, description
  boid task instructions  -> routed instruction(s) for this job (the LAST
                             element is the active instruction)
  boid task env           -> network.allowed_domains, host_commands
  boid task payload       -> existing artifacts, prior child results
Each command prints YAML by default; add --format json for JSON, or
--field <dotted.path> for a single value (e.g. ` + "`boid task current --field title`" + `).

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
//   - interactive == false (task hook path): `codex exec [-m M] <prompt>` is
//     the documented one-prompt entry point. Every dispatch is a fresh codex
//     run — there is no session-id resume.
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
// Session persistence and payload-patch application are deliberately NOT
// wired here — interactive sessions are run-and-done, no resume yet.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	// Fail fast when codex is not on PATH. See missingCLIError.
	if _, err := lookPath(harnessCLI); err != nil {
		return adapters.Result{}, missingCLIError(rc.Env["BOID_WORKSPACE_SLUG"], err)
	}

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
