package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/sigutil"
)

// harnessCLI is the binary name Run execs. Used by the PATH fail-fast check
// below.
const harnessCLI = "claude"

// lookPath resolves harnessCLI on the sandbox PATH; overridable for tests.
var lookPath = exec.LookPath

// missingCLIError builds the fail-fast error Run returns when harnessCLI is
// not on the sandbox PATH.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retired
// claude.Adapter.Bindings' own CLI bind-mount (see bindings.go) in favor of
// the workspace HOME volume: the claude binary now has to come from the
// workspace's init.sh, so a PATH lookup miss almost always means that
// init.sh is missing or hasn't installed the CLI yet — not a generic
// "command not found" a user has no actionable next step for. slug names
// the workspace whose init.sh needs the fix; it comes from
// rc.Env["BOID_WORKSPACE_SLUG"] (set by BuildSandboxSpec from
// SandboxRuntimeInfo.WorkspaceSlug, which Runner.Dispatch derives from the
// resolved workspace home) and falls back to "default" for callers that
// never wired it through (bare unit tests, or a caller that predates the
// wiring). cause is the underlying lookup error, wrapped with %w so
// errors.Is(err, exec.ErrNotFound) still holds for callers that want to
// distinguish this from other Run failure modes.
func missingCLIError(slug string, cause error) error {
	if slug == "" {
		slug = "default"
	}
	return fmt.Errorf(
		"%s CLI not found in workspace $HOME.\n"+
			"Phase 4 では workspace 単位の $HOME に harness CLI をインストールする必要があります。\n"+
			"~/.config/boid/workspaces/%s/init.sh に %s のインストールコマンドを記述し、次回 dispatch 時に自動セットアップされます。\n"+
			"例: init.sh の中で `curl -fsSL https://claude.ai/install.sh | bash` (実際のインストール方法はハーネスによる)。\n"+
			"詳細: docs/plans/home-workspace-volume.md の init.sh 契約節を参照。\n"+
			"(lookup error: %w)",
		harnessCLI, slug, harnessCLI, cause,
	)
}

// sessionType is the fixed tag for the agent's session entry in
// payload.artifact.claude_code.sessions[]. One claude session per task;
// kept as "execution" for backward compatibility with persisted entries.
// Skill selection is keyed off InvokedBehavior, not this tag.
const sessionType = "execution"

// taskSystemPrompt is appended to claude runs that drive a boid task lifecycle
// (BOID_TASK_ID set, i.e. JobKindHook). It reminds the agent to call boid task
// notify before terminating so the task never hangs in PTY input wait.
const taskSystemPrompt = "セッションを終える前に必ず `boid task notify \"$BOID_TASK_ID\"` を呼ぶこと。" +
	" 完了時は `--message \"<要約>\" --done \"<成果>\"`、 失敗時は" +
	" `--message \"<要約>\" --fail \"<原因>\"`、 ユーザへの質問・判断要求時は" +
	" `--message \"<コンテキスト>\" --ask \"<質問>\"` を使う。 呼ばずに応答を text" +
	" のみで返すと、 claude が PTY で永遠に入力待ちになり task が hang する。" +
	" notify 後は boid daemon が自動的にこのセッションを終了する。 詳細は" +
	" `/boid-task` skill 参照。"

// sessionSystemPrompt is appended to claude runs that are user-initiated
// sessions (JobKindSession, no BOID_TASK_ID). These have no task to notify
// and no behavior-driven instructions; the agent's only system-level cue is
// where to find the sandbox constraints.
//
// Points at `boid task env` (the Phase 5b PR1 broker RPC, docs/plans/
// phase5-shim-and-task-context.md) rather than reading
// ~/.boid/context/environment.yaml directly — fixed proactively during
// codex review on PR #800 (Minor 2): that file reference would have become
// stale/misleading the moment 5b-6 retires the file distribution, since
// nothing else in this PR would have caught it (the static
// "no ~/.boid/context/ reference" tests added for codex/opencode's
// taskBootstrapPrompt don't cover this claude-only, session-only prompt).
// `boid task env`'s reduced schema (WorkspaceEnvView, internal/dispatcher/
// workspace_env_view.go) only ever returns allowed_domains + host_commands —
// the sandbox/filesystem/notes sections legacy environment.yaml used to
// describe are gone from that RPC by design, so the prompt only promises
// what the command actually returns.
const sessionSystemPrompt = "あなたは boid のサンドボックス内で起動されました。" +
	" サンドボックスの制約 (ネットワーク egress の許可ドメイン、 利用可能な" +
	" host_commands とその allow/deny/reject ルール) は `boid task env`" +
	" を実行して確認してください。 これは固定の参照コマンドで、 ユーザに" +
	" 尋ねる必要はありません。"

// taskBootstrapSkill is the single unified skill that drives any task agent
// regardless of behavior name. With task_behaviors free naming (Track A2 #574)
// the daemon no longer guarantees canonical names, so we route every task to
// /boid-task which determines supervisor vs executor mode from
// `boid task current`'s `readonly` field (Phase 5b PR4; the file-based
// environment.yaml `readonly` this used to read was retired by 5b-4/5b-5).
const taskBootstrapSkill = "/boid-task"

// session mirrors one entry in payload.artifact.claude_code.sessions[].
type session struct {
	Type string `json:"type"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

// selectPrompt picks the bootstrap text for the claude positional arg.
// Precedence: UserAnswer (user-initiated session bootstrap text) → session
// mode (empty: no positional) → /boid-task.
//
// isSession=true (JobKindSession, no BOID_TASK_ID) skips the task-skill
// bootstrap entirely: user-initiated sessions have no task to dispatch and
// /boid-task is meaningless with no task to fetch via `boid task current`.
// The system prompt still points the agent at `boid task env` for the
// sandbox constraints it can't observe on its own (see sessionSystemPrompt).
//
// Note: Every dispatch is a fresh claude process (no --resume) since the
// reopen / Q&A session-id-resume path was removed. Persisted prior-turn
// context is pulled by the agent via `boid task current` / `instructions` /
// `payload` on cold start (broker RPCs, Phase 5b — the dispatch-time
// $HOME/.boid/context/*.yaml file distribution these replaced was retired
// by the Phase 5b PR6 cutover).
func selectPrompt(isSession bool, userAnswer string) string {
	if userAnswer != "" {
		return userAnswer
	}
	if isSession {
		return ""
	}
	return taskBootstrapSkill
}

// updateSessions returns sessions with the (invokedType, invokedName) entry
// replaced by id, or appended when no matching entry exists. Order is
// preserved.
func updateSessions(sessions []session, invokedType, invokedName, id string) []session {
	entry := session{Type: invokedType, Name: invokedName, ID: id}
	out := make([]session, 0, len(sessions)+1)
	found := false
	for _, s := range sessions {
		if s.Type == invokedType && s.Name == invokedName {
			out = append(out, entry)
			found = true
		} else {
			out = append(out, s)
		}
	}
	if !found {
		out = append(out, entry)
	}
	return out
}

// buildClaudeArgs constructs the argv handed to exec.Cmd. WebFetch is
// disabled because the sandbox egress allowlist (boid proxy) would 403 every
// fetch, burning agent turns.
//
// Every invocation starts a fresh claude session: --session-id is set to a
// boid-generated uuid so the jsonl transcript path is predictable, but
// --resume is never used. Persisted prior-turn context is pulled by the
// agent via the `boid task current` / `instructions` / `payload` broker
// RPCs on cold start (Phase 5b) rather than delivered as a file.
//
// Empty systemPrompt skips --append-system-prompt entirely. Empty prompt
// skips the trailing positional (passing "" makes claude treat it as a
// blank first user turn, which fires the agent on nothing).
func buildClaudeArgs(sessionID, model, prompt, systemPrompt string) []string {
	args := []string{
		"claude",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "WebFetch",
		"--session-id", sessionID,
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt != "" {
		args = append(args, "--append-system-prompt", systemPrompt)
	}
	if prompt != "" {
		args = append(args, prompt)
	}
	return args
}

// sessionsFieldPath is the dotted path into `boid task payload`'s JSON
// output that readSessionsFromRPC queries via --field. Mirrors the historical
// payload.json shape (artifact.claude_code.sessions[]) exactly — Phase 5b
// PR3 (docs/plans/phase5-shim-and-task-context.md) changed the transport
// (direct file read -> broker RPC over the sandbox PATH shim), not the
// schema, so a persisted session entry from before this PR round-trips the
// same way after it.
const sessionsFieldPath = "artifact.claude_code.sessions"

// fetchTaskPayloadSessions execs `boid task payload --field
// artifact.claude_code.sessions` and returns its raw stdout. Overridable for
// tests (mirrors the lookPath var pattern above) so adapter unit tests never
// spawn a real subprocess; production wiring is buildTaskPayloadSessionsCmd's
// default *exec.Cmd, unmodified.
var fetchTaskPayloadSessions = func(ctx context.Context, env map[string]string) ([]byte, error) {
	return buildTaskPayloadSessionsCmd(ctx, env).Output()
}

// buildTaskPayloadSessionsCmd builds (without running) the *exec.Cmd for
// `boid task payload --field artifact.claude_code.sessions`. Split out from
// fetchTaskPayloadSessions so adapter unit tests can assert on Args/Env
// without spawning a process — os/exec resolves a bare command name (no path
// separators) via the *current* process's PATH at exec.CommandContext call
// time (not from cmd.Env), so this relies on the sandbox's PATH already
// pointing at the boid shim, exactly like the harnessCLI lookPath check
// above.
//
// env is overlaid on the current process's environment, the same pattern
// Run() uses for the agent child's own cmd.Env below: the shim needs
// BOID_TASK_ID / BOID_JOB_ID / BOID_BROKER_SOCKET / BOID_BROKER_TOKEN /
// BOID_BUILTIN_SHIM=1, all of which are already present in rc.Env (the same
// map Run() hands to the agent child).
//
// cmd.Dir is pinned to "/tmp" rather than left unset. This call runs from
// inside internal/sandbox/runner.RunInnerChild (the process that becomes
// Run()'s caller): its own cwd is "/" the whole time — pivotInto's
// os.Chdir("/") right after pivot_root is never followed by a chdir into the
// project workdir before Run() executes. An unset cmd.Dir would inherit that
// "/" cwd, and the broker's validateBoidBuiltinCwd (internal/sandbox/broker.go)
// rejects any "boid" builtin call whose cwd falls outside the sandbox project
// dir / workspace $HOME / "/tmp" (boidPolicy's AllowedCwdRoots,
// internal/orchestrator/policy.go) with "boid builtin is restricted to the
// current project or worktree" — the exact failure a live `boid agent claude`
// hit in production (2026-07-22), silent until the withExitErrorStderr fix
// above stopped discarding this stderr. "/tmp" is the only entry
// AllowedCwdRoots contains unconditionally (no project/workspace dependency),
// so it is the one value guaranteed to pass regardless of caller context.
func buildTaskPayloadSessionsCmd(ctx context.Context, env map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "boid", "task", "payload", "--field", sessionsFieldPath)
	cmd.Dir = "/tmp"
	envSlice := make([]string, 0, len(os.Environ())+len(env))
	envSlice = append(envSlice, os.Environ()...)
	for k, v := range env {
		envSlice = append(envSlice, k+"="+v)
	}
	cmd.Env = envSlice
	return cmd
}

// readSessionsFromRPC returns sessions from `boid task payload --field
// artifact.claude_code.sessions`. It deliberately does NOT collapse "RPC
// failed" into "no sessions" the way the old file-based readSessionsFromPayload
// collapsed "file missing" into nil: that file read was 100% local (no
// broker round trip), so its only realistic failure was "never written yet"
// — a fresh task, correctly treated as no prior sessions. The RPC has a
// genuinely different failure surface (broker unreachable, daemon mid-
// restart, token expiry race, malformed shim output, …) that has nothing to
// do with whether sessions exist. Collapsing that into nil would make
// updateSessions synthesize a single-entry list from a transient hiccup, and
// the caller would then persist that truncated list as this task's payload
// patch — silently discarding every previously recorded jsonl session id
// (the exact class of loss flagged in codex review on PR #800; see
// wiring-seams.md #16 and memory phase3b-session-jsonl-not-persisted for the
// prior incident this rhymes with).
//
// Only "the field genuinely does not exist yet" (empty stdout, exit 0) is
// nil-with-no-error. Every other failure — exec error, non-zero exit,
// malformed JSON — returns a non-nil error so Run() aborts before writing
// any payload patch: failing the run outright is preferable to silently
// truncating the session history.
func readSessionsFromRPC(ctx context.Context, env map[string]string) ([]session, error) {
	out, err := fetchTaskPayloadSessions(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("boid task payload --field %s: %w", sessionsFieldPath, withExitErrorStderr(err))
	}
	return parseSessionsJSON(out)
}

// withExitErrorStderr re-wraps err to include the subprocess's captured
// stderr when err is an *exec.ExitError. cmd.Output() (fetchTaskPayloadSessions's
// default implementation) populates ExitError.Stderr, but *exec.ExitError's
// own Error() method (via os.ProcessState.String()) only ever renders "exit
// status N" — the caller's %w wrap around a bare err therefore silently
// discards the one piece of information that explains WHY the RPC failed
// (broker rejection message, "no context tracked for job", etc). Every other
// error type (context cancellation, exec.LookPath failure, …) passes through
// unchanged since only ExitError carries a separate Stderr field.
func withExitErrorStderr(err error) error {
	ee, ok := err.(*exec.ExitError)
	if !ok || len(bytes.TrimSpace(ee.Stderr)) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, bytes.TrimSpace(ee.Stderr))
}

// parseSessionsJSON parses the raw `--field artifact.claude_code.sessions`
// stdout into []session. Empty input (api.ResolveJSONField returns "" when
// the field is absent) is (nil, nil) — a legitimate "no sessions recorded
// yet" case, not an error. Malformed JSON returns a non-nil error rather
// than silently degrading to nil — see readSessionsFromRPC's doc comment for
// why swallowing this would risk truncating a task's session history.
func parseSessionsJSON(data []byte) ([]session, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var sessions []session
	if err := json.Unmarshal(trimmed, &sessions); err != nil {
		return nil, fmt.Errorf("boid task payload --field %s: parse JSON: %w", sessionsFieldPath, err)
	}
	return sessions, nil
}

// writePayloadPatch writes the session-id update into
// <outputDir>/payload_patch.json, preserving any other keys the agent already
// wrote (boid task notify, custom artifact entries, …). The session list
// replaces whatever was previously stored under
// payload_patch.artifact.claude_code.sessions.
func writePayloadPatch(outputDir string, sessions []session) error {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("mkdir output dir: %w", err)
	}
	path := filepath.Join(outputDir, "payload_patch.json")

	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		var loaded map[string]any
		if err := json.Unmarshal(data, &loaded); err == nil && loaded != nil {
			existing = loaded
		}
	}

	patch := mapAt(existing, "payload_patch")
	artifact := mapAt(patch, "artifact")
	claudeCode := mapAt(artifact, "claude_code")
	claudeCode["sessions"] = sessions

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal payload_patch: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write payload_patch.json: %w", err)
	}
	return nil
}

// mapAt returns the nested map at key, creating and inserting an empty one
// when the key is missing or the existing value is not a map. The returned
// map is wired into m so callers can mutate it directly.
func mapAt(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	child := map[string]any{}
	m[key] = child
	return child
}

// Run is the agent-process entry point. See RunContext / Result in package
// adapters for the I/O contract. Run owns:
//   - generating a fresh session id and persisting it to payload_patch.json
//     up front so the jsonl transcript path is recorded in the task artifact
//     even when the child terminates abnormally (SIGKILL, OOM)
//   - prompt selection (UserAnswer → bootstrap, otherwise /boid-task or "")
//   - claude argv construction (always --session-id, never --resume)
//   - signal.Notify(SIGUSR1 / SIGWINCH) and forwarding to the child
//   - exit code normalisation (StoppedByDaemon → 0)
//   - IS_SANDBOX=1 env injection (Claude CLI 2.1.181+ uid 0 bypass)
//
// Session-id resume was removed: reopen and Q&A both start a fresh claude
// process. The agent (via the /boid-task skill) recovers general
// task/instructions/environment context via the `boid task current` /
// `instructions` / `env` broker RPCs on cold start (Phase 5b — the
// dispatch-time $HOME/.boid/context/*.yaml file distribution these replaced
// was retired by the Phase 5b PR6 cutover; PR3, which predates PR6, only
// cut over the one payload field Run() itself needs directly: the prior
// artifact.claude_code.sessions[] entries it merges the fresh session id
// into, which come from the `boid task payload` broker RPC — see
// readSessionsFromRPC below).
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	// 0. Fail fast when claude is not on PATH, before touching any state
	// (session id generation, payload_patch.json). See missingCLIError's
	// doc comment for why this replaces the old adapter-bindings-based
	// guarantee that claude was always present.
	if _, err := lookPath(harnessCLI); err != nil {
		return adapters.Result{}, missingCLIError(rc.Env["BOID_WORKSPACE_SLUG"], err)
	}

	outputDir := rc.OutputDir
	if outputDir == "" {
		home, _ := os.UserHomeDir()
		outputDir = filepath.Join(home, ".boid", "output")
	}

	// 1. Generate a fresh session id. Persist it BEFORE starting claude so
	// that an abnormal termination (SIGKILL, OOM) still leaves a record of
	// which jsonl transcript file the agent wrote to (visible in the Web UI
	// task detail under artifact.claude_code.sessions[]). Prior sessions come
	// from the broker (`boid task payload --field
	// artifact.claude_code.sessions`, Phase 5b PR3) rather than a direct read
	// of payload.json — see readSessionsFromRPC's doc comment. A fetch/parse
	// error aborts here, before claude ever starts and before any payload
	// patch is written: readSessionsFromRPC only returns an error when it
	// cannot tell whether prior sessions exist, and proceeding anyway would
	// risk writePayloadPatch persisting a truncated session list over the
	// task's real history (codex review on PR #800).
	sessionID := uuid.NewString()
	sessions, err := readSessionsFromRPC(ctx, rc.Env)
	if err != nil {
		return adapters.Result{}, fmt.Errorf("read prior claude sessions: %w", err)
	}
	updated := updateSessions(sessions, sessionType, rc.InvokedName, sessionID)
	if err := writePayloadPatch(outputDir, updated); err != nil {
		return adapters.Result{}, err
	}

	// 2. Build claude argv.
	// isSession is the JobKindSession discriminator. Sessions are user-
	// initiated and have no task lifecycle, so they receive neither the
	// behaviour-skill bootstrap nor the "notify before exit" system prompt.
	isSession := rc.TaskID == ""
	prompt := selectPrompt(isSession, rc.UserAnswer)
	systemPrompt := taskSystemPrompt
	if isSession {
		systemPrompt = sessionSystemPrompt
	}
	args := buildClaudeArgs(sessionID, rc.Model, prompt, systemPrompt)

	// 4. Fork claude.
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = rc.Workspace
	cmd.Stdin = rc.Stdin
	cmd.Stdout = rc.Stdout
	cmd.Stderr = rc.Stderr
	// Setsid places the child in its own session/pgrp so the group SIGUSR1
	// (delivered by the daemon to the runtime pgrp) never reaches claude
	// directly — only our signal.Notify handler below sees it. Equivalent
	// to Python subprocess.Popen(start_new_session=True).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Env: inherit, overlay RunContext.Env, then force IS_SANDBOX=1.
	// Strip CLAUDE_CODE_CHILD_SESSION from the inherited env first: if boid
	// daemon was launched from a parent claude-code session it inherits this
	// var, which propagates through runner-outer/inner/inner-child to here.
	// Claude CLI 2.1.181+ treats CLAUDE_CODE_CHILD_SESSION=1 as a signal that
	// this run is a nested child session and disables jsonl persistence
	// entirely (jOe() → K8() → materializeSessionFile() early-return →
	// sessionFile stays null → zero writes), breaking ask→answer resume.
	parentEnv := os.Environ()
	env := make([]string, 0, len(parentEnv)+len(rc.Env)+2)
	for _, e := range parentEnv {
		if strings.HasPrefix(e, "CLAUDE_CODE_CHILD_SESSION=") {
			continue
		}
		env = append(env, e)
	}
	for k, v := range rc.Env {
		if k == "CLAUDE_CODE_CHILD_SESSION" {
			continue
		}
		env = append(env, k+"="+v)
	}
	// IS_SANDBOX=1 is Claude CLI's own escape hatch for uid 0 root checks
	// (Claude CLI 2.1.181+ rejects bypassPermissions when running as uid 0
	// unless this env var is set). boid Phase 3-a sandbox runs the agent
	// at inner-userns uid 0 (required to retain CAP_SYS_ADMIN for mounts),
	// so this must be injected unconditionally — see memory
	// claude-cli-uid0-rejection.
	env = append(env, "IS_SANDBOX=1")
	// Belt and suspenders: even if some other path flips a persistence-skip
	// flag (CLAUDE_CODE_SKIP_PROMPT_HISTORY etc.), this env forces the
	// internal jOe() check to false so jsonl write is always attempted.
	env = append(env, "CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1")
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return adapters.Result{}, fmt.Errorf("start claude: %w", err)
	}

	// 5. Drive the shared signal-forwarding loop. sigutil.ForwardAndWait
	// translates SIGUSR1 into a child SIGTERM, forwards SIGWINCH verbatim,
	// and normalises the daemon-initiated stop exit (143) into 0 so the
	// awaiting task settles as paused, not failed.
	//
	// Phase 3-b PoC (/tmp/sig-poc) verified that Go's signal.Notify
	// auto-overrides an inherited SIG_IGN or sigprocmask block. Python's
	// equivalent required pthread_sigmask SIG_UNBLOCK; the Go runtime's
	// dedicated signal thread makes that redundant.
	exitCode, stoppedByDaemon, werr := sigutil.ForwardAndWait(cmd, "claude")
	if werr != nil {
		return adapters.Result{}, werr
	}

	// Read the final payload_patch.json best-effort. Missing file →
	// nil PayloadPatch (the dispatcher treats nil as "no patch").
	payloadPatchPath := filepath.Join(outputDir, "payload_patch.json")
	var patch json.RawMessage
	if data, err := os.ReadFile(payloadPatchPath); err == nil {
		patch = data
	}

	return adapters.Result{
		ExitCode:        exitCode,
		PayloadPatch:    patch,
		StoppedByDaemon: stoppedByDaemon,
	}, nil
}
