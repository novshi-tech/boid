package api

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
func fileURL(dir string) string { return "file://" + dir }

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
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     store,
		DataDir:  dataDir,
		CloneBareRepo: func(ctx context.Context, url, dest, namespace string) error {
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

	project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	if project.ID != "proj-git" {
		t.Errorf("ID = %q, want proj-git", project.ID)
	}
	if project.Meta.Name != "Git URL Project" {
		t.Errorf("Meta.Name = %q, want %q", project.Meta.Name, "Git URL Project")
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

	project, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "custom-name")
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

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "", "")
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

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "ghost-ws", "")
	if err == nil {
		t.Fatal("expected error for a workspace that does not exist")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusNotFound {
		t.Fatalf("expected *StatusError{404}, got %T: %v", err, err)
	}
}

// TestCreateProjectFromGitURL_MissingProjectYAML_RollsBack pins the
// synchronous-validation contract (docs/plans/volume-only-daemon.md §論点a
// unresolved-point recommendation, implemented in CreateProjectFromGitURL's
// own doc comment): a clone that succeeds but has no .boid/project.yaml
// must fail the WHOLE registration — no DB row, no bare repo left behind —
// rather than leaving a half-added degraded project.
func TestCreateProjectFromGitURL_MissingProjectYAML_RollsBack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("no project.yaml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, src, "init", "-q", "-b", "main")
	runGitCmd(t, src, "config", "user.email", "test@example.com")
	runGitCmd(t, src, "config", "user.name", "Test")
	runGitCmd(t, src, "add", ".")
	runGitCmd(t, src, "commit", "-q", "-m", "initial")

	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, dataDir := newGitURLTestService(t, repo)

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "")
	if err == nil {
		t.Fatal("expected error for a source repo with no .boid/project.yaml")
	}

	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", filepath.Base(src))
	if _, statErr := os.Stat(wantBarePath); !os.IsNotExist(statErr) {
		t.Errorf("expected the bare repo at %s to be rolled back (removed), stat err = %v", wantBarePath, statErr)
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

	_, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "taken")
	if err == nil {
		t.Fatal("expected error for a bare-repo path collision")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusConflict {
		t.Fatalf("expected *StatusError{409}, got %T: %v", err, err)
	}
}

func TestFetchProject_UpdatesAfterRemoteChange(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "")
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
	if updated.Meta.Name != "Renamed Project" {
		t.Errorf("Meta.Name = %q, want %q", updated.Meta.Name, "Renamed Project")
	}
}

// TestFetchProject_UnreachableRemote_MarksDegradedNotDeleted pins the
// docs/plans/volume-only-daemon.md §論点a on-startup-auto-prune-retirement
// invariant for the fetch path specifically ("remote becomes unreachable
// → degraded, not deleted"): a fetch failure must never delete id's DB row
// or its previously-cached Meta — only mark it degraded and return an
// error.
func TestFetchProject_UnreachableRemote_MarksDegradedNotDeleted(t *testing.T) {
	src := setupGitURLSourceRepo(t, "proj-git", "Git URL Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	svc, _ := newGitURLTestService(t, repo)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fileURL(src), "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = []*orchestrator.Project{created}

	// Make the remote unreachable by deleting the source repo outright.
	if err := os.RemoveAll(src); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	if _, err := svc.FetchProject(context.Background(), created.ID); err == nil {
		t.Fatal("expected an error fetching from a now-unreachable remote")
	}

	// The DB row must still be there (stub repository never had it deleted).
	if _, getErr := repo.GetProject(created.ID); getErr != nil {
		t.Fatalf("expected project row to still exist after fetch failure, got: %v", getErr)
	}

	st := svc.Meta.Status(created.ID)
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("Status.State = %q, want %q", st.State, orchestrator.StatusDegraded)
	}
	if !strings.Contains(st.Message, "fetch failed") {
		t.Errorf("expected a fetch-failure message, got: %s", st.Message)
	}

	// project.yaml is still readable from the bare repo's last-known HEAD —
	// LoadBareRepo (a plain Load, not a fetch) must still succeed.
	if _, loadErr := svc.Meta.LoadBareRepo(created.WorkDir); loadErr != nil {
		t.Errorf("expected the bare repo's cached HEAD to still be readable: %v", loadErr)
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
