package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// setupBareRepoFixture builds a bare git repository at <dir>/repo.git whose
// HEAD tree contains .boid/project.yaml with the given id/name — the
// on-disk shape ProjectStore.LoadBareRepo/ReadProjectMetaFromBareRepo read
// from. It works by committing into a throwaway working clone, then
// `git clone --bare`-ing that into the final bare path (simplest way to get
// a populated bare repo without hand-writing git plumbing objects).
func setupBareRepoFixture(t *testing.T, id, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(filepath.Join(work, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(work, ".boid", "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "initial")

	barePath := filepath.Join(root, "repo.git")
	runGit(t, root, "clone", "-q", "--bare", work, barePath)
	return barePath
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

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
