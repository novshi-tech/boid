package server

// docs/plans/boid-internal-signal-inbox.md §10 グループB Q6: the OTHER
// actions-table write mouth (internal/dispatcher/store.go's daemon-restart
// abort) is out of scope for PR-1 because §4.5 argues dispatched children's
// terminal status is never silently missed by it — SweepReconcileChildren
// (internal/api/queue_sweep.go) re-derives it independently via
// recordChildClosedOnParent (internal/api/workflow_card.go), which
// self-records through the SAME tx.CreateAction every other write in this
// codebase goes through. This test proves that claim against the REAL
// production wiring (apiTxStore/apiTransactor, not a fake) — internal/api's
// own queue_sweep_test.go cannot do this itself: testutil (needed for a real
// migrated DB) imports internal/server, so an internal/api test file (same
// package, not _test) importing testutil would create a build cycle.
// internal/server has no such problem, since it legitimately imports
// internal/api already.

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// sweepStubMetaResolver is a tiny orchestrator.MetaProjectResolver test
// double (workspaceID -> metaproject ids), local to this file.
type sweepStubMetaResolver map[string][]string

func (r sweepStubMetaResolver) MetaProjectIDs(workspaceID string) []string {
	return r[workspaceID]
}

func TestSweepReconcileChildren_RecordChildClosedOnParent_IngestsInternalSignal(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-meta", WorkDir: "/tmp/proj-meta"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "proj-meta", "ws-1"); err != nil {
		t.Fatalf("assign workspace: %v", err)
	}

	// The parent card — the target of recordChildClosedOnParent's
	// child_closed self-record.
	card := &orchestrator.Task{
		ID: "card", ProjectID: "proj-meta", Title: "card", Type: orchestrator.TaskTypeCard, Status: orchestrator.TaskStatusWorking,
		Card: &orchestrator.CardAttrs{Detail: []byte(`{"children":[{"id":"c1","status":"dispatched","task_ref":"child-1"}]}`)},
	}
	if err := orchestrator.CreateTask(d.Conn, card); err != nil {
		t.Fatalf("create card task: %v", err)
	}

	// A dispatched child that already reached done — SweepReconcileChildren's
	// reconciliation target.
	child := &orchestrator.Task{
		ProjectID: "proj-meta", Title: "child", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone,
		ParentID: "card", Exec: &orchestrator.ExecAttrs{Behavior: "dev"},
	}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child task: %v", err)
	}

	// Repoint the card's detail.children[].task_ref at the REAL child id
	// CreateTask assigned (task_triage.detail is keyed by real task ids, not
	// the synthetic "child-1" placeholder used above for readability).
	tasks := orchestrator.NewTaskRepository(d.Conn)
	tt, err := tasks.GetTaskTriage("card")
	if err != nil {
		t.Fatalf("get task_triage: %v", err)
	}
	children, err := orchestrator.DetailChildren(tt.Detail)
	if err != nil {
		t.Fatalf("DetailChildren: %v", err)
	}
	children[0].TaskRef = child.ID
	fixedDetail, err := orchestrator.SetDetailChildren(tt.Detail, children)
	if err != nil {
		t.Fatalf("SetDetailChildren: %v", err)
	}
	tt.Detail = fixedDetail
	if err := tasks.UpsertTaskTriage(tt); err != nil {
		t.Fatalf("upsert task_triage: %v", err)
	}

	tasks.SetMetaProjectResolver(sweepStubMetaResolver{"ws-1": {"proj-meta"}})
	tx := apiTransactor{db: d.Conn, metaResolver: sweepStubMetaResolver{"ws-1": {"proj-meta"}}}

	svc := &api.TaskWorkflowService{Tasks: tasks, TaskTriage: tasks, Tx: tx}
	if err := svc.SweepReconcileChildren(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReconcileChildren: %v", err)
	}

	// The child_closed action landed on the card's own action log — same
	// assertion internal/api's TestSweepReconcileChildren_ClosesStaleDispatchedChild
	// makes, confirming this test's fixture is wired correctly.
	actions, err := tasks.ListActionsByTask("card")
	if err != nil {
		t.Fatalf("ListActionsByTask: %v", err)
	}
	found := false
	for _, a := range actions {
		if a.Type == "child_closed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a child_closed action recorded, got actions=%+v", actions)
	}

	// ...and THIS is the part that only holds if the write genuinely reached
	// orchestrator.CreateAction's ingest step: a signal for the card landed
	// in ws-1's inbox.
	signals, err := orchestrator.ListSignals(d.Conn, orchestrator.SignalFilter{WorkspaceID: "ws-1", State: orchestrator.SignalStateAll})
	if err != nil {
		t.Fatalf("ListSignals: %v", err)
	}
	if len(signals) != 1 {
		t.Fatalf("got %d signals, want 1 (recordChildClosedOnParent must reach CreateAction's ingest step)", len(signals))
	}
	if signals[0].Identity != "card" {
		t.Errorf("Identity = %q, want %q", signals[0].Identity, "card")
	}
	if signals[0].Title != "child_closed" {
		t.Errorf("Title = %q, want %q", signals[0].Title, "child_closed")
	}
}
