package codex

import (
	"context"
	"strings"
	"testing"
)

func TestBuildArgs_NonInteractive_Fresh(t *testing.T) {
	got := buildArgs(false, "", "", "hello")
	want := []string{
		"codex", "exec",
		"--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox",
		"hello",
	}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs fresh = %v, want %v", got, want)
	}
}

func TestBuildArgs_NonInteractive_WithModel(t *testing.T) {
	got := buildArgs(false, "", "gpt-5", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m gpt-5") {
		t.Errorf("buildArgs with model: %q missing `-m gpt-5`", joined)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
	}
}

func TestBuildArgs_NonInteractive_Resume(t *testing.T) {
	got := buildArgs(false, "session-uuid", "", "hello")
	if got[1] != "exec" || got[2] != "resume" || got[3] != "session-uuid" {
		t.Errorf("buildArgs resume order = %v, want codex exec resume <id> ...", got)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
	}
}

func TestBuildArgs_Interactive_NoSubcommand(t *testing.T) {
	got := buildArgs(true, "", "", "")
	// Interactive form is `codex` (no sub-command) — confirm `exec` is
	// absent, the bypass flags are present, and no prompt was appended.
	if len(got) < 1 || got[0] != "codex" {
		t.Fatalf("interactive argv must start with codex, got %v", got)
	}
	for _, a := range got {
		if a == "exec" {
			t.Errorf("interactive argv must not contain `exec`, got %v", got)
		}
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--skip-git-repo-check") ||
		!strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("interactive argv missing bypass flags: %v", got)
	}
	// SessionID and prompt should be ignored in interactive form.
	if strings.Contains(joined, "session-uuid") {
		t.Errorf("interactive argv must not include sessionID, got %v", got)
	}
}

func TestBuildArgs_Interactive_WithModel(t *testing.T) {
	got := buildArgs(true, "session-uuid", "gpt-5", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m gpt-5") {
		t.Errorf("interactive with model: %q missing `-m gpt-5`", joined)
	}
	// Prompt is positional only in non-interactive form. The TUI takes
	// input itself — the prompt arg must not leak into argv.
	if strings.Contains(joined, " hello") || got[len(got)-1] == "hello" {
		t.Errorf("interactive argv must not include prompt, got %v", got)
	}
}

func TestUsage_Stub(t *testing.T) {
	a := New()
	u, err := a.Usage(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.Model != "" || u.InputTokens != 0 || u.OutputTokens != 0 ||
		u.CacheCreationTokens != 0 || u.CacheReadTokens != 0 || u.Extra != nil {
		t.Errorf("Usage stub should be zero, got %+v", u)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
