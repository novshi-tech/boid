package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// TestCreateProject_UpstreamURL_NullableByDefault covers the PR2 (git-gateway
// cutover) DB-layer contract: upstream_url is nullable, and an empty string
// on the Go side round-trips as NULL / "" rather than an empty-string literal.
func TestCreateProject_UpstreamURL_NullableByDefault(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "p1", WorkDir: "/tmp/p1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := orchestrator.GetProject(d.Conn, "p1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.UpstreamURL != "" {
		t.Errorf("UpstreamURL = %q, want empty", got.UpstreamURL)
	}
}

// TestCreateProject_UpstreamURL_PersistsAndReloads verifies that a captured
// upstream_url set at creation time round-trips through GetProject and
// ListProjects.
func TestCreateProject_UpstreamURL_PersistsAndReloads(t *testing.T) {
	d := testutil.NewTestDB(t)

	want := "https://github.com/owner/repo.git"
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "p1", WorkDir: "/tmp/p1", UpstreamURL: want}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := orchestrator.GetProject(d.Conn, "p1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.UpstreamURL != want {
		t.Errorf("GetProject UpstreamURL = %q, want %q", got.UpstreamURL, want)
	}

	list, err := orchestrator.ListProjects(d.Conn)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(list) != 1 || list[0].UpstreamURL != want {
		t.Fatalf("ListProjects = %+v, want single project with UpstreamURL %q", list, want)
	}
}

// TestSetProjectUpstreamURL_UpdatesExistingProject covers the backfill /
// reload-recapture write path.
func TestSetProjectUpstreamURL_UpdatesExistingProject(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "p1", WorkDir: "/tmp/p1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	want := "https://bitbucket.org/owner/repo.git"
	if err := orchestrator.SetProjectUpstreamURL(d.Conn, "p1", want); err != nil {
		t.Fatalf("set upstream_url: %v", err)
	}

	got, err := orchestrator.GetProject(d.Conn, "p1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.UpstreamURL != want {
		t.Errorf("UpstreamURL after SetProjectUpstreamURL = %q, want %q", got.UpstreamURL, want)
	}
}

func TestSetProjectUpstreamURL_UnknownProjectErrors(t *testing.T) {
	d := testutil.NewTestDB(t)

	if err := orchestrator.SetProjectUpstreamURL(d.Conn, "nonexistent", "https://github.com/o/r.git"); err == nil {
		t.Fatal("expected error for nonexistent project, got nil")
	}
}

// TestRequireUpstreamURL is the PR2 building block described in
// project_catalog.go's doc comment — not yet wired into dispatch (deferred to
// PR6 per docs/plans/git-gateway-cutover.md), but covered here so PR6 has a
// tested, ready-made check to call.
func TestRequireUpstreamURL(t *testing.T) {
	if err := orchestrator.RequireUpstreamURL(nil); err == nil {
		t.Error("expected error for nil project")
	}
	if err := orchestrator.RequireUpstreamURL(&orchestrator.Project{ID: "p1"}); err == nil {
		t.Error("expected error for project with no upstream_url")
	}
	if err := orchestrator.RequireUpstreamURL(&orchestrator.Project{ID: "p1", UpstreamURL: "https://github.com/o/r.git"}); err != nil {
		t.Errorf("expected no error for project with upstream_url, got %v", err)
	}
}
