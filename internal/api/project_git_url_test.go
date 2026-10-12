package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// setupGitURLSourceRepo builds a plain local git repo with a committed
// .boid/project.yaml, usable as the "git URL" argument to
// CreateProjectFromGitURL — a local filesystem path is a valid git URL, so
// these tests exercise the real dispatcher.CloneBareRepo/FetchBareRepo code
// (wired the same way internal/server/wire.go wires them) end-to-end
// without a fake forge.
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

// newGitURLTestService builds a ProjectAppService wired with the real
// orchestrator.ProjectStore (so LoadBareRepo/Status/MarkDegraded all behave
// like production) and real dispatcher.CloneBareRepo/FetchBareRepo (no
// credential provider — nil creds proceed unauthenticated, matching the
// local-path source repos these tests clone from).
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
		DataDir:  dataDir,
		CloneBareRepo: func(ctx context.Context, url, dest, namespace string) error {
			if strings.HasPrefix(url, "https://fixtures.test/") {
				url = "file://" + strings.TrimPrefix(url, "https://fixtures.test")
			}
			return dispatcher.CloneBareRepo(ctx, url, dest, nil, namespace)
		},
		FetchBareRepo: func(ctx context.Context, bareRepoPath, namespace string) error {
			return dispatcher.FetchBareRepo(ctx, bareRepoPath, nil, namespace)
		},
	}
	return svc, dataDir
}

func TestCreateProjectFromGitURL_Success(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, dataDir := newGitURLTestService(t, repo)

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
	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", filepath.Base(src))
	if project.WorkDir != wantBarePath {
		t.Errorf("WorkDir = %q, want %q", project.WorkDir, wantBarePath)
	}
	if !orchestrator.IsBareRepoDir(project.WorkDir) {
		t.Errorf("expected a bare repo at %s", project.WorkDir)
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
	svc, dataDir := newGitURLTestService(t, repo)

	project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "custom-name", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", "custom-name")
	if project.WorkDir != wantBarePath {
		t.Errorf("WorkDir = %q, want %q", project.WorkDir, wantBarePath)
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

// TestCreateProjectFromGitURL_NameCollision_Refuses pins the "reject BEFORE
// any DB write" pattern (PR-1b/1d learning cited in this method's doc
// comment): a bare-repo directory already occupying the computed path
// refuses the whole call before any clone/DB mutation is attempted.
func TestCreateProjectFromGitURL_NameCollision_Refuses(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, dataDir := newGitURLTestService(t, repo)

	barePath := orchestrator.BareRepoPath(dataDir, "team-a", "taken")
	if err := os.MkdirAll(barePath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "taken", "")
	if err == nil {
		t.Fatal("expected error for a bare-repo path collision")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusConflict {
		t.Fatalf("expected *StatusError{409}, got %T: %v", err, err)
	}
}

func TestFetchProjectIgnoresRepositoryConfiguration(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	// Update project.yaml on the source and commit — FetchProject must pick
	// up the new name after `git fetch --all` + reload.
	if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte("id: proj-git\nname: Renamed Project\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, src, "add", ".")
	runGitCmd(t, src, "commit", "-q", "-m", "rename")

	updated, err := svc.FetchProject(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("FetchProject: %v", err)
	}
	if updated.Meta.Name != created.Meta.Name {
		t.Errorf("Meta.Name = %q, want %q", updated.Meta.Name, created.Meta.Name)
	}
}

// TestCreateProjectFromGitURL_RejectsPathTraversalName pins BLOCKER 2 (codex
// round-1 review) at the ProjectAppService integration level — see
// orchestrator.TestSafeBareRepoPath_RejectsTraversal for the underlying unit
// test — proving CreateProjectFromGitURL actually routes through
// orchestrator.SafeBareRepoPath rather than the raw (traversal-unsafe)
// BareRepoPath.
func TestCreateProjectFromGitURL_RejectsPathTraversalName(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-traversal", "Traversal Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, dataDir := newGitURLTestService(t, repo)

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
	if _, statErr := os.Stat(filepath.Join(dataDir, "repos", "team-a")); !os.IsNotExist(statErr) {
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
	svc, dataDir := newGitURLTestService(t, repo)

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

	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", filepath.Base(src))
	if winner.WorkDir != wantBarePath {
		t.Errorf("winner.WorkDir = %q, want %q", winner.WorkDir, wantBarePath)
	}
	if !orchestrator.IsBareRepoDir(winner.WorkDir) {
		t.Errorf("expected a healthy bare repo at %s after concurrent adds settled", winner.WorkDir)
	}
}

// TestCreateProjectFromGitURL_AssignFailsAndBareRepoRemovalFails_PreservesRow
// pins M1 (PR-2a codex round-3 review): the workspace-assignment rollback
// path used to delete the DB row FIRST and only THEN attempt os.RemoveAll on
// the bare repo — the opposite order from DeleteProject's own MAJOR 3 fix
// (codex round-2, see TestDeleteProject_BareRepoRemovalFails_PreservesRow
// below). Concrete failure this closes: AssignWorkspaceIfExists fails (here,
// simulated directly) AND the bare-repo directory cannot be removed
// (permission denied). Pre-fix, the DB row was already gone by the time the
// removal failure was discovered — an orphaned bare-repo directory with no
// row left to retry cleanup through, and re-adding the same name 409s
// forever (this method's own pre-flight "already registered at this path"
// check). Post-fix, os.RemoveAll runs FIRST; on its failure the DB row (and
// cached Meta) must survive so the operator can fix the permission issue and
// retry `boid project rm`.
func TestCreateProjectFromGitURL_AssignFailsAndBareRepoRemovalFails_PreservesRow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	src := setupGitURLSourceRepo(t, "proj-assignfail-rmfail", "Assign+RM Fail Project")
	id, _ := deriveProjectIDFromURL(fileURL(src))
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	// Unlike most tests in this file, this one needs the DB row to actually
	// exist after CreateProjectFromGitURL's own CreateProject call (the
	// stub's default CreateProject is a no-op — see its own doc comment) so
	// GetProject below can genuinely observe whether it survived a failed
	// rollback.
	repo.createProjectHook = func(project *orchestrator.Project) error {
		repo.projects = append(repo.projects, project)
		return nil
	}

	// Force the atomic assign step to fail, so the rollback path is
	// exercised (WorkspaceExists already passed above, unaffected by this).
	repo.assignWorkspaceIfExistsErr = fmt.Errorf("simulated assign failure")

	// Make the freshly cloned bare repo directory itself unwritable, right
	// after the clone completes, so the rollback's os.RemoveAll cannot
	// unlink anything inside it — same technique
	// TestDeleteProject_BareRepoRemovalFails_PreservesRow uses.
	realClone := svc.CloneBareRepo
	var barePath string
	svc.CloneBareRepo = func(ctx context.Context, url, dest, namespace string) error {
		if err := realClone(ctx, url, dest, namespace); err != nil {
			return err
		}
		barePath = dest
		if err := os.Chmod(dest, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		return nil
	}
	t.Cleanup(func() {
		if barePath != "" {
			_ = os.Chmod(barePath, 0o755)
		}
	})

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err == nil {
		t.Fatal("expected an error when both workspace assignment and bare-repo rollback fail")
	}

	// The fix: the DB row must survive a failed rollback removal.
	if _, getErr := repo.GetProject(id); getErr != nil {
		t.Errorf("expected the project row to survive a failed bare-repo rollback removal, got: %v", getErr)
	}
	// The bare repo directory itself must still be there too (the removal
	// genuinely failed, it was not silently skipped).
	if barePath == "" {
		t.Fatal("test setup: CloneBareRepo hook never ran")
	}
	if _, statErr := os.Stat(barePath); statErr != nil {
		t.Errorf("expected the bare repo at %s to still exist after a failed rollback removal: %v", barePath, statErr)
	}
	// Cached Meta must survive as well — nothing was cleaned up given the
	// bare repo itself could not be removed.
	if _, ok := svc.Meta.Get(id); !ok {
		t.Error("expected cached Meta to survive a failed bare-repo rollback removal")
	}

	// Fix the permission issue and retry `boid project rm` — must now
	// succeed, same recovery path DeleteProject's own MAJOR 3 fix restored.
	if err := os.Chmod(barePath, 0o755); err != nil {
		t.Fatalf("chmod restore: %v", err)
	}
	if err := svc.DeleteProject(id); err != nil {
		t.Fatalf("retry DeleteProject after fixing permissions: %v", err)
	}
	if _, getErr := repo.GetProject(id); getErr == nil {
		t.Error("expected the project row to be gone after the successful retry")
	}
}

// TestDeleteProject_RemovesBareRepo_AllowsReRegistration pins MAJOR 1 (codex
// round-1 review): DeleteProject must remove the daemon-managed bare repo
// directory, not just the DB row — otherwise re-adding the SAME URL/name
// after `boid project rm` always 409s (CreateProjectFromGitURL's pre-flight
// "already registered at this path" check), making the documented degraded-
// recovery procedure ("rm the project, re-add it") ineffective.
func TestDeleteProject_RemovesBareRepo_AllowsReRegistration(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-rm", "RM Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	if err := svc.DeleteProject(created.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, statErr := os.Stat(created.WorkDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the bare repo at %s to be removed after DeleteProject, stat err = %v", created.WorkDir, statErr)
	}

	// Re-add the SAME url/name — must succeed. Pre-fix, this always 409'd
	// because the old bare repo directory was left occupying the computed
	// path.
	recreated, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("re-CreateProjectFromGitURL after rm: %v", err)
	}
	if recreated.WorkDir != created.WorkDir {
		t.Errorf("WorkDir = %q, want the same path as before removal %q", recreated.WorkDir, created.WorkDir)
	}
}

// TestFetchProject_CorruptManagedRepo_DegradesNotLegacyRejected pins MAJOR 2
// (codex round-1 review): a managed (git-URL registered) project whose LOCAL
// bare repo loses its HEAD file (disk corruption, an operator `rm`ing the
// wrong thing) must degrade via FetchProject, not get misclassified as a
// legacy host-dir project and rejected with a plain 400 that never records
// the failure anywhere.
func TestFetchProject_CorruptManagedRepo_DegradesNotLegacyRejected(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-corrupt", "Corrupt Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	// Corrupt the LOCAL bare repo (not the source) by removing its HEAD
	// file — orchestrator.IsBareRepoDir now reports false for it, exactly
	// the pre-fix trigger for the legacy-rejection misclassification.
	if err := os.Remove(filepath.Join(created.WorkDir, "HEAD")); err != nil {
		t.Fatalf("remove HEAD: %v", err)
	}
	if orchestrator.IsBareRepoDir(created.WorkDir) {
		t.Fatal("test setup: expected IsBareRepoDir to report false after removing HEAD")
	}

	_, err = svc.FetchProject(context.Background(), created.ID)
	if err == nil {
		t.Fatal("expected an error fetching a corrupt managed bare repo")
	}
	statusErr, ok := err.(*StatusError)
	if !ok {
		t.Fatalf("expected *StatusError, got %T: %v", err, err)
	}
	if strings.Contains(statusErr.Message, "legacy host-dir project") {
		t.Errorf("expected the corrupt MANAGED repo NOT to be misclassified as legacy, got message: %s", statusErr.Message)
	}
	if statusErr.Code == http.StatusBadRequest {
		t.Errorf("expected a non-400 status for a corrupt managed repo (400 is the legacy-rejection code), got %d: %s", statusErr.Code, statusErr.Message)
	}

	st := svc.Meta.Status(created.ID)
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("Status.State = %q, want %q (a corrupt managed repo must degrade, not just 400)", st.State, orchestrator.StatusDegraded)
	}
}

func TestFetchProject_LegacyProjectRejected(t *testing.T) {
	repo := &stubProjectRepository{
		projects: []*orchestrator.Project{{ID: "proj-legacy", WorkDir: t.TempDir()}},
	}
	svc, _ := newGitURLTestService(t, repo)

	_, err := svc.FetchProject(context.Background(), "proj-legacy")
	if err == nil {
		t.Fatal("expected error fetching a non-bare-repo (legacy) project")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("expected *StatusError{400}, got %T: %v", err, err)
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

// TestCreateProjectFromGitURL_WorkspaceDeletedDuringClone_NoDanglingAssignment
// pins MAJOR 2 (PR-2a codex round-2 review): workspace existence is checked
// ONCE, before the (potentially slow) clone — and the pre-fix code
// committed the assignment afterward via the plain SetProjectWorkspace
// (no existence re-check of its own), so a workspace removed during that
// window produced a SUCCESSFUL registration silently pointing at a
// workspace_id with no corresponding workspaces row (project_workspaces has
// no FK to workspaces). AssignWorkspaceIfExists closes the window by
// re-checking atomically, in the same call, at commit time. This test
// simulates the race deterministically (via the CloneBareRepo hook, which
// runs at exactly the point a slow clone would leave the window open)
// rather than with real goroutines, matching this file's existing
// concurrency-test conventions.
func TestCreateProjectFromGitURL_WorkspaceDeletedDuringClone_NoDanglingAssignment(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-race", "Race Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, dataDir := newGitURLTestService(t, repo)

	realClone := svc.CloneBareRepo
	svc.CloneBareRepo = func(ctx context.Context, url, dest, namespace string) error {
		if err := realClone(ctx, url, dest, namespace); err != nil {
			return err
		}
		// Simulate a concurrent `boid workspace rm team-a` landing in the
		// window between the pre-flight WorkspaceExists check (already
		// passed, above) and the post-clone assign this call is about to
		// reach.
		repo.existingWorkspaces["team-a"] = false
		return nil
	}

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err == nil {
		t.Fatal("expected an error when the workspace is removed during clone")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("expected *StatusError{404}, got %T: %v", err, err)
	}

	// The call that failed must have been the atomic AssignWorkspaceIfExists
	// (not the old unconditional SetProjectWorkspace, which the stub always
	// succeeds regardless of existence — see its own doc comment).
	if len(repo.assignWorkspaceIfExistsCalls) == 0 {
		t.Fatal("expected AssignWorkspaceIfExists to have been called")
	}

	// No dangling assignment: the whole registration must have been rolled
	// back — no project row left, no bare repo directory left on disk.
	if _, getErr := repo.GetProject("proj-race"); getErr == nil {
		t.Error("expected no dangling project row after a failed workspace assignment")
	}
	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", filepath.Base(src))
	if _, statErr := os.Stat(wantBarePath); !os.IsNotExist(statErr) {
		t.Errorf("expected the bare repo at %s to be rolled back (removed), stat err = %v", wantBarePath, statErr)
	}
}

// TestDeleteProject_BareRepoRemovalFails_PreservesRow pins MAJOR 3 (PR-2a
// codex round-2 review): the pre-fix DeleteProject removed the DB row
// FIRST and only logged an os.RemoveAll failure as a warning — so a
// read-only/permission-failing repo directory stayed on disk forever
// (nothing left to retry through, since the row naming it was already
// gone) while `boid project rm` still reported SUCCESS. Removing the repo
// BEFORE the DB row means a removal failure now fails the whole call with
// the row (and cached Meta) intact, so the delete can simply be retried
// once the underlying issue is fixed.
func TestDeleteProject_BareRepoRemovalFails_PreservesRow(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("permission-bit test assumes POSIX permission semantics")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permission bits are not enforced")
	}

	src := setupGitURLSourceRepo(t, "proj-rmfail", "RM Fail Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	// Make the bare repo directory itself unwritable so os.RemoveAll cannot
	// unlink anything inside it (removing an entry requires write
	// permission on the CONTAINING directory).
	if err := os.Chmod(created.WorkDir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(created.WorkDir, 0o755) })

	if err := svc.DeleteProject(created.ID); err == nil {
		t.Fatal("expected DeleteProject to fail when the bare repo cannot be removed")
	}

	if _, getErr := repo.GetProject(created.ID); getErr != nil {
		t.Errorf("expected the project row to survive a failed bare-repo removal, got: %v", getErr)
	}
	if _, ok := svc.Meta.Get(created.ID); !ok {
		t.Error("expected cached Meta to survive a failed bare-repo removal")
	}

	// Fix the permission issue and retry — must now succeed.
	if err := os.Chmod(created.WorkDir, 0o755); err != nil {
		t.Fatalf("chmod restore: %v", err)
	}
	if err := svc.DeleteProject(created.ID); err != nil {
		t.Fatalf("retry DeleteProject after fixing permissions: %v", err)
	}
	if _, getErr := repo.GetProject(created.ID); getErr == nil {
		t.Error("expected the project row to be gone after the successful retry")
	}
}

// TestProjectMu_SerializesCreateProjectFromGitURL_And_DeleteProject pins
// MAJOR 1's sibling half (PR-2a codex round-2 review): DeleteProject did
// not hold registerMu (renamed projectMu — its scope now spans every
// project create/delete entry point) at all, so it could run its
// get -> DB-delete -> cache-remove -> RemoveAll sequence fully interleaved
// with an in-flight CreateProjectFromGitURL — see projectMu's doc comment
// for the concrete "successful registration whose DB row a concurrent
// delete already removed out from under it" scenario this closes. This
// test pauses CreateProjectFromGitURL mid-clone (while it holds projectMu)
// and proves a concurrent DeleteProject is genuinely blocked until the
// registration finishes, rather than running straight through — the same
// pattern internal/api's TestCreateProject_BlockedByAssign already uses for
// the s.mu workspace-cache lock.
func TestProjectMu_SerializesCreateProjectFromGitURL_And_DeleteProject(t *testing.T) {
	cloneStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once

	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	dataDir := t.TempDir()
	meta := &createProjectMetaStore{meta: &orchestrator.ProjectMeta{ID: "proj-race-lock"}}
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     meta,
		DataDir:  dataDir,
		CloneBareRepo: func(ctx context.Context, url, dest, namespace string) error {
			once.Do(func() {
				close(cloneStarted)
				<-proceed
			})
			return nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var createErr error
	go func() {
		defer wg.Done()
		_, createErr = svc.CreateProjectFromGitURL(context.Background(), "https://example.invalid/owner/proj-race-lock.git", "team-a", "", "")
	}()

	select {
	case <-cloneStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("CreateProjectFromGitURL never reached its clone step")
	}

	// Mirrors it having already been inserted by the time a concurrent
	// DeleteProject would resolve it via GetProject — safe to mutate
	// without synchronization, since the create goroutine is parked on
	// <-proceed and never touches repo.projects itself.
	repo.projects = append(repo.projects, &orchestrator.Project{ID: "proj-race-lock", WorkDir: "/fake/bare/repo"})

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.DeleteProject("proj-race-lock")
	}()

	select {
	case <-deleteDone:
		t.Fatal("DeleteProject completed while CreateProjectFromGitURL was still mid-clone — projectMu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: DeleteProject is blocked on projectMu.
	}

	close(proceed) // let CreateProjectFromGitURL finish and release projectMu.
	wg.Wait()
	if createErr != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", createErr)
	}

	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteProject never completed after CreateProjectFromGitURL released projectMu")
	}
}

// TestProjectMu_SerializesFetchProject_And_DeleteProject pins M3 (PR-2a
// codex round-3 review): FetchProject did not hold projectMu at all, so its
// get -> git-fetch -> reload -> cache-commit sequence could run fully
// interleaved with an in-flight DeleteProject for the SAME project — a
// concurrent delete-then-re-registration landing in that window could leave
// FetchProject returning a Project object combining the OLD DB fields it
// read before the race with the NEW bare repo's metadata loaded after it.
// This test pauses FetchProject mid-fetch (while it holds projectMu) and
// proves a concurrent DeleteProject is genuinely blocked until the fetch
// finishes — the same pattern
// TestProjectMu_SerializesCreateProjectFromGitURL_And_DeleteProject above
// already uses, confirming FetchProject either completes atomically or
// cleanly errors, with no half-old/half-new state ever observable.
func TestProjectMu_SerializesFetchProject_And_DeleteProject(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-fetch-lock", "Fetch Lock Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	fetchStarted := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once

	realFetch := svc.FetchBareRepo
	svc.FetchBareRepo = func(ctx context.Context, bareRepoPath, namespace string) error {
		once.Do(func() {
			close(fetchStarted)
			<-proceed
		})
		return realFetch(ctx, bareRepoPath, namespace)
	}

	fetchDone := make(chan error, 1)
	go func() {
		_, ferr := svc.FetchProject(context.Background(), created.ID)
		fetchDone <- ferr
	}()

	select {
	case <-fetchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("FetchProject never reached its fetch step")
	}

	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- svc.DeleteProject(created.ID)
	}()

	select {
	case <-deleteDone:
		t.Fatal("DeleteProject completed while FetchProject was still mid-fetch — projectMu did not serialize them")
	case <-time.After(100 * time.Millisecond):
		// expected: DeleteProject is blocked on projectMu.
	}

	close(proceed) // let FetchProject finish and release projectMu.

	select {
	case ferr := <-fetchDone:
		if ferr != nil {
			t.Fatalf("FetchProject: %v", ferr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchProject never completed after being unblocked")
	}

	select {
	case derr := <-deleteDone:
		if derr != nil {
			t.Fatalf("DeleteProject: %v", derr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DeleteProject never completed after FetchProject released projectMu")
	}

	// No half-state: DeleteProject only ever ran strictly after FetchProject
	// finished (never interleaved with it), so the project is now cleanly
	// gone — both the DB row and the bare repo directory.
	if _, getErr := repo.GetProject(created.ID); getErr == nil {
		t.Error("expected the project row to be gone after DeleteProject")
	}
	if _, statErr := os.Stat(created.WorkDir); !os.IsNotExist(statErr) {
		t.Errorf("expected the bare repo at %s to be removed after DeleteProject, stat err = %v", created.WorkDir, statErr)
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
