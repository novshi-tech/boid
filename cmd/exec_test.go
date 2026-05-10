package cmd

import (
	"os"
	"path/filepath"
	"reflect"
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
	kitYAML := "hooks:\n  - id: run-agent\n    \n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitHooksDir, "run-agent.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	return dir
}

func writeExecTestProjectWithKit(t *testing.T, id, name string) string {
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
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(""), 0o644); err != nil {
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

func TestBuildExecJob_WorkspaceVisibility(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir1 := writeExecTestProject(t, "proj-1", "Project 1")
	dir2 := writeExecTestProject(t, "proj-2", "Project 2")

	for _, dir := range []string{dir1, dir2} {
		var project struct {
			ID string `json:"id"`
		}
		if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
			t.Fatalf("create project: %v", err)
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

	prepared, err := buildExecJob("proj-1", "test-cmd", nil)
	if err != nil {
		t.Fatalf("buildExecJob: %v", err)
	}
	if prepared.spec.ProjectID != "proj-1" {
		t.Fatalf("project id = %q, want %q", prepared.spec.ProjectID, "proj-1")
	}
	peers := prepared.rt.WorkspacePeers
	if peers["proj-2"] != dir2 {
		t.Fatalf("workspace peers = %#v, want proj-2 => %q", peers, dir2)
	}
	if _, ok := peers["proj-1"]; ok {
		t.Fatalf("workspace peers should not include self: %#v", peers)
	}
}

func TestBuildExecJob_RegistersBrokerForBoidBuiltin(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeExecTestProjectWithKit(t, "proj-1", "Project 1")
	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	prepared, err := buildExecJob("proj-1", "test-cmd", nil)
	if err != nil {
		t.Fatalf("buildExecJob: %v", err)
	}
	if prepared.rt.BrokerSocket == "" || prepared.rt.BrokerToken == "" {
		t.Fatalf("expected broker registration for boid builtin, got socket=%q token=%q",
			prepared.rt.BrokerSocket, prepared.rt.BrokerToken)
	}
}

func TestBuildExecJob_ArgvPreserved(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	projectYAML := "id: proj-q\nname: proj-q\n" +
		"commands:\n  run:\n    command: [claude, --append-system-prompt, 'hello world']\n    kits:\n      - agent\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	prepared, err := buildExecJob("proj-q", "run", nil)
	if err != nil {
		t.Fatalf("buildExecJob: %v", err)
	}

	want := []string{"claude", "--append-system-prompt", "hello world"}
	if !reflect.DeepEqual(prepared.spec.Argv, want) {
		t.Errorf("argv = %#v, want %#v", prepared.spec.Argv, want)
	}
}

// TestBuildExecJob_ResolvedHostCommandsWired verifies that host_commands
// declared at the project level reach SandboxRuntimeInfo.ResolvedHostCommands
// via the broker register API. Without this wiring the shim bind-mounts are
// silently dropped and host commands run as their raw script inside the
// sandbox (regression: e2e/run.sh executed unshimmed).
func TestBuildExecJob_ResolvedHostCommandsWired(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	projectYAML := "id: proj-hc\nname: proj-hc\n" +
		"host_commands:\n  run-it:\n    path: run.sh\n" +
		"commands:\n  test-cmd:\n    command: [bash]\n    kits:\n      - agent\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	prepared, err := buildExecJob("proj-hc", "test-cmd", nil)
	if err != nil {
		t.Fatalf("buildExecJob: %v", err)
	}

	resolved := prepared.rt.ResolvedHostCommands
	if _, ok := resolved[scriptPath]; !ok {
		t.Fatalf("ResolvedHostCommands missing %q; got keys %v", scriptPath, mapKeys(resolved))
	}
	if got := resolved[scriptPath].Path; got != scriptPath {
		t.Errorf("ResolvedHostCommands[%q].Path = %q, want %q", scriptPath, got, scriptPath)
	}
}

func TestBuildExecJob_UserArgsAppended(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "agent")
	if err := os.MkdirAll(kitDir, 0o755); err != nil {
		t.Fatalf("mkdir kit: %v", err)
	}

	projectYAML := "id: proj-ua\nname: proj-ua\n" +
		"commands:\n  run:\n    command: [claude, --print]\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(""), 0o644); err != nil {
		t.Fatalf("write kit yaml: %v", err)
	}

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	userArgs := []string{"--model", "claude-opus-4-7", "write a test"}
	prepared, err := buildExecJob("proj-ua", "run", userArgs)
	if err != nil {
		t.Fatalf("buildExecJob: %v", err)
	}

	want := []string{"claude", "--print", "--model", "claude-opus-4-7", "write a test"}
	if !reflect.DeepEqual(prepared.spec.Argv, want) {
		t.Errorf("argv = %#v, want %#v", prepared.spec.Argv, want)
	}
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestBuildExecJob_CommandNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeExecTestProject(t, "proj-nc", "Project NC")
	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	setTestSocket(t, ts.Server.SocketPath())

	_, err := buildExecJob("proj-nc", "nonexistent-cmd", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-cmd") {
		t.Errorf("error %q should mention command name", err.Error())
	}
}
