package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type recordingTxStore struct {
	task             *orchestrator.Task
	updatedTask      *orchestrator.Task
	actions          []*orchestrator.Action
	triage           map[string]*orchestrator.CardAttrs
	parkedFromFn     func(taskID string) (orchestrator.TaskStatus, error)
	getTaskTriageErr error // when set, GetTaskTriage returns this instead of the usual not-found
	updateTaskErr    error // when set, UpdateTask returns this instead of succeeding — used to
	// simulate a genuine (non-race) Tx failure, PR #987 review round 2, LOW N4's
	// "any OTHER Tx failure still reports orphaned_child_task_ids" coverage.
	// tasks backs multi-task scenarios (e.g. recordChildClosedOnParent, which
	// looks up the PARENT task by id while the primary `task`/`updatedTask`
	// fields track the child under test). Checked before task/updatedTask.
	tasks map[string]*orchestrator.Task
	// unlinkAllForTaskCalls records every UnlinkAllForTask(taskID) call — used
	// by the ingestion-identity.md PR-1 drop-side-effect tests to pin that
	// `drop` (and ONLY drop) releases a task's identity bindings.
	unlinkAllForTaskCalls []string
	// touchedTaskUpdatedAtIDs records every TouchTaskUpdatedAt(id) call, in
	// call order (docs/plans/webui-detail-list-redesign.md PR-3) — the
	// updated_at bump tests assert against this list instead of a real
	// timestamp, since this fake has no wall-clock semantics of its own.
	touchedTaskUpdatedAtIDs []string
	touchTaskUpdatedAtErr   error // when set, TouchTaskUpdatedAt returns this instead of succeeding
}

func (s *recordingTxStore) CreateTask(task *orchestrator.Task) error { return nil }
func (s *recordingTxStore) GetTask(id string) (*orchestrator.Task, error) {
	// Prefer the most recently committed update (if any) over the original
	// snapshot, so a second WithinTx call within the same test (e.g. PR-2's
	// ApplyAction("ready") auto-chaining into Dispatch, which opens its own
	// separate transaction right after "ready"'s commits) observes the prior
	// transaction's write — mirroring production, where Tx and any later
	// GetTask both read the same underlying DB.
	if s.updatedTask != nil && s.updatedTask.ID == id {
		return s.updatedTask, nil
	}
	if s.tasks != nil {
		if t, ok := s.tasks[id]; ok {
			return t, nil
		}
	}
	if s.task == nil || s.task.ID != id {
		return nil, fmt.Errorf("task not found: %s", id)
	}
	return s.task, nil
}
func (s *recordingTxStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) UpdateTask(task *orchestrator.Task) error {
	if s.updateTaskErr != nil {
		return s.updateTaskErr
	}
	s.updatedTask = task
	return nil
}
func (s *recordingTxStore) DeleteTask(id string) error { return nil }
func (s *recordingTxStore) TouchTaskUpdatedAt(id string) error {
	if s.touchTaskUpdatedAtErr != nil {
		return s.touchTaskUpdatedAtErr
	}
	s.touchedTaskUpdatedAtIDs = append(s.touchedTaskUpdatedAtIDs, id)
	return nil
}
func (s *recordingTxStore) FindTaskByRemote(remoteID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) FindTaskByRef(ref, parentID, projectID string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) ListChildren(parentID string) ([]*orchestrator.Task, error) {
	return nil, nil
}
func (s *recordingTxStore) CreateAction(_ context.Context, action *orchestrator.Action) error {
	s.actions = append(s.actions, action)
	return nil
}

// ListActionsByTask filters s.actions (both pre-seeded fixture rows and any
// CreateAction'd during the test) by taskID — docs/plans/ingestion-identity.md
// PR-5 (B-6)'s autoReopen (triage_done.go) reads this inside its own Tx to
// derive the フラップ count (orchestrator.CountAutoReopens), so this stub
// returning an unconditional nil (its pre-PR-5 shape — no existing test
// asserted on its return value) would silently make every フラップ test pass
// for the wrong reason.
func (s *recordingTxStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	var out []*orchestrator.Action
	for _, a := range s.actions {
		if a.TaskID == taskID {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *recordingTxStore) UpsertTaskTriage(tt *orchestrator.CardAttrs) error {
	if s.triage == nil {
		s.triage = map[string]*orchestrator.CardAttrs{}
	}
	s.triage[tt.TaskID] = tt
	return nil
}
func (s *recordingTxStore) GetTaskTriage(taskID string) (*orchestrator.CardAttrs, error) {
	if s.getTaskTriageErr != nil {
		return nil, s.getTaskTriageErr
	}
	tt, ok := s.triage[taskID]
	if !ok {
		return nil, fmt.Errorf("task_triage not found: %s: %w", taskID, sql.ErrNoRows)
	}
	// Return a COPY, not the map's own pointer (PR #988 review, LOW 4): the
	// real orchestrator.GetTaskTriage does a fresh DB scan on every call, so
	// a caller mutating the struct it got back has no effect until
	// UpsertTaskTriage is called again. Handing back the map's own pointer
	// let a caller's field mutation silently "commit" through this fake
	// WITHOUT ever calling UpsertTaskTriage — which made the
	// suggestion_verb-clearing regression tests (suggestion_discard_test.go,
	// apply_action_pr3_noted_answered_test.go, accept_go_test.go) pass even
	// with the production tx.UpsertTaskTriage(tt) call they exist to pin
	// deleted, since the earlier `tt.SuggestionVerb = ""` mutation had
	// already aliased into this map before Upsert ever ran.
	cp := *tt
	return &cp, nil
}
func (s *recordingTxStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error) {
	out := map[string]*orchestrator.CardAttrs{}
	for _, id := range taskIDs {
		if tt, ok := s.triage[id]; ok {
			out[id] = tt
		}
	}
	return out, nil
}
func (s *recordingTxStore) DeleteTaskTriage(taskID string) error {
	delete(s.triage, taskID)
	return nil
}
func (s *recordingTxStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	if s.parkedFromFn != nil {
		return s.parkedFromFn(taskID)
	}
	return "", fmt.Errorf("parked_from not found: %s", taskID)
}
func (s *recordingTxStore) GetJob(id string) (*Job, error) { return nil, fmt.Errorf("not implemented") }
func (s *recordingTxStore) ListJobsByTask(taskID string) ([]*Job, error) {
	return nil, nil
}
func (s *recordingTxStore) UpdateJob(job *Job) error { return nil }

// LinkIdentity / UnlinkIdentity / ResolveIdentity / ListIdentitiesByTask are
// not exercised by the drop-side-effect tests (only UnlinkAllForTask is
// called from within ApplyAction) — trivial no-ops, kept just to satisfy
// TxStore.
func (s *recordingTxStore) LinkIdentity(projectID, identity, taskID string) error { return nil }
func (s *recordingTxStore) UnlinkIdentity(projectID, identity string) error       { return nil }
func (s *recordingTxStore) UnlinkAllForTask(taskID string) error {
	s.unlinkAllForTaskCalls = append(s.unlinkAllForTaskCalls, taskID)
	return nil
}
func (s *recordingTxStore) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	return nil, orchestrator.ErrTaskNotFound
}
func (s *recordingTxStore) ListIdentitiesByTask(taskID string) ([]string, error) {
	return nil, nil
}

type recordingTransactor struct {
	store *recordingTxStore
}

func (t recordingTransactor) WithinTx(fn func(TxStore) error) error {
	return fn(t.store)
}

type dispatchContextProbe struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
}

func newDispatchContextProbe() *dispatchContextProbe {
	return &dispatchContextProbe{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (p *dispatchContextProbe) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	select {
	case <-p.started:
	default:
		close(p.started)
	}

	select {
	case <-ctx.Done():
		select {
		case <-p.canceled:
		default:
			close(p.canceled)
		}
		return nil, ctx.Err()
	case <-p.release:
		return &orchestrator.DispatchResult{FinalPayload: task.Exec.Payload}, nil
	}
}

func (p *dispatchContextProbe) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Exec.Payload}, nil
}

func TestTaskWorkflowServiceApplyAction_BackgroundDispatchMustOutliveRequestContext(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Title:     "start task",
		Status:    orchestrator.TaskStatusPending,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}

	txStore := &recordingTxStore{}
	probe := newDispatchContextProbe()
	defer close(probe.release)

	svc := &TaskWorkflowService{
		Tasks:       &stubTaskStore{task: task},
		Tx:          recordingTransactor{store: txStore},
		Meta:        stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Coordinator: probe,
	}

	ctx, cancel := context.WithCancel(context.Background())
	result, err := svc.ApplyAction(ctx, task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction() error = %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusExecuting {
		t.Fatalf("task status = %q, want %q", result.Task.Status, orchestrator.TaskStatusExecuting)
	}

	select {
	case <-probe.started:
	case <-time.After(1 * time.Second):
		t.Fatal("dispatch loop did not start")
	}

	cancel()

	select {
	case <-probe.canceled:
		t.Fatal("background dispatch inherited request context cancellation")
	case <-time.After(100 * time.Millisecond):
	}
}

// ---- card machine v2 actions (docs/plans/suggestion-as-state-transition-impl.md §3) ----

// isPreExecutionCardStatus reports whether status is one of the TWO
// statuses that exist ONLY on the card side of the split (PR-B) and are
// non-terminal — parked/working. Renamed-in-spirit from v1 (which had FIVE:
// captured/triaged/parked/ready/working) but the identifier itself is left
// alone across the many test files that already call it, to keep this PR's
// test-file diffs about BEHAVIOR, not a mechanical rename. done/aborted/
// dropped (and every execution-lifecycle status) are deliberately excluded:
// those are either shared between both machines or execution-only, so "does
// this task have a sidecar row" is genuinely ambiguous/test-scenario-dependent
// there, unlike parked/working where production always seeds one at creation
// (see newTriageWorkflowService's own doc comment).
func isPreExecutionCardStatus(status orchestrator.TaskStatus) bool {
	switch status {
	case orchestrator.TaskStatusParked, orchestrator.TaskStatusWorking:
		return true
	default:
		return false
	}
}

// newTriageWorkflowService builds a TaskWorkflowService for a card (triage)
// task under test. PR-B (docs/plans/suggestion-as-state-transition-impl.md
// §2): machineFor now picks NewCardMachine vs NewExecutionMachine by
// checking whether the task carries a task_triage sidecar row, so — for a
// task whose status is parked or working — this helper seeds an empty row
// for task.ID when the caller's txStore doesn't already carry one, and wires
// TaskTriage to see it. This mirrors production's own invariant
// (task_create.go's CreateTask / task_resolve_or_capture.go's
// ResolveOrCapture both seed an empty task_triage row up front for any
// card-lifecycle initial_status, unconditional on the caller ever sending a
// park/attrs_set). Without this, every test built on this helper for those
// statuses would have machineFor fall back to NewExecutionMachine (no row =
// "not a card"), which has no rule at all for go/working/park/drop/etc. and
// would reject them with a spurious 400.
//
// Deliberately NOT extended to done/aborted/dropped/pending/executing/
// awaiting: several tests built on this helper (attrs_set_done_test.go's
// TaskTriageStoreNotWired/NoTaskTriageRow cases) construct a task in one of
// THOSE statuses specifically to exercise TaskTriage being nil, empty, or
// erroring — auto-wiring/seeding for them would silently defeat those tests'
// own premise. Each such test wires (or deliberately leaves unwired)
// TaskTriage itself, same as before this helper existed.
func newTriageWorkflowService(task *orchestrator.Task, txStore *recordingTxStore) *TaskWorkflowService {
	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{task: task},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
		// Coordinator intentionally nil: pre-execution transitions have no
		// hooks to dispatch (no runtime exists for a triage task).
	}
	if isPreExecutionCardStatus(task.Status) {
		if txStore.triage == nil {
			txStore.triage = map[string]*orchestrator.CardAttrs{}
		}
		if _, ok := txStore.triage[task.ID]; !ok {
			txStore.triage[task.ID] = &orchestrator.CardAttrs{TaskID: task.ID}
		}
		svc.TaskTriage = txStore
	}
	return svc
}

// humanCtx returns a ctx stamped ActorHuman — the Web UI / CLI's own
// convention (action.go, web_service.go). Card-lifecycle transition actions
// (go/working/park/drop/done/reopen) require this since 穴11's push-down
// defense (workflow_action.go); every other test in this file that drives
// one of those six verbs directly (not via accept(verb)) must use this
// instead of a bare context.Background().
func humanCtx() context.Context {
	return orchestrator.WithActor(context.Background(), orchestrator.ActorHuman)
}

func TestTaskWorkflowServiceApplyAction_Start_ParkedToWorking(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})
	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "start"})
	if err != nil {
		t.Fatalf("ApplyAction(start): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
}

// TestTaskWorkflowServiceApplyAction_Working_NormalizesToStart pins the
// card-next-step-and-timeline.md §8 receive-side compatibility shim: a
// caller still sending the retired "working" spelling (an old write CLI
// during the compat window) reaches exactly the same transition as "start".
func TestTaskWorkflowServiceApplyAction_Working_NormalizesToStart(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	svc := newTriageWorkflowService(task, &recordingTxStore{task: task})
	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "working"})
	if err != nil {
		t.Fatalf("ApplyAction(working): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusWorking {
		t.Fatalf("status = %q, want working", result.Task.Status)
	}
}

// TestTaskWorkflowServiceApplyAction_StampsActorFromContext verifies
// ApplyAction reads orchestrator.ActorFromContext(ctx) and stamps it onto the
// recorded Action — the propagation path 論点11「代行タスク」 depends on.
// Uses attrs_set (non-transitioning) rather than a card-lifecycle transition
// verb specifically so the "task"/"unset" cases below reach the same code
// path instead of being rejected by 穴11's push-down defense — see
// TestApplyAction_CardTransitions_RejectedForNonHumanActor for that negative
// pin, and TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion
// for the escape-hatch positive pin.
func TestTaskWorkflowServiceApplyAction_StampsActorFromContext(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"human", orchestrator.WithActor(context.Background(), orchestrator.ActorHuman), orchestrator.ActorHuman},
		{"task", orchestrator.WithActor(context.Background(), orchestrator.ActorTask("proxy-task-1")), orchestrator.ActorTask("proxy-task-1")},
		{"unset", context.Background(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			txStore := &recordingTxStore{task: task}
			svc := newTriageWorkflowService(task, txStore)
			if _, err := svc.ApplyAction(tc.ctx, task.ID, ApplyActionRequest{Type: "attrs_set", Payload: []byte(`{"summary":"x"}`)}); err != nil {
				t.Fatalf("ApplyAction(attrs_set): %v", err)
			}
			if len(txStore.actions) != 1 {
				t.Fatalf("expected 1 recorded action, got %d", len(txStore.actions))
			}
			if got := txStore.actions[0].Actor; got != tc.want {
				t.Fatalf("actor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion is the
// escape-hatch pin design doc §3.2 explicitly requires: "人の直接操作は常に
// 全遷移で可能であることを担保する" — every one of card machine v2's edges
// must be reachable directly by a human (Web UI / CLI, ActorHuman), with NO
// suggestion involved anywhere in the fixture. suggestion is one entry point
// into these transitions, never the only one.
//
// "go" (both edges — parked→working and the working→working self-loop) is
// deliberately NOT in this table: unlike the other five verbs, go bypasses
// ApplyAction's generic sm.Apply path entirely (acceptGo, workflow_card.go)
// and additionally requires a specced child to exist or it 409s (§3.2's
// "子なし Go は拒否" — see accept_go_test.go's own dedicated suite, which
// this table's generic empty-triage fixture cannot satisfy).
func TestApplyAction_CardTransitions_HumanCanApplyEveryEdge_NoSuggestion(t *testing.T) {
	cases := []struct {
		from   orchestrator.TaskStatus
		action string
		want   orchestrator.TaskStatus
	}{
		{orchestrator.TaskStatusParked, "start", orchestrator.TaskStatusWorking},
		{orchestrator.TaskStatusParked, "drop", orchestrator.TaskStatusDropped},
		// 8 本目の辺: 「外で片付いていた」「重複と判明した」card を 1 手で閉じる。
		// これが無いと khi は start→complete の 2 手を提案するしかなく、その回り道が
		// identity を解放する drop の誤用を誘っていた (machine_card.go の doc comment)。
		{orchestrator.TaskStatusParked, "complete", orchestrator.TaskStatusDone},
		{orchestrator.TaskStatusWorking, "park", orchestrator.TaskStatusParked},
		{orchestrator.TaskStatusWorking, "complete", orchestrator.TaskStatusDone},
		{orchestrator.TaskStatusDone, "reopen", orchestrator.TaskStatusParked},
		{orchestrator.TaskStatusDropped, "reopen", orchestrator.TaskStatusParked},
	}
	for _, c := range cases {
		t.Run(c.action+"_from_"+string(c.from), func(t *testing.T) {
			task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: c.from, Card: &orchestrator.CardAttrs{}}
			txStore := &recordingTxStore{
				task:   task,
				triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}},
			}
			svc := &TaskWorkflowService{
				Tasks:      &stubTaskStore{task: task},
				Tx:         recordingTransactor{store: txStore},
				Meta:       stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}}}},
				TaskTriage: txStore,
			}
			result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: c.action})
			if err != nil {
				t.Fatalf("%s from %s (human, no suggestion): %v", c.action, c.from, err)
			}
			if result.Task.Status != c.want {
				t.Fatalf("%s from %s: status = %q, want %q", c.action, c.from, result.Task.Status, c.want)
			}
			// no suggestion ever existed in this fixture at all — proving the
			// transition did not secretly depend on one being present/absent.
			if got := txStore.triage["t1"]; got != nil {
				if _, ok := orchestrator.DetailSuggestionRaw(got.Detail); ok {
					t.Fatalf("%s from %s: unexpected suggestion present, want none (this fixture never wrote one)", c.action, c.from)
				}
			}
		})
	}
}

// TestApplyAction_CardTransitions_RejectedForNonHumanActor is 穴11's negative
// pin: khi (or any non-human actor, including an unset one) may never apply
// a card-lifecycle transition action DIRECTLY — only accept(verb)
// (applyAnswered, suggestion_accept.go) or a human's own click may. Includes
// ActorTask("") specifically — the literal actor string khi's trigger-job
// path stamps (empty TaskID, per the brief's "着手前確定事項 A") — to pin
// that the check is an exact `!= ActorHuman` comparison, not a prefix match
// that could special-case an empty task id differently.
func TestApplyAction_CardTransitions_RejectedForNonHumanActor(t *testing.T) {
	actorCases := []struct {
		name string
		ctx  context.Context
	}{
		{"khi_trigger_job_empty_task_id", orchestrator.WithActor(context.Background(), orchestrator.ActorTask(""))},
		{"khi_via_task", orchestrator.WithActor(context.Background(), orchestrator.ActorTask("some-task-id"))},
		{"daemon", orchestrator.WithActor(context.Background(), orchestrator.ActorDaemon)},
		{"unset", context.Background()},
	}
	// Both the current spelling and the retired one (working/done — §8's
	// short compat window) must be rejected: normalization happens BEFORE
	// this 403 check, so a legacy spelling gets no free pass through it.
	for _, verb := range []string{"go", "start", "working", "park", "drop", "complete", "done", "reopen"} {
		for _, a := range actorCases {
			t.Run(verb+"_"+a.name, func(t *testing.T) {
				// A status the verb is at least NAME-valid from (the guard
				// fires on the action name alone, before FromStatus is even
				// checked, so the exact status doesn't matter for this test —
				// parked is used uniformly for simplicity).
				task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
				txStore := &recordingTxStore{task: task, triage: map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}}}
				svc := newTriageWorkflowService(task, txStore)
				svc.TaskTriage = txStore

				_, err := svc.ApplyAction(a.ctx, task.ID, ApplyActionRequest{Type: verb})
				if err == nil {
					t.Fatalf("%s: expected rejection for actor %q pushing a card transition directly", verb, a.name)
				}
				se, ok := err.(*StatusError)
				if !ok || se.Code != http.StatusForbidden {
					t.Fatalf("%s: expected 403 StatusError, got %v", verb, err)
				}
			})
		}
	}
}

// TestTaskWorkflowServiceApplyAction_Park_UpsertsWakeCondition verifies the
// park-specific post-processing writes wake_at/wake_task_id into task_triage,
// preserving any existing kind/urgency/detail on the sidecar row. park's
// only FromStatus in v2 is working (design doc §3.2's edge table).
func TestTaskWorkflowServiceApplyAction_Park_UpsertsWakeCondition(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{
		task: task,
		triage: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Kind: "issue", Urgency: "week", Detail: []byte(`{"summary":"keep me"}`)},
		},
	}
	svc := newTriageWorkflowService(task, txStore)

	payload := []byte(`{"wake_at":"2026-09-01T00:00:00Z","wake_task_id":"blocking-task"}`)
	result, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park", Payload: payload})
	if err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	if result.Task.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", result.Task.Status)
	}

	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected task_triage row to exist after park")
	}
	if got.Kind != "issue" || got.Urgency != "week" || string(got.Detail) != `{"summary":"keep me"}` {
		t.Fatalf("park must preserve existing kind/urgency/detail, got %+v", got)
	}
	wantWakeAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got.WakeAt == nil || !got.WakeAt.Equal(wantWakeAt) {
		t.Fatalf("WakeAt = %v, want %v", got.WakeAt, wantWakeAt)
	}
	if got.WakeTaskID != "blocking-task" {
		t.Fatalf("WakeTaskID = %q, want %q", got.WakeTaskID, "blocking-task")
	}
}

// park with no payload still creates a sidecar row (WakeAt/WakeTaskID empty).
func TestTaskWorkflowServiceApplyAction_Park_NoPayload_CreatesEmptySidecarRow(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task}
	svc := newTriageWorkflowService(task, txStore)

	if _, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park"}); err != nil {
		t.Fatalf("ApplyAction(park): %v", err)
	}
	got := txStore.triage["t1"]
	if got == nil {
		t.Fatal("expected task_triage row to be created")
	}
	if got.WakeAt != nil {
		t.Fatalf("WakeAt = %v, want nil (no payload provided)", got.WakeAt)
	}
}

// TestTaskWorkflowServiceApplyAction_Park_PropagatesNonNotFoundTriageError is
// the regression test for codex review round 1's Minor: applyParkSideEffect
// used to treat every GetTaskTriage error (not just "no row") as "create a
// fresh sidecar", which on a transient DB error would silently blow away an
// existing row's kind/urgency/detail instead of surfacing the failure.
func TestTaskWorkflowServiceApplyAction_Park_PropagatesNonNotFoundTriageError(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	txStore := &recordingTxStore{task: task, getTaskTriageErr: fmt.Errorf("db connection reset")}
	svc := newTriageWorkflowService(task, txStore)

	_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: "park"})
	if err == nil {
		t.Fatal("expected the transient GetTaskTriage error to propagate, got nil")
	}
	if !strings.Contains(err.Error(), "db connection reset") {
		t.Fatalf("error = %v, want it to mention the underlying transient failure", err)
	}
}

// TestTaskWorkflowServiceApplyAction_RejectsNonManualActions is the
// regression test for codex review round 2's Blocker (v1): a hand-maintained
// blocklist of "internal-only" action names had missed job_failed, letting
// triaged →(job_failed)→aborted →(reopen)→executing bypass the ready-gate
// entirely. ApplyAction derives rejection generically from each rule's own
// Manual flag (StateMachine.IsManualAction) instead of a separate list.
// wake_due replaces v1's wake_triaged/wake_ready/wake_working (all three
// deleted along with the Wake mechanism itself — card machine v2 has no park
// origin left to disambiguate; see machine_card.go's own doc comment).
func TestTaskWorkflowServiceApplyAction_RejectsNonManualActions(t *testing.T) {
	for _, actionType := range []string{"job_failed", "progress", "done_request", "fail_request", "wake_due"} {
		task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
		svc := newTriageWorkflowService(task, &recordingTxStore{task: task})

		_, err := svc.ApplyAction(humanCtx(), task.ID, ApplyActionRequest{Type: actionType})
		if err == nil {
			t.Fatalf("action %q: expected rejection via public ApplyAction, got nil error", actionType)
		}
		se, ok := err.(*StatusError)
		if !ok || se.Code != http.StatusBadRequest {
			t.Fatalf("action %q: expected 400 StatusError, got %v", actionType, err)
		}
	}
}

// TestExecutionMachine_JobFailed_StillApplicableDirectlyViaSmApply confirms
// job_failed's real (non-ApplyAction) caller, TaskWorkflowService.CompleteJob,
// still works — job_failed is rejected only from the PUBLIC ApplyAction path,
// not from the legitimate internal one, which builds its own
// orchestrator.Action and calls sm.Apply directly (internal/api/workflow_job.go),
// never going through ApplyAction at all.
func TestExecutionMachine_JobFailed_StillApplicableDirectlyViaSmApply(t *testing.T) {
	sm := orchestrator.NewExecutionMachine()
	task := &orchestrator.Task{Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	next, err := sm.Apply(task, &orchestrator.Action{Type: "job_failed"})
	if err != nil {
		t.Fatalf("job_failed via direct sm.Apply (CompleteJob's path): %v", err)
	}
	if next.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("status = %q, want aborted", next.Status)
	}
}

type fixedDispatchResult struct {
	result *orchestrator.DispatchResult
}

func (d fixedDispatchResult) DispatchAndAdvance(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine) (*orchestrator.DispatchResult, error) {
	return d.result, nil
}

func (d fixedDispatchResult) ReplayHook(ctx context.Context, task *orchestrator.Task, meta *orchestrator.ProjectMeta, sm *orchestrator.StateMachine, hookID string) (*orchestrator.ReplayResult, error) {
	return &orchestrator.ReplayResult{FinalPayload: task.Exec.Payload}, nil
}

func TestTaskWorkflowServiceRunDispatchLoop_MustNotOverwriteTerminalStatusWhenPersistingPayload(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{"prompt":"start"}`)},
	}
	completed := &orchestrator.Task{
		ID:        task.ID,
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusDone,
		Exec:      &orchestrator.ExecAttrs{Behavior: task.Exec.Behavior, Payload: task.Exec.Payload},
	}

	txStore := &recordingTxStore{task: completed}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: []byte(`{"prompt":"start","artifact":{"summary":"ok"}}`),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusDone {
		t.Fatalf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusDone)
	}
	if lifecycle.cleanupTaskID != task.ID {
		t.Fatalf("cleanup task id = %q, want %q", lifecycle.cleanupTaskID, task.ID)
	}
}

// If the DB shows the task as aborted by the time we come back from a hook
// dispatch that computed a NewStatus advance, the loop must drop the advance
// rather than overwriting the terminal status.
func TestTaskWorkflowServiceRunDispatchLoop_MustNotOverwriteTerminalStatusWhenAdvanceIsAvailable(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{"prompt":"start"}`)},
	}
	aborted := &orchestrator.Task{
		ID:        task.ID,
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusAborted,
		Exec:      &orchestrator.ExecAttrs{Behavior: task.Exec.Behavior, Payload: task.Exec.Payload},
	}

	txStore := &recordingTxStore{task: aborted}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				FinalPayload: []byte(`{"prompt":"start","artifact":{"summary":"ok"}}`),
				NewStatus:    orchestrator.TaskStatusDone,
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	if txStore.updatedTask.Status != orchestrator.TaskStatusAborted {
		t.Fatalf("updated task status = %q, want %q", txStore.updatedTask.Status, orchestrator.TaskStatusAborted)
	}
	if lifecycle.cleanupTaskID != task.ID {
		t.Fatalf("cleanup task id = %q, want %q", lifecycle.cleanupTaskID, task.ID)
	}
	for _, a := range txStore.actions {
		if a.Type == "auto_advance" {
			t.Fatalf("unexpected auto_advance action written after abort: %+v", a)
		}
	}
}

// After a dispatch cycle, the awaiting.pending_answer must be stripped from
// the persisted payload so the answer is not re-consumed on the next hook run.
// Other awaiting fields (question / question_id) must survive so the kit can
// still see what was asked. Legacy persisted records may also carry session_id
// — the deserialiser ignores it (the field was removed) but the raw payload
// passes through unchanged.
func TestTaskWorkflowServiceRunDispatchLoop_ClearsPendingAnswerAfterDispatch(t *testing.T) {
	withAnswer := `{"awaiting":{"session_id":"sess-1","question_id":"q-1","pending_answer":"yes"}}`
	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(withAnswer)},
	}
	// The DB still returns the task-with-answer so the tx refresh gets it.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: task.Exec.Behavior, Payload: []byte(withAnswer)},
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				// Hook wrote back the full payload unchanged.
				FinalPayload: []byte(withAnswer),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	ap := orchestrator.GetAwaitingPayload(txStore.updatedTask.Exec.Payload)
	if ap.PendingAnswer != "" {
		t.Errorf("pending_answer = %q, want empty after dispatch", ap.PendingAnswer)
	}
	if ap.QuestionID != "q-1" {
		t.Errorf("question_id = %q, want q-1 (must be preserved)", ap.QuestionID)
	}
}

// Regression: when a hook calls `boid task notify --ask` mid-flight, the new
// awaiting trait is written to the DB by ApplyAction("ask") *during* the hook.
// The coordinator's FinalPayload, however, derives from a snapshot of
// task.Payload taken before the hook ran, so it carries the *previous* turn's
// awaiting trait. The dispatch-loop merge must not let that stale awaiting
// clobber the freshly-persisted DB row. Bug observed: 2nd Q&A turn displayed
// the 1st turn's question text in the Web UI.
func TestTaskWorkflowServiceRunDispatchLoop_MidHookAsk_PreservesNewAwaiting(t *testing.T) {
	staleAwaiting := `{"awaiting":{"question":"OLD","question_id":"q-1","pending_answer":"approve"}}`
	freshAwaiting := `{"awaiting":{"question":"NEW","question_id":"q-2"}}`

	task := &orchestrator.Task{
		ID:        "task-1",
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Exec:      &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(staleAwaiting)},
	}
	// DB row already reflects the mid-hook ApplyAction("ask"): question_id=q-2,
	// fresh question text. Status is awaiting because the in-flight notify --ask
	// transitioned it.
	taskInDB := &orchestrator.Task{
		ID:        task.ID,
		Type:      orchestrator.TaskTypeExecution,
		ProjectID: task.ProjectID,
		Status:    orchestrator.TaskStatusAwaiting,
		Exec:      &orchestrator.ExecAttrs{Behavior: task.Exec.Behavior, Payload: []byte(freshAwaiting)},
	}

	txStore := &recordingTxStore{task: taskInDB}
	lifecycle := &stubLifecycle{}
	svc := &TaskWorkflowService{
		Tx: recordingTransactor{store: txStore},
		Coordinator: fixedDispatchResult{
			result: &orchestrator.DispatchResult{
				// Coordinator's snapshot still has q-1 / OLD content.
				FinalPayload: []byte(staleAwaiting),
			},
		},
		Lifecycle: lifecycle,
	}

	svc.runDispatchLoop(
		context.Background(),
		task,
		&orchestrator.ProjectMeta{},
		orchestrator.NewExecutionMachine(),
	)

	if txStore.updatedTask == nil {
		t.Fatal("expected payload persistence update")
	}
	ap := orchestrator.GetAwaitingPayload(txStore.updatedTask.Exec.Payload)
	if ap.QuestionID != "q-2" {
		t.Errorf("question_id = %q, want q-2 (mid-hook ask must not be clobbered)", ap.QuestionID)
	}
	if ap.Question != "NEW" {
		t.Errorf("question = %q, want NEW (mid-hook ask must not be clobbered)", ap.Question)
	}
	if ap.PendingAnswer != "" {
		t.Errorf("pending_answer = %q, want empty (must be cleared)", ap.PendingAnswer)
	}
}
