package api

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestTriageByTaskID_NilTaskTriage_DegradesToEmptyMap pins the existing
// "no TaskTriage wired" contract (h.TaskTriage == nil): must degrade to an
// empty map, not panic, and must not be mistaken for a real "no results"
// query error.
func TestTriageByTaskID_NilTaskTriage_DegradesToEmptyMap(t *testing.T) {
	h := &WebHandler{}
	got := h.triageByTaskID([]*orchestrator.Task{{ID: "t1"}, {ID: "t2"}})
	if len(got) != 0 {
		t.Fatalf("triageByTaskID with nil TaskTriage = %v, want empty map", got)
	}
}

// TestTriageByTaskID_BatchesIntoOneCall is the BD-8 残件1 regression: before
// this fix, triageByTaskID called GetTaskTriage once per task
// (internal/api/web.go's old for-loop) — the actual failure mode being
// fixed is query COUNT, which a test only checking the returned map's
// contents cannot catch (a correct result can come from either an O(1) or
// an O(N) query pattern). This asserts the underlying store method is
// invoked exactly once regardless of how many tasks are being enriched.
func TestTriageByTaskID_BatchesIntoOneCall(t *testing.T) {
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"t1": {TaskID: "t1", Urgency: "now"},
		"t2": {TaskID: "t2", Urgency: "today"},
	}}
	h := &WebHandler{TaskTriage: triage}

	tasks := []*orchestrator.Task{{ID: "t1"}, {ID: "t2"}, {ID: "t3"}}
	got := h.triageByTaskID(tasks)

	if triage.listTaskTriageByTaskIDsCalls != 1 {
		t.Errorf("ListTaskTriageByTaskIDs called %d times, want exactly 1 (N+1 regression)", triage.listTaskTriageByTaskIDsCalls)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got["t1"] == nil || got["t1"].Urgency != "now" {
		t.Errorf("got[t1] = %+v, want Urgency=now", got["t1"])
	}
	if got["t2"] == nil || got["t2"].Urgency != "today" {
		t.Errorf("got[t2] = %+v, want Urgency=today", got["t2"])
	}
	if _, ok := got["t3"]; ok {
		t.Error("t3 has no task_triage row, must be absent from the result")
	}
}
