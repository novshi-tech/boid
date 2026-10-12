package orchestrator_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestProjectStore_SetGet(t *testing.T) {
	s := orchestrator.NewProjectStore()

	meta := &projectspec.ProjectMeta{
		ID:   "proj-1",
		Name: "Test",
	}
	s.Set("proj-1", meta)

	got, ok := s.Get("proj-1")
	if !ok {
		t.Fatal("expected to find proj-1")
	}
	if got.ID != "proj-1" {
		t.Fatalf("expected id proj-1, got %s", got.ID)
	}
	if got.Name != "Test" {
		t.Fatalf("expected name Test, got %s", got.Name)
	}
}

func TestProjectStore_Get_NotFound(t *testing.T) {
	s := orchestrator.NewProjectStore()

	_, ok := s.Get("nonexistent")
	if ok {
		t.Fatal("expected not found")
	}
}

func TestProjectStore_Remove(t *testing.T) {
	s := orchestrator.NewProjectStore()

	s.Set("proj-1", &projectspec.ProjectMeta{ID: "proj-1", Name: "Test"})
	s.Remove("proj-1")

	_, ok := s.Get("proj-1")
	if ok {
		t.Fatal("expected not found after remove")
	}
}

// TestProjectStore_LoadAll_CorruptBareRepo_MarksDegradedNotDeleted and
// TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_MarksDegradedNotDeleted
// are the bare-repo-model counterparts of TestProjectStore_LoadAll_
// MarksDegradedNotDeleted above, pinning the two other §論点a scenarios the
// PR-2a task brief calls out by name ("Bare repo becomes corrupt → project
// marked degraded, NOT deleted" / "project.yaml deleted from bare repo HEAD
// → same") at the LoadAll (not just the lower-level
// ReadProjectMetaFromBareRepo, project_bare_repo_test.go) integration
// point — the level internal/server/wire.go's buildProjectStore actually
// calls.

// TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_PreservesExplicitNameAcrossRestart
// pins the Codex review P1 fix (PR5 round 1): an explicit --name given at
// registration time cannot be recovered from UpstreamURL alone (they can
// legitimately differ), so a cold-start first reload — a fresh
// *ProjectStore, no previously-cached Name to fall back on — must recover it
// from the bare-repo directory's own basename instead, which always equals
// the exact name (explicit or derived) CreateProjectFromGitURL used.
func TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_PreservesExplicitNameAcrossRestart(t *testing.T) {
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
	runGitLocal(t, work, "init", "-q", "-b", "main")
	runGitLocal(t, work, "config", "user.email", "test@example.com")
	runGitLocal(t, work, "config", "user.name", "Test")
	runGitLocal(t, work, "add", ".")
	runGitLocal(t, work, "commit", "-q", "-m", "initial")
	// The bare repo's directory name ("custom-name.git") deliberately
	// differs from what DeriveProjectNameFromURL(UpstreamURL) would produce
	// from work's own basename ("work") — simulating an explicit --name
	// given at registration time.
	bare := filepath.Join(root, "custom-name.git")
	runGitLocal(t, root, "clone", "-q", "--bare", work, bare)

	// See the sibling SurvivesReload test's comment: a real, resolvable
	// UpstreamURL is required now that the fallback below verifies
	// candidate.ID against api.DeriveProjectIDFromURL(candidate.UpstreamURL)
	// (Codex round-4 review, PR7), not just its prefix.
	upstreamURL := "https://example-forge.test/org/" + t.Name()
	expectedID, derr := api.DeriveProjectIDFromURL(upstreamURL)
	if derr != nil {
		t.Fatalf("api.DeriveProjectIDFromURL: %v", derr)
	}

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{{
		ID:          expectedID,
		WorkDir:     bare,
		WorkspaceID: "team-a",
		UpstreamURL: upstreamURL,
	}})
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}

	meta, ok := s.Get(expectedID)
	if !ok {
		t.Fatal("expected a synthesized meta to be cached after reload")
	}
	if meta.Name != "custom-name" {
		t.Errorf("meta.Name = %q, want %q (explicit --name preserved across restart via WorkDir basename)", meta.Name, "custom-name")
	}
}

func runGitLocal(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func setupProjectDir(t *testing.T, dir, id, name string) {
	t.Helper()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
}
