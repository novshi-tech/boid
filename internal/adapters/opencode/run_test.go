package opencode

import (
	"context"
	"strings"
	"testing"
)

func TestBuildArgs_Fresh(t *testing.T) {
	got := buildArgs("", "", "hello")
	want := []string{"opencode", "run", "hello"}
	if !equalSlice(got, want) {
		t.Errorf("buildArgs fresh = %v, want %v", got, want)
	}
}

func TestBuildArgs_Resume(t *testing.T) {
	got := buildArgs("session-uuid", "", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-s session-uuid") {
		t.Errorf("buildArgs resume missing `-s session-uuid`: %q", joined)
	}
	if !strings.Contains(joined, "--continue") {
		t.Errorf("buildArgs resume must include --continue (opencode requires it with -s): %q", joined)
	}
	if got[len(got)-1] != "hello" {
		t.Errorf("buildArgs prompt must be last positional, got %v", got)
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	got := buildArgs("", "anthropic/claude-sonnet-4-6", "hello")
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "-m anthropic/claude-sonnet-4-6") {
		t.Errorf("buildArgs with model missing `-m ...`: %q", joined)
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
