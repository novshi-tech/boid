package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// setupNoYAMLSourceRepo builds a plain local git repo with NO
// .boid/project.yaml at all — the project.yaml-less registration source
// docs/plans/workspace-default-project.md PR5 targets.
func setupNoYAMLSourceRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("no project.yaml here"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, dir, "init", "-q", "-b", "main")
	runGitCmd(t, dir, "config", "user.email", "test@example.com")
	runGitCmd(t, dir, "config", "user.name", "Test")
	runGitCmd(t, dir, "add", ".")
	runGitCmd(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// TestDeriveProjectIDFromURL_Deterministic_And_GitSuffixInvariant pins 決定3:
// the hash input is the repo SLUG (host/owner/repo, ".git" stripped), not
// the raw normalized URL — "https://host/o/r" and "https://host/o/r.git"
// must derive the SAME id (fable review M1's dedup concern).
func TestDeriveProjectIDFromURL_Deterministic_And_GitSuffixInvariant(t *testing.T) {
	id1, err := deriveProjectIDFromURL("https://github.com/foo/bar.git")
	if err != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", err)
	}
	id2, err := deriveProjectIDFromURL("https://github.com/foo/bar")
	if err != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", err)
	}
	if id1 != id2 {
		t.Errorf("id with .git = %q, id without .git = %q; want equal (same repo slug)", id1, id2)
	}
	if len(id1) != len(noYAMLProjectIDPrefix)+16 {
		t.Errorf("id length = %d, want %d (prefix + 16 hex chars)", len(id1), len(noYAMLProjectIDPrefix)+16)
	}

	other, err := deriveProjectIDFromURL("https://github.com/foo/other.git")
	if err != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", err)
	}
	if other == id1 {
		t.Error("different repos derived the same id")
	}
}

// TestDeriveProjectIDFromURL_FileURL_Errors pins the documented file://
// limitation (決定3): a file:// URL cannot be slugified, so no id can be
// derived — this is a pre-existing constraint of the same normalization
// gitgateway grants already accept, not a new gap.
func TestDeriveProjectIDFromURL_FileURL_Errors(t *testing.T) {
	if _, err := deriveProjectIDFromURL("file:///tmp/some/repo"); err == nil {
		t.Fatal("expected an error deriving an id from a file:// URL")
	}
}

// newNoYAMLTestService is newGitURLTestService plus a wired stubWorkspaceStore
// (docs/plans/workspace-default-project.md PR5's registration path reads
// s.Workspaces to find the workspace's default project definition).
func newNoYAMLTestService(t *testing.T, repo *stubProjectRepository, ws *stubWorkspaceStore) (*ProjectAppService, string) {
	t.Helper()
	svc, dataDir := newGitURLTestService(t, repo)
	svc.Workspaces = ws
	return svc, dataDir
}

// fakeHTTPSGitURL is a syntactically valid https:// git URL (unique per test
// via t.Name()) used in place of a real forge URL. 決定3 requires the id
// derivation's hash input to be a slugifiable URL — unlike the rest of this
// package's git-URL tests, which clone from a real local "file://" source
// repo (a file:// URL cannot be slugified, by design — see
// TestDeriveProjectIDFromURL_FileURL_Errors), the no-project.yaml success
// path needs an https:// argument. CloneBareRepo/FetchBareRepo are overridden
// below to actually clone/fetch from the real local source directory,
// ignoring this URL string entirely — it only needs to look like a valid
// git URL for NormalizeOriginURL/deriveProjectIDFromURL's sake.
func fakeHTTPSGitURL(t *testing.T) string {
	return "https://example-forge.test/org/" + t.Name()
}

// newNoYAMLTestServiceFakeClone is newNoYAMLTestService with CloneBareRepo/
// FetchBareRepo overridden to clone/fetch from sourceDir (a real local git
// repo) regardless of the URL argument passed in — see fakeHTTPSGitURL's doc
// comment for why this indirection is needed.
func newNoYAMLTestServiceFakeClone(t *testing.T, repo *stubProjectRepository, ws *stubWorkspaceStore, sourceDir string) (*ProjectAppService, string) {
	t.Helper()
	svc, dataDir := newNoYAMLTestService(t, repo, ws)
	svc.CloneBareRepo = func(ctx context.Context, url, dest, namespace string) error {
		runGitCmd(t, "", "clone", "-q", "--bare", sourceDir, dest)
		return nil
	}
	svc.FetchBareRepo = func(ctx context.Context, bareRepoPath, namespace string) error {
		cmd := exec.Command("git", "-C", bareRepoPath, "fetch", "--all")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git fetch --all: %v\n%s", err, out)
		}
		return nil
	}
	return svc, dataDir
}

// TestCreateProjectFromGitURL_NoYAML_FallsBackToWorkspaceDefault_Succeeds
// pins 論点b/c/d + PR4's dynamic merge: a repo with no .boid/project.yaml
// registers successfully when the workspace has a usable default project
// definition (a "supervisor" behavior here), deriving both id (決定3) and
// name (論点b) from the git URL.
func TestCreateProjectFromGitURL_NoYAML_FallsBackToWorkspaceDefault_Succeeds(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {Traits: []string{"careful"}},
		},
	}
	svc, dataDir := newNoYAMLTestServiceFakeClone(t, repo, ws, src)

	gitURL := fakeHTTPSGitURL(t)
	project, err := svc.CreateProjectFromGitURL(context.Background(), gitURL, "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	wantName, nerr := orchestrator.DeriveProjectNameFromURL(gitURL)
	if nerr != nil {
		t.Fatalf("DeriveProjectNameFromURL: %v", nerr)
	}
	if project.Meta.Name != wantName {
		t.Errorf("Meta.Name = %q, want %q", project.Meta.Name, wantName)
	}
	if project.Meta.ID == "" || project.Meta.ID != project.ID {
		t.Errorf("project.ID = %q, Meta.ID = %q; want equal and non-empty", project.ID, project.Meta.ID)
	}
	wantID, derr := deriveProjectIDFromURL(gitURL)
	if derr != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", derr)
	}
	if project.ID != wantID {
		t.Errorf("project.ID = %q, want %q (derived from URL)", project.ID, wantID)
	}
	// The cached meta itself must stay minimal — TaskBehaviors is filled in
	// dynamically by GetWithWorkspace at hydrate time, not snapshotted here.
	cached, ok := svc.Meta.Get(project.ID)
	if !ok {
		t.Fatal("expected the synthesized meta to be cached")
	}
	if len(cached.TaskBehaviors) != 0 {
		t.Errorf("cached meta.TaskBehaviors = %v, want empty (workspace default merges dynamically at hydrate time)", cached.TaskBehaviors)
	}

	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", wantName)
	if project.WorkDir != wantBarePath {
		t.Errorf("WorkDir = %q, want %q", project.WorkDir, wantBarePath)
	}
}

// TestCreateProjectFromGitURL_NoYAML_NoUsableDefault_Rejects pins 論点d
// 2巡目レビュー m2: a workspace with task_behaviors defined but NEITHER
// default_task_behavior set NOR a "supervisor" behavior must still be
// rejected at add time — merely "a default definition exists" is not
// sufficient; the check mirrors ResolveBehavior's own resolution order.
func TestCreateProjectFromGitURL_NoYAML_NoUsableDefault_Rejects(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"release": {},
		},
		// No DefaultTaskBehavior, no "supervisor" entry.
	}
	svc, dataDir := newNoYAMLTestServiceFakeClone(t, repo, ws, src)

	gitURL := fakeHTTPSGitURL(t)
	_, err := svc.CreateProjectFromGitURL(context.Background(), gitURL, "team-a", "")
	if err == nil {
		t.Fatal("expected rejection: workspace default has task_behaviors but no resolvable default behavior")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("expected *StatusError{400}, got %T: %v", err, err)
	}

	// Rolled back: no bare repo left behind.
	wantName, nerr := orchestrator.DeriveProjectNameFromURL(gitURL)
	if nerr != nil {
		t.Fatalf("DeriveProjectNameFromURL: %v", nerr)
	}
	wantBarePath := orchestrator.BareRepoPath(dataDir, "team-a", wantName)
	if _, statErr := os.Stat(wantBarePath); !os.IsNotExist(statErr) {
		t.Errorf("expected the bare repo at %s to be rolled back, stat err = %v", wantBarePath, statErr)
	}
}

// TestCreateProjectFromGitURL_NoYAML_NoWorkspaceDefaultAtAll_Rejects pins
// 論点d's baseline case: a workspace with no default project definition at
// all (empty WorkspaceMeta) refuses a project.yaml-less registration.
func TestCreateProjectFromGitURL_NoYAML_NoWorkspaceDefaultAtAll_Rejects(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{}
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)

	_, err := svc.CreateProjectFromGitURL(context.Background(), fakeHTTPSGitURL(t), "team-a", "")
	if err == nil {
		t.Fatal("expected rejection: workspace has no default project definition at all")
	}
	if statusErr, ok := err.(*StatusError); !ok || statusErr.Code != http.StatusBadRequest {
		t.Fatalf("expected *StatusError{400}, got %T: %v", err, err)
	}
}

// TestCreateProjectFromGitURL_NoYAML_NameCollision_DerivedName_Rejects pins
// 論点b m2: an auto-derived name that collides with an already-registered
// project's name is refused (rather than silently registering the
// collision, which would later break workspace export/apply's exact-name
// resolution for BOTH projects).
func TestCreateProjectFromGitURL_NoYAML_NameCollision_DerivedName_Rejects(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	existingID := "existing-proj"
	gitURL := fakeHTTPSGitURL(t)
	existingName, nerr := orchestrator.DeriveProjectNameFromURL(gitURL) // guaranteed to collide with the derived name
	if nerr != nil {
		t.Fatalf("DeriveProjectNameFromURL: %v", nerr)
	}
	repo := &stubProjectRepository{
		existingWorkspaces: map[string]bool{"team-a": true},
		projects:           []*orchestrator.Project{{ID: existingID, WorkDir: "/some/other/path"}},
	}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		DefaultTaskBehavior: "supervisor",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)
	svc.Meta.SetSynthesizedMeta(existingID, &orchestrator.ProjectMeta{ID: existingID, Name: existingName})

	_, err := svc.CreateProjectFromGitURL(context.Background(), gitURL, "team-a", "")
	if err == nil {
		t.Fatal("expected rejection: derived name collides with an already-registered project")
	}
	statusErr, ok := err.(*StatusError)
	if !ok || statusErr.Code != http.StatusConflict {
		t.Fatalf("expected *StatusError{409}, got %T: %v", err, err)
	}
}

// TestCreateProjectFromGitURL_NoYAML_NameCollision_ExplicitNameWarnsButSucceeds
// pins 論点b m2's other half: an EXPLICIT --name that collides is allowed
// through (the caller made an informed choice), unlike an auto-derived one.
func TestCreateProjectFromGitURL_NoYAML_NameCollision_ExplicitNameWarnsButSucceeds(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	existingID := "existing-proj"
	collidingName := "shared-name"
	repo := &stubProjectRepository{
		existingWorkspaces: map[string]bool{"team-a": true},
		projects:           []*orchestrator.Project{{ID: existingID, WorkDir: "/some/other/path"}},
	}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		DefaultTaskBehavior: "supervisor",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)
	svc.Meta.SetSynthesizedMeta(existingID, &orchestrator.ProjectMeta{ID: existingID, Name: collidingName})

	project, err := svc.CreateProjectFromGitURL(context.Background(), fakeHTTPSGitURL(t), "team-a", collidingName)
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL with explicit colliding --name: %v", err)
	}
	if project.Meta.Name != collidingName {
		t.Errorf("Meta.Name = %q, want %q", project.Meta.Name, collidingName)
	}
}

// TestCreateProjectFromGitURL_NoYAML_InsertConflict_DoesNotClobberEstablishedProjectsCache
// pins the Codex review P1 fix (PR5 round 1): when this call's derived id
// (決定3, deterministic per repo slug) collides with an ALREADY-REGISTERED
// project (re-adding the same repo under a different workspace/name, or two
// distinct repos whose slug happens to collide), the DB insert fails on the
// primary key — s.Meta.SetSynthesizedMeta must not have been called before
// that point, or it would have overwritten the established project's cached
// meta (name/status) with this call's synthesized one first, and
// removeCachedMetaUnlessRegistered's failure-path check (which only guards
// against REMOVING an established project's entry) would then leave that
// clobbered value in place instead of the original.
func TestCreateProjectFromGitURL_NoYAML_InsertConflict_DoesNotClobberEstablishedProjectsCache(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	gitURL := fakeHTTPSGitURL(t)
	wantID, err := deriveProjectIDFromURL(gitURL)
	if err != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", err)
	}

	established := &orchestrator.Project{ID: wantID, WorkDir: "/already/registered/path"}
	repo := &stubProjectRepository{
		existingWorkspaces: map[string]bool{"team-a": true},
		projects:           []*orchestrator.Project{established},
		createProjectHook: func(*orchestrator.Project) error {
			// Simulate the primary-key conflict a real DB insert would
			// report when wantID is already an established project's row.
			return fmt.Errorf("UNIQUE constraint failed: projects.id")
		},
	}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		DefaultTaskBehavior: "supervisor",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)
	establishedMeta := &orchestrator.ProjectMeta{ID: wantID, Name: "established-project"}
	svc.Meta.SetSynthesizedMeta(wantID, establishedMeta)

	_, err = svc.CreateProjectFromGitURL(context.Background(), gitURL, "team-a", "")
	if err == nil {
		t.Fatal("expected the DB insert conflict to surface as an error")
	}

	cached, ok := svc.Meta.Get(wantID)
	if !ok {
		t.Fatal("expected the established project's cached meta to still be present")
	}
	if cached.Name != "established-project" {
		t.Errorf("cached meta.Name = %q, want %q (established project's cache must survive the failed registration attempt unclobbered)", cached.Name, "established-project")
	}
}

// TestFetchProject_NoYAML_SurvivesReload pins §現状の実測5's FetchProject
// half: `boid project fetch` on a project.yaml-less project must not
// MarkDegraded when the reason project.yaml can't be read is simply "it was
// never committed" — it falls back to a synthesized meta instead, same as
// LoadAll.
func TestFetchProject_NoYAML_SurvivesReload(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	ws := newStubWorkspaceStore()
	ws.metas["team-a"] = &orchestrator.WorkspaceMeta{
		DefaultTaskBehavior: "supervisor",
		TaskBehaviors: map[string]orchestrator.TaskBehavior{
			"supervisor": {},
		},
	}
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)

	created, err := svc.CreateProjectFromGitURL(context.Background(), fakeHTTPSGitURL(t), "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = append(repo.projects, created)

	fetched, err := svc.FetchProject(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("FetchProject: %v", err)
	}
	if fetched.Meta.Name != created.Meta.Name {
		t.Errorf("Meta.Name after fetch = %q, want %q (preserved across reload)", fetched.Meta.Name, created.Meta.Name)
	}
	st := svc.Meta.Status(created.ID)
	if st.State != orchestrator.StatusReady {
		t.Errorf("Status after fetch = %q, want %q (not degraded)", st.State, orchestrator.StatusReady)
	}
}
