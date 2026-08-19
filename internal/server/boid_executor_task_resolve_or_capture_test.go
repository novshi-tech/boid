package server

// docs/plans/ingestion-identity.md PR-2 (B-2): boid_executor.go's
// task_resolve_or_capture case. Broker-side scoping (project_id) is covered
// separately (internal/sandbox's TestBroker_BoidTaskResolveOrCapture_*) —
// these pin the store-error -> ExecResponse translation (the shared
// IdentityConflictExitCode with BoidOpTaskIdentityLink, and the generic
// ExitCode:1 path for everything else), the request forwarding (Title/
// Description/ProjectID/Identity all reach TaskWorkflowService.
// ResolveOrCapture unmodified), and the "unavailable" guard when the
// workflow value doesn't implement resolveOrCaptureService.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakeResolveOrCaptureService is a minimal resolveOrCaptureService double.
type fakeResolveOrCaptureService struct {
	calls  []api.ResolveOrCaptureRequest
	result *api.ResolveOrCaptureResult
	err    error
}

func (f *fakeResolveOrCaptureService) ResolveOrCapture(ctx context.Context, req api.ResolveOrCaptureRequest) (*api.ResolveOrCaptureResult, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func TestBoidBuiltinExecutor_ResolveOrCapture_RequiresIdentity(t *testing.T) {
	fake := &fakeResolveOrCaptureService{}
	exec := &boidBuiltinExecutor{resolveOrCapture: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskResolveOrCapture,
		ProjectID: "proj-1",
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for missing identity")
	}
	if len(fake.calls) != 0 {
		t.Fatal("service should not be called when identity is missing")
	}
}

func TestBoidBuiltinExecutor_ResolveOrCapture_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{} // resolveOrCapture left nil
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskResolveOrCapture,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "unavailable") {
		t.Errorf("stderr = %q, want it to mention unavailable", resp.Stderr)
	}
}

func TestBoidBuiltinExecutor_ResolveOrCapture_ForwardsRequestAndReturnsCreated(t *testing.T) {
	fake := &fakeResolveOrCaptureService{result: &api.ResolveOrCaptureResult{TaskID: "t1", Created: true}}
	exec := &boidBuiltinExecutor{resolveOrCapture: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpTaskResolveOrCapture,
		ProjectID:   "proj-1",
		Identity:    "jira:X-1",
		Title:       "something broke",
		Description: "the body",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"task_id":"t1"`) || !strings.Contains(resp.Stdout, `"created":true`) {
		t.Fatalf("stdout = %q, want it to carry task_id and created:true", resp.Stdout)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("service calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	want := api.ResolveOrCaptureRequest{ProjectID: "proj-1", Identity: "jira:X-1", Title: "something broke", Description: "the body"}
	if got != want {
		t.Errorf("forwarded request = %+v, want %+v", got, want)
	}
}

func TestBoidBuiltinExecutor_ResolveOrCapture_ReturnsCreatedFalse(t *testing.T) {
	fake := &fakeResolveOrCaptureService{result: &api.ResolveOrCaptureResult{TaskID: "existing-task", Created: false}}
	exec := &boidBuiltinExecutor{resolveOrCapture: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskResolveOrCapture,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"task_id":"existing-task"`) || !strings.Contains(resp.Stdout, `"created":false`) {
		t.Fatalf("stdout = %q, want it to carry task_id and created:false", resp.Stdout)
	}
}

// TestBoidBuiltinExecutor_ResolveOrCapture_ConflictSurfacesAsDistinctExitCode
// pins the design doc's explicit contract: "identity 衝突時は PR-1 の
// ErrIdentityConflict をそのまま返す" — via the SAME distinguished exit code
// BoidOpTaskIdentityLink uses, not the generic ExitCode:1 path.
func TestBoidBuiltinExecutor_ResolveOrCapture_ConflictSurfacesAsDistinctExitCode(t *testing.T) {
	fake := &fakeResolveOrCaptureService{err: orchestrator.ErrIdentityConflict}
	exec := &boidBuiltinExecutor{resolveOrCapture: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskResolveOrCapture,
		ProjectID: "proj-1",
		Identity:  "jira:TAKEN-1",
	})
	if resp.ExitCode != sandbox.IdentityConflictExitCode {
		t.Fatalf("exit code = %d, want IdentityConflictExitCode (%d)", resp.ExitCode, sandbox.IdentityConflictExitCode)
	}
}

// TestBoidBuiltinExecutor_ResolveOrCapture_OtherErrorStaysGeneric confirms a
// NON-conflict error still uses the ordinary ExitCode:1 error path — only
// ErrIdentityConflict gets the special code.
func TestBoidBuiltinExecutor_ResolveOrCapture_OtherErrorStaysGeneric(t *testing.T) {
	fake := &fakeResolveOrCaptureService{err: errors.New("db exploded")}
	exec := &boidBuiltinExecutor{resolveOrCapture: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskResolveOrCapture,
		ProjectID: "proj-1",
		Identity:  "jira:X-1",
	})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1 for a non-conflict error", resp.ExitCode)
	}
	if !strings.Contains(resp.Stderr, "db exploded") {
		t.Fatalf("stderr = %q, want it to surface the underlying error", resp.Stderr)
	}
}

// TestNewBoidBuiltinExecutor_WiresResolveOrCaptureFromWorkflow pins the
// runtime interface-check wiring resolveOrCaptureService's own doc comment
// describes: newBoidBuiltinExecutor populates the resolveOrCapture field
// from the SAME workflow value passed in, with no separate constructor
// parameter and no wire.go change required.
func TestNewBoidBuiltinExecutor_WiresResolveOrCaptureFromWorkflow(t *testing.T) {
	workflow := &api.TaskWorkflowService{} // *api.TaskWorkflowService implements resolveOrCaptureService
	got := newBoidBuiltinExecutor(workflow, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.resolveOrCapture == nil {
		t.Fatal("resolveOrCapture was not wired from the workflow value")
	}
}

// TestNewBoidBuiltinExecutor_ResolveOrCaptureNilWhenWorkflowDoesNotImplementIt
// confirms a WorkflowService test double that does NOT implement
// ResolveOrCapture leaves the field nil rather than panicking — this is what
// lets the 3 pre-existing WorkflowService test doubles in this package and
// internal/api/web_test.go keep compiling with no new pass-through method.
func TestNewBoidBuiltinExecutor_ResolveOrCaptureNilWhenWorkflowDoesNotImplementIt(t *testing.T) {
	got := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.resolveOrCapture != nil {
		t.Fatal("resolveOrCapture should stay nil for a workflow double that doesn't implement it")
	}
}
