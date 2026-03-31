package db_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/projectspec"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	p := &projectspec.Project{ID: "proj-1", WorkDir: "/tmp/proj1"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if p.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
	if p.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}
}

func TestGetProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	p := &projectspec.Project{ID: "proj-1", WorkDir: "/tmp/proj1"}
	if err := orchestrator.CreateProject(d.Conn, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ID != "proj-1" {
		t.Fatalf("expected id proj-1, got %s", got.ID)
	}
	if got.WorkDir != "/tmp/proj1" {
		t.Fatalf("expected work_dir /tmp/proj1, got %s", got.WorkDir)
	}
}

func TestGetProject_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := orchestrator.GetProject(d.Conn, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListProjects(t *testing.T) {
	d := testutil.NewTestDB(t)

	projects, err := orchestrator.ListProjects(d.Conn)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("expected 0 projects, got %d", len(projects))
	}

	if err := orchestrator.CreateProject(d.Conn, &projectspec.Project{ID: "proj-1", WorkDir: "/tmp/a"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &projectspec.Project{ID: "proj-2", WorkDir: "/tmp/b"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	projects, err = orchestrator.ListProjects(d.Conn)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
}

func TestDeleteProject(t *testing.T) {
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &projectspec.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := orchestrator.DeleteProject(d.Conn, "proj-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := orchestrator.GetProject(d.Conn, "proj-1")
	if err == nil {
		t.Fatal("expected not found after delete")
	}
}

func TestDeleteProject_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	err := orchestrator.DeleteProject(d.Conn, "nonexistent")
	if err == nil {
		t.Fatal("expected error for deleting nonexistent project")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}
