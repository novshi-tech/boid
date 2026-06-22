package codex

import (
	"context"
	"strings"
	"testing"
)

func TestBuildArgs_NonInteractive_Fresh(t *testing.T) {
	got := buildArgs(false, "", "hello")
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
	got := buildArgs(false, "gpt-5", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m gpt-5") {
		t.Errorf("buildArgs with model: %q missing `-m gpt-5`", joined)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
	}
}

// Session-id resume was removed repo-wide; the non-interactive argv must
// never contain `resume`, regardless of what session metadata the caller
// might still pass through other channels.
func TestBuildArgs_NonInteractive_NeverResumes(t *testing.T) {
	got := buildArgs(false, "", "hello")
	for _, a := range got {
		if a == "resume" {
			t.Errorf("argv must never contain `resume`, got %v", got)
		}
	}
}

func TestBuildArgs_Interactive_NoSubcommand(t *testing.T) {
	got := buildArgs(true, "", "")
	// Interactive form is `codex` (no sub-command) — confirm `exec` is
	// absent, the bypass flag is present, and no prompt was appended.
	if len(got) < 1 || got[0] != "codex" {
		t.Fatalf("interactive argv must start with codex, got %v", got)
	}
	for _, a := range got {
		if a == "exec" {
			t.Errorf("interactive argv must not contain `exec`, got %v", got)
		}
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("interactive argv missing bypass flag: %v", got)
	}
	// codex-cli 0.141.0 moved --skip-git-repo-check under the `exec`
	// subcommand; top-level argv must not carry it or codex errors out
	// with "unexpected argument".
	if strings.Contains(joined, "--skip-git-repo-check") {
		t.Errorf("interactive argv must not include --skip-git-repo-check, got %v", got)
	}
}

func TestBuildArgs_Interactive_WithModel(t *testing.T) {
	got := buildArgs(true, "gpt-5", "hello")
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

// selectPrompt encodes the four-way decision for the codex first turn:
//   - hook (isSession=false): always bootstrap, never UserAnswer
//   - session + UserAnswer empty: empty (TUI takes no positional)
//   - session + UserAnswer non-empty: that text verbatim
func TestSelectPrompt(t *testing.T) {
	cases := []struct {
		name       string
		isSession  bool
		userAnswer string
		want       string
	}{
		{"hook empty UserAnswer returns bootstrap", false, "", taskBootstrapPrompt},
		{"hook non-empty UserAnswer is ignored (still bootstrap)", false, "ignored", taskBootstrapPrompt},
		{"session empty UserAnswer is empty positional", true, "", ""},
		{"session non-empty UserAnswer is verbatim", true, "boot me up", "boot me up"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := selectPrompt(c.isSession, c.userAnswer)
			if got != c.want {
				t.Errorf("selectPrompt(%v, %q) = %q (len=%d), want %q (len=%d)",
					c.isSession, c.userAnswer, got, len(got), c.want, len(c.want))
			}
		})
	}
}

// Hook argv must end with the full bootstrap prompt — the bootstrap is what
// tells the agent to read SKILL.md + the task context yaml, so an argv that
// silently drops it would leave the task agent with no instructions at all.
func TestBuildArgs_Hook_AppendsBootstrap(t *testing.T) {
	got := buildArgs(false, "", selectPrompt(false, ""))
	if len(got) == 0 {
		t.Fatalf("buildArgs returned no argv")
	}
	if got[len(got)-1] != taskBootstrapPrompt {
		t.Errorf("hook argv last element should be the bootstrap prompt; got tail %q", got[len(got)-1])
	}
	// Sanity: the bootstrap text is the canonical one (catches a typo'd copy).
	if !strings.Contains(taskBootstrapPrompt, "boid task notify") ||
		!strings.Contains(taskBootstrapPrompt, "~/.claude/skills/boid-task/SKILL.md") {
		t.Errorf("taskBootstrapPrompt missing required hooks: %q", taskBootstrapPrompt)
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
