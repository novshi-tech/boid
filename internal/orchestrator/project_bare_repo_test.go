package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
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

func TestIsBareRepoDir(t *testing.T) {
	bare := setupBareRepoFixture(t, "proj-bare", "Bare Project")
	if !orchestrator.IsBareRepoDir(bare) {
		t.Errorf("expected IsBareRepoDir(%q) = true", bare)
	}

	checkout := t.TempDir()
	if orchestrator.IsBareRepoDir(checkout) {
		t.Errorf("expected IsBareRepoDir(%q) = false for a plain (non-bare, non-git) dir", checkout)
	}
}

func TestReadProjectMetaFromBareRepo_Success(t *testing.T) {
	bare := setupBareRepoFixture(t, "proj-bare", "Bare Project")

	meta, err := orchestrator.ReadProjectMetaFromBareRepo(bare)
	if err != nil {
		t.Fatalf("ReadProjectMetaFromBareRepo: %v", err)
	}
	if meta.ID != "proj-bare" {
		t.Errorf("ID = %q, want proj-bare", meta.ID)
	}
	if meta.Name != "Bare Project" {
		t.Errorf("Name = %q, want %q", meta.Name, "Bare Project")
	}
}

func TestReadProjectMetaFromBareRepo_MissingProjectYAML(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("no project.yaml here"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, work, "init", "-q", "-b", "main")
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Test")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-q", "-m", "initial")

	bare := filepath.Join(root, "repo.git")
	runGit(t, root, "clone", "-q", "--bare", work, bare)

	if _, err := orchestrator.ReadProjectMetaFromBareRepo(bare); err == nil {
		t.Fatal("expected error for a bare repo with no .boid/project.yaml at HEAD")
	}
}

func TestReadProjectMetaFromBareRepo_CorruptRepo(t *testing.T) {
	// A directory that merely LOOKS bare (HEAD file + objects dir) but has
	// no real git plumbing underneath — simulates a corrupted bare repo.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}

	if !orchestrator.IsBareRepoDir(dir) {
		t.Fatal("expected IsBareRepoDir to report true for a dir with HEAD+objects (even if otherwise corrupt)")
	}
	if _, err := orchestrator.ReadProjectMetaFromBareRepo(dir); err == nil {
		t.Fatal("expected error reading project.yaml from a corrupt bare repo")
	}
}

func TestDefaultBranch(t *testing.T) {
	bare := setupBareRepoFixture(t, "proj-bare", "Bare Project")
	branch, err := orchestrator.DefaultBranch(bare)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if branch != "main" {
		t.Errorf("DefaultBranch = %q, want main", branch)
	}
}

func TestBareRepoPath(t *testing.T) {
	got := orchestrator.BareRepoPath("/data", "myws", "myproj")
	want := filepath.Join("/data", "repos", "myws", "myproj.git")
	if got != want {
		t.Errorf("BareRepoPath = %q, want %q", got, want)
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
