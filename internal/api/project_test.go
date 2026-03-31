package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// setupTestProject creates a temp directory with .boid/project.yaml and a hook script.
func setupTestProject(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}

	yaml := `id: test-project
workspace_id: ws-1
name: Test Project
task_behaviors:
  planning:
    name: Planning
    transition: standard
  implementation:
    name: Implementation
    transition: standard
hooks:
  - id: run-agent
    on: executing
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "run-agent.sh"), []byte("#!/bin/bash\necho hello\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	return dir
}

func TestProjectAPI_CreateAndGet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := setupTestProject(t)

	// Create project
	reqBody := map[string]string{"work_dir": dir}
	var created orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", reqBody, &created); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if created.ID != "test-project" {
		t.Errorf("created project ID = %q, want %q", created.ID, "test-project")
	}
	if created.Meta.WorkspaceID != "ws-1" {
		t.Errorf("created project meta workspace_id = %q, want %q", created.Meta.WorkspaceID, "ws-1")
	}

	// Get project
	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/test-project", nil, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ID != "test-project" {
		t.Errorf("got project ID = %q, want %q", got.ID, "test-project")
	}
	if got.Meta.WorkspaceID != "ws-1" {
		t.Errorf("got project meta workspace_id = %q, want %q", got.Meta.WorkspaceID, "ws-1")
	}
}

func TestProjectAPI_List(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := setupTestProject(t)

	reqBody := map[string]string{"work_dir": dir}
	var created orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", reqBody, &created); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var projects []*orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(projects))
	}
	if projects[0].ID != "test-project" {
		t.Errorf("project ID = %q, want %q", projects[0].ID, "test-project")
	}
}

func TestProjectAPI_ListByWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := setupTestProject(t)

	reqBody := map[string]string{"work_dir": dir}
	var created orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", reqBody, &created); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Filter by correct workspace_id
	var matched []*orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects?workspace_id=ws-1", nil, &matched); err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("expected 1 project for ws-1, got %d", len(matched))
	}

	// Filter by wrong workspace_id
	var empty []*orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects?workspace_id=ws-999", nil, &empty); err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 projects for ws-999, got %d", len(empty))
	}
}

func TestProjectAPI_Delete(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := setupTestProject(t)

	reqBody := map[string]string{"work_dir": dir}
	var created orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", reqBody, &created); err != nil {
		t.Fatalf("create project: %v", err)
	}

	// Delete project
	var delResult map[string]string
	if err := ts.Client.Do("DELETE", "/api/projects/test-project", nil, &delResult); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if delResult["status"] != "deleted" {
		t.Errorf("delete status = %q, want %q", delResult["status"], "deleted")
	}

	// Verify GET returns error
	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/test-project", nil, &got); err == nil {
		t.Error("expected error on GET after DELETE, got nil")
	}
}

func TestProjectAPI_GetNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/nonexistent", nil, &got); err == nil {
		t.Error("expected error for non-existent project, got nil")
	}
}
