package dispatcher_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/testutil"
)

// TestRunnerDispatch_StagesGatesPerJob ensures each gate Dispatch stages
// scripts under a jobID-unique path, preventing same-task sibling gates
// from racing to delete each other's staging dir.
func TestRunnerDispatch_StagesGatesPerJob(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        "task-gate-staging",
		ProjectID: "proj-1",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	kitGatesDir := t.TempDir()
	scriptPath := filepath.Join(kitGatesDir, "mergeable-check.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/bash\necho ok\n"), 0o755); err != nil {
		t.Fatalf("write kit script: %v", err)
	}

	preparer := &fakeSandboxPreparer{}
	runner := &dispatcher.Runner{
		DB:      db.Conn,
		Runtime: newStatefulRuntime(),
		Broker:  &fakeBroker{socketPath: "/tmp/fake-broker.sock", tokens: []string{"t1", "t2"}},
		Sandbox: preparer,
	}

	request := &orchestrator.DispatchRequest{
		TaskID:          "task-gate-staging",
		ProjectID:       "proj-1",
		HandlerID:       "github-auto-merge/mergeable-check",
		Role:            orchestrator.RoleGate,
		ProjectDir:      projectDir,
		ProjectGatesDir: filepath.Join(projectDir, ".boid", "gates"),
		KitGatesDirs:    []orchestrator.KitGatesInfo{{GatesDir: kitGatesDir}},
		HookScript:      "mergeable-check.sh",
		BoidBinary:      "/bin/true",
		ServerSocket:    "/tmp/boid.sock",
	}

	job1, err := runner.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}
	job2, err := runner.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}

	if job1 == job2 {
		t.Fatalf("two Dispatch calls returned the same jobID %q", job1)
	}
	if len(preparer.calls) != 2 {
		t.Fatalf("PrepareSandbox calls = %d, want 2", len(preparer.calls))
	}

	stagingOf := func(spec sandbox.Spec) string {
		if len(spec.CleanupPaths) == 0 {
			return ""
		}
		return spec.CleanupPaths[0]
	}
	spec1, spec2 := preparer.calls[0], preparer.calls[1]
	staging1, staging2 := stagingOf(spec1), stagingOf(spec2)

	for i, p := range []struct {
		spec  sandbox.Spec
		stage string
	}{{spec1, staging1}, {spec2, staging2}} {
		if p.stage == "" {
			t.Fatalf("spec[%d] staging dir missing from CleanupPaths", i)
		}
		if !strings.Contains(filepath.Base(p.stage), p.spec.ID) {
			t.Fatalf("spec[%d] staging %q does not include jobID %q", i, p.stage, p.spec.ID)
		}
		if _, err := os.Stat(filepath.Join(p.stage, "mergeable-check.sh")); err != nil {
			t.Fatalf("spec[%d] staged script missing: %v", i, err)
		}
	}

	if staging1 == staging2 {
		t.Fatalf("sibling gates share staging dir %q — race hazard", staging1)
	}
}
