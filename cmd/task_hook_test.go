package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
)

// hookTestServer holds a running server with a kits directory configured.
type hookTestServer struct {
	Server *server.Server
	Client *client.Client
}

// newHookTestServerWithKitsDir starts a server with a custom kits base directory.
func newHookTestServerWithKitsDir(t *testing.T, kitsDir string) *hookTestServer {
	t.Helper()
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    kitsDir,
	}
	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return &hookTestServer{Server: srv, Client: client.NewUnixClient(sockPath)}
}

// writeHookKitProject creates a kits directory with a simple hook kit and a
// project that references it. Returns (workDir, kitsDir).
func writeHookKitProject(t *testing.T, projectID, projectName string) (workDir, kitsDir string) {
	t.Helper()
	base := t.TempDir()

	kitsDir = filepath.Join(base, "kits")
	kitDir := filepath.Join(kitsDir, "local", "hook-kit")
	hooksScriptDir := filepath.Join(kitDir, "hooks")
	if err := os.MkdirAll(hooksScriptDir, 0o755); err != nil {
		t.Fatalf("mkdir kit hooks: %v", err)
	}
	kitYAML := "hooks:\n  - id: main-hook\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hooksScriptDir, "main-hook.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write hook script: %v", err)
	}

	workDir = filepath.Join(base, "project")
	boidDir := filepath.Join(workDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	projectYAML := "id: " + projectID + "\nname: " + projectName + "\ntask_behaviors:\n  dev:\n    name: development\n    kits:\n      - local/hook-kit\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return workDir, kitsDir
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

// TestRunTaskHookList_ReturnsMatchingHooks verifies that hooks from a kit are
// listed when they match the executing status.
func TestRunTaskHookList_ReturnsMatchingHooks(t *testing.T) {
	workDir, kitsDir := writeHookKitProject(t, "hook-list-proj", "Hook List Project")
	ts := newHookTestServerWithKitsDir(t, kitsDir)

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

	// Transition task to executing so hooks match
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
	if !strings.Contains(got, "main-hook") {
		t.Errorf("output %q should contain main-hook", got)
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
