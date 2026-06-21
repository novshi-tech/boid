package opencode

import (
	"context"
	"strings"
	"testing"
)

func TestBuildArgs_NonInteractive_Fresh(t *testing.T) {
	got := buildArgs(false, "/ws", "", "hello")
	want := []string{"opencode", "run", "hello"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs fresh = %v, want %v", got, want)
	}
}

// Session-id resume was removed repo-wide; the non-interactive argv must
// never contain `-s` / `--continue` regardless of what session metadata the
// caller might still pass through other channels.
func TestBuildArgs_NonInteractive_NeverResumes(t *testing.T) {
	got := buildArgs(false, "/ws", "", "hello")
	joined := strings.Join(got, " ")
	for _, bad := range []string{"-s ", "--continue"} {
		if strings.Contains(joined, bad) {
			t.Errorf("argv must never contain %q, got %v", bad, got)
		}
	}
}

func TestBuildArgs_NonInteractive_WithModel(t *testing.T) {
	got := buildArgs(false, "/ws", "anthropic/claude-sonnet-4-6", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m anthropic/claude-sonnet-4-6") {
		t.Errorf("buildArgs with model missing `-m ...`: %q", joined)
	}
}

func TestBuildArgs_Interactive_WorkspacePositional(t *testing.T) {
	got := buildArgs(true, "/workspace", "", "")
	if len(got) < 1 || got[0] != "opencode" {
		t.Fatalf("interactive argv must start with opencode, got %v", got)
	}
	// `run` is the non-interactive sub-command — must not appear in
	// interactive form.
	for _, a := range got {
		if a == "run" {
			t.Errorf("interactive argv must not contain `run`, got %v", got)
		}
	}
	// Workspace must land as the first positional arg (opencode treats it
	// as the project root).
	if len(got) < 2 || got[1] != "/workspace" {
		t.Errorf("interactive argv must include workspace as positional, got %v", got)
	}
}

func TestBuildArgs_Interactive_NoWorkspace(t *testing.T) {
	// No workspace: opencode falls back to cwd (which the adapter still
	// sets via cmd.Dir). argv must not contain an empty string.
	got := buildArgs(true, "", "", "")
	for _, a := range got {
		if a == "" {
			t.Errorf("interactive argv must not contain empty string, got %v", got)
		}
	}
	if got[0] != "opencode" {
		t.Errorf("interactive argv[0] = %q, want opencode", got[0])
	}
}

func TestBuildArgs_Interactive_WithModel(t *testing.T) {
	got := buildArgs(true, "/workspace", "anthropic/claude-sonnet-4-6", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m anthropic/claude-sonnet-4-6") {
		t.Errorf("interactive with model missing `-m ...`: %q", joined)
	}
	// Prompt has no place in the TUI invocation — opencode reads input
	// itself.
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
