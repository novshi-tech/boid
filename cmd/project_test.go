package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRenderProjectDetail_BasicFields(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkspaceID: "ws-1",
		WorkDir:     "/home/user/repo",
		CreatedAt:   time.Unix(0, 0).UTC(),
		UpdatedAt:   time.Unix(0, 0).UTC(),
		Meta: projectspec.ProjectMeta{
			Name: "My Project",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"ID:", "proj-abc",
		"Name:", "My Project",
		"WorkDir:", "/home/user/repo",
		"WorkspaceID:", "ws-1",
		"CreatedAt:",
		"UpdatedAt:",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectDetail_UpstreamURL_Set(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkDir:     "/home/user/repo",
		UpstreamURL: "https://github.com/owner/repo.git",
		Meta:        projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: https://github.com/owner/repo.git") {
		t.Errorf("output missing captured UpstreamURL\n%s", got)
	}
}

func TestRenderProjectDetail_UpstreamURL_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID:      "proj-abc",
		WorkDir: "/home/user/repo",
		Meta:    projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: (none") {
		t.Errorf("output missing empty-UpstreamURL guidance\n%s", got)
	}
}

func TestRenderProjectDetail_MetaSections(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-meta",
		Meta: projectspec.ProjectMeta{
			Name: "Meta Test",
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Hooks: []projectspec.Hook{
						{ID: "on-start", Requires: []string{"gh"}},
					},
				},
			},
			HostCommands: projectspec.HostCommands{"gh": {}},
			AdditionalBindings: []projectspec.BindMount{
				{Source: "/data", Mode: "ro"},
			},
			Env: map[string]string{
				"GITHUB_TOKEN": "secret",
				"FOO":          "bar",
			},
			SecretNamespace: "myns",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"TaskBehaviors:",
		"dev",
		"hook: on-start",
		"HostCommands:",
		"gh",
		"AdditionalBindings:",
		"/data",
		"ro",
		"Env:",
		"FOO",
		"GITHUB_TOKEN",
		"SecretNamespace:",
		"myns",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_AlphaOrder(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"zzz": {},
				"aaa": {},
				"mmm": {},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	// キーがアルファベット順で出ること
	idxA := strings.Index(got, "aaa")
	idxM := strings.Index(got, "mmm")
	idxZ := strings.Index(got, "zzz")
	if idxA < 0 || idxM < 0 || idxZ < 0 {
		t.Fatalf("missing keys in output:\n%s", got)
	}
	if !(idxA < idxM && idxM < idxZ) {
		t.Errorf("behaviors not in alphabetical order (a=%d m=%d z=%d):\n%s", idxA, idxM, idxZ, got)
	}
}

func TestRenderProjectBehaviors_Fields(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh2",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Traits: []string{"artifact", "worktree"},
				},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	checks := []string{
		"dev",
		"artifact",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-empty",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	if !strings.Contains(got, "no behaviors") {
		t.Errorf("expected 'no behaviors' message, got:\n%s", got)
	}
}

// TestProjectAddCmd_HasWorkspaceFlag verifies that the --workspace flag is
// registered on `boid project add`.
func TestProjectAddCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectAddCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project add")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}

// TestProjectInitSubCmd_HasWorkspaceFlag verifies that --workspace is
// registered on `boid project init`.
func TestProjectInitSubCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectInitSubCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project init")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}

// withProjectAddWorkspaceFlag sets the package-level --workspace flag value
// `runProjectAdd` reads (projectAddWorkspace) for the duration of the
// calling test, restoring it to "" afterward so this global does not leak
// across other tests in the same binary.
func withProjectAddWorkspaceFlag(t *testing.T, slug string) {
	t.Helper()
	projectAddWorkspace = slug
	t.Cleanup(func() { projectAddWorkspace = "" })
}

// writeGitURLTestProject builds a plain (non-bare) local git repo with a
// COMMITTED .boid/project.yaml, usable as the `boid project add <git-url>`
// argument — unlike writeImportTestProject (task_import_test.go), whose
// project.yaml sits UNCOMMITTED on top of an --allow-empty initial commit
// (fine for the legacy dir-based CreateProject flow, which reads straight
// off disk, but useless as a `git clone` source: only committed content
// travels). A local filesystem path is itself a perfectly valid git URL, so
// this exercises runProjectAdd's real git-clone code path end-to-end
// without a fake forge.
func writeGitURLTestProject(t *testing.T, id, name string) string {
	t.Helper()
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "add", ".")
	runGitTestCmd(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// gitFileURL turns a local filesystem path into a "file://" URL —
// looksLikeGitURL (project.go) requires an explicit scheme or the scp-like
// form, so a bare local path (which git itself happily clones) is rejected
// outright by runProjectAdd since PR-4 removed the legacy dir-based form
// (see TestProjectAdd_RejectsHostDirectory). Wrapping test fixture
// directories in file:// lets these tests exercise the git-URL code path
// while still pointing at a local, credential-free source repo.
func gitFileURL(dir string) string { return "file://" + dir }

func runGitTestCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestLooksLikeGitURL pins Minor 1 (PR-2a codex round-2 review): the
// pre-fix strings.Contains(s, "://") / bare-"@...:" heuristic searched
// anywhere in the argument rather than anchoring at the start, so a
// relative path like "./https://repo" was misclassified as a URL, while a
// valid scp-style URL with no explicit login user
// ("host:org/repo.git") was misclassified as a directory.
func TestLooksLikeGitURL(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"https://github.com/owner/repo.git", true},
		{"ssh://git@github.com/owner/repo.git", true},
		{"git://github.com/owner/repo.git", true},
		{"git@github.com:owner/repo.git", true},
		{"host:org/repo.git", true}, // scp-like, no explicit login user
		{"./my-project", false},
		{"/home/user/my-project", false},
		{"my-project", false},
		{".", false},
		// The two concrete misclassifications codex round-2 named:
		{"./https://repo", false},
		{"./git@host:repo", false},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := looksLikeGitURL(tt.arg); got != tt.want {
				t.Errorf("looksLikeGitURL(%q) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

// TestProjectAdd_RejectsHostDirectory pins PR-4's removal of the legacy
// `boid project add <dir>` form (docs/plans/volume-only-daemon.md §論点e):
// any argument that does not look like a git URL (looksLikeGitURL) is
// rejected client-side, before any daemon round trip, with the exact
// message the plan doc's CLI-scope bullet specifies — pointing the operator
// at the still-working git-URL form rather than silently registering
// nothing or (worse) misinterpreting the path as something else.
func TestProjectAdd_RejectsHostDirectory(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	err := runProjectAdd(projectAddCmd, []string{t.TempDir()})
	if err == nil {
		t.Fatal("expected an error registering a host directory — the legacy form was removed in PR-4")
	}
	if !strings.Contains(err.Error(), "host directory registration was removed") {
		t.Errorf("expected the PR-4 removal message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "boid project add <git-url> --workspace=<name>") {
		t.Errorf("expected the error to point at the git-URL form, got: %v", err)
	}
}

// TestProjectAdd_RejectsHostDirectory_EvenOverRemoteProfile pins that the
// rejection fires purely from the argument's shape (looksLikeGitURL),
// before any profile/transport check runs at all — a remote (non-unix)
// profile gets the identical rejection, not a different "remote profile
// unsupported" message the pre-PR-4 legacy form used to produce.
func TestProjectAdd_RejectsHostDirectory_EvenOverRemoteProfile(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	remoteClient, err := client.NewClient("https://example.invalid", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cmd := projectAddCmd
	cmd.SetContext(client.WithClient(context.Background(), remoteClient))
	t.Cleanup(func() { cmd.SetContext(context.Background()) })

	err = runProjectAdd(cmd, []string{t.TempDir()})
	if err == nil {
		t.Fatal("expected an error registering a host directory over a remote profile")
	}
	if !strings.Contains(err.Error(), "host directory registration was removed") {
		t.Errorf("expected the PR-4 removal message, got: %v", err)
	}
}

// TestProjectAdd_RequiresWorkspaceFlag pins the CLI-side half of the
// "--workspace is required" contract (the server-side half is
// internal/api's TestCreateProjectFromGitURL_RequiresWorkspace) — checked
// before any daemon round trip, same as the directory-argument rejection
// above.
func TestProjectAdd_RequiresWorkspaceFlag(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	err := runProjectAdd(projectAddCmd, []string{"https://example.invalid/owner/repo.git"})
	if err == nil {
		t.Fatal("expected an error for a missing --workspace flag")
	}
	if !strings.Contains(err.Error(), "--workspace is required") {
		t.Errorf("expected a --workspace-required error, got: %v", err)
	}
}

// TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns is the git-URL-flow
// counterpart of MAJOR 4 (codex review, docs/plans/
// workspace-db-consolidation.md): `project add <git-url> --workspace
// <unknown-slug>` must get-or-create an empty workspace DB row for the slug
// client-side (ensureWorkspaceExistsGetOrCreate) before the daemon's own
// CreateProjectFromGitURL — which requires the workspace to already exist,
// unlike the legacy dir-based CreateProject's eager default-workspace
// assign — ever runs.
func TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "brand-new-ws")

	src := writeGitURLTestProject(t, "project-add-unknown-ws", "Project Add Unknown WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/brand-new-ws", nil, &detail); err != nil {
		t.Fatalf("expected workspace %q to have been get-or-created: %v", "brand-new-ws", err)
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected exactly one registered project, got %d", len(projects))
	}
	if projects[0].WorkspaceID != "brand-new-ws" {
		t.Errorf("WorkspaceID = %q, want brand-new-ws", projects[0].WorkspaceID)
	}
	if projects[0].UpstreamURL != gitFileURL(src) {
		t.Errorf("UpstreamURL = %q, want %q", projects[0].UpstreamURL, gitFileURL(src))
	}
}

// TestProjectAdd_WithExistingWorkspace_JustAssigns is the regression guard
// alongside the get-or-create test above: assigning to a slug that already
// has a DB row must not error (no spurious "already exists" 409 surfacing
// from the get-or-create step) and must still assign normally.
func TestProjectAdd_WithExistingWorkspace_JustAssigns(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	testutil.SeedWorkspace(t, ts, "existing-ws")
	withProjectAddWorkspaceFlag(t, "existing-ws")

	src := writeGitURLTestProject(t, "project-add-existing-ws", "Project Add Existing WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected exactly one registered project, got %d", len(projects))
	}
	if projects[0].WorkspaceID != "existing-ws" {
		t.Errorf("WorkspaceID = %q, want existing-ws", projects[0].WorkspaceID)
	}
}

// TestProjectAdd_NameOverride verifies --name governs the derived project
// name (and thus the bare-repo storage path) independent of the URL's own
// last path component.
func TestProjectAdd_NameOverride(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "default")
	projectAddName = "custom-name"
	t.Cleanup(func() { projectAddName = "" })

	src := writeGitURLTestProject(t, "project-add-name-override", "Name Override")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	if !strings.Contains(out.String(), "custom-name.git") {
		t.Errorf("expected output to reference the overridden bare-repo name, got:\n%s", out.String())
	}
}

// TestProjectFetch_UpdatesProjectYAML exercises `boid project fetch` end to
// end against a real daemon: register via a git URL, change the source
// repo's project.yaml and commit, fetch, and verify the change landed.
func TestProjectFetch_UpdatesProjectYAML(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "default")

	src := writeGitURLTestProject(t, "project-fetch-test", "Before Fetch")

	var addOut bytes.Buffer
	addCmd := projectAddCmd
	addCmd.SetOut(&addOut)
	if err := runProjectAdd(addCmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte("id: project-fetch-test\nname: After Fetch\n"), 0o644); err != nil {
		t.Fatalf("rewrite project.yaml: %v", err)
	}
	runGitTestCmd(t, src, "add", ".")
	runGitTestCmd(t, src, "commit", "-q", "-m", "rename")

	var fetchOut bytes.Buffer
	fetchCmd := projectFetchCmd
	fetchCmd.SetOut(&fetchOut)
	if err := runProjectFetch(fetchCmd, []string{"project-fetch-test"}); err != nil {
		t.Fatalf("runProjectFetch: %v", err)
	}
	if !strings.Contains(fetchOut.String(), "After Fetch") {
		t.Errorf("expected fetch output to reflect the updated name, got:\n%s", fetchOut.String())
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Meta.Name != "After Fetch" {
		t.Fatalf("expected the registered project's name to be updated after fetch, got: %+v", projects)
	}
}
