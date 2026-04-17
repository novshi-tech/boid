package api_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func setupTestProject(t *testing.T, id, name string, includeDeprecatedWorkspace bool) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatalf("mkdir kit hooks: %v", err)
	}

	yaml := "id: " + id + "\nname: " + name + "\n"
	if includeDeprecatedWorkspace {
		yaml += "workspace_id: ws-1\n"
	}
	yaml += `task_behaviors:
  planning:
    name: Planning
    kits:
      - agent
  implementation:
    name: Implementation
    kits:
      - agent
`
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	kitYAML := `hooks:
  - id: run-agent
    on: executing
`
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/bash\necho hello\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	return dir
}

func createProject(t *testing.T, ts *testutil.TestServer, id, name string) orchestrator.Project {
	t.Helper()

	dir := setupTestProject(t, id, name, false)
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return project
}

func TestProjectAPI_CreateAndGet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	created := createProject(t, ts, "test-project", "Test Project")

	if created.ID != "test-project" {
		t.Errorf("created project ID = %q, want %q", created.ID, "test-project")
	}
	if created.WorkspaceID != "" {
		t.Errorf("created workspace_id = %q, want empty", created.WorkspaceID)
	}

	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/test-project", nil, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.ID != "test-project" {
		t.Errorf("got project ID = %q, want %q", got.ID, "test-project")
	}
	if got.WorkspaceID != "" {
		t.Errorf("got workspace_id = %q, want empty", got.WorkspaceID)
	}
}

func TestProjectAPI_CreateRejectsDeprecatedWorkspaceID(t *testing.T) {
	ts := testutil.NewTestServer(t)
	dir := setupTestProject(t, "deprecated-project", "Deprecated Project", true)

	var created orchestrator.Project
	err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &created)
	if err == nil {
		t.Fatal("expected project creation to fail for deprecated workspace_id")
	}
}

func TestProjectAPI_List(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "test-project", "Test Project")

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

func TestProjectAPI_SetWorkspaceAndGet(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "test-project", "Test Project")

	var updated orchestrator.Project
	if err := ts.Client.Do("PUT", "/api/projects/test-project/workspace", map[string]string{"workspace_id": "ws-1"}, &updated); err != nil {
		t.Fatalf("set workspace: %v", err)
	}
	if updated.WorkspaceID != "ws-1" {
		t.Fatalf("workspace_id = %q, want %q", updated.WorkspaceID, "ws-1")
	}

	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/test-project", nil, &got); err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.WorkspaceID != "ws-1" {
		t.Fatalf("workspace_id = %q, want %q", got.WorkspaceID, "ws-1")
	}

	if err := ts.Client.Do("PUT", "/api/projects/test-project/workspace", map[string]string{"workspace_id": ""}, &updated); err != nil {
		t.Fatalf("clear workspace: %v", err)
	}
	if updated.WorkspaceID != "" {
		t.Fatalf("workspace_id = %q, want empty", updated.WorkspaceID)
	}
}

func TestProjectAPI_ListByWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "proj-1", "Project 1")
	createProject(t, ts, "proj-2", "Project 2")

	var updated orchestrator.Project
	if err := ts.Client.Do("PUT", "/api/projects/proj-1/workspace", map[string]string{"workspace_id": "ws-1"}, &updated); err != nil {
		t.Fatalf("set workspace: %v", err)
	}

	var matched []*orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects?workspace_id=ws-1", nil, &matched); err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(matched) != 1 {
		t.Fatalf("expected 1 project for ws-1, got %d", len(matched))
	}
	if matched[0].ID != "proj-1" {
		t.Fatalf("matched project = %q, want %q", matched[0].ID, "proj-1")
	}

	var empty []*orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects?workspace_id=ws-999", nil, &empty); err != nil {
		t.Fatalf("list by workspace: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("expected 0 projects for ws-999, got %d", len(empty))
	}
}

func TestProjectAPI_ListWorkspaces(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "proj-1", "Project 1")
	createProject(t, ts, "proj-2", "Project 2")
	createProject(t, ts, "proj-3", "Project 3")

	var updated orchestrator.Project
	for projectID, workspaceID := range map[string]string{
		"proj-1": "ws-1",
		"proj-2": "ws-1",
		"proj-3": "ws-2",
	} {
		if err := ts.Client.Do("PUT", "/api/projects/"+projectID+"/workspace", map[string]string{"workspace_id": workspaceID}, &updated); err != nil {
			t.Fatalf("set workspace for %s: %v", projectID, err)
		}
	}

	var workspaces []*orchestrator.WorkspaceSummary
	if err := ts.Client.Do("GET", "/api/workspaces", nil, &workspaces); err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(workspaces) != 2 {
		t.Fatalf("expected 2 workspaces, got %d", len(workspaces))
	}
	if workspaces[0].ID != "ws-1" || workspaces[0].ProjectCount != 2 {
		t.Fatalf("unexpected workspace 0: %+v", workspaces[0])
	}
	if workspaces[1].ID != "ws-2" || workspaces[1].ProjectCount != 1 {
		t.Fatalf("unexpected workspace 1: %+v", workspaces[1])
	}
}

func TestProjectAPI_Delete(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "test-project", "Test Project")

	var delResult map[string]string
	if err := ts.Client.Do("DELETE", "/api/projects/test-project", nil, &delResult); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if delResult["status"] != "deleted" {
		t.Errorf("delete status = %q, want %q", delResult["status"], "deleted")
	}

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

func setupTestProjectWithCommand(t *testing.T, id, name string) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "mykit")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	projectYAML := "id: " + id + "\nname: " + name + "\n" +
		"commands:\n" +
		"  run:\n" +
		"    command: [bash, -c, echo hello]\n" +
		"    kits:\n" +
		"      - mykit\n" +
		"task_behaviors:\n" +
		"  impl:\n" +
		"    name: implementation\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	kitYAML := "host_commands:\n  curl: {}\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}
	return dir
}

func TestProjectAPI_GetCommand(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := setupTestProjectWithCommand(t, "cmd-proj", "Command Project")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var resp struct {
		Command      []string       `json:"command"`
		HostCommands map[string]any `json:"host_commands"`
	}
	if err := ts.Client.Do("GET", "/api/projects/cmd-proj/commands/run", nil, &resp); err != nil {
		t.Fatalf("get command: %v", err)
	}

	if len(resp.Command) < 2 || resp.Command[0] != "bash" {
		t.Errorf("command = %v, want [bash ...]", resp.Command)
	}
	if _, ok := resp.HostCommands["curl"]; !ok {
		t.Errorf("host_commands %v should contain 'curl'", resp.HostCommands)
	}
}

func TestProjectAPI_GetCommandNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)
	created := createProject(t, ts, "no-cmds", "No Commands")

	var resp map[string]any
	if err := ts.Client.Do("GET", "/api/projects/"+created.ID+"/commands/nonexistent", nil, &resp); err == nil {
		t.Error("expected error for nonexistent command, got nil")
	}
}

func TestProjectAPI_GetByName(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "unique-id", "my-unique-name")

	// Resolve by name (no project has id="my-unique-name").
	var got orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/my-unique-name", nil, &got); err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if got.ID != "unique-id" {
		t.Errorf("got ID = %q, want %q", got.ID, "unique-id")
	}
}

func TestProjectAPI_GetAmbiguous(t *testing.T) {
	ts := testutil.NewTestServer(t)
	createProject(t, ts, "proj-one", "shared-prefix-alpha")
	createProject(t, ts, "proj-two", "shared-prefix-beta")

	// "shared-prefix" is a substring of both project names → 409 Conflict.
	var got orchestrator.Project
	err := ts.Client.Do("GET", "/api/projects/shared-prefix", nil, &got)
	if err == nil {
		t.Fatal("expected error for ambiguous ref, got nil")
	}
	if err.Error() != "multiple projects match" {
		t.Errorf("error = %q, want %q", err.Error(), "multiple projects match")
	}
}
