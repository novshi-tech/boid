package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// ---- Phase 1 PR-5b (docs/plans/cross-project-issue-triage.md 決定15/16/17) ----
//
// S6 (done の自動落ち) and S7 (フォローアップによる再浮上) at the service layer.

// TestApplyAction_ReopenRoutesTriageTaskToTriaged pins 決定17's routing. This
// is the hazard the plan doc flagged as "must not be dropped in
// implementation": the Web UI shows a `reopen` button on any done task, and
// the state machine cannot tell a done TRIAGE task from a done executor task
// (it never sees the sidecar). Unrouted, pressing it sends the meta project's
// card into `executing` — i.e. tries to RUN the card.
func TestApplyAction_ReopenRoutesTriageTaskToTriaged(t *testing.T) {
	for _, from := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted} {
		task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: from, Behavior: "dev", Payload: []byte(`{}`)}
		txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}
		svc := newTriageWorkflowService(task, txStore)
		svc.TaskTriage = &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}

		result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "reopen"})
		if err != nil {
			t.Fatalf("from %s: reopen on a triage task: %v", from, err)
		}
		if result.Task.Status != orchestrator.TaskStatusTriaged {
			t.Fatalf("from %s: status = %q, want triaged (executing would try to RUN the card)", from, result.Task.Status)
		}
		if result.Action.Type != "reopen_triaged" {
			t.Fatalf("from %s: action type = %q, want reopen_triaged in the audit log", from, result.Action.Type)
		}
	}
}

// TestApplyAction_ReopenStillWorksForOrdinaryTask is the other half: the
// routing must key on "is a triage task", not on the status. An ordinary done
// task's reopen is untouched.
func TestApplyAction_ReopenStillWorksForOrdinaryTask(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)
	svc.TaskTriage = &stubTriageStore{}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen on an ordinary done task: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("status = %q, want executing", result.Task.Status)
	}
	if result.Action.Type != "reopen" {
		t.Fatalf("action type = %q, want reopen", result.Action.Type)
	}
}

// TestApplyAction_ReopenTriagedNotPushableDirectly pins that the internal
// target stays internal: like wake_triaged/wake_ready, it is Manual:false, so
// a caller cannot bypass the routing (and thereby drag an ORDINARY done task
// into the triage lifecycle) by naming it directly.
func TestApplyAction_ReopenTriagedNotPushableDirectly(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)
	svc.TaskTriage = &stubTriageStore{}

	_, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "reopen_triaged"})
	if err == nil {
		t.Fatal("reopen_triaged was pushable directly")
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want a 400 StatusError", err)
	}
}

// TestApplyAction_ReopenTriaged_ReturnsCardToTriaged pins S7's core: a
// follow-up on a card that already fell to done brings it back to triaged,
// where attrs_set / child_added are pushable again.
func TestApplyAction_ReopenTriaged_ReturnsCardToTriaged(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}
	txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}
	svc := newTriageWorkflowService(task, txStore)
	svc.TaskTriage = &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}}}

	result, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{Type: "reopen"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("status = %q, want triaged", result.Task.Status)
	}
	// Fixture bookkeeping: the stub task store hands out the original pointer,
	// so mirror the committed transition onto it before the second action.
	task.Status = result.Task.Status

	// And the card is writable again from the workspace side.
	if _, err := svc.ApplyAction(context.Background(), task.ID, ApplyActionRequest{
		Type:    "attrs_set",
		Payload: []byte(`{"summary":"follow-up landed"}`),
	}); err != nil {
		t.Fatalf("attrs_set after reopen_triaged: %v", err)
	}
}

// TestSweepDone_FallsOnlyWhenBothConditionsHold pins 決定15 end-to-end through
// the sweep: only the card whose children are all closed AND whose source is
// reported closed transitions.
func TestSweepDone_FallsOnlyWhenBothConditionsHold(t *testing.T) {
	both := &orchestrator.Task{ID: "both", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	childOpen := &orchestrator.Task{ID: "child-open", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	srcOpen := &orchestrator.Task{ID: "src-open", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}

	closedChildren := `{"attrs":{"observed":{"source_closed":true}},"children":[{"id":"c","status":"closed"}]}`
	liveChildren := `{"attrs":{"observed":{"source_closed":true}},"children":[{"id":"c","status":"dispatched"}]}`
	sourceOpen := `{"attrs":{"observed":{"source_closed":false}},"children":[{"id":"c","status":"closed"}]}`

	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"both":       {TaskID: "both", Detail: json.RawMessage(closedChildren)},
		"child-open": {TaskID: "child-open", Detail: json.RawMessage(liveChildren)},
		"src-open":   {TaskID: "src-open", Detail: json.RawMessage(sourceOpen)},
	}}
	tasks := &multiTaskStore{tasks: []*orchestrator.Task{both, childOpen, srcOpen}}
	txStore := &recordingTxStore{
		tasks: map[string]*orchestrator.Task{
			"both": both, "child-open": childOpen, "src-open": srcOpen,
		},
		// autoDone re-reads the sidecar from INSIDE the transaction, so the
		// tx-side store has to carry the same rows.
		triage: map[string]*orchestrator.TaskTriage{
			"both":       {TaskID: "both", Detail: json.RawMessage(closedChildren)},
			"child-open": {TaskID: "child-open", Detail: json.RawMessage(liveChildren)},
			"src-open":   {TaskID: "src-open", Detail: json.RawMessage(sourceOpen)},
		},
	}
	svc := &TaskWorkflowService{
		Tasks:      tasks,
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepTriage(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriage: %v", err)
	}
	if len(result.Completed) != 1 || result.Completed[0] != "both" {
		t.Fatalf("completed = %v, want exactly [both]", result.Completed)
	}
	if txStore.updatedTask == nil || txStore.updatedTask.ID != "both" || txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Fatalf("updated task = %+v, want both→done", txStore.updatedTask)
	}
	var sawTriageDone bool
	for _, a := range txStore.actions {
		if a.Type == "triage_done" {
			sawTriageDone = true
			if a.Actor != orchestrator.ActorDaemon {
				t.Fatalf("triage_done actor = %q, want daemon (決定15 は機械の判断)", a.Actor)
			}
		}
	}
	if !sawTriageDone {
		t.Fatal("no triage_done action recorded")
	}
}

// TestSweepDone_NotifiesOnAutoDone pins the 2026-08-14 fix
// (次セッション申し送り: implement task が無言で完了する問題の一因):
// SweepTriage's autoDone runs entirely inside the daemon (no agent session,
// no `boid task notify` call ever happens for this path), so without an
// explicit Notifier call here a triage card silently reaching working→done
// never reaches the user at all — not even the muted "child task" case
// task_notify.go's fireUserNotify gate covers, since that gate only ever
// applies to a call that happens; this path has no call. The card itself is
// always root (parent_id == "", per autoDone's own doc comment), so this
// mirrors notifyQueueEntryIfUrgent's existing Notifier usage rather than
// requiring a new distribution mechanism.
func TestSweepDone_NotifiesOnAutoDone(t *testing.T) {
	both := &orchestrator.Task{ID: "both", ProjectID: "p1", Title: "cross-project card", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	closedChildren := `{"attrs":{"observed":{"source_closed":true}},"children":[{"id":"c","status":"closed"}]}`
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"both": {TaskID: "both", Detail: json.RawMessage(closedChildren)},
	}}
	tasks := &multiTaskStore{tasks: []*orchestrator.Task{both}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"both": both},
		triage: map[string]*orchestrator.TaskTriage{"both": {TaskID: "both", Detail: json.RawMessage(closedChildren)}},
	}
	notifier := &recordingNotifier{}
	svc := &TaskWorkflowService{
		Tasks:      tasks,
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
		Notifier:   notifier,
	}

	if _, err := svc.SweepTriage(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepTriage: %v", err)
	}

	if len(notifier.events) != 1 {
		t.Fatalf("notify events = %v, want exactly 1 (auto-done must notify the user)", notifier.events)
	}
	if notifier.events[0].TaskID != "both" {
		t.Errorf("TaskID = %q, want both", notifier.events[0].TaskID)
	}
}

// TestSweepDone_NilNotifierIsNoop pins the existing "Notifier optional"
// contract (mirrors notifyQueueEntryIfUrgent's own nil tolerance): a daemon
// with no notify.command configured must not panic or fail the sweep.
func TestSweepDone_NilNotifierIsNoop(t *testing.T) {
	both := &orchestrator.Task{ID: "both", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	closedChildren := `{"attrs":{"observed":{"source_closed":true}},"children":[{"id":"c","status":"closed"}]}`
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"both": {TaskID: "both", Detail: json.RawMessage(closedChildren)},
	}}
	tasks := &multiTaskStore{tasks: []*orchestrator.Task{both}}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"both": both},
		triage: map[string]*orchestrator.TaskTriage{"both": {TaskID: "both", Detail: json.RawMessage(closedChildren)}},
	}
	svc := &TaskWorkflowService{
		Tasks:      tasks,
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	if _, err := svc.SweepTriage(context.Background(), time.Now()); err != nil {
		t.Fatalf("SweepTriage with nil Notifier: %v", err)
	}
}

// TestSweepDone_MissingCanonicalSourceIsReported pins 決定16's watchdog half:
// a working card with no canonical source never falls to done, and the sweep
// reports it rather than staying silent (a card that can never finish is
// exactly the kind of silence the queue's trust story cannot absorb).
func TestSweepDone_MissingCanonicalSourceIsReported(t *testing.T) {
	working := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	// A pre-execution card in breach too: these are fetched through ListTasks'
	// "queue" branch rather than a per-status query, so covering both reaches
	// both halves of the sweep.
	triaged := &orchestrator.Task{ID: "t2", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	// captured is 起票前 and exempt; a theme card never rides the lifecycle.
	captured := &orchestrator.Task{ID: "t3", ProjectID: "p1", Status: orchestrator.TaskStatusCaptured, Behavior: "dev", Payload: []byte(`{}`)}
	theme := &orchestrator.Task{ID: "t4", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}
	// Compliant card: reports the key, so not in breach.
	ok := &orchestrator.Task{ID: "t5", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged, Behavior: "dev", Payload: []byte(`{}`)}

	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"t1": {TaskID: "t1", Detail: json.RawMessage(`{"children":[{"id":"c","status":"closed"}]}`)},
		"t2": {TaskID: "t2"},
		"t3": {TaskID: "t3"},
		"t4": {TaskID: "t4", Kind: "theme"},
		"t5": {TaskID: "t5", Detail: json.RawMessage(`{"attrs":{"observed":{"source_closed":false}}}`)},
	}}
	txStore := &recordingTxStore{task: working}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{working, triaged, captured, theme, ok}},
		TaskTriage: triage,
		Tx:         recordingTransactor{store: txStore},
	}

	result, err := svc.SweepTriage(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("SweepTriage: %v", err)
	}
	if len(result.Completed) != 0 {
		t.Fatalf("completed = %v, want none (no canonical source)", result.Completed)
	}

	got := map[string]bool{}
	for _, b := range result.Breaches {
		got[b.TaskID] = true
	}
	if len(got) != 2 || !got["t1"] || !got["t2"] {
		t.Fatalf("breaches = %v, want exactly t1 (working) and t2 (triaged) — captured/theme are exempt and t5 reports the key", got)
	}
}

// fakeSweepStore records which sweeps QueueSweepLoop.runOnce invoked.
type fakeSweepStore struct {
	wakeCalls int
	doneCalls int
	wakeErr   error
	breaches  []CanonicalSourceBreach
}

func (f *fakeSweepStore) SweepWake(context.Context, time.Time) ([]string, error) {
	f.wakeCalls++
	return nil, f.wakeErr
}

func (f *fakeSweepStore) SweepTriage(context.Context, time.Time) (TriageSweepResult, error) {
	f.doneCalls++
	return TriageSweepResult{Breaches: f.breaches}, nil
}

// TestQueueSweepLoop_RunOnce_RunsEverySweep pins that 決定15's periodic
// evaluation is actually on the tick — SweepDone being written but never
// called would leave every card in working forever while looking implemented.
func TestQueueSweepLoop_RunOnce_RunsEverySweep(t *testing.T) {
	store := &fakeSweepStore{}
	loop := &QueueSweepLoop{Store: store}
	loop.runOnce(context.Background())

	if store.wakeCalls != 1 || store.doneCalls != 1 {
		t.Fatalf("wake=%d triage=%d, want 1 each", store.wakeCalls, store.doneCalls)
	}
}

// TestQueueSweepLoop_RunOnce_WakeFailureDoesNotStopDoneSweep pins the
// deliberate non-return on a wake error: the sweeps are independent, and a
// failing wake evaluation must not silently stop cards from finishing.
func TestQueueSweepLoop_RunOnce_WakeFailureDoesNotStopDoneSweep(t *testing.T) {
	store := &fakeSweepStore{wakeErr: errors.New("boom")}
	loop := &QueueSweepLoop{Store: store}
	loop.runOnce(context.Background())

	if store.doneCalls != 1 {
		t.Fatalf("triage sweep ran %d times after a wake failure, want 1", store.doneCalls)
	}
}

// TestQueueSweepLoop_BreachReportFiresOnlyOnChange pins the log-flood guard:
// until khi starts delivering observed.source_closed, EVERY card is a 決定16
// breach, and this loop ticks every minute — an unconditional report would
// emit 1,440 lines a day and bury every other daemon log.
func TestQueueSweepLoop_BreachReportFiresOnlyOnChange(t *testing.T) {
	store := &fakeSweepStore{breaches: []CanonicalSourceBreach{{TaskID: "a"}, {TaskID: "b"}}}
	loop := &QueueSweepLoop{Store: store}

	loop.runOnce(context.Background())
	first := loop.lastBreachFingerprint
	if first == "" {
		t.Fatal("expected the first report to record a fingerprint")
	}

	// Same set, different order: still the same breach set, so no re-report.
	store.breaches = []CanonicalSourceBreach{{TaskID: "b"}, {TaskID: "a"}}
	loop.runOnce(context.Background())
	if loop.lastBreachFingerprint != first {
		t.Fatalf("fingerprint changed for an identical set: %q → %q", first, loop.lastBreachFingerprint)
	}

	// A new breach appears: the set changed, so it must be reported again.
	store.breaches = append(store.breaches, CanonicalSourceBreach{TaskID: "c"})
	loop.runOnce(context.Background())
	if loop.lastBreachFingerprint == first {
		t.Fatal("fingerprint did not change when a new breach appeared")
	}

	// All clear: also a change, and worth saying out loud.
	store.breaches = nil
	loop.runOnce(context.Background())
	if loop.lastBreachFingerprint != "" {
		t.Fatalf("fingerprint = %q after all breaches cleared, want empty", loop.lastBreachFingerprint)
	}
}

// TestRecordChildClosedOnParent_TriggersAutoDone pins 決定15's SECOND
// evaluation trigger: closing the last child is the moment most likely to
// have just satisfied the rule, so the card must not have to wait for the
// next periodic tick.
func TestRecordChildClosedOnParent_TriggersAutoDone(t *testing.T) {
	parent := &orchestrator.Task{ID: "card", ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Behavior: "dev", Payload: []byte(`{}`)}
	child := &orchestrator.Task{ID: "child", ProjectID: "p1", ParentID: "card", Status: orchestrator.TaskStatusDone, Behavior: "dev", Payload: []byte(`{}`)}

	detail := `{"attrs":{"observed":{"source_closed":true}},"children":[{"id":"c","status":"dispatched","task_ref":"child"}]}`
	row := &orchestrator.TaskTriage{TaskID: "card", Detail: json.RawMessage(detail)}
	txStore := &recordingTxStore{
		tasks:  map[string]*orchestrator.Task{"card": parent, "child": child},
		triage: map[string]*orchestrator.TaskTriage{"card": row},
	}
	svc := &TaskWorkflowService{
		Tasks:      &multiTaskStore{tasks: []*orchestrator.Task{parent, child}},
		TaskTriage: &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{"card": row}},
		Tx:         recordingTransactor{store: txStore},
	}

	svc.recordChildClosedOnParent(child)

	if txStore.updatedTask == nil || txStore.updatedTask.ID != "card" {
		t.Fatalf("parent card was not updated: %+v", txStore.updatedTask)
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Fatalf("parent status = %q, want done (the last child closing satisfied 決定15)", txStore.updatedTask.Status)
	}
}
