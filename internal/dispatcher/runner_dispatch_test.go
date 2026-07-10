package dispatcher_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/testutil"
)

// fakeSandboxPrep is a minimal SandboxPreparer stub for dispatch tests.
// It writes a placeholder outer script on each call.
type fakeSandboxPrep struct {
	dir string
}

func newFakeSandboxPrep(t *testing.T) *fakeSandboxPrep {
	t.Helper()
	return &fakeSandboxPrep{dir: t.TempDir()}
}

func (p *fakeSandboxPrep) PrepareSandbox(_ sandbox.Spec) (*dispatcher.PreparedSandbox, error) {
	specPath := filepath.Join(p.dir, "runner-spec.json")
	if err := os.WriteFile(specPath, []byte("{}"), 0o600); err != nil {
		return nil, fmt.Errorf("write runner spec: %w", err)
	}
	return &dispatcher.PreparedSandbox{
		SpecPath:  specPath,
		StatePath: filepath.Join(p.dir, "runner-state.json"),
	}, nil
}

// newDispatchRunner returns a Runner backed by a fresh in-memory DB with
// project "proj-1" pre-created.
func newDispatchRunner(t *testing.T) (*dispatcher.Runner, *db.DB) {
	t.Helper()
	d := testutil.NewTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &dispatcher.Runner{DB: d.Conn}, d
}

// specWithBadHostCmd returns a JobSpec whose HostCommands entry has a
// non-existent path, causing BuildSandboxSpec to return an error.
func specWithBadHostCmd() *orchestrator.JobSpec {
	return &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo"},
		Kind:      orchestrator.JobKindHook,
		HostCommands: map[string]orchestrator.CommandDef{
			"mytool": {Path: "/nonexistent/path/mytool"},
		},
	}
}

// TestDispatch_BuildSandboxSpecError_MarksJobFailed verifies that when
// BuildSandboxSpec returns an error the job row in the DB is updated to
// status=failed with the error message in Output.
func TestDispatch_BuildSandboxSpecError_MarksJobFailed(t *testing.T) {
	r, d := newDispatchRunner(t)
	r.BoidBinary = "/boid"

	_, err := r.Dispatch(context.Background(), specWithBadHostCmd(), nil)
	if err == nil {
		t.Fatal("expected Dispatch to return an error, got nil")
	}

	jobs, listErr := dispatcher.ListJobsFiltered(d.Conn, dispatcher.JobFilter{Status: string(dispatcher.JobStatusFailed)})
	if listErr != nil {
		t.Fatalf("list failed jobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 failed job, got %d", len(jobs))
	}
	if jobs[0].Output == "" {
		t.Error("failed job Output should contain the error message, got empty string")
	}
	if !strings.Contains(jobs[0].Output, "mytool") {
		t.Errorf("job Output %q should mention the failed command", jobs[0].Output)
	}
}

// TestDispatch_BuildSandboxSpecError_CallsCleanup verifies that the cleanup
// callback is invoked even when BuildSandboxSpec fails.
func TestDispatch_BuildSandboxSpecError_CallsCleanup(t *testing.T) {
	r, _ := newDispatchRunner(t)
	r.BoidBinary = "/boid"

	var called bool
	_, err := r.Dispatch(context.Background(), specWithBadHostCmd(), func() { called = true })
	if err == nil {
		t.Fatal("expected Dispatch to return an error, got nil")
	}
	if !called {
		t.Error("cleanup callback was not called on BuildSandboxSpec error")
	}
}

// TestDispatch_NormalPath_JobIsRunning is a regression guard: a successful
// sandbox launch must leave the job in running state with a non-empty RuntimeID,
// and WaitForJobCtx must deliver the completion result after CompleteJob.
func TestDispatch_NormalPath_JobIsRunning(t *testing.T) {
	r, d := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	r.Runtime = newStatefulRuntime() // defined in runtime_test_helpers_test.go

	spec := &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected non-empty job ID from successful Dispatch")
	}

	job, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != dispatcher.JobStatusRunning {
		t.Errorf("job status = %v, want running immediately after launch", job.Status)
	}
	if job.RuntimeID == "" {
		t.Error("job RuntimeID should be set after successful launch")
	}

	// Deliver completion and verify WaitForJobCtx returns the result.
	r.CompleteJob(jobID, dispatcher.JobCompletionResult{ExitCode: 0})
	result, wErr := r.WaitForJobCtx(context.Background(), jobID)
	if wErr != nil {
		t.Fatalf("WaitForJobCtx: %v", wErr)
	}
	if result.ExitCode != 0 {
		t.Errorf("result ExitCode = %d, want 0", result.ExitCode)
	}
}

// TestDispatch_ExecKindNonInteractive_SetsStdinForward is the git gateway
// cutover's exec-via-Dispatch regression guard: a non-interactive
// JobKindExec job (`boid exec` piped from a non-TTY stdin) must ask the
// runtime for stdin forwarding, so live piped input reaches the sandboxed
// command (see runtime_local_linux.go's StdinForward branch).
func TestDispatch_ExecKindNonInteractive_SetsStdinForward(t *testing.T) {
	r, d := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	runtime := newStatefulRuntime()
	r.Runtime = runtime

	spec := &orchestrator.JobSpec{
		ProjectID:   "proj-1",
		Argv:        []string{"cat"},
		Kind:        orchestrator.JobKindExec,
		HarnessType: "shell",
		Interactive: false,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	job, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	startSpec, ok := runtime.StartSpec(job.RuntimeID)
	if !ok {
		t.Fatalf("no RuntimeStartSpec recorded for runtime %q", job.RuntimeID)
	}
	if !startSpec.StdinForward {
		t.Error("StdinForward = false, want true for a non-interactive JobKindExec dispatch")
	}
}

// TestDispatch_HookKindNonInteractive_LeavesStdinForwardFalse guards the
// other side of the same carve-out: a non-interactive hook job must NOT ask
// for stdin forwarding — a hook script probing stdin must keep observing an
// immediate EOF (see RuntimeStartSpec.StdinForward's doc comment), not block
// on a forwarder nothing will ever attach with real input.
func TestDispatch_HookKindNonInteractive_LeavesStdinForwardFalse(t *testing.T) {
	r, d := newDispatchRunner(t)
	r.Sandbox = newFakeSandboxPrep(t)
	runtime := newStatefulRuntime()
	r.Runtime = runtime

	spec := &orchestrator.JobSpec{
		ProjectID:   "proj-1",
		Argv:        []string{"echo", "hi"},
		Kind:        orchestrator.JobKindHook,
		Interactive: false,
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	job, err := dispatcher.GetJob(d.Conn, jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	startSpec, ok := runtime.StartSpec(job.RuntimeID)
	if !ok {
		t.Fatalf("no RuntimeStartSpec recorded for runtime %q", job.RuntimeID)
	}
	if startSpec.StdinForward {
		t.Error("StdinForward = true, want false for a hook job")
	}
}
