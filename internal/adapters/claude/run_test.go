package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
// determination happens inside the skill from environment.yaml `readonly`,
// so the prompt does not branch on behavior name.
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

func TestReadSessionsFromPayload_MissingFileReturnsNil(t *testing.T) {
	got := readSessionsFromPayload(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestReadSessionsFromPayload_MalformedJSONReturnsNil(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readSessionsFromPayload(path)
	if got != nil {
		t.Errorf("got %+v, want nil for malformed JSON", got)
	}
}

func TestReadSessionsFromPayload_ExtractsSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.json")
	body := `{
		"artifact": {
			"claude_code": {
				"sessions": [
					{"type": "execution", "name": "", "id": "abc"},
					{"type": "execution", "name": "verifier", "id": "def"}
				]
			}
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readSessionsFromPayload(path)
	want := []session{
		{Type: "execution", Name: "", ID: "abc"},
		{Type: "execution", Name: "verifier", ID: "def"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestWritePayloadPatch_FreshFile(t *testing.T) {
	dir := t.TempDir()
	sessions := []session{
		{Type: "execution", Name: "", ID: "abc"},
	}
	if err := writePayloadPatch(dir, sessions); err != nil {
		t.Fatalf("writePayloadPatch: %v", err)
	}

	got := readWrappedSessions(t, filepath.Join(dir, "payload_patch.json"))
	if !reflect.DeepEqual(got, sessions) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, sessions)
	}
}

func TestWritePayloadPatch_PreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload_patch.json")

	// Simulate an agent already having written boid task notify output.
	prior := map[string]any{
		"payload_patch": map[string]any{
			"task_notify": map[string]any{
				"message": "halfway done",
				"ask":     "should I continue?",
			},
			"artifact": map[string]any{
				"other_subsystem": map[string]any{"foo": "bar"},
			},
		},
	}
	data, _ := json.Marshal(prior)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sessions := []session{{Type: "execution", Name: "", ID: "abc"}}
	if err := writePayloadPatch(dir, sessions); err != nil {
		t.Fatalf("writePayloadPatch: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]any
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	patch, _ := loaded["payload_patch"].(map[string]any)
	if patch == nil {
		t.Fatalf("missing payload_patch in %s", raw)
	}

	notify, _ := patch["task_notify"].(map[string]any)
	if notify == nil || notify["message"] != "halfway done" {
		t.Errorf("task_notify lost: %v", patch)
	}

	artifact, _ := patch["artifact"].(map[string]any)
	otherSubsystem, _ := artifact["other_subsystem"].(map[string]any)
	if otherSubsystem == nil || otherSubsystem["foo"] != "bar" {
		t.Errorf("other_subsystem lost under artifact: %v", artifact)
	}

	// And the sessions update landed.
	if got := readWrappedSessions(t, path); !reflect.DeepEqual(got, sessions) {
		t.Errorf("sessions not applied: got %+v, want %+v", got, sessions)
	}
}

func TestWritePayloadPatch_OverwritesExistingSessions(t *testing.T) {
	dir := t.TempDir()
	if err := writePayloadPatch(dir, []session{{Type: "execution", Name: "", ID: "old"}}); err != nil {
		t.Fatal(err)
	}
	fresh := []session{{Type: "execution", Name: "", ID: "new"}}
	if err := writePayloadPatch(dir, fresh); err != nil {
		t.Fatal(err)
	}
	got := readWrappedSessions(t, filepath.Join(dir, "payload_patch.json"))
	if !reflect.DeepEqual(got, fresh) {
		t.Errorf("got %+v, want %+v", got, fresh)
	}
}

// readWrappedSessions reads sessions from a payload_patch.json file.
func readWrappedSessions(t *testing.T, path string) []session {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		PayloadPatch struct {
			Artifact struct {
				ClaudeCode struct {
					Sessions []session `json:"sessions"`
				} `json:"claude_code"`
			} `json:"artifact"`
		} `json:"payload_patch"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	return p.PayloadPatch.Artifact.ClaudeCode.Sessions
}

func TestPauseSystemPromptMentionsNotify(t *testing.T) {
	// Smoke test: a future maintainer that drops the notify guidance from
	// the prompt should fail this so the regression is loud.
	if !strings.Contains(taskSystemPrompt, "boid task notify") {
		t.Error("taskSystemPrompt no longer mentions `boid task notify`")
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
