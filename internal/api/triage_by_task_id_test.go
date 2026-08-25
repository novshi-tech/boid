package api

import (
	"fmt"
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
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
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

// TestTriageByTaskID_ErrorStillUsesPartialResults is the Opus review
// finding (2026-08-18): an earlier version of triageByTaskID discarded
// ListTaskTriageByTaskIDs's returned map entirely on any non-nil error,
// throwing away rows the store DID manage to fetch. The real
// orchestrator.ListTaskTriageByTaskIDs is best-effort across chunks/rows
// (card.go's doc comment): a non-nil error means *something* went
// wrong for at least one row or chunk, but the map can still be partially
// or fully populated — e.g. one chunk's query failed while another
// chunk's rows were gathered fine, or a single row's Scan failed while
// the rest of that same chunk succeeded. Silently downgrading a partial
// success to zero enrichment for the whole view is exactly the "見えて
// いなければ存在しない" failure class 決定9 exists to prevent.
func TestTriageByTaskID_ErrorStillUsesPartialResults(t *testing.T) {
	triage := &stubTriageStore{
		rows: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Urgency: "now"},
		},
		listErr: fmt.Errorf("simulated: one row failed to scan"),
	}
	h := &WebHandler{TaskTriage: triage}

	got := h.triageByTaskID([]*orchestrator.Task{{ID: "t1"}, {ID: "t2"}})

	if len(got) != 1 || got["t1"] == nil {
		t.Fatalf("triageByTaskID with a partial-success error = %v, want the partial result (t1) to survive, not be discarded", got)
	}
}

// stubTriageStoreNilOnError is a CardStore whose
// ListTaskTriageByTaskIDs returns (nil, err) — the total-failure case (as
// opposed to stubTriageStore's partial-success case above), e.g. every
// chunk's Query call itself failed before any row was ever scanned.
type stubTriageStoreNilOnError struct{ stubTriageStore }

func (s *stubTriageStoreNilOnError) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error) {
	return nil, fmt.Errorf("simulated: total failure, nothing fetched")
}

// TestTriageByTaskID_NilMapOnTotalFailure_DegradesToEmptyMap covers the
// other half of the error-handling contract: a nil map (rather than a
// non-nil-but-empty or partially-populated one) on error must still
// degrade cleanly to an empty map, not panic on a nil map read/return.
func TestTriageByTaskID_NilMapOnTotalFailure_DegradesToEmptyMap(t *testing.T) {
	h := &WebHandler{TaskTriage: &stubTriageStoreNilOnError{}}

	got := h.triageByTaskID([]*orchestrator.Task{{ID: "t1"}})

	if len(got) != 0 {
		t.Fatalf("triageByTaskID with a nil map + error = %v, want empty map", got)
	}
}
