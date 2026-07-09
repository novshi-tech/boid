package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// writeHookProject creates a project with a local kit providing a hook.
// Returns workDir.
func writeHookProject(t *testing.T, projectID, projectName string) (workDir string) {
	t.Helper()
	base := t.TempDir()

	workDir = filepath.Join(base, "project")
	boidDir := filepath.Join(workDir, ".boid")
	kitDir := filepath.Join(boidDir, "kits", "hook-kit")
	hooksScriptDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(hooksScriptDir, 0o755); err != nil {
		t.Fatalf("mkdir kit hooks: %v", err)
	}
	testutil.InitGitRepoWithOrigin(t, workDir)
	kitYAML := "hooks:\n  - id: main-hook\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksScriptDir, "main-hook.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	projectYAML := "id: " + projectID + "\nname: " + projectName + "\ntask_behaviors:\n  dev:\n    name: development\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return workDir
}

func resetTaskHookListCmd(t *testing.T) {
	t.Helper()
	taskHookListCmd.ResetFlags()
	taskHookListCmd.Flags().String("status", "", "Status to query hooks for")
}

func resetTaskHookReplayCmd(t *testing.T) {
	t.Helper()
	taskHookReplayCmd.ResetFlags()
	taskHookReplayCmd.Flags().String("status", "", "Override task status for replay")
}

// TestRunTaskHookList_ReturnsMatchingHooks verifies that hook list works for a
// task that has no hooks (returns "no matching hooks"). Hooks are now
// workspace-kit-level and not wired in project.yaml; this test verifies the
// command does not error on a valid project.
func TestRunTaskHookList_ReturnsMatchingHooks(t *testing.T) {
	ts := testutil.NewTestServer(t)

	workDir := writeHookProject(t, "hook-list-proj", "Hook List Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": workDir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "hook-list-proj",
		"title":      "hook list task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Transition task to executing so the hook-list command can query it.
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/actions", map[string]any{"type": "start"}, nil); err != nil {
		t.Fatalf("start task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskHookListCmd(t)

	var out bytes.Buffer
	taskHookListCmd.SetOut(&out)
	if err := runTaskHookList(taskHookListCmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskHookList() error = %v", err)
	}

	got := out.String()
	// The project has no hooks (kits are not declared in project.yaml in the
	// new schema), so "no matching hooks" is the expected output.
	if !strings.Contains(got, "no matching hooks") {
		t.Errorf("output %q should contain 'no matching hooks'", got)
	}
}

// TestRunTaskHookList_NoMatchingHooks verifies the "no matching hooks" message
// when no hooks match the given status.
func TestRunTaskHookList_NoMatchingHooks(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "hook-empty-proj", "Hook Empty Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "hook-empty-proj",
		"title":      "no hooks task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskHookListCmd(t)
	var out bytes.Buffer
	taskHookListCmd.SetOut(&out)

	if err := runTaskHookList(taskHookListCmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskHookList() error = %v", err)
	}
	if !strings.Contains(out.String(), "no matching hooks") {
		t.Errorf("expected 'no matching hooks' in output %q", out.String())
	}
}

// TestRunTaskHookReplay_HookNotFound verifies that replaying a nonexistent hook
// returns an error.
func TestRunTaskHookReplay_HookNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "hook-replay-proj", "Hook Replay Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "hook-replay-proj",
		"title":      "hook replay task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskHookReplayCmd(t)

	err := runTaskHookReplay(taskHookReplayCmd, []string{task.ID, "nonexistent-hook"})
	if err == nil {
		t.Fatal("runTaskHookReplay() expected error for nonexistent hook, got nil")
	}
	if !strings.Contains(err.Error(), "hook replay") {
		t.Errorf("error %q should mention 'hook replay'", err.Error())
	}
}

// TestRunTaskHookReplay_TaskNotFound verifies 404 for an unknown task.
func TestRunTaskHookReplay_TaskNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskHookReplayCmd(t)

	if err := runTaskHookReplay(taskHookReplayCmd, []string{"nonexistent-task-id", "some-hook"}); err == nil {
		t.Fatal("runTaskHookReplay() expected error for nonexistent task, got nil")
	}
}
