package claude

import (
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
const sessionSystemPrompt = "あなたは boid のサンドボックス内で起動されました。" +
	" サンドボックスの制約 (network / filesystem の制限、 利用可能な builtin と" +
	" host_commands、 git/gh などの quirk) は `~/.boid/context/environment.yaml`" +
	" を読んで確認してください (sandbox / network / filesystem / host_commands /" +
	" notes 節)。 これは固定の参照ファイルで、 ユーザに尋ねる必要はありません。"

// taskBootstrapSkill is the single unified skill that drives any task agent
// regardless of behavior name. With task_behaviors free naming (Track A2 #574)
// the daemon no longer guarantees canonical names, so we route every task to
// /boid-task which determines supervisor vs executor mode from environment.yaml
// `readonly`.
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
// /boid-task is meaningless without a task.yaml to read. The system prompt
// still points the agent at environment.yaml.
//
// Note: Every dispatch is a fresh claude process (no --resume) since the
// reopen / Q&A session-id-resume path was removed. Persisted prior-turn
// context is read by the agent through ~/.boid/context/*.yaml on cold start.
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
// --resume is never used. Persisted prior-turn context is delivered to the
// agent through ~/.boid/context/*.yaml on cold start.
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

// readSessionsFromPayload returns sessions from payload.json at path, or nil
// on any error. Missing / malformed files yield nil rather than an error so
// callers can treat absent payloads as a fresh start.
func readSessionsFromPayload(path string) []session {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var p struct {
		Artifact struct {
			ClaudeCode struct {
				Sessions []session `json:"sessions"`
			} `json:"claude_code"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil
	}
	return p.Artifact.ClaudeCode.Sessions
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
// process. The agent recovers prior-turn context by reading
// ~/.boid/context/{task,instructions,payload}.yaml on cold start.
func (a *Adapter) Run(ctx context.Context, rc adapters.RunContext) (adapters.Result, error) {
	payloadPath := rc.PayloadPath
	if payloadPath == "" {
		home, _ := os.UserHomeDir()
		payloadPath = filepath.Join(home, ".boid", "context", "payload.json")
	}
	outputDir := rc.OutputDir
	if outputDir == "" {
		home, _ := os.UserHomeDir()
		outputDir = filepath.Join(home, ".boid", "output")
	}

	// 1. Generate a fresh session id. Persist it BEFORE starting claude so
	// that an abnormal termination (SIGKILL, OOM) still leaves a record of
	// which jsonl transcript file the agent wrote to (visible in the Web UI
	// task detail under artifact.claude_code.sessions[]).
	sessionID := uuid.NewString()
	sessions := readSessionsFromPayload(payloadPath)
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
