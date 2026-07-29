package orchestrator_test

import (
	"errors"
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

	_, err := orchestrator.ReadProjectMetaFromBareRepo(bare)
	if err == nil {
		t.Fatal("expected error for a bare repo with no .boid/project.yaml at HEAD")
	}
	assertGitHeadReadKind(t, err, orchestrator.GitHeadReadFailurePathAbsent)
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
	_, err := orchestrator.ReadProjectMetaFromBareRepo(dir)
	if err == nil {
		t.Fatal("expected error reading project.yaml from a corrupt bare repo")
	}
	// `git rev-parse --verify HEAD` fails the same way for "not a git
	// repository at all" as it does for "a real repo with zero commits" —
	// the design doc groups both under HeadUnresolved since both fail
	// registration identically (docs/plans/workspace-default-project.md
	// 論点c: "HEAD が解決できない ... repo 自体の異常なので従来通り失敗させる").
	assertGitHeadReadKind(t, err, orchestrator.GitHeadReadFailureHeadUnresolved)
}

// TestReadProjectMetaFromBareRepo_EmptyRepo covers the other half of the
// HeadUnresolved bucket: a real (not faked) bare repo that git itself
// recognizes fine, just with zero commits yet — `git init --bare` and
// nothing else. Distinct from TestReadProjectMetaFromBareRepo_CorruptRepo
// above, which is not a valid git repository at all.
func TestReadProjectMetaFromBareRepo_EmptyRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	bare := filepath.Join(t.TempDir(), "empty.git")
	runGit(t, filepath.Dir(bare), "init", "-q", "--bare", bare)

	_, err := orchestrator.ReadProjectMetaFromBareRepo(bare)
	if err == nil {
		t.Fatal("expected error reading project.yaml from a bare repo with zero commits")
	}
	assertGitHeadReadKind(t, err, orchestrator.GitHeadReadFailureHeadUnresolved)
}

// assertGitHeadReadKind unwraps err via errors.As to *orchestrator.
// GitHeadReadError and checks its Kind — the classification PR1 adds
// (docs/plans/workspace-default-project.md 論点c). No caller reads Kind yet
// (that starts in a future PR); these tests pin the classification itself
// so that future wiring can rely on it.
func assertGitHeadReadKind(t *testing.T, err error, want orchestrator.GitHeadReadFailureKind) {
	t.Helper()
	var headErr *orchestrator.GitHeadReadError
	if !errors.As(err, &headErr) {
		t.Fatalf("expected error to be (or wrap) *orchestrator.GitHeadReadError, got %T: %v", err, err)
	}
	if headErr.Kind != want {
		t.Errorf("Kind = %v, want %v", headErr.Kind, want)
	}
}

func TestGitHeadReadFailureKind_String(t *testing.T) {
	cases := []struct {
		kind orchestrator.GitHeadReadFailureKind
		want string
	}{
		{orchestrator.GitHeadReadFailureOther, "other"},
		{orchestrator.GitHeadReadFailureHeadUnresolved, "head-unresolved"},
		{orchestrator.GitHeadReadFailurePathAbsent, "path-absent"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.want {
			t.Errorf("%v.String() = %q, want %q", c.kind, got, c.want)
		}
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

// TestSafeBareRepoPath_RejectsTraversal pins the concrete failure scenario
// codex's Blocker 2 finding describes: a --name of
// "../../../../tmp/owned" must never produce a path outside
// <data_dir>/repos/<workspace> — SafeBareRepoPath must refuse it outright
// rather than let filepath.Join silently clean it into an escaping path.
func TestSafeBareRepoPath_RejectsTraversal(t *testing.T) {
	dataDir := "/data"
	if _, err := orchestrator.SafeBareRepoPath(dataDir, "team-a", "../../../../tmp/owned"); err == nil {
		t.Fatal("expected SafeBareRepoPath to reject a traversal name")
	}
}

// TestSafeBareRepoPath_ValidName mirrors TestBareRepoPath's expectation for
// an ordinary name, confirming SafeBareRepoPath's extra validation does not
// change the computed path for the common case.
func TestSafeBareRepoPath_ValidName(t *testing.T) {
	got, err := orchestrator.SafeBareRepoPath("/data", "myws", "myproj")
	if err != nil {
		t.Fatalf("SafeBareRepoPath: %v", err)
	}
	want := filepath.Join("/data", "repos", "myws", "myproj.git")
	if got != want {
		t.Errorf("SafeBareRepoPath = %q, want %q", got, want)
	}
}

// TestSafeBareRepoPath_RejectsSymlinkedWorkspaceDir pins BLOCKER 2 (PR-2a
// codex round-2 review): the concrete failure scenario codex describes is
// <dataDir>/repos/<workspace> itself being replaced with a symlink to
// somewhere outside dataDir entirely — lexically, BareRepoPath's join still
// "looks" contained (it starts with the right string prefix), but the
// directory a subsequent clone/RemoveAll would actually touch is wherever
// the symlink points. PathIsUnderResolved (which SafeBareRepoPath now
// routes through) must catch this even though the symlink's TARGET
// directory does not exist yet — SafeBareRepoPath always runs BEFORE any
// clone, so there is nothing at the leaf path itself to resolve; only the
// ANCESTOR symlink needs to be caught.
func TestSafeBareRepoPath_RejectsSymlinkedWorkspaceDir(t *testing.T) {
	dataDir := t.TempDir()
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		t.Fatalf("mkdir repos: %v", err)
	}
	// Replace <dataDir>/repos/team-a with a symlink pointing OUTSIDE dataDir
	// entirely — the exact scenario codex describes ("If /data/repos/team-a
	// is replaced with a symlink to /srv/shared").
	if err := os.Symlink(outside, filepath.Join(dataDir, "repos", "team-a")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := orchestrator.SafeBareRepoPath(dataDir, "team-a", "myproj")
	if err == nil {
		t.Fatal("expected SafeBareRepoPath to reject a bare-repo path whose workspace ancestor is a symlink escaping dataDir")
	}
}

// TestSafeBareRepoPath_ReposRootItselfSymlinked_StillAllowed is the
// negative-control counterpart: SafeBareRepoPath's boundary is
// ReposRoot(dataDir) (<dataDir>/repos), so an operator relocating the ENTIRE
// repos tree by making <dataDir>/repos itself a symlink to alternate
// storage (e.g. a bigger disk mounted elsewhere) must not be rejected —
// root and path's ancestor both resolve through the SAME symlink, so they
// stay consistently related regardless of where it points.
func TestSafeBareRepoPath_ReposRootItselfSymlinked_StillAllowed(t *testing.T) {
	dataDir := t.TempDir()
	realStorage := t.TempDir()
	if err := os.Symlink(realStorage, filepath.Join(dataDir, "repos")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := orchestrator.SafeBareRepoPath(dataDir, "team-a", "myproj")
	if err != nil {
		t.Fatalf("SafeBareRepoPath: %v", err)
	}
	want := filepath.Join(dataDir, "repos", "team-a", "myproj.git")
	if got != want {
		t.Errorf("SafeBareRepoPath = %q, want %q (unresolved form — the leaf itself does not exist yet)", got, want)
	}
}

// TestSafeBareRepoPath_RejectsWorkspaceSymlinkEscapingReposRoot is a second
// escape variant: a per-workspace subdirectory symlinked to somewhere
// OUTSIDE <dataDir>/repos entirely — even a location still nominally inside
// dataDir elsewhere (not just outside dataDir altogether, as the primary
// Blocker 2 test above covers) — must still be rejected, since
// ReposRoot(dataDir) is the actual managed boundary this whole mechanism
// protects, not dataDir as a whole.
func TestSafeBareRepoPath_RejectsWorkspaceSymlinkEscapingReposRoot(t *testing.T) {
	dataDir := t.TempDir()
	elsewhereInDataDir := filepath.Join(dataDir, "actual-repos", "team-a")
	if err := os.MkdirAll(elsewhereInDataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "repos"), 0o755); err != nil {
		t.Fatalf("mkdir repos: %v", err)
	}
	if err := os.Symlink(elsewhereInDataDir, filepath.Join(dataDir, "repos", "team-a")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := orchestrator.SafeBareRepoPath(dataDir, "team-a", "myproj"); err == nil {
		t.Fatal("expected SafeBareRepoPath to reject a per-workspace symlink escaping ReposRoot(dataDir), even to a location still nominally under dataDir")
	}
}

// TestPathIsUnderResolved_MissingAncestorsStillCompare pins
// PathIsUnderResolved's register-time behavior directly: neither root nor
// path need exist yet (SafeBareRepoPath always calls this BEFORE any
// clone), and the comparison must still succeed for the ordinary
// no-symlinks-anywhere case.
func TestPathIsUnderResolved_MissingAncestorsStillCompare(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "repos", "team-a")
	path := filepath.Join(root, "myproj.git")

	under, err := orchestrator.PathIsUnderResolved(root, path)
	if err != nil {
		t.Fatalf("PathIsUnderResolved: %v", err)
	}
	if !under {
		t.Error("expected path to be reported as under root when neither exists yet")
	}

	notUnder, err := orchestrator.PathIsUnderResolved(root, filepath.Join(dataDir, "elsewhere", "x.git"))
	if err != nil {
		t.Fatalf("PathIsUnderResolved: %v", err)
	}
	if notUnder {
		t.Error("expected a sibling path to be reported as NOT under root")
	}
}
