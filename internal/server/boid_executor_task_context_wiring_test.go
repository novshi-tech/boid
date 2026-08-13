package server

import (
	"context"
	"encoding/json"
	"strings"
	"syscall"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// TestBoidBuiltinExecutor_TaskEnvAndPayload_RealRunnerWiring is the
// wiring-invariant guard for Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md):
// every other test in this package drives ExecuteBoidBuiltin against a
// *stub* jobContextProvider, which only proves the executor's own logic is
// correct given a snapshot — it says nothing about whether the real
// *dispatcher.Runner actually gets wired in (wire.go's
// `newBoidBuiltinExecutor(..., runner)` call) and produces a snapshot that
// survives the trip. This test drives a real Runner.Dispatch() (with fake
// sandbox/runtime backends, so no process actually launches) and then feeds
// that exact Runner into boidBuiltinExecutor, closing the "both ends wired,
// but never crossed together" gap the boid-review skill's Lens 1 flags.
func TestBoidBuiltinExecutor_TaskEnvAndPayload_RealRunnerWiring(t *testing.T) {
	d := openTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	runner := &dispatcher.Runner{
		DB:             d.Conn,
		Backend:        &fakeSandboxBackend{},
		AllowedDomains: []string{"github.com"},
	}

	payload := json.RawMessage(`{"artifact":{"claude_code":{"sessions":["s1"]}}}`)
	spec := &orchestrator.JobSpec{
		ProjectID:    "proj-1",
		Argv:         []string{"echo", "hi"},
		Kind:         orchestrator.JobKindHook,
		PrimaryInput: payload,
		// Path is set explicitly (rather than relying on exec.LookPath("gh"))
		// so this test doesn't depend on gh actually being installed on the
		// machine running it — ResolveHostCommands only needs *some* file to
		// exist on host for a host_commands entry with no explicit Path.
		HostCommands: map[string]orchestrator.CommandDef{
			"gh": {Name: "gh", Path: "/bin/echo", AllowedSubcommands: []string{"pr"}},
		},
	}
	jobID, err := runner.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// The exact same *dispatcher.Runner Dispatch just populated is what
	// wire.go threads into newBoidBuiltinExecutor's jobContexts param in
	// production.
	exec := &boidBuiltinExecutor{
		tasks:       &api.TaskAppService{Tasks: orchestrator.NewTaskRepository(d.Conn)},
		jobContexts: runner,
	}
	ctx := sandbox.TokenContext{JobID: jobID, ProjectID: "proj-1"}

	envResp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskEnv,
		JobID: jobID,
	})
	if envResp.ExitCode != 0 {
		t.Fatalf("task env: exit=%d stderr=%q", envResp.ExitCode, envResp.Stderr)
	}
	var view dispatcher.WorkspaceEnvView
	if err := json.Unmarshal([]byte(envResp.Stdout), &view); err != nil {
		t.Fatalf("task env stdout not JSON: %q: %v", envResp.Stdout, err)
	}
	if len(view.AllowedDomains) != 1 || view.AllowedDomains[0] != "github.com" {
		t.Errorf("AllowedDomains = %v, want [github.com] (from Runner.AllowedDomains)", view.AllowedDomains)
	}
	if len(view.HostCommands) != 1 || view.HostCommands[0].Name != "gh" {
		t.Errorf("HostCommands = %+v, want 1 entry named gh (from spec.HostCommands)", view.HostCommands)
	}

	payloadResp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskPayload,
		JobID:     jobID,
		TaskField: "artifact.claude_code.sessions",
	})
	if payloadResp.ExitCode != 0 {
		t.Fatalf("task payload: exit=%d stderr=%q", payloadResp.ExitCode, payloadResp.Stderr)
	}
	if payloadResp.Stdout != `["s1"]` {
		t.Errorf("task payload --field stdout = %q, want the sessions array (from spec.PrimaryInput)", payloadResp.Stdout)
	}

	// After UnregisterJob (the real post-job cleanup path), the same request
	// must fail — the snapshot must not outlive the job.
	runner.UnregisterJob(jobID)
	afterResp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskEnv,
		JobID: jobID,
	})
	if afterResp.ExitCode != 1 || !strings.Contains(afterResp.Stderr, "no context tracked") {
		t.Errorf("after UnregisterJob: exit=%d stderr=%q, want a 'no context tracked' error", afterResp.ExitCode, afterResp.Stderr)
	}
}

// TestBoidBuiltinExecutor_TaskInstructions_RealRunnerWiring_NoCrossJobLeak is
// the end-to-end regression guard for the codex-review finding on PR #797
// (see wiring-seams.md #13): a task's instruction history can address two
// different agents ([claude-code, codex]), and orchestrator.Evaluator fires
// an agent-kind hook for BOTH — but only the hook matching the *active*
// (last) history entry gets a routed instruction; the other gets nil. This
// dispatches two real jobs from the same conceptual task/history (the exact
// JobSpec.Instruction values orchestrator.PlanHook would produce for each —
// see TestPlanHook_Instruction_NonMatchingAgent_ReturnsNil in
// internal/orchestrator/planner_test.go for that half of the chain) through
// a single real *dispatcher.Runner, and asserts `boid task instructions`
// answers each job with ONLY its own data — the claude job never sees the
// codex instruction, even though both jobs are tracked simultaneously in the
// same Runner and (in production) would belong to the same task.
func TestBoidBuiltinExecutor_TaskInstructions_RealRunnerWiring_NoCrossJobLeak(t *testing.T) {
	d := openTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	runner := &dispatcher.Runner{
		DB:      d.Conn,
		Backend: &fakeSandboxBackend{},
	}

	// claude-code hook's JobSpec: Evaluator fired it (claude-code is in the
	// history), but selectInstruction found no match against the active
	// (codex) entry, so Instruction is nil — exactly what PlanHook produces
	// per TestPlanHook_Instruction_NonMatchingAgent_ReturnsNil.
	claudeJobID, err := runner.Dispatch(context.Background(), &orchestrator.JobSpec{
		ProjectID:   "proj-1",
		Argv:        []string{"echo", "hi"},
		Kind:        orchestrator.JobKindHook,
		Instruction: nil,
	}, nil)
	if err != nil {
		t.Fatalf("Dispatch (claude job): %v", err)
	}

	// codex hook's JobSpec: matches the active (last) history entry, so it
	// gets the routed instruction.
	codexJobID, err := runner.Dispatch(context.Background(), &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
		Instruction: &orchestrator.RoutedInstruction{
			Agent:   "codex",
			Message: "do Y",
		},
	}, nil)
	if err != nil {
		t.Fatalf("Dispatch (codex job): %v", err)
	}
	if claudeJobID == codexJobID {
		t.Fatalf("expected distinct job ids, got the same one twice: %q", claudeJobID)
	}

	exec := &boidBuiltinExecutor{jobContexts: runner}

	claudeResp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{JobID: claudeJobID, ProjectID: "proj-1"}, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskInstructions,
		JobID: claudeJobID,
	})
	if claudeResp.ExitCode != 0 {
		t.Fatalf("claude job task instructions: exit=%d stderr=%q", claudeResp.ExitCode, claudeResp.Stderr)
	}
	if strings.TrimSpace(claudeResp.Stdout) != "[]" {
		t.Errorf("claude job task instructions = %q, want empty array — got the codex job's instruction instead (cross-job leak)", claudeResp.Stdout)
	}

	codexResp := exec.ExecuteBoidBuiltin(context.Background(), sandbox.TokenContext{JobID: codexJobID, ProjectID: "proj-1"}, &sandbox.BoidRequest{
		Op:    sandbox.BoidOpTaskInstructions,
		JobID: codexJobID,
	})
	if codexResp.ExitCode != 0 {
		t.Fatalf("codex job task instructions: exit=%d stderr=%q", codexResp.ExitCode, codexResp.Stderr)
	}
	var codexList []orchestrator.RoutedInstruction
	if err := json.Unmarshal([]byte(codexResp.Stdout), &codexList); err != nil {
		t.Fatalf("codex job task instructions stdout not JSON: %q: %v", codexResp.Stdout, err)
	}
	if len(codexList) != 1 || codexList[0].Agent != "codex" || codexList[0].Message != "do Y" {
		t.Errorf("codex job task instructions = %+v, want the codex routed instruction", codexList)
	}
}

// fakeSandboxBackend is a minimal backend.SandboxBackend stub that "starts"
// without launching any real process, so Dispatch's launchSandbox call
// succeeds synchronously — the successor to the pre-PR-4
// fakeSandboxPreparer (dispatcher.SandboxPreparer) + fakeJobRuntime
// (dispatcher.JobRuntime) pair, both userns-only seams removed in PR-4
// (docs/plans/volume-only-daemon.md §論点e). Reused across this package's
// other Runner-DI test files (wire_orphan_runtimes_dir_test.go,
// wire_task_runtimes_dir_test.go) via Config.Backend, the successor to
// their own pre-PR-4 Config.JobRuntime usage.
type fakeSandboxBackend struct{}

var _ backend.SandboxBackend = (*fakeSandboxBackend)(nil)

func (b *fakeSandboxBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return &fakeSandboxSession{id: "runtime-fake"}, nil
}

func (b *fakeSandboxBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}

// EnsureWorkspaceHomeVolume and RunWorkspaceInit satisfy
// dispatcher.WorkspaceInitExecutor — required of every backend since PR5/PR6;
// see workspace_init_stub_test.go.
func (b *fakeSandboxBackend) EnsureWorkspaceHomeVolume(_ context.Context, req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	return ensureWorkspaceHomeVolumeStub(req)
}

func (b *fakeSandboxBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	return runWorkspaceInitStub(ctx, req)
}

func (b *fakeSandboxBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return backend.ReapReport{}, nil
}

// fakeSandboxSession is fakeSandboxBackend's minimal backend.SandboxSession.
type fakeSandboxSession struct{ id string }

var _ backend.SandboxSession = (*fakeSandboxSession)(nil)

func (s *fakeSandboxSession) ID() string { return s.id }
func (s *fakeSandboxSession) Subscribe() (dispatcher.RuntimeSnapshot, <-chan []byte, func(), bool, bool) {
	return dispatcher.RuntimeSnapshot{}, nil, func() {}, false, true
}
func (s *fakeSandboxSession) WriteInput([]byte) error { return dispatcher.ErrRuntimeUnsupported }
func (s *fakeSandboxSession) CloseInput() error       { return dispatcher.ErrRuntimeUnsupported }
func (s *fakeSandboxSession) Resize(backend.TerminalSize) error {
	return dispatcher.ErrRuntimeUnsupported
}
func (s *fakeSandboxSession) Wait(context.Context) (backend.RuntimeExit, error) {
	return backend.RuntimeExit{}, dispatcher.ErrRuntimeUnsupported
}
func (s *fakeSandboxSession) Stop(context.Context) error                   { return nil }
func (s *fakeSandboxSession) Signal(context.Context, syscall.Signal) error { return nil }
