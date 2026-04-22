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

// gateTestServer holds a running server with a kits directory configured.
type gateTestServer struct {
	Server *server.Server
	Client *client.Client
}

// newTestServerWithKitsDir starts a server with a custom kits base directory.
// Use this for tests that need gate definitions loaded from kits.
func newTestServerWithKitsDir(t *testing.T, kitsDir string) *gateTestServer {
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
	return &gateTestServer{Server: srv, Client: client.NewUnixClient(sockPath)}
}

// writeGateKitProject creates a kits directory with a simple gate kit and a
// project that references it. Returns (workDir, kitsDir).
// The kit defines "check-gate" (exit, verifying) and "setup-gate" (entry, executing).
func writeGateKitProject(t *testing.T, projectID, projectName string) (workDir, kitsDir string) {
	t.Helper()
	base := t.TempDir()

	// Kit dir: kitsDir/local/gate-kit/{kit.yaml,gates/check-gate.sh,...}
	kitsDir = filepath.Join(base, "kits")
	kitDir := filepath.Join(kitsDir, "local", "gate-kit")
	gatesScriptDir := filepath.Join(kitDir, "gates")
	if err := os.MkdirAll(gatesScriptDir, 0o755); err != nil {
		t.Fatalf("mkdir kit gates: %v", err)
	}
	kitYAML := "gates:\n  - id: check-gate\n    on: [verifying]\n    phase: exit\n  - id: setup-gate\n    on: [executing]\n    phase: entry\n"
	if err := os.WriteFile(filepath.Join(kitDir, "kit.yaml"), []byte(kitYAML), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
	// Gate scripts must exist at kit load time (they are never executed in these tests).
	for _, name := range []string{"check-gate.sh", "setup-gate.sh"} {
		if err := os.WriteFile(filepath.Join(gatesScriptDir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write gate script %s: %v", name, err)
		}
	}

	// Project dir: workDir/.boid/project.yaml referencing the kit
	workDir = filepath.Join(base, "project")
	boidDir := filepath.Join(workDir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	projectYAML := "id: " + projectID + "\nname: " + projectName + "\ntask_behaviors:\n  dev:\n    name: development\n    kits:\n      - local/gate-kit\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return workDir, kitsDir
}

func resetTaskGateListCmd(t *testing.T) {
	t.Helper()
	taskGateListCmd.ResetFlags()
	taskGateListCmd.Flags().String("status", "", "Status to query gates for")
}

func resetTaskGateReplayCmd(t *testing.T) {
	t.Helper()
	taskGateReplayCmd.ResetFlags()
	taskGateReplayCmd.Flags().String("status", "", "Override task status for replay")
}

// TestRunTaskGateList_ReturnsMatchingGates verifies that gates from a kit are
// listed when they match the queried status.
func TestRunTaskGateList_ReturnsMatchingGates(t *testing.T) {
	workDir, kitsDir := writeGateKitProject(t, "gate-list-proj", "Gate List Project")
	ts := newTestServerWithKitsDir(t, kitsDir)

	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": workDir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "gate-list-proj",
		"title":      "gate list task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskGateListCmd(t)

	var out bytes.Buffer
	taskGateListCmd.SetOut(&out)
	if err := taskGateListCmd.Flags().Set("status", "verifying"); err != nil {
		t.Fatalf("set --status: %v", err)
	}
	if err := runTaskGateList(taskGateListCmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskGateList() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "check-gate") {
		t.Errorf("output %q should contain check-gate", got)
	}
	// setup-gate is entry/executing; must not appear for verifying.
	if strings.Contains(got, "setup-gate") {
		t.Errorf("setup-gate must not appear for verifying status: %q", got)
	}
}

// TestRunTaskGateList_StatusOverride verifies that --status queries a different
// status than the task's current one.
func TestRunTaskGateList_StatusOverride(t *testing.T) {
	workDir, kitsDir := writeGateKitProject(t, "gate-status-proj", "Gate Status Project")
	ts := newTestServerWithKitsDir(t, kitsDir)

	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": workDir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "gate-status-proj",
		"title":      "status override task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	// Query executing gates: should return setup-gate (entry/executing) only.
	resetTaskGateListCmd(t)
	var out bytes.Buffer
	taskGateListCmd.SetOut(&out)
	if err := taskGateListCmd.Flags().Set("status", "executing"); err != nil {
		t.Fatalf("set --status: %v", err)
	}
	if err := runTaskGateList(taskGateListCmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskGateList() error = %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "setup-gate") {
		t.Errorf("expected setup-gate in output %q", got)
	}
	if strings.Contains(got, "check-gate") {
		t.Errorf("check-gate must not appear for executing status: %q", got)
	}
}

// TestRunTaskGateList_NoMatchingGates verifies the "no matching gates" message
// when no gates match the given status.
func TestRunTaskGateList_NoMatchingGates(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "gate-empty-proj", "Gate Empty Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "gate-empty-proj",
		"title":      "no gates task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskGateListCmd(t)
	var out bytes.Buffer
	taskGateListCmd.SetOut(&out)

	if err := runTaskGateList(taskGateListCmd, []string{task.ID}); err != nil {
		t.Fatalf("runTaskGateList() error = %v", err)
	}
	if !strings.Contains(out.String(), "no matching gates") {
		t.Errorf("expected 'no matching gates' in output %q", out.String())
	}
}

// TestRunTaskGateReplay_GateNotFound verifies that replaying a nonexistent gate
// returns an error.
func TestRunTaskGateReplay_GateNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "gate-replay-proj", "Gate Replay Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "gate-replay-proj",
		"title":      "gate replay task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskGateReplayCmd(t)

	err := runTaskGateReplay(taskGateReplayCmd, []string{task.ID, "nonexistent-gate"})
	if err == nil {
		t.Fatal("runTaskGateReplay() expected error for nonexistent gate, got nil")
	}
	if !strings.Contains(err.Error(), "gate replay") {
		t.Errorf("error %q should mention 'gate replay'", err.Error())
	}
}

// TestRunTaskGateReplay_TaskNotFound verifies 404 for an unknown task.
func TestRunTaskGateReplay_TaskNotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskGateReplayCmd(t)

	if err := runTaskGateReplay(taskGateReplayCmd, []string{"nonexistent-task-id", "some-gate"}); err == nil {
		t.Fatal("runTaskGateReplay() expected error for nonexistent task, got nil")
	}
}

// TestRunTaskGateReplay_StatusFlagWired verifies that --status is forwarded to
// the server. The server updates the task status before gate lookup, so even
// when the gate is not found the status override is persisted.
func TestRunTaskGateReplay_StatusFlagWired(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeImportTestProject(t, "gate-status-flag-proj", "Gate Status Flag Project")
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var task orchestrator.Task
	if err := ts.Client.Do("POST", "/api/tasks", map[string]any{
		"project_id": "gate-status-flag-proj",
		"title":      "status flag task",
		"behavior":   "dev",
	}, &task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Abort so we can test recovery via --status.
	if err := ts.Client.Do("POST", "/api/tasks/"+task.ID+"/actions", map[string]any{"type": "abort"}, nil); err != nil {
		t.Fatalf("abort task: %v", err)
	}

	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetTaskGateReplayCmd(t)
	if err := taskGateReplayCmd.Flags().Set("status", "reworking"); err != nil {
		t.Fatalf("set --status: %v", err)
	}

	// Gate doesn't exist → error, but status override is applied before the lookup.
	_ = runTaskGateReplay(taskGateReplayCmd, []string{task.ID, "nonexistent-gate"})

	var updated orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+task.ID, nil, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if updated.Status != orchestrator.TaskStatusReworking {
		t.Errorf("task status = %q, want reworking; --status flag may not be wired", updated.Status)
	}
}
