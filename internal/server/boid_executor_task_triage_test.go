package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md 決定14):
// brokered triage read surface ----
//
// These pin S5 (読み戻しの完全性) at the transport layer. The daemon becoming
// the sole source of truth for triage state is only viable if the workspace
// side can read it back — and reading it back must not become a way to read
// ANOTHER workspace's triage state, which is the one thing 決定2 exists to
// prevent.

func triageExec(store *capturingTaskStore, workflow *recordingWorkflow) *boidBuiltinExecutor {
	return &boidBuiltinExecutor{
		tasks:    &api.TaskAppService{Tasks: store},
		workflow: workflow,
	}
}

// TestBoidBuiltinExecutor_TaskTriageGet_ReturnsProjection pins the happy path:
// a caller inside its own workspace gets the full projection as JSON,
// including the actions-derived parked_from.
func TestBoidBuiltinExecutor_TaskTriageGet_ReturnsProjection(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "t1", ProjectID: "proj-1", Status: orchestrator.TaskStatusParked},
	}}
	workflow := &recordingWorkflow{triageViews: map[string]*api.CardView{
		"t1": {
			TaskID:     "t1",
			ProjectID:  "proj-1",
			Status:     orchestrator.TaskStatusParked,
			Urgency:    "today",
			ParkedFrom: orchestrator.TaskStatusReady,
			Detail:     json.RawMessage(`{"attrs":{"summary":"s"}}`),
		},
	}}
	exec := triageExec(store, workflow)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskTriageGet,
		TaskID: "t1",
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	var view api.CardView
	if err := json.Unmarshal([]byte(resp.Stdout), &view); err != nil {
		t.Fatalf("stdout is not the view JSON (%v): %s", err, resp.Stdout)
	}
	if view.ParkedFrom != orchestrator.TaskStatusReady {
		t.Fatalf("parked_from = %q, want ready (the derived field is the point of this op)", view.ParkedFrom)
	}
	if view.Urgency != "today" {
		t.Fatalf("urgency = %q, want today", view.Urgency)
	}
}

// TestBoidBuiltinExecutor_TaskTriageGet_CrossWorkspaceDenied is the escape
// test: knowing another workspace's task UUID must not reveal its triage
// state (title/summary/children are exactly the cross-workspace leak 決定2
// forbids). The check must happen BEFORE the workflow call, so nothing is
// even read.
func TestBoidBuiltinExecutor_TaskTriageGet_CrossWorkspaceDenied(t *testing.T) {
	store := &capturingTaskStore{created: []*orchestrator.Task{
		{ID: "other", ProjectID: "proj-2", Status: orchestrator.TaskStatusTriaged},
	}}
	workflow := &recordingWorkflow{}
	exec := triageExec(store, workflow)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskTriageGet,
		TaskID: "other",
	})
	if resp.ExitCode == 0 {
		t.Fatalf("cross-workspace triage read succeeded: %s", resp.Stdout)
	}
	if !strings.Contains(resp.Stderr, "restricted to the current workspace") {
		t.Fatalf("stderr = %q, want the workspace restriction message", resp.Stderr)
	}
	if len(workflow.triageGets) != 0 {
		t.Fatalf("workflow.GetCard was called for a denied task: %v", workflow.triageGets)
	}
}

// TestBoidBuiltinExecutor_TaskTriageList_CrossWorkspaceProjectDenied pins the
// same gate on the list form's explicit project filter.
func TestBoidBuiltinExecutor_TaskTriageList_CrossWorkspaceProjectDenied(t *testing.T) {
	workflow := &recordingWorkflow{}
	exec := triageExec(&capturingTaskStore{}, workflow)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:        sandbox.BoidOpTaskTriageList,
		ProjectID: "proj-2",
	})
	if resp.ExitCode == 0 {
		t.Fatalf("cross-workspace triage list succeeded: %s", resp.Stdout)
	}
	if len(workflow.triageFilters) != 0 {
		t.Fatalf("workflow.ListCards was called for a denied project: %v", workflow.triageFilters)
	}
}

// TestBoidBuiltinExecutor_TaskTriageList_UnfilteredStaysScoped pins that a
// list with NO filter does not run unscoped: it iterates the token's allowed
// projects, exactly as BoidOpTaskList does. An unscoped ListCards would hand
// every workspace's queue to any sandbox.
func TestBoidBuiltinExecutor_TaskTriageList_UnfilteredStaysScoped(t *testing.T) {
	workflow := &recordingWorkflow{triageList: []*api.CardView{
		{TaskID: "a", ProjectID: "proj-1", Status: orchestrator.TaskStatusTriaged},
		{TaskID: "z", ProjectID: "proj-9", Status: orchestrator.TaskStatusTriaged},
	}}
	exec := triageExec(&capturingTaskStore{}, workflow)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1", "proj-3"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op: sandbox.BoidOpTaskTriageList,
	})
	if resp.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", resp.ExitCode, resp.Stderr)
	}
	if len(workflow.triageFilters) != 2 {
		t.Fatalf("filters = %+v, want one per allowed project", workflow.triageFilters)
	}
	for _, f := range workflow.triageFilters {
		if f.ProjectID != "proj-1" && f.ProjectID != "proj-3" {
			t.Fatalf("ListCards called with an out-of-scope filter: %+v", f)
		}
	}
	var views []*api.CardView
	if err := json.Unmarshal([]byte(resp.Stdout), &views); err != nil {
		t.Fatalf("stdout is not a view list (%v): %s", err, resp.Stdout)
	}
	for _, v := range views {
		if v.ProjectID == "proj-9" {
			t.Fatalf("an out-of-scope project's triage task leaked into the listing: %s", resp.Stdout)
		}
	}
}

// TestBoidBuiltinExecutor_TaskTriageList_RejectsUnknownStatus pins that the
// list form validates its status the same way BoidOpTaskList does, rather than
// passing an unrecognized value straight to the DB layer (実測結果 項10).
func TestBoidBuiltinExecutor_TaskTriageList_RejectsUnknownStatus(t *testing.T) {
	workflow := &recordingWorkflow{}
	exec := triageExec(&capturingTaskStore{}, workflow)
	ctx := sandbox.TokenContext{ProjectID: "proj-1", AllowedProjectIDs: []string{"proj-1"}}

	resp := exec.ExecuteBoidBuiltin(context.Background(), ctx, &sandbox.BoidRequest{
		Op:     sandbox.BoidOpTaskTriageList,
		Status: "not-a-status",
	})
	if resp.ExitCode == 0 {
		t.Fatalf("unknown status accepted: %s", resp.Stdout)
	}
	if len(workflow.triageFilters) != 0 {
		t.Fatalf("ListCards was called with an unvalidated status: %v", workflow.triageFilters)
	}
}
