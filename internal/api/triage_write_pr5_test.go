package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md) ----
//
// The queue predicates (queue の決定論的評価 節 rule 2/3) read
// task_triage.urgency as a REAL COLUMN — ListTasks("queue_next") INNER JOINs
// on it and orders by it, and notifyQueueEntryIfUrgent reads it for rule 4.
// Before PR-5a nothing in the daemon ever wrote that column: attrs_set folded
// everything, urgency included, into the opaque detail.attrs blob. The queue
// view was therefore permanently empty and notify could never fire. These
// tests pin the promotion that closes it.

// TestApplyAction_AttrsSet_PromotesUrgencyToColumn pins that an urgency sent
// via attrs_set lands in the queue predicate's column, not only in the opaque
// blob.
func TestApplyAction_AttrsSet_PromotesUrgencyToColumn(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":"today","kind":"issue","summary":"見積もり依頼"}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}

	tt := txStore.triage["t1"]
	if tt == nil {
		t.Fatal("expected a task_triage row")
	}
	if tt.Urgency != "today" {
		t.Fatalf("urgency column = %q, want %q (queue_next joins on this column)", tt.Urgency, "today")
	}
	if tt.Kind != "issue" {
		t.Fatalf("kind column = %q, want %q", tt.Kind, "issue")
	}

	// The remaining keys still fold into the opaque blob untouched.
	var detail struct {
		Attrs map[string]json.RawMessage `json:"attrs"`
	}
	if err := json.Unmarshal(tt.Detail, &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if string(detail.Attrs["summary"]) != `"見積もり依頼"` {
		t.Fatalf("summary not folded into detail.attrs: %s", tt.Detail)
	}
	// urgency/kind are promoted, not duplicated: a second copy in the blob
	// could drift from the column that actually drives the queue.
	if _, dup := detail.Attrs["urgency"]; dup {
		t.Fatalf("urgency duplicated into detail.attrs as well as the column: %s", tt.Detail)
	}
	if _, dup := detail.Attrs["kind"]; dup {
		t.Fatalf("kind duplicated into detail.attrs as well as the column: %s", tt.Detail)
	}
}

// TestApplyAction_AttrsSet_RejectsUnknownUrgency pins vocabulary validation on
// the two promoted keys. Unlike the opaque blob (whose keys the daemon
// deliberately does not interpret — 逆輸入3), urgency and kind ARE daemon
// vocabulary: urgency drives a SQL predicate, so a typo would silently drop
// the card out of the queue forever with no error anywhere.
func TestApplyAction_AttrsSet_RejectsUnknownUrgency(t *testing.T) {
	for _, payload := range []string{
		`{"urgency":"urgent"}`,
		`{"urgency":123}`,
		`{"kind":"epic"}`,
	} {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task}
		svc := newTriageWorkflowService(task, txStore)

		_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "attrs_set", Payload: []byte(payload)})
		if err == nil {
			t.Fatalf("payload %s: expected a 400, got success", payload)
		}
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 400 {
			t.Fatalf("payload %s: err = %v, want a 400 StatusError", payload, err)
		}
		if txStore.triage["t1"] != nil {
			t.Fatalf("payload %s: rejected attrs_set still wrote a sidecar row", payload)
		}
	}
}

// TestApplyAction_AttrsSet_UrgencyNullClears pins that an explicit JSON null
// clears the column (khi walking a card back to "no urgency" must be
// expressible — otherwise urgency would be a one-way ratchet at the daemon
// level, which is policy the daemon deliberately does not own).
func TestApplyAction_AttrsSet_UrgencyNullClears(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{
		"t1": {TaskID: "t1", Urgency: "now"},
	}}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"urgency":null}`),
	}); err != nil {
		t.Fatalf("ApplyAction(attrs_set): %v", err)
	}
	if got := txStore.triage["t1"].Urgency; got != "" {
		t.Fatalf("urgency = %q after explicit null, want cleared", got)
	}
}

// TestCreateTask_PreExecutionSeedsTriageRow pins the invariant PR-5a's list
// predicate rests on: a task created directly into a pre-execution status IS a
// triage task and gets its sidecar row at birth. Without this, khi's freshly
// ingested cards would be invisible to ListTriage until their first attrs_set
// happened to land.
func TestCreateTask_PreExecutionSeedsTriageRow(t *testing.T) {
	triage := &stubTriageStore{}
	svc := newInitialStatusTestService()
	svc.TaskTriage = triage

	task, err := svc.CreateTask(CreateTaskRequest{
		ProjectID:     "proj-1",
		Title:         "見積もり依頼",
		Behavior:      "triage",
		InitialStatus: "triaged",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if triage.rows[task.ID] == nil {
		t.Fatalf("no task_triage row seeded for a triaged task (rows=%v)", triage.rows)
	}
}

// TestCreateTask_OrdinaryTaskSeedsNoTriageRow is the other half of the
// invariant: an ordinary (pending) task must NOT get a sidecar row, or the
// "has a row = is a triage task" discriminator collapses and every ingest /
// sweep executor task would show up in the queue listing.
func TestCreateTask_OrdinaryTaskSeedsNoTriageRow(t *testing.T) {
	triage := &stubTriageStore{}
	svc := newInitialStatusTestService()
	svc.TaskTriage = triage

	task, err := svc.CreateTask(CreateTaskRequest{ProjectID: "proj-1", Title: "ingest", Behavior: "triage"})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if triage.rows[task.ID] != nil {
		t.Fatalf("ordinary pending task got a task_triage row: %+v", triage.rows[task.ID])
	}
}
