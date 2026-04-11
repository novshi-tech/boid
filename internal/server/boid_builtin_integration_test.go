package server_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/testutil"
)

func TestBoidBuiltinIntegration_RegisterAndCreateAcrossWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)

	project1Dir := writeBoidProject(t, "proj-1", "Project 1")
	project2Dir := writeBoidProject(t, "proj-2", "Project 2")
	project3Dir := writeBoidProject(t, "proj-3", "Project 3")

	for id, dir := range map[string]string{
		"proj-1": project1Dir,
		"proj-2": project2Dir,
		"proj-3": project3Dir,
	} {
		var project orchestrator.Project
		if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
		if project.ID != id {
			t.Fatalf("created project %s had id %q", id, project.ID)
		}
	}

	for _, id := range []string{"proj-1", "proj-2"} {
		var updated orchestrator.Project
		if err := ts.Client.Do("PUT", "/api/projects/"+id+"/workspace", map[string]string{"workspace_id": "ws-1"}, &updated); err != nil {
			t.Fatalf("assign workspace to %s: %v", id, err)
		}
	}
	var updated orchestrator.Project
	if err := ts.Client.Do("PUT", "/api/projects/proj-3/workspace", map[string]string{"workspace_id": "ws-2"}, &updated); err != nil {
		t.Fatalf("assign workspace to proj-3: %v", err)
	}

	var brokerResp struct {
		Token  string `json:"token"`
		Socket string `json:"socket"`
	}
	if err := ts.Client.Do("POST", "/api/broker/register", map[string]any{
		"builtin_policies": orchestrator.DefaultBuiltinPolicies(orchestrator.RoleGate, []string{"boid"}),
		"project_id":       "proj-1",
	}, &brokerResp); err != nil {
		t.Fatalf("register broker commands: %v", err)
	}
	if brokerResp.Token == "" || brokerResp.Socket == "" {
		t.Fatalf("unexpected broker response: %+v", brokerResp)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(project1Dir); err != nil {
		t.Fatalf("chdir project dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	oldToken := os.Getenv("BOID_BROKER_TOKEN")
	oldSocket := os.Getenv("BOID_BROKER_SOCKET")
	if err := os.Setenv("BOID_BROKER_TOKEN", brokerResp.Token); err != nil {
		t.Fatalf("set BOID_BROKER_TOKEN: %v", err)
	}
	if err := os.Setenv("BOID_BROKER_SOCKET", brokerResp.Socket); err != nil {
		t.Fatalf("set BOID_BROKER_SOCKET: %v", err)
	}
	t.Cleanup(func() {
		if oldToken == "" {
			_ = os.Unsetenv("BOID_BROKER_TOKEN")
		} else {
			_ = os.Setenv("BOID_BROKER_TOKEN", oldToken)
		}
		if oldSocket == "" {
			_ = os.Unsetenv("BOID_BROKER_SOCKET")
		} else {
			_ = os.Setenv("BOID_BROKER_SOCKET", oldSocket)
		}
	})

	tmpDir := t.TempDir()

	spec1Path := filepath.Join(tmpDir, "task1.yaml")
	if err := os.WriteFile(spec1Path, []byte("project_id: proj-2\ntitle: peer workspace task\nbehavior: planning\n"), 0o644); err != nil {
		t.Fatalf("write spec1: %v", err)
	}
	resp, err := sandbox.RunBoidShim([]string{"task", "create", "-f", spec1Path})
	if err != nil {
		t.Fatalf("RunBoidShim same workspace: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Fatalf("same-workspace task create exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, "task created:") {
		t.Fatalf("same-workspace stdout = %q, want task created", resp.Stdout)
	}

	var tasks []*orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks?project_id=proj-2", nil, &tasks); err != nil {
		t.Fatalf("list tasks for proj-2: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ProjectID != "proj-2" {
		t.Fatalf("tasks for proj-2 = %+v, want one created task", tasks)
	}

	spec2Path := filepath.Join(tmpDir, "task2.yaml")
	if err := os.WriteFile(spec2Path, []byte("project_id: proj-3\ntitle: cross workspace task\nbehavior: planning\n"), 0o644); err != nil {
		t.Fatalf("write spec2: %v", err)
	}
	resp, err = sandbox.RunBoidShim([]string{"task", "create", "-f", spec2Path})
	if err != nil {
		t.Fatalf("RunBoidShim cross workspace: %v", err)
	}
	if resp.ExitCode == 0 {
		t.Fatalf("cross-workspace task create should fail, stdout=%q stderr=%q", resp.Stdout, resp.Stderr)
	}
	if !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("cross-workspace stderr = %q, want workspace restriction", resp.Stderr)
	}

	tasks = nil
	if err := ts.Client.Do("GET", "/api/tasks?project_id=proj-3", nil, &tasks); err != nil {
		t.Fatalf("list tasks for proj-3: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("tasks for proj-3 = %+v, want none", tasks)
	}
}

func writeBoidProject(t *testing.T, id, name string) string {
	t.Helper()

	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	hooksDir := filepath.Join(boidDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}

	projectYAML := "id: " + id + "\nname: " + name + "\ntask_behaviors:\n  planning:\n    name: Planning\n    transition: standard\n  dev:\n    name: Development\n    transition: standard\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "run-agent.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook: %v", err)
	}

	return dir
}
