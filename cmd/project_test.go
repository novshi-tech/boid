package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
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

// TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns pins MAJOR 4 (codex
// review, docs/plans/workspace-db-consolidation.md): `project add
// --workspace <unknown-slug>` must get-or-create an empty workspace DB row
// for the slug (not just call the assign PUT and let it 404) — this is the
// contract runProjectAdd's own docstring already promised ("get-or-create:
// DB row is created even for unknown slug") but never actually implemented
// before this fix.
func TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "brand-new-ws")

	dir := writeImportTestProject(t, "project-add-unknown-ws", "Project Add Unknown WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{dir}); err != nil {
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

	dir := writeImportTestProject(t, "project-add-existing-ws", "Project Add Existing WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{dir}); err != nil {
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
