package codex

import (
	"context"
	"strings"
	"testing"
)

func TestBuildArgs_Fresh(t *testing.T) {
	got := buildArgs("", "", "hello")
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

func TestBuildArgs_WithModel(t *testing.T) {
	got := buildArgs("", "gpt-5", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m gpt-5") {
		t.Errorf("buildArgs with model: %q missing `-m gpt-5`", joined)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
	}
}

func TestBuildArgs_Resume(t *testing.T) {
	got := buildArgs("session-uuid", "", "hello")
	if got[1] != "exec" || got[2] != "resume" || got[3] != "session-uuid" {
		t.Errorf("buildArgs resume order = %v, want codex exec resume <id> ...", got)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
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
