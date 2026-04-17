package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/testutil"
)

// writeExecTestProject creates a minimal project with a test command.
func writeExecTestProject(t *testing.T, id, name string) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatalf("mkdir kit hooks: %v", err)
	}

	projectYAML := "id: " + id + "\nname: " + name + "\n" +
		"commands:\n  test-cmd:\n    command: [bash]\n    kits:\n      - agent\n" +
		"task_behaviors:\n  impl:\n    name: implementation\n    kits:\n      - agent\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	kitYAML := "hooks:\n  - id: run-agent\n    on: executing\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	return dir
}

// writeExecTestProjectWithBoidBuiltin creates a project with a kit that has boid builtin.
func writeExecTestProjectWithBoidBuiltin(t *testing.T, id, name string) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	kitHooksDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(kitHooksDir, 0o755); err != nil {
		t.Fatalf("mkdir kit hooks: %v", err)
	}

	projectYAML := "id: " + id + "\nname: " + name + "\n" +
		"commands:\n  test-cmd:\n    command: [bash]\n    kits:\n      - agent\n" +
		"task_behaviors:\n  impl:\n    name: implementation\n    kits:\n      - agent\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	kitYAML := "builtin_commands:\n  - boid\nhooks:\n  - id: run-agent\n    on: executing\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}
	return dir
}

func setTestSocket(t *testing.T, socketPath string) {
	t.Helper()
	old := os.Getenv("BOID_SOCKET")
	if err := os.Setenv("BOID_SOCKET", socketPath); err != nil {
		t.Fatalf("set BOID_SOCKET: %v", err)
	}
	t.Cleanup(func() {
		if old == "" {
			_ = os.Unsetenv("BOID_SOCKET")
		} else {
			_ = os.Setenv("BOID_SOCKET", old)
		}
	})
}

func TestBuildExecRequest_UsesProjectWorkspaceMembership(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir1 := writeExecTestProject(t, "proj-1", "Project 1")
	dir2 := writeExecTestProject(t, "proj-2", "Project 2")

	for id, dir := range map[string]string{"proj-1": dir1, "proj-2": dir2} {
		var project struct {
			ID string `json:"id"`
		}
		if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}

	for _, id := range []string{"proj-1", "proj-2"} {
		var project struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
		}
		if err := ts.Client.Do("PUT", "/api/projects/"+id+"/workspace", map[string]string{"workspace_id": "ws-1"}, &project); err != nil {
			t.Fatalf("assign workspace for %s: %v", id, err)
		}
	}

	setTestSocket(t, ts.Server.SocketPath())

	req, err := buildExecRequest("proj-1", "test-cmd")
	if err != nil {
		t.Fatalf("buildExecRequest: %v", err)
	}
	if req.ProjectID != "proj-1" {
		t.Fatalf("project id = %q, want %q", req.ProjectID, "proj-1")
	}
	if req.WorkspaceDirs["proj-2"] != dir2 {
		t.Fatalf("workspace dirs = %#v, want proj-2 => %q", req.WorkspaceDirs, dir2)
	}
	if _, ok := req.WorkspaceDirs["proj-1"]; ok {
		t.Fatalf("workspace dirs should not include self: %#v", req.WorkspaceDirs)
	}
}

func TestBuildExecRequest_RegistersBrokerForBoidBuiltin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeExecTestProjectWithBoidBuiltin(t, "proj-1", "Project 1")
	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	req, err := buildExecRequest("proj-1", "test-cmd")
	if err != nil {
		t.Fatalf("buildExecRequest: %v", err)
	}
	if req.BrokerSocket == "" || req.BrokerToken == "" {
		t.Fatalf("expected broker registration for boid builtin, got socket=%q token=%q", req.BrokerSocket, req.BrokerToken)
	}
}

func TestBuildExecRequest_CommandShellQuoted(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	// Command with argument that requires quoting
	projectYAML := "id: proj-q\nname: proj-q\n" +
		"commands:\n  run:\n    command: [claude, --append-system-prompt, 'hello world']\n    kits:\n      - agent\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}

	var project struct{ ID string `json:"id"` }
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	req, err := buildExecRequest("proj-q", "run")
	if err != nil {
		t.Fatalf("buildExecRequest: %v", err)
	}

	// Command should be shell-quoted: "claude --append-system-prompt 'hello world'"
	if !strings.Contains(req.Command, "claude") {
		t.Errorf("command %q should contain 'claude'", req.Command)
	}
	// 'hello world' should be quoted (contains a space)
	if strings.Contains(req.Command, "hello world") && !strings.Contains(req.Command, "'hello world'") {
		t.Errorf("command %q: 'hello world' should be shell-quoted", req.Command)
	}
}

func TestBuildExecRequest_EnvironmentYAMLContainsBuiltins(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "mykit")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	projectYAML := "id: proj-env\nname: proj-env\n" +
		"commands:\n  run:\n    command: [bash]\n    kits:\n      - mykit\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	kitYAML := "builtin_commands:\n  - git\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}

	var project struct{ ID string `json:"id"` }
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	req, err := buildExecRequest("proj-env", "run")
	if err != nil {
		t.Fatalf("buildExecRequest: %v", err)
	}

	if req.EnvironmentYAML == "" {
		t.Fatal("EnvironmentYAML should not be empty")
	}
	if !strings.Contains(req.EnvironmentYAML, "git") {
		t.Errorf("EnvironmentYAML should contain 'git', got:\n%s", req.EnvironmentYAML)
	}
}

func TestBuildExecRequest_CommandNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeExecTestProject(t, "proj-nc", "Project NC")
	var project struct{ ID string `json:"id"` }
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	_, err := buildExecRequest("proj-nc", "nonexistent-cmd")
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-cmd") {
		t.Errorf("error %q should mention command name", err.Error())
	}
}
