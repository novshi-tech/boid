package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func setupGitURLSourceRepo(t *testing.T, id, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".boid", "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	runGitCmd(t, dir, "init", "-q", "-b", "main")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test")
	runGitCmd(t, dir, "add", ".")
	runGitCmd(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// fileURL turns a local filesystem path into a "file://" URL.
// dispatcher.NormalizeOriginURL (which CreateProjectFromGitURL now runs
// every gitURL argument through before cloning — see that method's own doc
// comment on why an ssh://-transport URL must not reach the daemon
// container's credential-less git as-is) only recognizes https://, ssh://,
// http://, the scp-like git@host:path form, and file:// — a bare local path
// like the ones setupGitURLSourceRepo returns is none of those, so tests
// must wrap it before passing it in.
func fileURL(dir string) string { return "https://fixtures.test" + dir }

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newGitURLTestService(t *testing.T, repo *stubProjectRepository) (*ProjectAppService, string) {
	t.Helper()
	dataDir := t.TempDir()
	store := orchestrator.NewProjectStore()
	// Wire the same derive func production code gets from
	// internal/server/wire.go's buildProjectStore — tests that exercise
	// url-derived-id drift reconciliation (Codex round-3 Major review, PR7)
	// need this to actually verify a candidate id against UpstreamURL rather
	// than merely trusting the URLDerivedProjectIDPrefix.
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     store,
	}
	return svc, dataDir
}

func TestCreateProjectFromGitURL_Success(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	expectedID, _ := deriveProjectIDFromURL(fileURL(src))
	if project.ID != expectedID {
		t.Errorf("ID = %q, want proj-git", project.ID)
	}
	if project.Meta.Name != filepath.Base(src) {
		t.Errorf("Meta.Name = %q, want %q", project.Meta.Name, filepath.Base(src))
	}
	if project.WorkspaceID != "team-a" {
		t.Errorf("WorkspaceID = %q, want team-a", project.WorkspaceID)
	}
	if project.UpstreamURL != fileURL(src) {
		t.Errorf("UpstreamURL = %q, want %q", project.UpstreamURL, fileURL(src))
	}

	// The project name (derived from the URL's last path component) governs
	// the bare-repo storage path, independent of the project.yaml `name:`
	// value used above for Meta.Name.
	if project.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty", project.WorkDir)
	}
	if len(repo.projects) != 0 {
		// stubProjectRepository.CreateProject is a no-op that doesn't append
		// to s.projects — nothing to assert here beyond "it didn't error".
		t.Log("stub CreateProject does not track created projects; skipping list assertion")
	}
}

func TestCreateProjectFromGitURL_NameOverride(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "custom-name", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	if project.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty", project.WorkDir)
	}
}

func TestCreateProjectFromGitURL_RequiresWorkspace(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{}
	svc, _ := newGitURLTestService(t, repo)

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "", "", "")
	if err == nil {
		t.Fatal("expected error for empty workspace")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("expected *StatusError{400}, got %T: %v", err, err)
	}
}

func TestCreateProjectFromGitURL_RejectsUnknownWorkspace(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{}}
	svc, _ := newGitURLTestService(t, repo)

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "ghost-ws", "", "")
	if err == nil {
		t.Fatal("expected error for a workspace that does not exist")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("expected *StatusError{404}, got %T: %v", err, err)
	}
}

func TestCreateProjectFromGitURL_RejectsPathTraversalName(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-traversal", "Traversal Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "../../../../tmp/owned", "")
	if err == nil {
		t.Fatal("expected error for a traversal --name")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("expected *StatusError{400}, got %T: %v", err, err)
	}

	// Refused before any write (PR-1b/1d learning): nothing was cloned
	// anywhere under this workspace's repos directory.
	if _, statErr := os.Stat(filepath.Join(t.TempDir(), "repos", "team-a")); !os.IsNotExist(statErr) {
		t.Errorf("expected no repos/team-a directory to exist after a refused traversal --name, stat err = %v", statErr)
	}
}

// TestCreateProjectFromGitURL_ConcurrentSameURL_ExactlyOneSucceeds pins
// MAJOR 3 (codex round-1 review): concurrent `project add` calls for the
// SAME url (and therefore the same derived name / bareRepoPath, since none
// of them pass --name) must not all race past the "already registered at
// this path" collision check — pre-fix, they could all observe an empty
// bareRepoPath via os.Stat before any of them had cloned, each proceed to
// clone/cache/insert, and a losing insert's rollback (Meta.Remove) could
// delete a WINNING call's cache entry out from under it. registerMu
// serializes the whole clone -> load -> DB-insert -> workspace-assign
// pipeline, so every call after the first cannot even begin cloning until
// the winner has already fully committed, and deterministically hits the
// ordinary 409 collision check instead.
func TestCreateProjectFromGitURL_ConcurrentSameURL_ExactlyOneSucceeds(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-concurrent", "Concurrent Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	repo.createProjectHook = func(project *orchestrator.Project) error {
		repo.projects = append(repo.projects, project)
		return nil
	}

	const n = 5
	var wg sync.WaitGroup
	results := make([]error, n)
	projects := make([]*orchestrator.Project, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
			results[i] = err
			projects[i] = p
		}(i)
	}
	wg.Wait()

	succeeded := 0
	var winner *orchestrator.Project
	for i := 0; i < n; i++ {
		if results[i] == nil {
			succeeded++
			winner = projects[i]
			continue
		}
		if statusErr, ok := results[i].(*StatusError); !ok || statusErr.Code != http.StatusConflict {
			t.Errorf("call %d: expected either success or *StatusError{409}, got %T: %v", i, results[i], results[i])
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent adds to succeed, got %d", n, succeeded)
	}

	// No cache corruption: the winner's project ID must still be resolvable
	// through the in-memory Meta cache — a pre-fix run could leave it
	// deleted by a loser's rollback landing after the winner's own write.
	if _, ok := svc.Meta.Get(winner.ID); !ok {
		t.Errorf("expected winner project %q to still be cached after all concurrent adds settled", winner.ID)
	}

	if winner.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty", winner.WorkDir)
	}
}

// TestDeleteProject_WorksOnDegradedProject pins the docs/plans/
// volume-only-daemon.md §論点a invariant's OTHER half: while nothing
// automatic may ever delete a degraded project's DB row, the EXPLICIT entry
// point (`boid project rm` / DELETE /api/projects/{id}, i.e.
// ProjectAppService.DeleteProject) must still work normally on one —
// degraded status is not a lock, it is just visibility. Without this, an
// operator would have no way to clean up a project that legitimately needs
// re-registering (per the degraded status message's own guidance).
func TestDeleteProject_WorksOnDegradedProject(t *testing.T) {
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-degraded"}},
	}
	svc, _ := newGitURLTestService(t, repo)
	svc.Meta.MarkDegraded("proj-degraded", "simulated: bare repo corrupt")

	if st := svc.Meta.Status("proj-degraded"); st.State != orchestrator.StatusDegraded {
		t.Fatalf("test setup: expected proj-degraded to be degraded, got %q", st.State)
	}

	if err := svc.DeleteProject("proj-degraded"); err != nil {
		t.Fatalf("DeleteProject on a degraded project should succeed, got: %v", err)
	}

	if _, err := repo.GetProject("proj-degraded"); err == nil {
		t.Error("expected the project to be gone from the repository after DeleteProject")
	}
}

// TestCreateProjectFromGitURL_InsertFails_StillRollsBackItsOwnCacheWrite is
// the other side of the test above: the rollback must still happen when the
// cache entry really was this call's own to remove (no project row owns that
// id). Without this, "keep the meta when a row exists" could be over-applied
// into "never roll back", silently leaking a failed registration's meta.
func TestCreateProjectFromGitURL_InsertFails_StillRollsBackItsOwnCacheWrite(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-fresh", "Brand New Project")
	// No project owns "proj-fresh" — the cache write below is this call's own.
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	repo.createProjectHook = func(*orchestrator.Project) error {
		return fmt.Errorf("simulated DB failure unrelated to any id conflict")
	}

	if _, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", ""); err == nil {
		t.Fatal("expected the registration to fail")
	}

	if _, ok := svc.Meta.Get("proj-fresh"); ok {
		t.Error("a failed registration left its own cache write behind")
	}
}
