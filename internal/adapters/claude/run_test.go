package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/adapters"
)

func TestSelectPrompt_UserAnswerWins(t *testing.T) {
	got := selectPrompt(false, "reply text")
	if got != "reply text" {
		t.Errorf("got %q, want UserAnswer to take precedence", got)
	}
}

// Fresh task-mode start bootstraps via the unified /boid-task skill. Mode
// determination happens inside the skill from `boid task current`'s
// `readonly` field, so the prompt does not branch on behavior name.
func TestSelectPrompt_TaskModeReturnsTaskSkill(t *testing.T) {
	got := selectPrompt(false, "")
	if got != "/boid-task" {
		t.Errorf("got %q, want /boid-task", got)
	}
}

// Session mode (JobKindSession, no BOID_TASK_ID) never falls through to a
// skill bootstrap. A user typed `boid agent claude -p <project>` to open a
// blank chat, not to dispatch behaviour-driven work.
func TestSelectPrompt_SessionFreshReturnsEmpty(t *testing.T) {
	got := selectPrompt(true, "")
	if got != "" {
		t.Errorf("got %q, want empty prompt for fresh session", got)
	}
}

// Session mode still honours an explicit --instruction (delivered via
// BOID_USER_ANSWER).
func TestSelectPrompt_SessionWithInstructionDelivers(t *testing.T) {
	got := selectPrompt(true, "fix bug X")
	if got != "fix bug X" {
		t.Errorf("got %q, want instruction text to pass through", got)
	}
}

func TestUpdateSessions_InsertNew(t *testing.T) {
	in := []session{{Type: "execution", Name: "verifier", ID: "old"}}
	got := updateSessions(in, "execution", "", "fresh")

	want := []session{
		{Type: "execution", Name: "verifier", ID: "old"},
		{Type: "execution", Name: "", ID: "fresh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUpdateSessions_ReplaceExisting(t *testing.T) {
	in := []session{
		{Type: "execution", Name: "verifier", ID: "old"},
		{Type: "execution", Name: "", ID: "stale"},
	}
	got := updateSessions(in, "execution", "", "fresh")

	want := []session{
		{Type: "execution", Name: "verifier", ID: "old"},
		{Type: "execution", Name: "", ID: "fresh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUpdateSessions_PreservesOrder(t *testing.T) {
	in := []session{
		{Type: "execution", Name: "a", ID: "1"},
		{Type: "execution", Name: "b", ID: "2"},
		{Type: "execution", Name: "c", ID: "3"},
	}
	got := updateSessions(in, "execution", "b", "fresh")
	if got[0].ID != "1" || got[1].ID != "fresh" || got[2].ID != "3" {
		t.Errorf("order lost: %+v", got)
	}
}

func TestBuildClaudeArgs_FreshSession(t *testing.T) {
	args := buildClaudeArgs("sess-1", "claude-opus-4-8", "/boid-task", taskSystemPrompt)

	want := []string{
		"claude",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "WebFetch",
		"--session-id", "sess-1",
		"--model", "claude-opus-4-8",
		"--append-system-prompt", taskSystemPrompt,
		"/boid-task",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("got %v, want %v", args, want)
	}
}

// Resume is gone repo-wide; the argv never includes `--resume`, only
// `--session-id` (with a freshly generated uuid each call). UserAnswer flows
// through as the trailing positional, mirroring how an --instruction is
// delivered in session mode.
func TestBuildClaudeArgs_UserAnswerBecomesPositional(t *testing.T) {
	args := buildClaudeArgs("sess-1", "", "user answer text", taskSystemPrompt)
	want := []string{
		"claude",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "WebFetch",
		"--session-id", "sess-1",
		"--append-system-prompt", taskSystemPrompt,
		"user answer text",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("got %v, want %v", args, want)
	}
	for _, a := range args {
		if a == "--resume" {
			t.Errorf("--resume must never appear in argv, got %v", args)
		}
	}
}

func TestBuildClaudeArgs_NoModelOmitsFlag(t *testing.T) {
	args := buildClaudeArgs("sess-1", "", "/boid-task", taskSystemPrompt)
	for i, a := range args {
		if a == "--model" {
			t.Errorf("unexpected --model flag at %d: %v", i, args)
		}
	}
}

func TestBuildClaudeArgs_PromptIsLast(t *testing.T) {
	// Claude binary treats the trailing positional as the prompt; if it
	// slips earlier the agent will not see it.
	args := buildClaudeArgs("sess-1", "claude-opus-4-8", "/boid-task", taskSystemPrompt)
	if args[len(args)-1] != "/boid-task" {
		t.Errorf("last arg = %q, want prompt /boid-task", args[len(args)-1])
	}
}

// Session-mode fresh start: no positional prompt at all (we'd otherwise pass
// "" which claude treats as a blank first turn). Also sessionSystemPrompt
// must replace taskSystemPrompt so the agent isn't told to call notify on a
// task that doesn't exist.
func TestBuildClaudeArgs_SessionFreshOmitsPromptAndUsesSessionSystemPrompt(t *testing.T) {
	args := buildClaudeArgs("sess-1", "", "", sessionSystemPrompt)
	want := []string{
		"claude",
		"--permission-mode", "bypassPermissions",
		"--disallowedTools", "WebFetch",
		"--session-id", "sess-1",
		"--append-system-prompt", sessionSystemPrompt,
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("got %v, want %v", args, want)
	}
}

// Empty systemPrompt skips --append-system-prompt entirely. Belt-and-
// suspenders for callers that explicitly opt out of a system prompt.
func TestBuildClaudeArgs_EmptySystemPromptOmitsFlag(t *testing.T) {
	args := buildClaudeArgs("sess-1", "", "", "")
	for _, a := range args {
		if a == "--append-system-prompt" {
			t.Errorf("--append-system-prompt should be omitted when systemPrompt is empty, got args=%v", args)
		}
	}
}

// ---------- parseSessionsJSON (pure) ----------

// TestParseSessionsJSON_EmptyReturnsNilNoError pins the one non-error "no
// sessions" case: a genuinely absent field (empty stdout, exit 0) is normal
// for a brand-new task and must not be conflated with a parse failure.
func TestParseSessionsJSON_EmptyReturnsNilNoError(t *testing.T) {
	got, err := parseSessionsJSON(nil)
	if err != nil || got != nil {
		t.Errorf("got (%+v, %v), want (nil, nil) for nil input", got, err)
	}
	got, err = parseSessionsJSON([]byte("   \n"))
	if err != nil || got != nil {
		t.Errorf("got (%+v, %v), want (nil, nil) for whitespace-only input", got, err)
	}
}

// TestParseSessionsJSON_MalformedReturnsError pins the codex-review fix
// (PR #800): malformed JSON must surface as an error, not silently degrade
// to nil — see readSessionsFromRPC's doc comment for why swallowing it would
// risk the caller truncating a task's real session history.
func TestParseSessionsJSON_MalformedReturnsError(t *testing.T) {
	got, err := parseSessionsJSON([]byte("{not json"))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if got != nil {
		t.Errorf("got %+v, want nil sessions alongside the error", got)
	}
}

func TestParseSessionsJSON_ExtractsSessions(t *testing.T) {
	body := `[
		{"type": "execution", "name": "", "id": "abc"},
		{"type": "execution", "name": "verifier", "id": "def"}
	]`
	got, err := parseSessionsJSON([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []session{
		{Type: "execution", Name: "", ID: "abc"},
		{Type: "execution", Name: "verifier", ID: "def"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// ---------- buildTaskPayloadSessionsCmd (pure, no process spawn) ----------

// TestBuildTaskPayloadSessionsCmd_Args pins the exact subcommand + flags the
// claude adapter sends the boid shim — a typo here (e.g. a stray "s" on
// "payload", or the wrong --field path) would silently make every claude job
// forget its own jsonl session id across restarts, since readSessionsFromRPC
// swallows all errors as "fresh start".
func TestBuildTaskPayloadSessionsCmd_Args(t *testing.T) {
	cmd := buildTaskPayloadSessionsCmd(context.Background(), nil)
	want := []string{"boid", "task", "payload", "--field", "artifact.claude_code.sessions"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

// TestBuildTaskPayloadSessionsCmd_DirIsAllowedByBoidPolicy pins the real
// production incident: this call runs from inside runner-inner-child itself
// (internal/sandbox/runner.RunInnerChild), whose own cwd is "/" the whole
// time — pivotInto's os.Chdir("/") after pivot_root is never followed by a
// chdir into the project workdir before Run() executes. Leaving cmd.Dir unset
// here means the nested "boid task payload" subprocess inherits that "/"
// cwd, and the broker's validateBoidBuiltinCwd (internal/sandbox/broker.go)
// rejects it with "boid builtin is restricted to the current project or
// worktree" — since "/" is not the sandbox project dir, workspace $HOME, or
// "/tmp" (the only entries boidPolicy's AllowedCwdRoots ever contains, see
// internal/orchestrator/policy.go's boidPolicy). Every other caller of this
// exact command (a `boid exec` shell-adapter dispatch, or this package's own
// TestReadSessionsFromRPC_EndToEnd fake-broker test) inherits a cwd of
// rc.Workspace instead, which is why unit/e2e coverage never caught this:
// none of them replicate runner-inner-child's own bare "/" cwd. cmd.Dir must
// be pinned to a value boidPolicy's AllowedCwdRoots always contains
// regardless of project/workspace context — "/tmp" is the only unconditional
// entry, so that's the target this test locks in.
func TestBuildTaskPayloadSessionsCmd_DirIsAllowedByBoidPolicy(t *testing.T) {
	cmd := buildTaskPayloadSessionsCmd(context.Background(), nil)
	if cmd.Dir != "/tmp" {
		t.Errorf("cmd.Dir = %q, want \"/tmp\" (the only cwd boidPolicy's AllowedCwdRoots always contains)", cmd.Dir)
	}
}

// TestBuildTaskPayloadSessionsCmd_EnvOverlaysRunContextEnv confirms the shim
// gets the RunContext.Env entries it needs to reach the broker
// (BOID_TASK_ID / BOID_JOB_ID / BOID_BROKER_SOCKET / BOID_BROKER_TOKEN /
// BOID_BUILTIN_SHIM=1) — the same map Run() hands the agent child's own
// cmd.Env — layered on top of (not replacing) the current process env.
func TestBuildTaskPayloadSessionsCmd_EnvOverlaysRunContextEnv(t *testing.T) {
	t.Setenv("SOME_PARENT_VAR", "keep-me")
	env := map[string]string{
		"BOID_TASK_ID":       "t1",
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
		"BOID_BROKER_TOKEN":  "tok",
		"BOID_BUILTIN_SHIM":  "1",
	}
	cmd := buildTaskPayloadSessionsCmd(context.Background(), env)

	got := map[string]bool{}
	for _, kv := range cmd.Env {
		got[kv] = true
	}
	for k, v := range env {
		if !got[k+"="+v] {
			t.Errorf("cmd.Env missing %s=%s; env=%v", k, v, cmd.Env)
		}
	}
	if !got["SOME_PARENT_VAR=keep-me"] {
		t.Error("cmd.Env should still carry the current process's own env (SOME_PARENT_VAR)")
	}
}

// ---------- readSessionsFromRPC (fetchTaskPayloadSessions injected) ----------

// withFakeTaskPayloadSessions overrides fetchTaskPayloadSessions so
// readSessionsFromRPC never spawns a real subprocess.
func withFakeTaskPayloadSessions(t *testing.T, fn func(ctx context.Context, env map[string]string) ([]byte, error)) {
	t.Helper()
	saved := fetchTaskPayloadSessions
	fetchTaskPayloadSessions = fn
	t.Cleanup(func() { fetchTaskPayloadSessions = saved })
}

// TestReadSessionsFromRPC_FetchErrorPropagates pins the codex-review fix
// (PR #800, Major finding): a broker/exec failure must propagate as an
// error, not collapse to nil the way "no sessions" does. Swallowing it would
// make Run()'s caller synthesize a fresh single-entry session list from a
// transient failure and persist that over the task's real history — see
// readSessionsFromRPC's doc comment and wiring-seams.md #16.
func TestReadSessionsFromRPC_FetchErrorPropagates(t *testing.T) {
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return nil, errors.New("boid: not found on PATH")
	})
	got, err := readSessionsFromRPC(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error when the RPC fetch fails")
	}
	if got != nil {
		t.Errorf("got %+v, want nil sessions alongside the error", got)
	}
}

// TestReadSessionsFromRPC_ExitErrorIncludesStderr pins a real production
// incident (boid agent claude -p <project> failing with nothing but "exit
// status 1" — no way to tell why from the daemon's stored transcript or the
// user-visible error). fetchTaskPayloadSessions's default implementation
// runs `boid task payload --field ...` via cmd.Output(), which DOES capture
// the subprocess's stderr into ExitError.Stderr — but the old wrap
// (`fmt.Errorf("...: %w", err)`) only used err.Error(), which for
// *exec.ExitError is just the bare "exit status N" from os.ProcessState,
// discarding the actual diagnostic. The wrap must surface Stderr too.
func TestReadSessionsFromRPC_ExitErrorIncludesStderr(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo 'boid task payload: no context tracked for job \"abc\"' >&2; exit 1")
	_, cmdErr := cmd.Output()
	exitErr, ok := cmdErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("test setup: expected *exec.ExitError, got %T: %v", cmdErr, cmdErr)
	}

	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return nil, exitErr
	})
	_, err := readSessionsFromRPC(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error when the RPC fetch fails")
	}
	if !strings.Contains(err.Error(), "no context tracked for job") {
		t.Errorf("error %q does not include the subprocess stderr; a real failure would be undiagnosable from this alone", err.Error())
	}
}

func TestReadSessionsFromRPC_EmptyFieldReturnsNilNoError(t *testing.T) {
	// api.ResolveJSONField returns "" (empty stdout) when the field path is
	// absent — e.g. a brand-new task with no artifact.claude_code.sessions
	// entry yet. Must behave like the old "no payload.json yet" case: nil,
	// no error.
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return []byte(""), nil
	})
	got, err := readSessionsFromRPC(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil for empty field", got)
	}
}

// TestReadSessionsFromRPC_MalformedJSONPropagatesError covers the exec-
// succeeded-but-garbage-stdout case (a shim/broker bug, not a "no sessions"
// signal) — must also error rather than silently return nil.
func TestReadSessionsFromRPC_MalformedJSONPropagatesError(t *testing.T) {
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return []byte("not json"), nil
	})
	got, err := readSessionsFromRPC(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if got != nil {
		t.Errorf("got %+v, want nil sessions alongside the error", got)
	}
}

func TestReadSessionsFromRPC_Success(t *testing.T) {
	var gotEnv map[string]string
	withFakeTaskPayloadSessions(t, func(_ context.Context, env map[string]string) ([]byte, error) {
		gotEnv = env
		return []byte(`[{"type":"execution","name":"","id":"abc"}]`), nil
	})
	env := map[string]string{"BOID_TASK_ID": "t1"}
	got, err := readSessionsFromRPC(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []session{{Type: "execution", Name: "", ID: "abc"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Errorf("fetchTaskPayloadSessions received env %+v, want %+v", gotEnv, env)
	}
}

// ---------- sessionsPayloadPatchBody (pure) ----------

// TestSessionsPayloadPatchBody_Shape pins the exact RPC body shape: no outer
// "payload_patch" envelope (unlike the retired file convention) — boid_shim.go's
// parseBoidTaskUpdatePayloadPatch reads --payload-patch's value as the patch
// body directly (see docs/plans/phase5-shim-and-task-context.md decision 6/7).
func TestSessionsPayloadPatchBody_Shape(t *testing.T) {
	sessions := []session{{Type: "execution", Name: "", ID: "abc"}}
	body, err := sessionsPayloadPatchBody(sessions)
	if err != nil {
		t.Fatalf("sessionsPayloadPatchBody: %v", err)
	}

	var got struct {
		Artifact struct {
			ClaudeCode struct {
				Sessions []session `json:"sessions"`
			} `json:"claude_code"`
		} `json:"artifact"`
		PayloadPatch json.RawMessage `json:"payload_patch"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, body)
	}
	if got.PayloadPatch != nil {
		t.Errorf("body must NOT be wrapped in a payload_patch envelope, got %s", body)
	}
	if !reflect.DeepEqual(got.Artifact.ClaudeCode.Sessions, sessions) {
		t.Errorf("got sessions %+v, want %+v (body=%s)", got.Artifact.ClaudeCode.Sessions, sessions, body)
	}
}

// ---------- buildTaskUpdatePayloadPatchCmd (pure, no process spawn) ----------

// TestBuildTaskUpdatePayloadPatchCmd_Args pins the exact CLI form the claude
// adapter uses to apply its own payload patch: `boid task update
// --payload-patch @-`, matching the plan doc's decision 6/7 example verbatim.
func TestBuildTaskUpdatePayloadPatchCmd_Args(t *testing.T) {
	cmd := buildTaskUpdatePayloadPatchCmd(context.Background(), nil, []byte("{}"))
	want := []string{"boid", "task", "update", "--payload-patch", "@-"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

// TestBuildTaskUpdatePayloadPatchCmd_DirIsAllowedByBoidPolicy mirrors
// TestBuildTaskPayloadSessionsCmd_DirIsAllowedByBoidPolicy — same
// runner-inner-child bare "/" cwd concern applies to every boid-shim
// subprocess this package spawns, not just the sessions-read one.
func TestBuildTaskUpdatePayloadPatchCmd_DirIsAllowedByBoidPolicy(t *testing.T) {
	cmd := buildTaskUpdatePayloadPatchCmd(context.Background(), nil, []byte("{}"))
	if cmd.Dir != "/tmp" {
		t.Errorf("cmd.Dir = %q, want \"/tmp\"", cmd.Dir)
	}
}

// TestBuildTaskUpdatePayloadPatchCmd_StdinCarriesBody confirms body is wired
// as the subprocess's stdin (the `@-` convention reads from stdin).
func TestBuildTaskUpdatePayloadPatchCmd_StdinCarriesBody(t *testing.T) {
	body := []byte(`{"artifact":{"claude_code":{"sessions":[]}}}`)
	cmd := buildTaskUpdatePayloadPatchCmd(context.Background(), nil, body)
	got, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		t.Fatalf("read cmd.Stdin: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("cmd.Stdin = %s, want %s", got, body)
	}
}

// TestBuildTaskUpdatePayloadPatchCmd_EnvOverlaysRunContextEnv mirrors
// TestBuildTaskPayloadSessionsCmd_EnvOverlaysRunContextEnv.
func TestBuildTaskUpdatePayloadPatchCmd_EnvOverlaysRunContextEnv(t *testing.T) {
	t.Setenv("SOME_PARENT_VAR", "keep-me")
	env := map[string]string{
		"BOID_JOB_ID":        "job-1",
		"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
		"BOID_BROKER_TOKEN":  "tok",
		"BOID_BUILTIN_SHIM":  "1",
	}
	cmd := buildTaskUpdatePayloadPatchCmd(context.Background(), env, []byte("{}"))

	got := map[string]bool{}
	for _, kv := range cmd.Env {
		got[kv] = true
	}
	for k, v := range env {
		if !got[k+"="+v] {
			t.Errorf("cmd.Env missing %s=%s; env=%v", k, v, cmd.Env)
		}
	}
	if !got["SOME_PARENT_VAR=keep-me"] {
		t.Error("cmd.Env should still carry the current process's own env (SOME_PARENT_VAR)")
	}
}

// ---------- sendTaskUpdatePayloadPatch (injected for Run()-level tests) ----------

// withFakeSendTaskUpdatePayloadPatch overrides sendTaskUpdatePayloadPatch so
// Run() never spawns a real subprocess.
func withFakeSendTaskUpdatePayloadPatch(t *testing.T, fn func(ctx context.Context, env map[string]string, body []byte) error) {
	t.Helper()
	saved := sendTaskUpdatePayloadPatch
	sendTaskUpdatePayloadPatch = fn
	t.Cleanup(func() { sendTaskUpdatePayloadPatch = saved })
}

// withStubbedClaudeCLI overrides lookPath to succeed without requiring an
// actual claude binary on the test host's PATH — used by tests that need
// Run() to get past the step-0 fail-fast gate without ever reaching
// cmd.Start() (e.g. because a fake fetchTaskPayloadSessions error aborts
// Run() first).
func withStubbedClaudeCLI(t *testing.T) {
	t.Helper()
	saved := lookPath
	lookPath = func(string) (string, error) { return "/usr/bin/claude", nil }
	t.Cleanup(func() { lookPath = saved })
}

// TestRun_SessionsFetchError_AbortsBeforeStartingClaude is the Run()-level
// regression test for the codex-review Major finding on PR #800: when
// readSessionsFromRPC cannot determine the prior session list, Run() must
// fail outright — before ever forking claude and before
// sendTaskUpdatePayloadPatch applies anything — rather than silently
// proceeding with a truncated (or entirely fresh) session list that would
// then get persisted as this task's payload patch.
func TestRun_SessionsFetchError_AbortsBeforeStartingClaude(t *testing.T) {
	withStubbedClaudeCLI(t)
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return nil, errors.New("broker unreachable")
	})
	sendCalled := false
	withFakeSendTaskUpdatePayloadPatch(t, func(context.Context, map[string]string, []byte) error {
		sendCalled = true
		return nil
	})

	a := New()
	_, err := a.Run(context.Background(), adapters.RunContext{})
	if err == nil {
		t.Fatal("expected Run to fail when the prior-sessions RPC fails")
	}
	if sendCalled {
		t.Error("sendTaskUpdatePayloadPatch must not be called when the session fetch fails (would truncate session history)")
	}
}

// TestRun_SendPayloadPatchError_AbortsBeforeStartingClaude pins the mirror
// case: a broker failure while applying the session-id payload patch must
// also abort Run() before claude ever starts, not silently proceed with an
// un-recorded session id.
func TestRun_SendPayloadPatchError_AbortsBeforeStartingClaude(t *testing.T) {
	withStubbedClaudeCLI(t)
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return nil, nil
	})
	withFakeSendTaskUpdatePayloadPatch(t, func(context.Context, map[string]string, []byte) error {
		return errors.New("broker unreachable")
	})

	a := New()
	_, err := a.Run(context.Background(), adapters.RunContext{})
	if err == nil {
		t.Fatal("expected Run to fail when applying the payload patch fails")
	}
}

// TestRun_SendPayloadPatchReceivesSessionUpdate confirms Run() feeds
// sendTaskUpdatePayloadPatch the same env map the agent child itself gets,
// and a body containing the freshly generated session id — mirrors
// TestReadSessionsFromRPC_Success's env-plumbing assertion for the sibling
// read path.
func TestRun_SendPayloadPatchReceivesSessionUpdate(t *testing.T) {
	withStubbedClaudeCLI(t)
	withFakeTaskPayloadSessions(t, func(context.Context, map[string]string) ([]byte, error) {
		return nil, nil
	})

	var gotEnv map[string]string
	var gotBody []byte
	withFakeSendTaskUpdatePayloadPatch(t, func(_ context.Context, env map[string]string, body []byte) error {
		gotEnv = env
		gotBody = body
		return errors.New("stop before forking claude") // Run() has no way to fake-exec claude in this unit test.
	})

	env := map[string]string{"BOID_TASK_ID": "t1", "BOID_JOB_ID": "job-1"}
	a := New()
	_, err := a.Run(context.Background(), adapters.RunContext{Env: env})
	if err == nil {
		t.Fatal("expected an error (the fake sender always fails)")
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Errorf("sendTaskUpdatePayloadPatch received env %+v, want %+v", gotEnv, env)
	}
	var got struct {
		Artifact struct {
			ClaudeCode struct {
				Sessions []session `json:"sessions"`
			} `json:"claude_code"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("unmarshal body: %v (body=%s)", err, gotBody)
	}
	if len(got.Artifact.ClaudeCode.Sessions) != 1 || got.Artifact.ClaudeCode.Sessions[0].ID == "" {
		t.Errorf("expected exactly one fresh session entry, got %+v", got.Artifact.ClaudeCode.Sessions)
	}
}

func TestPauseSystemPromptMentionsNotify(t *testing.T) {
	// Smoke test: a future maintainer that drops the notify guidance from
	// the prompt should fail this so the regression is loud.
	if !strings.Contains(taskSystemPrompt, "boid task notify") {
		t.Error("taskSystemPrompt no longer mentions `boid task notify`")
	}
}

// TestSessionSystemPrompt_NoRetiredContextFilePath is the claude-side sibling
// of codex/opencode's TestTaskBootstrapPrompt_NoRetiredContextFilePath —
// added proactively during codex review on PR #800 (Minor 2), since
// sessionSystemPrompt is claude-only and session-only, so nothing else in
// this PR's static tests would have caught it referencing the retired
// ~/.boid/context/ file path.
func TestSessionSystemPrompt_NoRetiredContextFilePath(t *testing.T) {
	if strings.Contains(sessionSystemPrompt, "~/.boid/context/") {
		t.Errorf("sessionSystemPrompt still references the retired ~/.boid/context/ file path:\n%s", sessionSystemPrompt)
	}
}

func TestSessionSystemPrompt_ReferencesTaskEnvCLI(t *testing.T) {
	if !strings.Contains(sessionSystemPrompt, "boid task env") {
		t.Errorf("sessionSystemPrompt missing `boid task env`:\n%s", sessionSystemPrompt)
	}
}

// withMissingClaudeCLI overrides lookPath for deterministic test runs: it
// forces the fail-fast PATH lookup in Run() to miss, regardless of whether
// the host actually has claude installed.
func withMissingClaudeCLI(t *testing.T) {
	t.Helper()
	saved := lookPath
	lookPath = func(string) (string, error) {
		return "", exec.ErrNotFound
	}
	t.Cleanup(func() { lookPath = saved })
}

// TestRun_MissingCLI_ReturnsFailFastError pins the Phase 4 PR3 fail-fast
// contract (docs/plans/home-workspace-volume.md): now that
// claude.Adapter.Bindings no longer bind-mounts a claude CLI (see
// bindings.go), a PATH lookup miss almost always means the workspace's
// init.sh hasn't installed claude yet — not a generic "command not found".
// Run() must return an actionable error naming both the CLI and the
// workspace slug (read from rc.Env["BOID_WORKSPACE_SLUG"], set by
// BuildSandboxSpec from SandboxRuntimeInfo.WorkspaceSlug) before ever
// attempting cmd.Start().
func TestRun_MissingCLI_ReturnsFailFastError(t *testing.T) {
	withMissingClaudeCLI(t)
	a := New()

	_, err := a.Run(context.Background(), adapters.RunContext{
		Env: map[string]string{"BOID_WORKSPACE_SLUG": "myws"},
	})
	if err == nil {
		t.Fatal("expected an error when claude is not on PATH")
	}
	for _, want := range []string{"claude", "myws", "init.sh", "docs/plans/home-workspace-volume.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

// TestRun_MissingCLI_DefaultsSlugWhenEnvAbsent covers RunContext.Env not
// carrying BOID_WORKSPACE_SLUG at all (e.g. bare test wiring, or a caller
// that predates BuildSandboxSpec's PR3 wiring) — the error must still name
// a workspace ("default", the fallback slug every unassigned project
// resolves to) rather than produce a blank or malformed message.
func TestRun_MissingCLI_DefaultsSlugWhenEnvAbsent(t *testing.T) {
	withMissingClaudeCLI(t)
	a := New()

	_, err := a.Run(context.Background(), adapters.RunContext{})
	if err == nil {
		t.Fatal("expected an error when claude is not on PATH")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error = %q, want it to name the default workspace slug", err.Error())
	}
}

// TestMissingCLIError_WrapsLookupMiss ensures the exec.LookPath failure
// itself (e.g. exec.ErrNotFound) is preserved via %w so errors.Is still
// works for callers that want to distinguish "CLI missing" from other Run()
// failure modes.
func TestMissingCLIError_WrapsLookupMiss(t *testing.T) {
	withMissingClaudeCLI(t)
	a := New()

	_, err := a.Run(context.Background(), adapters.RunContext{})
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("error = %v, want errors.Is(err, exec.ErrNotFound) to hold", err)
	}
}
