package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): auto-reopen (I-5) ----
//
// SweepReopen is 決定15's mirror, sharing QueueSweepLoop's tick with
// SweepWake/SweepTriage (the design doc's own placement instruction: "評価
// 契機は QueueSweepLoop — 決定15のSweepDoneと同じ場所に並べる").

// TestSweepReopen_FlipsToFalse_ReopensDoneTriageTask pins I-5's core: a done
// triage task whose canonical source reports open again (source_closed
// false) is reopened back to triaged, with the reopen_triaged action
// stamped ActorDaemon (機械の判断、決定12).
func TestSweepReopen_FlipsToFalse_ReopensDoneTriageTask(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
	}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 1 || result.Reopened[0] != "t1" {
		t.Fatalf("reopened = %v, want [t1]", result.Reopened)
	}
	if len(result.Flapped) != 0 {
		t.Fatalf("flapped = %v, want none", result.Flapped)
	}
	if txStore.updatedTask == nil || txStore.updatedTask.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("updated task = %+v, want t1→triaged", txStore.updatedTask)
	}
	var sawReopenTriaged bool
	for _, a := range txStore.actions {
		if a.Type == "reopen_triaged" {
			sawReopenTriaged = true
			if a.Actor != orchestrator.ActorDaemon {
				t.Fatalf("reopen_triaged actor = %q, want daemon (機械の判断)", a.Actor)
			}
		}
	}
	if !sawReopenTriaged {
		t.Fatal("no reopen_triaged action recorded")
	}
}

// TestSweepReopen_SourceStillClosed_DoesNotReopen pins the OTHER half of
// I-5c: a done triage task whose canonical source is STILL reported closed
// has nothing to reopen — visibility for "something else landed while
// staying closed" is logAttrsSetOnDoneTriage's job at attrs_set-apply time
// (attrs_set_done_test.go), not this sweep's.
func TestSweepReopen_SourceStillClosed_DoesNotReopen(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":true}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{tasks: map[string]*orchestrator.Task{"t1": task}}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 0 {
		t.Fatalf("reopened = %v, want none (source still closed)", result.Reopened)
	}
	if txStore.updatedTask != nil {
		t.Fatalf("task was mutated: %+v, want untouched", txStore.updatedTask)
	}
}

// TestSweepReopen_NoTaskTriageRow_SkippedNotErrored pins the doc's stated
// invariant: an ordinary done task with no task_triage row at all is simply
// not a candidate — not an error, not a notify.
func TestSweepReopen_NoTaskTriageRow_SkippedNotErrored(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	triage := &stubTriageStore{} // no rows at all
	txStore := &recordingTxStore{tasks: map[string]*orchestrator.Task{"t1": task}}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 0 || len(result.Flapped) != 0 {
		t.Fatalf("result = %+v, want a fully empty result", result)
	}
}

// TestSweepReopen_SecondAutoReopenWithinSameEpisode_DoesNotReopenAgain_NotifiesInstead
// pins 12節 B-6 のフラップ対策, エピソード単位版 (2026-08-19, nose 判断 —
// Opus review): a prior reopen_triaged action stamped ActorDaemon sitting in
// the task's history counts toward the budget ONLY when no fresh
// triage_done has landed since (i.e. still the SAME done episode) — see
// orchestrator.CountAutoReopens's own doc comment for why lifetime counting
// was wrong (it silently refused every "2周目" reopen forever). This
// fixture deliberately has NO triage_done between the two events, so the
// second flip is still within the SAME episode the first reopen already
// spent its budget on — it does NOT reopen a second time, and is surfaced
// via Notifier instead.
func TestSweepReopen_SecondAutoReopenWithinSameEpisode_DoesNotReopenAgain_NotifiesInstead(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Title: "flapping card", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		actions: []*orchestrator.Action{
			{TaskID: "t1", Type: "triage_done", Actor: orchestrator.ActorDaemon},
			{TaskID: "t1", Type: "reopen_triaged", Actor: orchestrator.ActorDaemon},
		},
	}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
		Notifier:   notifier,
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 0 {
		t.Fatalf("reopened = %v, want none (フラップ対策 should have blocked it)", result.Reopened)
	}
	if len(result.Flapped) != 1 || result.Flapped[0] != "t1" {
		t.Fatalf("flapped = %v, want [t1]", result.Flapped)
	}
	if txStore.updatedTask != nil {
		t.Fatalf("task was mutated: %+v, want untouched (blocked, not applied)", txStore.updatedTask)
	}
	if len(notifier.events) != 1 || notifier.events[0].TaskID != "t1" {
		t.Fatalf("notify events = %v, want exactly 1 for t1", notifier.events)
	}
}

// TestSweepReopen_NewDoneEpisode_ResetsBudget_ReopensAgain is the fix's core
// positive case (2026-08-19, nose 判断 — Opus review): a task that already
// spent ITS FIRST episode's auto-reopen, then cycled all the way back through
// triaged→ready→working→triage_done into a FRESH done episode, gets a fresh
// budget — the SECOND episode's own flip auto-reopens, exactly like the
// first one did. Before this fix, CountAutoReopens counted the task's
// entire lifetime, so this second, entirely legitimate reopen would have
// been silently refused forever.
func TestSweepReopen_NewDoneEpisode_ResetsBudget_ReopensAgain(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Title: "long-lived card", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		actions: []*orchestrator.Action{
			{TaskID: "t1", Type: "triage_done", Actor: orchestrator.ActorDaemon},    // episode 1 starts
			{TaskID: "t1", Type: "reopen_triaged", Actor: orchestrator.ActorDaemon}, // episode 1's one auto-reopen
			{TaskID: "t1", Type: "triage_done", Actor: orchestrator.ActorDaemon},    // episode 2 starts — budget resets
		},
	}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
		Notifier:   notifier,
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 1 || result.Reopened[0] != "t1" {
		t.Fatalf("reopened = %v, want [t1] (episode 2's budget is fresh)", result.Reopened)
	}
	if len(result.Flapped) != 0 {
		t.Fatalf("flapped = %v, want none", result.Flapped)
	}
	if len(notifier.events) != 0 {
		t.Fatalf("notify events = %v, want none (a successful reopen doesn't fire the フラップ notify)", notifier.events)
	}
}

// TestSweepReopen_HumanReopen_DoesNotCountTowardFlapBudget pins that a
// human's own manual reopen of a done triage task (Actor==human — the Web UI
// button, resolveReopenVariant's routing) does not consume the AUTOMATIC
// フラップ budget: this is still the FIRST automatic reopen, so it proceeds.
func TestSweepReopen_HumanReopen_DoesNotCountTowardFlapBudget(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		actions: []*orchestrator.Action{
			{TaskID: "t1", Type: "reopen_triaged", Actor: orchestrator.ActorHuman},
		},
	}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepReopen(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepReopen: %v", err)
	}
	if len(result.Reopened) != 1 || result.Reopened[0] != "t1" {
		t.Fatalf("reopened = %v, want [t1] (a human reopen must not count against the automatic budget)", result.Reopened)
	}
}

// TestSweepReopen_FlapNotify_DedupsWithinSameEpisode_RefiresAfterClearing
// pins the notify-flood guard mirroring queue_sweep.go's
// logCanonicalSourceBreaches fingerprint-on-change discipline: a task stuck
// in the SAME flapped state must notify once, not every sweep tick, but a
// FRESH flap episode (after the task left the flapped set in between)
// notifies again.
func TestSweepReopen_FlapNotify_DedupsWithinSameEpisode_RefiresAfterClearing(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	flappedDetail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	closedDetail := json.RawMessage(`{"attrs":{"observed":{"source_closed":true}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: flappedDetail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: flappedDetail}},
		actions: []*orchestrator.Action{
			{TaskID: "t1", Type: "reopen_triaged", Actor: orchestrator.ActorDaemon},
		},
	}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
		Notifier:   notifier,
	}

	// Tick 1: flapped, notify fires once.
	if _, err := svc.SweepReopen(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReopen (tick 1): %v", err)
	}
	if len(notifier.events) != 1 {
		t.Fatalf("after tick 1: events = %v, want 1", notifier.events)
	}

	// Tick 2: same flapped state persists — must NOT re-notify.
	if _, err := svc.SweepReopen(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReopen (tick 2): %v", err)
	}
	if len(notifier.events) != 1 {
		t.Fatalf("after tick 2 (same episode): events = %v, want still 1", notifier.events)
	}

	// The episode clears (source re-closes).
	triage.rows["t1"].Detail = closedDetail
	if _, err := svc.SweepReopen(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReopen (tick 3, cleared): %v", err)
	}
	if len(notifier.events) != 1 {
		t.Fatalf("after tick 3 (cleared): events = %v, want still 1", notifier.events)
	}

	// A fresh flap episode starts — must notify again.
	triage.rows["t1"].Detail = flappedDetail
	if _, err := svc.SweepReopen(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReopen (tick 4, fresh episode): %v", err)
	}
	if len(notifier.events) != 2 {
		t.Fatalf("after tick 4 (fresh episode): events = %v, want 2", notifier.events)
	}
}

// TestSweepReopen_NilNotifierIsNoop mirrors TestSweepDone_NilNotifierIsNoop:
// a daemon with no notify.command configured must not panic or fail the
// sweep when a flap would otherwise notify.
func TestSweepReopen_NilNotifierIsNoop(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	detail := json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"t1": task},
		triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1", Detail: detail}},
		actions: []*orchestrator.Action{
			{TaskID: "t1", Type: "reopen_triaged", Actor: orchestrator.ActorDaemon},
		},
	}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{task}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	if _, err := svc.SweepReopen(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepReopen with nil Notifier: %v", err)
	}
}
