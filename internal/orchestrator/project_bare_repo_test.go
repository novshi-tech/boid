package orchestrator_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDeriveProjectNameFromURL(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://github.com/owner/repo.git", "repo"},
		{"https://github.com/owner/repo", "repo"},
		{"git@github.com:owner/repo.git", "repo"},
		{"ssh://git@bitbucket.org/owner/repo.git", "repo"},
		{"https://github.com/owner/repo/", "repo"},
	}
	for _, c := range cases {
		got, err := orchestrator.DeriveProjectNameFromURL(c.url)
		if err != nil {
			t.Errorf("DeriveProjectNameFromURL(%q): %v", c.url, err)
			continue
		}
		if got != c.want {
			t.Errorf("DeriveProjectNameFromURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestDeriveProjectNameFromURL_Invalid(t *testing.T) {
	for _, url := range []string{"", "   ", "noslash"} {
		if _, err := orchestrator.DeriveProjectNameFromURL(url); err == nil {
			t.Errorf("DeriveProjectNameFromURL(%q): expected error", url)
		}
	}
}

// TestValidProjectName pins the PR-2a codex round-1 Blocker 2 fix: a
// traversal-shaped --name (or a URL-derived name that happens to look like
// one) must be rejected before it ever reaches BareRepoPath's filepath.Join.
func TestValidProjectName(t *testing.T) {
	valid := []string{"myproj", "my-proj_2", "my.proj", "a", "PROJECT-123"}
	for _, name := range valid {
		if err := orchestrator.ValidProjectName(name); err != nil {
			t.Errorf("ValidProjectName(%q): unexpected error: %v", name, err)
		}
	}

	invalid := []string{
		"",
		"../../../../tmp/owned",
		"..",
		".",
		".hidden",
		"sub/dir",
		`sub\dir`,
		"foo..bar",
		"has space",
		"has/../traversal",
		strings.Repeat("x", 129),
	}
	for _, name := range invalid {
		if err := orchestrator.ValidProjectName(name); err == nil {
			t.Errorf("ValidProjectName(%q): expected error, got nil", name)
		}
	}
}
