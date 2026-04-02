package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/testutil"
)

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

	oldSocket := os.Getenv("BOID_SOCKET")
	if err := os.Setenv("BOID_SOCKET", ts.Server.SocketPath()); err != nil {
		t.Fatalf("set BOID_SOCKET: %v", err)
	}
	defer func() {
		if oldSocket == "" {
			_ = os.Unsetenv("BOID_SOCKET")
			return
		}
		_ = os.Setenv("BOID_SOCKET", oldSocket)
	}()

	req, err := buildExecRequest("proj-1")
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

	oldSocket := os.Getenv("BOID_SOCKET")
	if err := os.Setenv("BOID_SOCKET", ts.Server.SocketPath()); err != nil {
		t.Fatalf("set BOID_SOCKET: %v", err)
	}
	defer func() {
		if oldSocket == "" {
			_ = os.Unsetenv("BOID_SOCKET")
			return
		}
		_ = os.Setenv("BOID_SOCKET", oldSocket)
	}()

	req, err := buildExecRequest("proj-1")
	if err != nil {
		t.Fatalf("buildExecRequest: %v", err)
	}
	if req.BrokerSocket == "" || req.BrokerToken == "" {
		t.Fatalf("expected broker registration for boid builtin, got socket=%q token=%q", req.BrokerSocket, req.BrokerToken)
	}
}

func writeExecTestProject(t *testing.T, id, name string) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}

	projectYAML := "id: " + id + "\nname: " + name + "\ntask_behaviors:\n  impl:\n    name: implementation\n    transition: standard\nhooks:\n  - id: run-agent\n    on: executing\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "run-agent.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	return dir
}

func writeExecTestProjectWithBoidBuiltin(t *testing.T, id, name string) string {
	t.Helper()

	dir := writeExecTestProject(t, id, name)
	projectYAML := "id: " + id + "\nname: " + name + "\nbuiltin_commands:\n  - boid\n"
	boidDir := filepath.Join(dir, ".boid")
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	return dir
}
