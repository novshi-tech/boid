package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// errNoTriageRow mirrors what orchestrator.GetTaskTriage returns when the
// sidecar row is absent (a wrapped sql.ErrNoRows) — the read surface's
// "no row yet" tolerance is keyed on errors.Is(err, sql.ErrNoRows).
var errNoTriageRow = fmt.Errorf("get task_triage: %w", sql.ErrNoRows)

// ---- Phase 1 PR-5a (docs/plans/cross-project-issue-triage.md) ----
//
// These tests pin S5 (読み戻しの完全性): the daemon-side read surface for the
// task_triage sidecar. 決定14 makes the daemon the sole source of truth for a
// triage task's state, which is only viable if workspace-side callers can
// read that state back — including the fields DERIVED from the actions log
// (parked_from), not just the stored columns.

// stubTriageStore is a CardStore backed by in-memory maps.
type stubTriageStore struct {
	rows       map[string]*orchestrator.CardAttrs
	parkedFrom map[string]orchestrator.TaskStatus
	getErr     error

	// listErr, when non-nil, is returned by ListTaskTriageByTaskIDs
	// alongside whatever partial `out` it can still build from rows — the
	// same best-effort "a non-nil error means something went wrong, but
	// out may still be partially or fully useful" contract the real
	// orchestrator.ListTaskTriageByTaskIDs has (see its doc comment,
	// internal/orchestrator/card.go). Kept separate from getErr
	// (which only affects GetTaskTriage) so a test can exercise the batch
	// path's error handling without also breaking single-row lookups.
	listErr error

	// listTaskTriageByTaskIDsCalls counts ListTaskTriageByTaskIDs
	// invocations — used by TestTriageByTaskID_BatchesIntoOneCall
	// (web_test.go) to pin the N+1 fix (BD-8 残件1): triageByTaskID must
	// call this once per view render, never once per task.
	listTaskTriageByTaskIDsCalls int
}

func (s *stubTriageStore) UpsertTaskTriage(tt *orchestrator.CardAttrs) error {
	if s.rows == nil {
		s.rows = map[string]*orchestrator.CardAttrs{}
	}
	s.rows[tt.TaskID] = tt
	return nil
}

func (s *stubTriageStore) GetTaskTriage(taskID string) (*orchestrator.CardAttrs, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if tt, ok := s.rows[taskID]; ok {
		return tt, nil
	}
	return nil, errNoTriageRow
}

func (s *stubTriageStore) ListTaskTriageByTaskIDs(taskIDs []string) (map[string]*orchestrator.CardAttrs, error) {
	s.listTaskTriageByTaskIDsCalls++
	out := map[string]*orchestrator.CardAttrs{}
	for _, id := range taskIDs {
		if tt, ok := s.rows[id]; ok {
			out[id] = tt
		}
	}
	return out, s.listErr
}

func (s *stubTriageStore) DeleteTaskTriage(taskID string) error {
	delete(s.rows, taskID)
	return nil
}

func (s *stubTriageStore) ParkedFrom(taskID string) (orchestrator.TaskStatus, error) {
	if from, ok := s.parkedFrom[taskID]; ok {
		return from, nil
	}
	return "", errNoTriageRow
}

// multiTaskStore is a TaskStore over a fixed set of tasks, supporting the
// ProjectID/Status filters ListCards passes through.
type multiTaskStore struct {
	tasks []*orchestrator.Task
}

func (s *multiTaskStore) CreateTask(*orchestrator.Task) error { return nil }
func (s *multiTaskStore) GetTask(id string) (*orchestrator.Task, error) {
	for _, t := range s.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, orchestrator.ErrTaskNotFound
}

func (s *multiTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	var out []*orchestrator.Task
	for _, t := range s.tasks {
		if filter.ProjectID != "" && t.ProjectID != filter.ProjectID {
			continue
		}
		if filter.Status == "triage" {
			// card-model-cleanup PR-2 (migration 0045) restates the "triage"
			// floor on tasks.type instead of a status enumeration: every card
			// that hasn't reached a terminal status yet (parked ∪ working) —
			// mirrors store.go's own ListTasks "triage" branch. IsPreExecution
			// Status is gone entirely (REMOVED, no replacement function — the
			// concept is now just task.Type == TaskTypeCard).
			if t.Type != orchestrator.TaskTypeCard ||
				(t.Status != orchestrator.TaskStatusParked && t.Status != orchestrator.TaskStatusWorking) {
				continue
			}
		} else if filter.Status != "" && string(t.Status) != filter.Status {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *multiTaskStore) UpdateTask(*orchestrator.Task) error { return nil }
func (s *multiTaskStore) DeleteTask(string) error             { return nil }
func (s *multiTaskStore) FindTaskByRemote(string) (*orchestrator.Task, error) {
	return nil, nil
}

func (s *multiTaskStore) FindTaskByRef(string, string, string) (*orchestrator.Task, error) {
	return nil, nil
}
func (s *multiTaskStore) ListChildren(string) ([]*orchestrator.Task, error) { return nil, nil }

// TestGetCard_ReturnsStoredAndDerivedFields pins S5: a parked triage task's
// view carries the stored sidecar columns, the opaque detail blob, AND
// parked_from — which exists nowhere as a column and is derived from the
// actions log (決定13). A read surface that returns only the columns would
// leave workspace-side callers unable to tell which side (triaged/ready) a
// parked task will wake back to.
func TestGetCard_ReturnsStoredAndDerivedFields(t *testing.T) {
	wakeAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Title: "見積もり依頼", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	triage := &stubTriageStore{
		rows: map[string]*orchestrator.CardAttrs{
			"t1": {
				TaskID:         "t1",
				Kind:           "issue",
				Urgency:        "today",
				WakeAt:         &wakeAt,
				SuggestionVerb: "park",
				Detail:         json.RawMessage(`{"attrs":{"summary":"見積もり"},"children":[{"id":"ch_00","status":"open"}]}`),
			},
		},
		// card-model-cleanup PR-2: orchestrator.TaskStatusReady no longer
		// exists (folded into parked well before this PR). park's only
		// FromStatus under card machine v2 is "working" (machine_card.go), so
		// that is the only value ParkedFrom can actually return now.
		parkedFrom: map[string]orchestrator.TaskStatus{"t1": orchestrator.TaskStatusWorking},
	}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: triage}

	view, err := svc.GetCard("t1")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if view.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", view.Status)
	}
	if view.ProjectID != "p1" || view.Title != "見積もり依頼" {
		t.Fatalf("task fields not carried through: %+v", view)
	}
	if view.Kind != "issue" || view.Urgency != "today" {
		t.Fatalf("sidecar columns not carried through: %+v", view)
	}
	// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1):
	// suggestion_verb is a promoted column like Kind/Urgency, so the read
	// projection carries it through the same way.
	if view.SuggestionVerb != "park" {
		t.Fatalf("suggestion_verb not carried through: %+v", view)
	}
	if view.WakeAt == nil || !view.WakeAt.Equal(wakeAt) {
		t.Fatalf("wake_at = %v, want %v", view.WakeAt, wakeAt)
	}
	if view.ParkedFrom != orchestrator.TaskStatusWorking {
		t.Fatalf("parked_from = %q, want working (derived from the actions log)", view.ParkedFrom)
	}
	children, err := orchestrator.DetailChildren(view.Detail)
	if err != nil {
		t.Fatalf("detail children: %v", err)
	}
	if len(children) != 1 || children[0].ID != "ch_00" {
		t.Fatalf("detail blob not carried through verbatim: %s", view.Detail)
	}
}

// TestGetCard_ParkedFromOnlyForParked pins that parked_from is not
// fabricated for a non-parked task: a working task has no meaningful "which
// side does it wake to", and a stale value there would be actively
// misleading.
func TestGetCard_ParkedFromOnlyForParked(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	triage := &stubTriageStore{
		rows:       map[string]*orchestrator.CardAttrs{"t1": {TaskID: "t1"}},
		parkedFrom: map[string]orchestrator.TaskStatus{"t1": orchestrator.TaskStatusWorking},
	}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: triage}

	view, err := svc.GetCard("t1")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if view.ParkedFrom != "" {
		t.Fatalf("parked_from = %q for a working task, want empty", view.ParkedFrom)
	}
}

// TestGetCard_NoSidecarRowStillReturnsState pins the fail-open posture: a
// triage task whose sidecar row does not exist yet must still be readable
// (its status is the state that matters most). Returning 404 here would make
// the read surface report "this task does not exist" for a task that plainly
// does.
func TestGetCard_NoSidecarRowStillReturnsState(t *testing.T) {
	// card-model-cleanup PR-2: orchestrator.TaskStatusTriaged no longer
	// exists (folded into parked well before this PR — see model.go's
	// TaskStatus doc comment); parked is now a card's initial/resting state.
	task := &orchestrator.Task{ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: &stubTriageStore{}}

	view, err := svc.GetCard("t1")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if view.Status != orchestrator.TaskStatusParked {
		t.Fatalf("status = %q, want parked", view.Status)
	}
	if view.Urgency != "" || view.Detail != nil {
		t.Fatalf("expected empty sidecar fields, got %+v", view)
	}
}

// TestGetCard_MissingTaskIs404 pins that a genuinely absent task is still an
// error — the fail-open tolerance above applies to the sidecar row, not to the
// task itself.
func TestGetCard_MissingTaskIs404(t *testing.T) {
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{}, TaskTriage: &stubTriageStore{}}
	if _, err := svc.GetCard("nope"); err == nil {
		t.Fatal("GetCard on a missing task returned no error")
	} else {
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 404 {
			t.Fatalf("err = %v, want a 404 StatusError", err)
		}
	}
}

// TestListCards_OnlyTriageTasks pins the list predicate: a project's ordinary
// tasks (the ingest/sweep executor tasks that live in the very same meta
// project) must not appear in the triage listing. The discriminator is the
// sidecar row, which CreateTask now seeds for every pre-execution task.
func TestListCards_OnlyTriageTasks(t *testing.T) {
	// card-model-cleanup PR-2: orchestrator.TaskStatusTriaged no longer
	// exists (folded into parked well before this PR); the "triage" floor
	// (multiTaskStore.ListTasks above, mirroring store.go's own ListTasks) is
	// now `Type == TaskTypeCard && Status IN (parked, working)`.
	tasks := []*orchestrator.Task{
		{ID: "triage1", Type: orchestrator.TaskTypeCard, ProjectID: "meta", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}},
		{ID: "ingest1", Type: orchestrator.TaskTypeExecution, ProjectID: "meta", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}},
		{ID: "triage2", Type: orchestrator.TaskTypeCard, ProjectID: "meta", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
		{ID: "other", Type: orchestrator.TaskTypeCard, ProjectID: "elsewhere", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}},
	}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"triage1": {TaskID: "triage1", Urgency: "now"},
		"triage2": {TaskID: "triage2"},
		"other":   {TaskID: "other"},
	}}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: tasks}, TaskTriage: triage}

	views, err := svc.ListCards(orchestrator.TaskFilter{ProjectID: "meta"})
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	got := map[string]bool{}
	for _, v := range views {
		got[v.TaskID] = true
	}
	if len(got) != 2 || !got["triage1"] || !got["triage2"] {
		t.Fatalf("listed %v, want exactly triage1 and triage2", got)
	}
}

// TestListCards_PassesStatusFilter pins that the caller's status filter
// reaches the task store (khi's sweep lists by state).
func TestListCards_PassesStatusFilter(t *testing.T) {
	// card-model-cleanup PR-2: "a" used to be orchestrator.TaskStatusTriaged
	// (a status that no longer exists — folded into parked well before this
	// PR). This test's whole point is an explicit status filter narrowing
	// from a fixture with TWO distinct statuses, so "a" is now working
	// instead — any card status other than "b"'s parked keeps that intent.
	tasks := []*orchestrator.Task{
		{ID: "a", Type: orchestrator.TaskTypeCard, ProjectID: "meta", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}},
		{ID: "b", Type: orchestrator.TaskTypeCard, ProjectID: "meta", Status: orchestrator.TaskStatusParked, Card: &orchestrator.CardAttrs{}},
	}
	triage := &stubTriageStore{rows: map[string]*orchestrator.CardAttrs{
		"a": {TaskID: "a"},
		"b": {TaskID: "b"},
	}}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: tasks}, TaskTriage: triage}

	views, err := svc.ListCards(orchestrator.TaskFilter{ProjectID: "meta", Status: "parked"})
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(views) != 1 || views[0].TaskID != "b" {
		t.Fatalf("listed %+v, want only the parked task", views)
	}
}

// TestListCards_DefaultsToLiveTriageStatuses pins the query floor: an unset
// status must not degrade into "every task row ever created, then one sidecar
// point query per row" — the shape WebHandler.TaskList avoids by defaulting to
// status=open.
func TestListCards_DefaultsToLiveTriageStatuses(t *testing.T) {
	store := &filterRecordingTaskStore{}
	svc := &TaskWorkflowService{Tasks: store, TaskTriage: &stubTriageStore{}}

	if _, err := svc.ListCards(orchestrator.TaskFilter{ProjectID: "meta"}); err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if store.last.Status != "triage" {
		t.Fatalf("status filter = %q, want the \"triage\" floor (pre-execution ∪ working)", store.last.Status)
	}

	// An explicit status is never overridden — done cards stay reachable.
	if _, err := svc.ListCards(orchestrator.TaskFilter{Status: "done"}); err != nil {
		t.Fatalf("ListCards(done): %v", err)
	}
	if store.last.Status != "done" {
		t.Fatalf("status filter = %q, want the caller's explicit value", store.last.Status)
	}
}

type filterRecordingTaskStore struct {
	multiTaskStore
	last orchestrator.TaskFilter
}

func (s *filterRecordingTaskStore) ListTasks(filter orchestrator.TaskFilter) ([]*orchestrator.Task, error) {
	s.last = filter
	return nil, nil
}

// TestTriageView_CarriesDescription pins that the read surface returns the task
// row's description. Without it the workspace side has to make a second
// `boid task show --field description` call per card just to diff its own
// rendered body against what the daemon holds — which is exactly the
// "読み戻しが完全でない" gap PR-5a set out to close, only one field further in.
// khi's project_card.py renders the note body FROM this view, so a missing
// description means one extra round trip per note on every sweep.
func TestTriageView_CarriesDescription(t *testing.T) {
	// card-model-cleanup PR-2: orchestrator.TaskStatusTriaged no longer
	// exists (folded into parked well before this PR).
	task := &orchestrator.Task{
		ID: "t1", Type: orchestrator.TaskTypeCard, ProjectID: "p1", Title: "見積もり依頼",
		Description: "顧客から見積もりの催促。\n\n- canonical source: ROOKPF-303",
		Status:      orchestrator.TaskStatusParked,
		Card:        &orchestrator.CardAttrs{},
	}
	triage := &stubTriageStore{
		rows: map[string]*orchestrator.CardAttrs{
			"t1": {TaskID: "t1", Kind: "issue", Urgency: "today"},
		},
	}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: triage}

	view, err := svc.GetCard("t1")
	if err != nil {
		t.Fatalf("GetCard: %v", err)
	}
	if view.Description != task.Description {
		t.Fatalf("GetCard description = %q, want %q", view.Description, task.Description)
	}

	views, err := svc.ListCards(orchestrator.TaskFilter{})
	if err != nil {
		t.Fatalf("ListCards: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("ListCards returned %d views, want 1", len(views))
	}
	if views[0].Description != task.Description {
		t.Fatalf("ListCards description = %q, want %q", views[0].Description, task.Description)
	}
}
