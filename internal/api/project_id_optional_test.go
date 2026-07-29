package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// setupIDLessYAMLSourceRepo builds a local git repo with a committed
// .boid/project.yaml that has NO `id:` field but DOES declare name/
// task_behaviors — the docs/plans/workspace-default-project.md 論点h 案1
// case: project.yaml is present (unlike setupNoYAMLSourceRepo, which has no
// .boid/project.yaml at all), it simply omits the optional id.
func setupIDLessYAMLSourceRepo(t *testing.T, name string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "name: " + name + "\ntask_behaviors:\n  dev:\n    traits: [\"careful\"]\n"
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

// TestCreateProjectFromGitURL_IDLessYAML_DerivesURLBasedID pins
// docs/plans/workspace-default-project.md 論点h 案1 (PR7): a project.yaml
// that is present but omits `id:` still registers successfully, deriving
// the same URL-based id a wholly-missing project.yaml gets (決定3) — but,
// unlike the no-yaml path, KEEPS every other field project.yaml declared
// (here: name and task_behaviors), rather than falling back to the
// workspace default.
func TestCreateProjectFromGitURL_IDLessYAML_DerivesURLBasedID(t *testing.T) {
	src := setupIDLessYAMLSourceRepo(t, "ID Less Project")
	repo := &stubProjectRepository{existingWorkspaces: map[string]bool{"team-a": true}}
	ws := newStubWorkspaceStore()
	svc, _ := newNoYAMLTestServiceFakeClone(t, repo, ws, src)

	gitURL := fakeHTTPSGitURL(t)
	project, err := svc.CreateProjectFromGitURL(context.Background(), gitURL, "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}

	wantID, derr := deriveProjectIDFromURL(gitURL)
	if derr != nil {
		t.Fatalf("deriveProjectIDFromURL: %v", derr)
	}
	if project.ID != wantID {
		t.Errorf("project.ID = %q, want %q (URL-derived, same as the no-yaml path)", project.ID, wantID)
	}
	if project.Meta.Name != "ID Less Project" {
		t.Errorf("Meta.Name = %q, want %q (project.yaml's own name, NOT URL-derived)", project.Meta.Name, "ID Less Project")
	}
	if _, ok := project.Meta.TaskBehaviors["dev"]; !ok {
		t.Errorf("expected project.yaml's own task_behaviors to survive, got %v", project.Meta.TaskBehaviors)
	}

	cached, ok := svc.Meta.Get(project.ID)
	if !ok {
		t.Fatal("expected the meta to be cached under the derived id")
	}
	if cached.ID != wantID {
		t.Errorf("cached.ID = %q, want %q", cached.ID, wantID)
	}
	// No stray cache entry left under the empty-string key LoadBareRepo
	// would have written to pre-fix.
	if _, ok := svc.Meta.Get(""); ok {
		t.Error("expected no cache entry under the empty-string key")
	}
}

// TestFetchProject_URLDerivedProject_LaterGetsIDInYAML_IgnoresDrift pins
// docs/plans/workspace-default-project.md 論点h 案1: a project registered
// via the no-project.yaml (or id-less-project.yaml) path, whose repo later
// gets a committed `.boid/project.yaml` with an explicit `id:` field, must
// NOT be rejected or re-id'd on the next fetch — the registered url-
// derived id is authoritative and never changes, since existing tasks
// reference it directly. This is the id-drift case §論点h calls out as
// currently mishandled by FetchProject's `meta.ID != id` refusal.
func TestFetchProject_URLDerivedProject_LaterGetsIDInYAML_IgnoresDrift(t *testing.T) {
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
	// newNoYAMLTestServiceFakeClone's default FetchBareRepo runs a bare
	// `git fetch --all`, which (unlike `git clone --bare`'s remote, which
	// sets no fetch refspec at all) never actually updates the bare repo's
	// own refs/heads/main — every OTHER test using this helper never
	// modifies src after cloning, so this gap has never mattered before.
	// This test needs the post-registration commit below to actually land,
	// so fetch with an explicit refspec instead.
	svc.FetchBareRepo = func(ctx context.Context, bareRepoPath, namespace string) error {
		cmd := exec.Command("git", "-C", bareRepoPath, "fetch", src, "+refs/heads/*:refs/heads/*")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git fetch (explicit refspec): %v\n%s", err, out)
		}
		return nil
	}

	created, err := svc.CreateProjectFromGitURL(context.Background(), fakeHTTPSGitURL(t), "team-a", "")
	if err != nil {
		t.Fatalf("CreateProjectFromGitURL: %v", err)
	}
	repo.projects = append(repo.projects, created)
	registeredID := created.ID

	// Someone later commits a .boid/project.yaml with an explicit,
	// completely different id: upstream.
	if err := os.MkdirAll(filepath.Join(src, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte("id: khi\nname: KHI Project\ntask_behaviors:\n  dev:\n    traits: [\"careful\"]\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGitCmd(t, src, "add", ".")
	runGitCmd(t, src, "config", "user.email", "test@example.com")
	runGitCmd(t, src, "config", "user.name", "Test")
	runGitCmd(t, src, "commit", "-q", "-m", "add project.yaml with an id")

	// Pre-seed a degraded status (Codex review, PR7 round-1 Minor): without
	// this, the StatusReady assertion below would pass vacuously even if
	// the ignored-drift path never actually called MarkReady.
	svc.Meta.MarkDegraded(registeredID, "pre-seeded degraded status for this test")

	fetched, err := svc.FetchProject(context.Background(), registeredID)
	if err != nil {
		t.Fatalf("FetchProject: %v (expected the drifted id: to be ignored, not rejected)", err)
	}
	if fetched.ID != registeredID {
		t.Errorf("fetched.ID = %q, want %q (the registered url-derived id must never change)", fetched.ID, registeredID)
	}
	if fetched.Meta.ID != registeredID {
		t.Errorf("fetched.Meta.ID = %q, want %q", fetched.Meta.ID, registeredID)
	}
	if fetched.Meta.Name != "KHI Project" {
		t.Errorf("Meta.Name = %q, want %q (project.yaml's other fields still take effect)", fetched.Meta.Name, "KHI Project")
	}
	if _, ok := fetched.Meta.TaskBehaviors["dev"]; !ok {
		t.Errorf("expected project.yaml's task_behaviors to load, got %v", fetched.Meta.TaskBehaviors)
	}

	st := svc.Meta.Status(registeredID)
	if st.State != orchestrator.StatusReady {
		t.Errorf("Status after fetch = %q, want %q (must not degrade on ignored id drift)", st.State, orchestrator.StatusReady)
	}
	if _, ok := svc.Meta.Get("khi"); ok {
		t.Error("expected no cache entry under the project.yaml-declared id; the registered id must be the only cache key")
	}
}

// TestFetchProject_HandAuthoredURLPrefixedID_ProjectYAMLDeletedUpstream_StillDegrades
// pins the Codex round-4 review Major fix: FetchProject's
// GitHeadReadFailurePathAbsent fallback (docs/plans/
// workspace-default-project.md §現状の実測5 / PR5) is gated on the
// project's registered id being VERIFIED url-derived — actually equal to
// deriveProjectIDFromURL(project.UpstreamURL) — not merely carrying
// orchestrator.URLDerivedProjectIDPrefix. A project row that was hand-
// authored with `id: url-custom` at some point in its history (never
// actually derived from a URL) must NOT get silently routed to the
// workspace-default fallback just because its .boid/project.yaml later
// disappears upstream — that would mask what is, for this project, a
// genuine and reportable failure (project.yaml deleted), exactly as an
// ordinary non-"url-"-prefixed project already does not get this
// treatment.
func TestFetchProject_HandAuthoredURLPrefixedID_ProjectYAMLDeletedUpstream_StillDegrades(t *testing.T) {
	src := setupNoYAMLSourceRepo(t)
	dataDir := t.TempDir()
	bareDir := filepath.Join(dataDir, "repos", "team-a", "hand-authored.git")
	if err := os.MkdirAll(filepath.Dir(bareDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGitCmd(t, "", "clone", "-q", "--bare", src, bareDir)

	const handAuthoredID = "url-custom"
	repo := &stubProjectRepository{
		existingWorkspaces: map[string]bool{"team-a": true},
		projects: []*orchestrator.Project{{
			ID:          handAuthoredID,
			WorkDir:     bareDir,
			WorkspaceID: "team-a",
			// A real, resolvable UpstreamURL that does NOT derive to
			// handAuthoredID — the whole point of this test is that a
			// url- prefix alone must not be trusted as proof of
			// provenance.
			UpstreamURL: "https://example-forge.test/org/" + t.Name(),
		}},
	}
	store := orchestrator.NewProjectStore()
	store.SetDeriveProjectIDFunc(DeriveProjectIDFromURL)
	svc := &ProjectAppService{
		Projects: repo,
		Meta:     store,
		DataDir:  dataDir,
		FetchBareRepo: func(ctx context.Context, bareRepoPath, namespace string) error {
			cmd := exec.Command("git", "-C", bareRepoPath, "fetch", "--all")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git fetch --all: %v\n%s", err, out)
			}
			return nil
		},
	}

	_, err := svc.FetchProject(context.Background(), handAuthoredID)
	if err == nil {
		t.Fatal("expected FetchProject to fail (project.yaml missing, id not verified url-derived) — must degrade, not silently fall back to the workspace default")
	}

	st := svc.Meta.Status(handAuthoredID)
	if st.State != orchestrator.StatusDegraded {
		t.Errorf("Status.State = %q, want %q (a hand-authored url-prefixed id must degrade like any ordinary project, not silently recover)", st.State, orchestrator.StatusDegraded)
	}
	if _, ok := svc.Meta.Get(handAuthoredID); ok {
		t.Error("expected no synthesized meta to be cached under the unverified hand-authored id")
	}
}
