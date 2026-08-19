package server

// docs/plans/ingestion-identity.md PR-3 (B-3): boid_executor.go's
// action_list case. Broker-side scoping (project_id/workspace_id) is covered
// separately (internal/sandbox's TestBroker_BoidActionList_*) — these pin the
// filter-building branches (project_id / workspace_id / neither ->
// AllowedProjectIDs), the "unavailable" guard when the workflow value
// doesn't implement actionListService, and the runtime interface-check
// wiring (PR-1/PR-2 review note #4: pin that the PRODUCTION wire — passing a
// real *api.TaskWorkflowService — actually satisfies the narrow interface,
// not just a test double built to satisfy it).

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// fakeActionListService is a minimal actionListService double.
type fakeActionListService struct {
	calls  []orchestrator.ActionListFilter
	result *api.ActionListResult
	err    error
}

func (f *fakeActionListService) ListActions(filter orchestrator.ActionListFilter) (*api.ActionListResult, error) {
	f.calls = append(f.calls, filter)
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

func TestBoidBuiltinExecutor_ActionList_Unavailable(t *testing.T) {
	exec := &boidBuiltinExecutor{} // actionList left nil
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpActionList})
	if resp.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", resp.ExitCode)
	}
	if resp.Stderr == "" {
		t.Error("expected an unavailable error message")
	}
}

// TestBoidBuiltinExecutor_ActionList_ExplicitProjectID pins the
// req.ProjectID != "" branch: filter.ProjectIDs is a 1-element slice of the
// (already broker-checked) project, and the executor's own AllowsProject
// re-check (defense in depth) passes for a project inside the token's scope.
func TestBoidBuiltinExecutor_ActionList_ExplicitProjectID(t *testing.T) {
	fake := &fakeActionListService{result: &api.ActionListResult{Actions: nil, NextCursor: ""}}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpActionList,
		ProjectID: "proj-1",
		TaskID:    "task-1",
		Since:     "cursor-1",
		Limit:     10,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("service calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if len(got.ProjectIDs) != 1 || got.ProjectIDs[0] != "proj-1" {
		t.Errorf("ProjectIDs = %v, want [proj-1]", got.ProjectIDs)
	}
	if got.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want empty", got.WorkspaceID)
	}
	if got.TaskID != "task-1" || got.Since != "cursor-1" || got.Limit != 10 {
		t.Errorf("filter = %+v, want TaskID=task-1 Since=cursor-1 Limit=10", got)
	}
}

// TestBoidBuiltinExecutor_ActionList_ExplicitProjectIDOutsideScopeDenied is
// the executor-side defense-in-depth re-check (a handwritten request that
// bypassed the shim/broker) — mirrors BoidOpTaskTriageList's own
// AllowsProject re-check.
func TestBoidBuiltinExecutor_ActionList_ExplicitProjectIDOutsideScopeDenied(t *testing.T) {
	fake := &fakeActionListService{}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpActionList,
		ProjectID: "proj-elsewhere",
	})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for an out-of-scope project_id")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("service should not be called for a denied project, calls=%+v", fake.calls)
	}
}

// TestBoidBuiltinExecutor_ActionList_WorkspaceID pins the req.WorkspaceID
// branch — filter.WorkspaceID is forwarded, ProjectIDs stays empty.
func TestBoidBuiltinExecutor_ActionList_WorkspaceID(t *testing.T) {
	fake := &fakeActionListService{result: &api.ActionListResult{}}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", WorkspaceID: "ws-1"}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{
		Op:          sandbox.BoidOpActionList,
		WorkspaceID: "ws-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("service calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0]
	if got.WorkspaceID != "ws-1" || len(got.ProjectIDs) != 0 {
		t.Errorf("filter = %+v, want WorkspaceID=ws-1 and no ProjectIDs", got)
	}
}

// TestBoidBuiltinExecutor_ActionList_NoFilter_FallsBackToAllowedProjectIDs
// pins the "無指定時は AllowedProjectIDs を回して決して無スコープで引かない"
// invariant (B-3) at the executor layer: with neither project_id nor
// workspace_id given, filter.ProjectIDs is set to the token's own
// AllowedProjectIDs — never left empty (which orchestrator.ListActionsSince
// would otherwise treat as unscoped).
func TestBoidBuiltinExecutor_ActionList_NoFilter_FallsBackToAllowedProjectIDs(t *testing.T) {
	fake := &fakeActionListService{result: &api.ActionListResult{}}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1", "proj-2"}}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpActionList})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("service calls = %d, want 1", len(fake.calls))
	}
	got := fake.calls[0].ProjectIDs
	if len(got) != 2 || got[0] != "proj-1" || got[1] != "proj-2" {
		t.Errorf("ProjectIDs = %v, want [proj-1 proj-2]", got)
	}
}

// TestBoidBuiltinExecutor_ActionList_NoFilterNoAllowedProjectIDs_FallsBackToProjectID
// covers the AllowedProjectIDs-empty insurance branch, mirroring
// BoidOpTaskTriageList's own.
func TestBoidBuiltinExecutor_ActionList_NoFilterNoAllowedProjectIDs_FallsBackToProjectID(t *testing.T) {
	fake := &fakeActionListService{result: &api.ActionListResult{}}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1"}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpActionList})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	got := fake.calls[0].ProjectIDs
	if len(got) != 1 || got[0] != "proj-1" {
		t.Errorf("ProjectIDs = %v, want [proj-1]", got)
	}
}

// TestBoidBuiltinExecutor_ActionList_NoScopeAtAllDenied is the last-resort
// guard (Opus review round 2 precedent on BoidOpTaskTriageList): no
// project_id/workspace_id given AND no AllowedProjectIDs AND no
// ctx.ProjectID — refused outright rather than reaching the store unscoped.
func TestBoidBuiltinExecutor_ActionList_NoScopeAtAllDenied(t *testing.T) {
	fake := &fakeActionListService{}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{Op: sandbox.BoidOpActionList})
	if resp.ExitCode == 0 {
		t.Fatal("expected rejection for a completely unscoped request")
	}
	if len(fake.calls) != 0 {
		t.Fatalf("service should not be called when unscoped, calls=%+v", fake.calls)
	}
}

// TestBoidBuiltinExecutor_ActionList_ReturnsActionsAndCursor pins the
// response shape: the JSON body carries both "actions" and "next_cursor".
func TestBoidBuiltinExecutor_ActionList_ReturnsActionsAndCursor(t *testing.T) {
	fake := &fakeActionListService{result: &api.ActionListResult{
		Actions:    []*orchestrator.Action{{ID: "a1", TaskID: "t1", Type: "noted"}},
		NextCursor: "2026-08-19T00:00:00Z|a1",
	}}
	exec := &boidBuiltinExecutor{actionList: fake}
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(t.Context(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpActionList,
		ProjectID: "proj-1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", resp.ExitCode, resp.Stderr)
	}
	if !strings.Contains(resp.Stdout, `"id": "a1"`) {
		t.Errorf("stdout missing action id: %s", resp.Stdout)
	}
	if !strings.Contains(resp.Stdout, `"next_cursor": "2026-08-19T00:00:00Z|a1"`) {
		t.Errorf("stdout missing next_cursor: %s", resp.Stdout)
	}
}

// TestNewBoidBuiltinExecutor_WiresActionListFromWorkflow pins the runtime
// interface-check wiring actionListService's own doc comment describes:
// newBoidBuiltinExecutor populates the actionList field from the SAME
// workflow value passed in — with a REAL *api.TaskWorkflowService (the
// production wire), not a hand-built test double, per PR-1/PR-2 review note
// #4 ("テストが具象型を直接渡し本番の wire だけアサーションを通る構図だと、
// テストが通るのに本番で動かない" — this is exactly the check that closes
// that gap).
func TestNewBoidBuiltinExecutor_WiresActionListFromWorkflow(t *testing.T) {
	workflow := &api.TaskWorkflowService{} // *api.TaskWorkflowService implements actionListService
	got := newBoidBuiltinExecutor(workflow, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.actionList == nil {
		t.Fatal("actionList was not wired from the workflow value")
	}
}

// TestNewBoidBuiltinExecutor_ActionListNilWhenWorkflowDoesNotImplementIt
// confirms a WorkflowService test double that does NOT implement ListActions
// leaves the field nil rather than panicking.
func TestNewBoidBuiltinExecutor_ActionListNilWhenWorkflowDoesNotImplementIt(t *testing.T) {
	got := newBoidBuiltinExecutor(&recordingWorkflow{}, nil, nil, nil, nil, "", nil)
	exec, ok := got.(*boidBuiltinExecutor)
	if !ok {
		t.Fatalf("newBoidBuiltinExecutor returned %T, want *boidBuiltinExecutor", got)
	}
	if exec.actionList != nil {
		t.Fatal("actionList should stay nil for a workflow double that doesn't implement it")
	}
}
