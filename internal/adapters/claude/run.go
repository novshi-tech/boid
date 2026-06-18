package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/adapters"
)

// sessionType is the fixed tag for the agent's session entry in
// payload.artifact.claude_code.sessions[]. One claude session per task;
// kept as "execution" for backward compatibility with persisted entries.
// Skill selection is keyed off InvokedBehavior, not this tag.
const sessionType = "execution"

// pauseSystemPrompt is appended to every claude run via --append-system-prompt.
// It reminds the agent to call boid task notify before terminating so the
// task never hangs in PTY input wait.
const pauseSystemPrompt = "セッションを終える前に必ず `boid task notify \"$BOID_TASK_ID\"` を呼ぶこと。" +
	" 完了時は `--message \"<要約>\" --done \"<成果>\"`、 失敗時は" +
	" `--message \"<要約>\" --fail \"<原因>\"`、 ユーザへの質問・判断要求時は" +
	" `--message \"<コンテキスト>\" --ask \"<質問>\"` を使う。 呼ばずに応答を text" +
	" のみで返すと、 claude が PTY で永遠に入力待ちになり task が hang する。" +
	" notify 後は boid daemon が自動的にこのセッションを終了する。 詳細は" +
	" `/boid-q-and-a` および `/boid-supervisor` / `/boid-executor` skill 参照。"

// resumePrompt is used when resuming an existing session without a user
// answer (e.g. reopen with new instruction). It counters claude's implicit
// "Continue from where you left off" bias toward "no new work" when prior
// context shows a completed task.
const resumePrompt = "状態が更新されました。 BOID_USER_ANSWER 環境変数 (Q&A 回答があれば設定されている) " +
	"と ~/.boid/context/ 以下のファイル (task.yaml, instructions.yaml, payload.yaml) を" +
	"確認し、 新しい状況に対応してください。 prior context が done に見えても、" +
	" instructions.yaml の末尾要素は新しい指示の可能性があるので必ず読むこと。" +
	" **セッションを終える前に `boid task notify --done` / `--fail` /" +
	" `--ask` のいずれかを必ず呼ぶこと (idle 離脱は禁止)**。"

// daemonRestartResumePrompt is the recovery prompt used when the task's
// abort_code is "daemon_restart". The agent is told this is a recovery
// context, not a normal reopen.
const daemonRestartResumePrompt = "daemon が再起動したため、前回の作業が中断されました。" +
	" ~/.boid/context/ 以下のファイル (task.yaml, instructions.yaml, payload.yaml) を確認し、" +
	" 中断前に作業していたファイルや状態を把握してリカバリを試みてください。" +
	" 不明な点は `boid task notify \"$BOID_TASK_ID\"" +
	" --message \"<コンテキスト>\" --ask \"<質問>\"` でユーザに確認してください。" +
	" **セッションを終える前には必ず `--done` / `--fail` / `--ask` のいずれかを呼ぶこと" +
	" (idle 離脱は禁止)**。"

// skillByBehavior maps a canonical behaviour name to its bootstrap skill.
// Unknown behaviours fall back to /boid-sandbox.
var skillByBehavior = map[string]string{
	"supervisor": "/boid-supervisor",
	"executor":   "/boid-executor",
}

// session mirrors one entry in payload.artifact.claude_code.sessions[].
type session struct {
	Type string `json:"type"`
	Name string `json:"name"`
	ID   string `json:"id"`
}

// selectPrompt picks the bootstrap text for the claude positional arg.
// Precedence: UserAnswer (Q&A reply) → resume prompt (daemon_restart vs
// normal) → behaviour-specific skill invocation → /boid-sandbox fallback.
func selectPrompt(isResume bool, userAnswer, invokedBehavior, abortCode string) string {
	if userAnswer != "" {
		return userAnswer
	}
	if isResume {
		if abortCode == "daemon_restart" {
			return daemonRestartResumePrompt
		}
		return resumePrompt
	}
	if s, ok := skillByBehavior[invokedBehavior]; ok {
		return s
	}
	return "/boid-sandbox"
}

// resolveSession returns the existing session id for (invokedType, invokedName)
// if present in sessions, otherwise a freshly generated uuid and isResume=false.
func resolveSession(sessions []session, invokedType, invokedName string) (id string, isResume bool) {
	for _, s := range sessions {
		if s.Type == invokedType && s.Name == invokedName {
			return s.ID, true
		}
	}
	return uuid.NewString(), false
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

// buildClaudeArgs constructs the argv handed to exec.Cmd. Order matches
// run-agent.py so existing claude binary behaviour is preserved byte-for-byte.
// WebFetch is disabled because the sandbox egress allowlist (boid proxy)
// would 403 every fetch, burning agent turns.
func buildClaudeArgs(isResume bool, sessionID, model, prompt string) []string {
	args := []string{
		"claude",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "WebFetch",
	}
	if isResume {
		args = append(args, "--resume", sessionID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--append-system-prompt", pauseSystemPrompt)
	args = append(args, prompt)
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

// defaultAbortCodeLookup shells out to `boid task get <id> --field
// lifecycle.abort.code` to fetch the abort code. Returns "" on any error
// (missing CLI, timeout, task without abort metadata).
func defaultAbortCodeLookup(ctx context.Context, taskID string) string {
	if taskID == "" {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "boid", "task", "get", taskID, "--field", "lifecycle.abort.code").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Run is the Phase 3-b agent-process entry point. See RunContext / Result
// in package adapters for the I/O contract. Run owns:
//   - session resolution (payload.json → resolveSession → uuid fallback)
//   - up-front payload_patch persistence so SIGKILL / OOM still leave a
//     resumable session id
//   - prompt selection (UserAnswer → resume / daemon_restart → behaviour skill)
//   - claude argv construction
//   - signal.Notify(SIGUSR1 / SIGWINCH) and forwarding to the child
//   - exit code normalisation (StoppedByDaemon → 0)
//   - IS_SANDBOX=1 env injection (Claude CLI 2.1.181+ uid 0 bypass)
//
// PR1 introduces Run but does not call it from dispatcher / runner — those
// paths still route through run-agent.py until PR2 flips the runner
// inner-child to invoke Run directly.
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

	// 1. Resolve session id and resume status.
	var (
		sessionID string
		isResume  bool
	)
	if rc.SessionID != "" {
		sessionID = rc.SessionID
		isResume = true
	} else {
		sessions := readSessionsFromPayload(payloadPath)
		sessionID, isResume = resolveSession(sessions, sessionType, rc.InvokedName)

		// Persist session id BEFORE starting claude so that an abnormal
		// termination (SIGKILL, OOM) still leaves a usable resume target.
		updated := updateSessions(sessions, sessionType, rc.InvokedName, sessionID)
		if err := writePayloadPatch(outputDir, updated); err != nil {
			return adapters.Result{}, err
		}
	}

	// 2. Resolve abort_code only on resume without a user answer; this is
	// the narrow path where daemon_restart prompt vs normal resume prompt
	// matters. Cached resumes (Q&A reply) and fresh starts skip the CLI hit.
	abortCode := ""
	if isResume && rc.UserAnswer == "" {
		lookup := a.abortCodeLookup
		if lookup == nil {
			lookup = defaultAbortCodeLookup
		}
		abortCode = lookup(ctx, rc.TaskID)
	}

	// 3. Build claude argv.
	prompt := selectPrompt(isResume, rc.UserAnswer, rc.InvokedBehavior, abortCode)
	args := buildClaudeArgs(isResume, sessionID, rc.Model, prompt)

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
	env := os.Environ()
	for k, v := range rc.Env {
		env = append(env, k+"="+v)
	}
	// IS_SANDBOX=1 is Claude CLI's own escape hatch for uid 0 root checks
	// (Claude CLI 2.1.181+ rejects bypassPermissions when running as uid 0
	// unless this env var is set). boid Phase 3-a sandbox runs the agent
	// at inner-userns uid 0 (required to retain CAP_SYS_ADMIN for mounts),
	// so this must be injected unconditionally — see memory
	// claude-cli-uid0-rejection.
	env = append(env, "IS_SANDBOX=1")
	cmd.Env = env

	if err := cmd.Start(); err != nil {
		return adapters.Result{}, fmt.Errorf("start claude: %w", err)
	}

	// 5. Wire signal forwarding.
	//
	// Phase 3-b PoC (/tmp/sig-poc) verified that Go's signal.Notify
	// auto-overrides an inherited SIG_IGN or sigprocmask block. Python's
	// equivalent required pthread_sigmask SIG_UNBLOCK; the Go runtime's
	// dedicated signal thread makes that redundant.
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
			// daemon agent-stop. Translate to child SIGTERM only; bash
			// equivalent (Phase 3-a runner-inner-child) keeps payload
			// capture alive.
			stoppedByDaemon = true
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case <-winchCh:
			// Terminal resize forwarding. Without this claude renders at
			// its startup width and garbles narrower (mobile) clients.
			if cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.SIGWINCH)
			}
		case err := <-done:
			exitCode := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					exitCode = ee.ExitCode()
				} else {
					return adapters.Result{}, fmt.Errorf("wait claude: %w", err)
				}
			}
			if stoppedByDaemon {
				// daemon-initiated stop is success from boid's perspective
				// (Q&A pause). Without this normalisation, SIGTERM-induced
				// 143 would propagate to `boid job done --exit-code 143`
				// and the awaiting task would settle as job_failed.
				exitCode = 0
			}

			// Read the final payload_patch.json best-effort. Missing file
			// → nil PayloadPatch (the dispatcher treats nil as "no patch").
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
	}
}
