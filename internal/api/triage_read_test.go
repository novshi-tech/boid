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

// stubTriageStore is a TaskTriageStore backed by in-memory maps.
type stubTriageStore struct {
	rows       map[string]*orchestrator.TaskTriage
	parkedFrom map[string]orchestrator.TaskStatus
	getErr     error
}

func (s *stubTriageStore) UpsertTaskTriage(tt *orchestrator.TaskTriage) error {
	if s.rows == nil {
		s.rows = map[string]*orchestrator.TaskTriage{}
	}
	s.rows[tt.TaskID] = tt
	return nil
}

func (s *stubTriageStore) SeedTaskTriage(taskID string) error {
	if s.rows == nil {
		s.rows = map[string]*orchestrator.TaskTriage{}
	}
	if _, ok := s.rows[taskID]; !ok {
		s.rows[taskID] = &orchestrator.TaskTriage{TaskID: taskID}
	}
	return nil
}

func (s *stubTriageStore) GetTaskTriage(taskID string) (*orchestrator.TaskTriage, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if tt, ok := s.rows[taskID]; ok {
		return tt, nil
	}
	return nil, errNoTriageRow
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
// ProjectID/Status filters ListTriage passes through.
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
		// "queue" mirrors ListTasks' pre-execution branch (store.go) so callers
		// that use it — SweepCanonicalSourceBreaches — are exercised for real
		// rather than silently matching nothing.
		if filter.Status == "queue" {
			if !orchestrator.IsPreExecutionStatus(t.Status) {
				continue
			}
		} else if filter.Status == "triage" {
			// pre-execution ∪ working — ListTriage's default floor (store.go).
			if !orchestrator.IsPreExecutionStatus(t.Status) && t.Status != orchestrator.TaskStatusWorking {
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

// TestGetTriage_ReturnsStoredAndDerivedFields pins S5: a parked triage task's
// view carries the stored sidecar columns, the opaque detail blob, AND
// parked_from — which exists nowhere as a column and is derived from the
// actions log (決定13). A read surface that returns only the columns would
// leave workspace-side callers unable to tell which side (triaged/ready) a
// parked task will wake back to.
func TestGetTriage_ReturnsStoredAndDerivedFields(t *testing.T) {
	wakeAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Title: "見積もり依頼", Status: orchestrator.TaskStatusParked}
	triage := &stubTriageStore{
		rows: map[string]*orchestrator.TaskTriage{
			"t1": {
				TaskID:  "t1",
				Kind:    "issue",
				Urgency: "today",
				WakeAt:  &wakeAt,
				Detail:  json.RawMessage(`{"attrs":{"summary":"見積もり"},"children":[{"id":"ch_00","status":"open"}]}`),
			},
		},
		parkedFrom: map[string]orchestrator.TaskStatus{"t1": orchestrator.TaskStatusReady},
	}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: triage}

	view, err := svc.GetTriage("t1")
	if err != nil {
		t.Fatalf("GetTriage: %v", err)
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
	if view.WakeAt == nil || !view.WakeAt.Equal(wakeAt) {
		t.Fatalf("wake_at = %v, want %v", view.WakeAt, wakeAt)
	}
	if view.ParkedFrom != orchestrator.TaskStatusReady {
		t.Fatalf("parked_from = %q, want ready (derived from the actions log)", view.ParkedFrom)
	}
	children, err := orchestrator.DetailChildren(view.Detail)
	if err != nil {
		t.Fatalf("detail children: %v", err)
	}
	if len(children) != 1 || children[0].ID != "ch_00" {
		t.Fatalf("detail blob not carried through verbatim: %s", view.Detail)
	}
}

// TestGetTriage_ParkedFromOnlyForParked pins that parked_from is not
// fabricated for a non-parked task: a working task has no meaningful "which
// side does it wake to", and a stale value there would be actively
// misleading.
func TestGetTriage_ParkedFromOnlyForParked(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusWorking}
	triage := &stubTriageStore{
		rows:       map[string]*orchestrator.TaskTriage{"t1": {TaskID: "t1"}},
		parkedFrom: map[string]orchestrator.TaskStatus{"t1": orchestrator.TaskStatusReady},
	}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: triage}

	view, err := svc.GetTriage("t1")
	if err != nil {
		t.Fatalf("GetTriage: %v", err)
	}
	if view.ParkedFrom != "" {
		t.Fatalf("parked_from = %q for a working task, want empty", view.ParkedFrom)
	}
}

// TestGetTriage_NoSidecarRowStillReturnsState pins the fail-open posture: a
// triage task whose sidecar row does not exist yet must still be readable
// (its status is the state that matters most). Returning 404 here would make
// the read surface report "this task does not exist" for a task that plainly
// does.
func TestGetTriage_NoSidecarRowStillReturnsState(t *testing.T) {
	task := &orchestrator.Task{ID: "t1", ProjectID: "p1", Status: orchestrator.TaskStatusTriaged}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: []*orchestrator.Task{task}}, TaskTriage: &stubTriageStore{}}

	view, err := svc.GetTriage("t1")
	if err != nil {
		t.Fatalf("GetTriage: %v", err)
	}
	if view.Status != orchestrator.TaskStatusTriaged {
		t.Fatalf("status = %q, want triaged", view.Status)
	}
	if view.Urgency != "" || view.Detail != nil {
		t.Fatalf("expected empty sidecar fields, got %+v", view)
	}
}

// TestGetTriage_MissingTaskIs404 pins that a genuinely absent task is still an
// error — the fail-open tolerance above applies to the sidecar row, not to the
// task itself.
func TestGetTriage_MissingTaskIs404(t *testing.T) {
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{}, TaskTriage: &stubTriageStore{}}
	if _, err := svc.GetTriage("nope"); err == nil {
		t.Fatal("GetTriage on a missing task returned no error")
	} else {
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 404 {
			t.Fatalf("err = %v, want a 404 StatusError", err)
		}
	}
}

// TestListTriage_OnlyTriageTasks pins the list predicate: a project's ordinary
// tasks (the ingest/sweep executor tasks that live in the very same meta
// project) must not appear in the triage listing. The discriminator is the
// sidecar row, which CreateTask now seeds for every pre-execution task.
func TestListTriage_OnlyTriageTasks(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "triage1", ProjectID: "meta", Status: orchestrator.TaskStatusTriaged},
		{ID: "ingest1", ProjectID: "meta", Status: orchestrator.TaskStatusExecuting},
		{ID: "triage2", ProjectID: "meta", Status: orchestrator.TaskStatusWorking},
		{ID: "other", ProjectID: "elsewhere", Status: orchestrator.TaskStatusTriaged},
	}
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"triage1": {TaskID: "triage1", Urgency: "now"},
		"triage2": {TaskID: "triage2"},
		"other":   {TaskID: "other"},
	}}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: tasks}, TaskTriage: triage}

	views, err := svc.ListTriage(orchestrator.TaskFilter{ProjectID: "meta"})
	if err != nil {
		t.Fatalf("ListTriage: %v", err)
	}
	got := map[string]bool{}
	for _, v := range views {
		got[v.TaskID] = true
	}
	if len(got) != 2 || !got["triage1"] || !got["triage2"] {
		t.Fatalf("listed %v, want exactly triage1 and triage2", got)
	}
}

// TestListTriage_PassesStatusFilter pins that the caller's status filter
// reaches the task store (khi's sweep lists by state).
func TestListTriage_PassesStatusFilter(t *testing.T) {
	tasks := []*orchestrator.Task{
		{ID: "a", ProjectID: "meta", Status: orchestrator.TaskStatusTriaged},
		{ID: "b", ProjectID: "meta", Status: orchestrator.TaskStatusParked},
	}
	triage := &stubTriageStore{rows: map[string]*orchestrator.TaskTriage{
		"a": {TaskID: "a"},
		"b": {TaskID: "b"},
	}}
	svc := &TaskWorkflowService{Tasks: &multiTaskStore{tasks: tasks}, TaskTriage: triage}

	views, err := svc.ListTriage(orchestrator.TaskFilter{ProjectID: "meta", Status: "parked"})
	if err != nil {
		t.Fatalf("ListTriage: %v", err)
	}
	if len(views) != 1 || views[0].TaskID != "b" {
		t.Fatalf("listed %+v, want only the parked task", views)
	}
}

// TestListTriage_DefaultsToLiveTriageStatuses pins the query floor: an unset
// status must not degrade into "every task row ever created, then one sidecar
// point query per row" — the shape WebHandler.TaskList avoids by defaulting to
// status=open.
func TestListTriage_DefaultsToLiveTriageStatuses(t *testing.T) {
	store := &filterRecordingTaskStore{}
	svc := &TaskWorkflowService{Tasks: store, TaskTriage: &stubTriageStore{}}

	if _, err := svc.ListTriage(orchestrator.TaskFilter{ProjectID: "meta"}); err != nil {
		t.Fatalf("ListTriage: %v", err)
	}
	if store.last.Status != "triage" {
		t.Fatalf("status filter = %q, want the \"triage\" floor (pre-execution ∪ working)", store.last.Status)
	}

	// An explicit status is never overridden — done cards stay reachable.
	if _, err := svc.ListTriage(orchestrator.TaskFilter{Status: "done"}); err != nil {
		t.Fatalf("ListTriage(done): %v", err)
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
