package orchestrator_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

func TestProjectStore_Load(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	yaml := `
id: loaded-proj
name: Loaded Project
task_behaviors:
  dev:
    name: dev
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	_ = hooksDir

	s := orchestrator.NewProjectStore()
	meta, err := s.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if meta.ID != "loaded-proj" {
		t.Fatalf("expected id loaded-proj, got %s", meta.ID)
	}

	got, ok := s.Get("loaded-proj")
	if !ok {
		t.Fatal("expected to find loaded-proj in store after Load")
	}
	if got.Name != "Loaded Project" {
		t.Fatalf("expected name 'Loaded Project', got %s", got.Name)
	}
}

// TestProjectStore_Load_ProjectLocalIgnored verifies that the deprecated
// project.local.yaml is silently ignored; its env/host_commands/additional_bindings
// are no longer merged into behaviors. Users should migrate to workspace.yaml.
func TestProjectStore_Load_ProjectLocalIgnored(t *testing.T) {
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir boid dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: loaded-proj\nname: Loaded Project\ntask_behaviors:\n  dev:\n    name: dev\n"), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.local.yaml"), []byte("env:\n  LOCAL: yes\n"), 0o644); err != nil {
		t.Fatalf("write project.local.yaml: %v", err)
	}

	s := orchestrator.NewProjectStore()
	meta, err := s.Load(dir)
	if err != nil {
		t.Fatalf("load with project.local.yaml present: %v", err)
	}
	// project.local.yaml is deprecated and must not be merged.
	if meta.TaskBehaviors["dev"].Env["LOCAL"] == "yes" {
		t.Fatalf("expected project.local.yaml env to be ignored, but LOCAL=yes was applied")
	}
}

func TestProjectStore_Load_InvalidYAML(t *testing.T) {
	dir := t.TempDir()

	s := orchestrator.NewProjectStore()
	_, err := s.Load(dir)
	if err == nil {
		t.Fatal("expected error for missing project.yaml")
	}
}

func TestProjectStore_LoadAll(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	dir2 := t.TempDir()
	setupProjectDir(t, dir2, "proj-b", "Project B")

	dir3 := t.TempDir()

	projects := []*projectspec.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
		{ID: "proj-c", WorkDir: dir3},
	}

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll(projects)

	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	if _, ok := s.Get("proj-a"); !ok {
		t.Fatal("expected proj-a to be loaded")
	}
	if _, ok := s.Get("proj-b"); !ok {
		t.Fatal("expected proj-b to be loaded")
	}
}

// TestProjectStore_LoadAll_IDDriftToExistingProject_DoesNotCorruptOtherProjectsCache
// pins M2's LoadAll sibling (PR-2a codex round-3 review): pre-fix, LoadAll
// dispatched into the plain Load/LoadBareRepo, which cached the freshly-read
// meta under its OWN id BEFORE the mismatch check below ever ran — so if
// proj-a's project.yaml `id:` drifts to collide with an ALREADY-cached,
// completely unrelated proj-b, and proj-b is processed EARLIER in the same
// LoadAll call (candidate order matters here, unlike the single-project
// FetchProject case), proj-a's load would clobber proj-b's just-committed
// cache entry, and the mismatch branch's cleanup (a bare Remove(meta.ID))
// then deleted that just-clobbered entry outright — leaving proj-b entirely
// UNCACHED by the time LoadAll returns, even though proj-b's own source
// directory was never touched. LoadExpectingID/LoadBareRepoExpectingID close
// this by validating BEFORE committing to the shared cache.
func TestProjectStore_LoadAll_IDDriftToExistingProject_DoesNotCorruptOtherProjectsCache(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	dir2 := t.TempDir()
	setupProjectDir(t, dir2, "proj-b", "Project B")

	s := orchestrator.NewProjectStore()

	// proj-b listed BEFORE proj-a: this ordering is what exposes the bug —
	// proj-b must already be correctly cached by the time proj-a's drifted
	// load is processed later in this same call.
	projects := []*projectspec.Project{
		{ID: "proj-b", WorkDir: dir2},
		{ID: "proj-a", WorkDir: dir1},
	}
	if errs := s.LoadAll(projects); len(errs) != 0 {
		t.Fatalf("initial LoadAll: unexpected errors %v", errs)
	}
	if _, ok := s.Get("proj-b"); !ok {
		t.Fatal("test setup: expected proj-b to be cached after the initial LoadAll")
	}

	// Drift proj-a's project.yaml id: to collide with proj-b's already-
	// registered id.
	setupProjectDir(t, dir1, "proj-b", "Project A Renamed")

	errs := s.LoadAll(projects)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error (proj-a's drifted id), got %d: %v", len(errs), errs)
	}

	// The fix: proj-b must still be cached (pre-fix, it would have been
	// deleted outright by proj-a's mismatch-cleanup landing after it).
	bAfter, ok := s.Get("proj-b")
	if !ok {
		t.Fatal("expected proj-b to still be cached after proj-a's reload failed on an id collision — it must not have been deleted as a side effect of proj-a's mismatch cleanup")
	}
	if bAfter.Name != "Project B" {
		t.Errorf("proj-b Meta.Name = %q, want %q (must not have been clobbered by proj-a's drifted metadata, even transiently)", bAfter.Name, "Project B")
	}

	// proj-a's own stale cache entry must have been dropped (unchanged
	// behavior from before this fix — it no longer loads under its OWN
	// registered id) and marked degraded.
	if _, ok := s.Get("proj-a"); ok {
		t.Error("expected proj-a's stale cache entry to be dropped after its id drifted away from its own registered id")
	}
	st := s.Status("proj-a")
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("proj-a Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
}

// TestProjectStore_LoadAll_MissingDirReturnsTypedError pins the
// classification that lets the boid start parent / server wire auto-prune
// stale project DB rows instead of refusing daemon startup. When the
// candidate's WorkDir has no .boid/project.yaml on disk, LoadAll must
// return a *ProjectMissingError (not a plain fmt.Errorf wrap), so callers
// can errors.As it out and decide to delete the row.
//
// This separates the "dir physically gone" case (data-safe to prune) from
// other load failures (parse errors, permission errors) that remain
// fail-fast because they can mask real config bugs.
func TestProjectStore_LoadAll_MissingDirReturnsTypedError(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	// dir2 exists but has no .boid/project.yaml — simulates a project whose
	// directory was wiped (e.g. test artifact under /tmp).
	dir2 := t.TempDir()

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error (proj-b missing), got %d: %v", len(errs), errs)
	}
	var missErr *orchestrator.ProjectMissingError
	if !errors.As(errs[0], &missErr) {
		t.Fatalf("expected *ProjectMissingError for missing dir, got %T: %v", errs[0], errs[0])
	}
	if missErr.ProjectID != "proj-b" {
		t.Errorf("ProjectID = %q, want %q", missErr.ProjectID, "proj-b")
	}
	if missErr.Dir != dir2 {
		t.Errorf("Dir = %q, want %q", missErr.Dir, dir2)
	}
	// errors.Is(fs.ErrNotExist) must still resolve through Unwrap so callers
	// that key off the standard sentinel keep working.
	if !errors.Is(missErr, os.ErrNotExist) {
		t.Errorf("errors.Is(*ProjectMissingError, os.ErrNotExist) = false, want true")
	}
	// The healthy project should still be loaded.
	if _, ok := s.Get("proj-a"); !ok {
		t.Error("proj-a should be loaded despite proj-b being missing")
	}
}

func TestProjectStore_LoadAll_InvalidatesStaleMetaOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	setupProjectDir(t, dir, "proj-a", "Project A")

	s := orchestrator.NewProjectStore()
	if _, err := s.Load(dir); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	if _, ok := s.Get("proj-a"); !ok {
		t.Fatal("expected proj-a to be loaded before reload")
	}

	boidDir := filepath.Join(dir, ".boid")
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte("id: proj-a\nname: [\n"), 0o644); err != nil {
		t.Fatalf("write invalid yaml: %v", err)
	}

	errs := s.LoadAll([]*projectspec.Project{{ID: "proj-a", WorkDir: dir}})
	if len(errs) != 1 {
		t.Fatalf("expected 1 reload error, got %d: %v", len(errs), errs)
	}

	if _, ok := s.Get("proj-a"); ok {
		t.Fatal("stale meta should be invalidated after reload failure")
	}
}

// TestProjectStore_LoadAll_MarksDegradedNotDeleted pins the docs/plans/
// volume-only-daemon.md §論点a on-startup-auto-prune-retirement invariant at
// the ProjectStore level: a project whose project.yaml fails to load (any
// reason — missing dir here, corrupt bare repo covered by
// project_bare_repo_test.go's TestReadProjectMetaFromBareRepo_CorruptRepo)
// must come out of LoadAll in the StatusDegraded state, never removed from
// the caller's candidate list / never causing the store to forget it was
// ever registered. The actual DB row deletion decision lives one layer up
// (internal/server/wire.go); this test only pins what LoadAll itself must
// guarantee: MarkDegraded fires, the project stays queryable via Status.
func TestProjectStore_LoadAll_MarksDegradedNotDeleted(t *testing.T) {
	dir1 := t.TempDir()
	setupProjectDir(t, dir1, "proj-a", "Project A")

	dir2 := t.TempDir() // no .boid/project.yaml — simulates a load failure

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{
		{ID: "proj-a", WorkDir: dir1},
		{ID: "proj-b", WorkDir: dir2},
	})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	st := s.Status("proj-b")
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("proj-b Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
	if st.Message == "" {
		t.Error("expected a non-empty degraded status message for proj-b")
	}

	// proj-a loaded fine and must report ready (the implicit default).
	if st := s.Status("proj-a"); st.State != orchestrator.StatusReady {
		t.Errorf("proj-a Status.State = %q, want %q", st.State, orchestrator.StatusReady)
	}
}

// TestProjectStore_LoadAll_LegacyHostDirMessage pins the migration-path
// guidance docs/plans/volume-only-daemon.md §論点a describes: once
// SetReposRoot is configured, a project whose WorkDir falls outside the
// daemon's bare-repo storage root (and isn't a bare repo itself) is
// recognized as a pre-cutover, host-filesystem registration and gets
// pointed at the new `boid project add <git-url>` form instead of a bare
// "no such file or directory".
func TestProjectStore_LoadAll_LegacyHostDirMessage(t *testing.T) {
	reposRoot := filepath.Join(t.TempDir(), "repos")
	legacyDir := t.TempDir() // outside reposRoot entirely, no project.yaml

	s := orchestrator.NewProjectStore()
	s.SetReposRoot(reposRoot)

	errs := s.LoadAll([]*projectspec.Project{{ID: "proj-legacy", WorkDir: legacyDir}})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	st := s.Status("proj-legacy")
	if st.State != orchestrator.StatusDegraded {
		t.Fatalf("Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
	if !strings.Contains(st.Message, "boid project add <git-url>") {
		t.Errorf("expected legacy-migration guidance in status message, got: %s", st.Message)
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

func TestProjectStore_LoadAll_CorruptBareRepo_MarksDegradedNotDeleted(t *testing.T) {
	// A directory that LOOKS bare (HEAD file + objects dir, so
	// IsBareRepoDir routes LoadAll into LoadBareRepo) but has no real git
	// plumbing underneath — simulates a corrupted bare repo.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir objects: %v", err)
	}

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{{ID: "proj-corrupt", WorkDir: dir}})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}

	st := s.Status("proj-corrupt")
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
	if st.Message == "" {
		t.Error("expected a non-empty degraded status message")
	}
}

// TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_SurvivesReload pins
// docs/plans/workspace-default-project.md PR5's core persistence guarantee
// (§現状の実測5): a project.yaml-less project registered via
// CreateProjectFromGitURL's workspace-default fallback must NOT be
// removed/degraded on the next daemon startup/reload — LoadAll's own
// GitHeadReadFailurePathAbsent classification (the same one
// CreateProjectFromGitURL's fallback uses) now falls back to a synthesized
// meta instead. Pre-PR5, this exact scenario used to Remove the project and
// report 1 error — see git history for the old assertions this replaced.
func TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_SurvivesReload(t *testing.T) {
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
	bare := filepath.Join(root, "repo.git")
	runGitLocal(t, root, "clone", "-q", "--bare", work, bare)

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{{
		ID:          "proj-no-yaml",
		WorkDir:     bare,
		WorkspaceID: "team-a",
		UpstreamURL: fileURLForTest(work),
	}})
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors (project.yaml-less project survives reload), got %d: %v", len(errs), errs)
	}

	st := s.Status("proj-no-yaml")
	if st.State != orchestrator.StatusReady {
		t.Errorf("Status.State = %q, want %q (not degraded)", st.State, orchestrator.StatusReady)
	}

	meta, ok := s.Get("proj-no-yaml")
	if !ok {
		t.Fatal("expected a synthesized meta to be cached after reload")
	}
	if meta.ID != "proj-no-yaml" {
		t.Errorf("meta.ID = %q, want proj-no-yaml", meta.ID)
	}
	// Name is recovered from WorkDir's own basename ("repo", ".git"
	// stripped) — the priority order ahead of UpstreamURL re-derivation
	// (Codex review, PR5 round 1 P1): the bare-repo directory name IS the
	// exact name (explicit --name or derived) CreateProjectFromGitURL used
	// at registration, which UpstreamURL alone cannot always reproduce (see
	// TestProjectStore_LoadAll_MissingProjectYAMLInBareRepo_PreservesExplicitNameAcrossRestart).
	if meta.Name != "repo" {
		t.Errorf("meta.Name = %q, want %q (recovered from WorkDir basename)", meta.Name, "repo")
	}
}

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

	s := orchestrator.NewProjectStore()
	errs := s.LoadAll([]*projectspec.Project{{
		ID:          "proj-explicit-name",
		WorkDir:     bare,
		WorkspaceID: "team-a",
		UpstreamURL: fileURLForTest(work),
	}})
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %v", len(errs), errs)
	}

	meta, ok := s.Get("proj-explicit-name")
	if !ok {
		t.Fatal("expected a synthesized meta to be cached after reload")
	}
	if meta.Name != "custom-name" {
		t.Errorf("meta.Name = %q, want %q (explicit --name preserved across restart via WorkDir basename)", meta.Name, "custom-name")
	}
}

// fileURLForTest turns a local filesystem path into a "file://" URL, mirroring
// internal/api's fileURL test helper (kept package-local here to avoid a
// cross-package test-only dependency).
func fileURLForTest(dir string) string { return "file://" + dir }

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
