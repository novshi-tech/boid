package api

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/apiwire"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// A child task's blocking ask used to be silently unpageable: the old rule
// was `task.ParentID != "" → never notify`, justified by "their supervisor's
// monitoring loop notices the awaiting transition". A triage card's child has
// no such loop — the parent (`task_triage`) never runs an agent at all — so
// on 2026-08-24 four asks in a row went out with no notification and no UI
// affordance, and were only answered because a human happened to be watching
// the task list.
//
// The replacement rule asks the question the old one assumed the answer to:
// does the parent actually have a live agent that could notice? These tests
// pin both sides of it.
func askNotifyFixture(parentID string, parentJobs []*Job) (*TaskAppService, *recordingNotifier) {
	notifier := &recordingNotifier{}
	tasks := &stubTaskStore{tasks: map[string]*orchestrator.Task{
		"parent-1": {ID: "parent-1", Title: "triage card", ProjectID: "p1", Status: orchestrator.TaskStatusWorking},
	}}
	jobs := &stubJobStore{jobsByTask: map[string][]*Job{}}
	if parentJobs != nil {
		jobs.jobsByTask["parent-1"] = parentJobs
	}
	svc := &TaskAppService{Tasks: tasks, Jobs: jobs, Notify: notifier}
	return svc, notifier
}

func runningJob() *Job {
	return &Job{ID: "j-run", TaskID: "parent-1", Status: apiwire.JobStatusRunning}
}

func finishedJob() *Job {
	return &Job{ID: "j-fin", TaskID: "parent-1", Status: apiwire.JobStatusCompleted}
}

// The case that actually broke: a triage card's child. The parent has no job
// at all, so nothing is watching — the user must be paged.
func TestFireUserAskNotification_ChildOfWatcherlessParent_Notifies(t *testing.T) {
	svc, notifier := askNotifyFixture("parent-1", nil)
	task := &orchestrator.Task{ID: "child-1", ParentID: "parent-1", Title: "reply to X", ProjectID: "p1"}

	svc.fireUserAskNotification(context.Background(), task, "send this?", "q-1")

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1 (nothing is watching this child)", len(notifier.events))
	}
	if got, want := notifier.events[0].URLPath, "/tasks/child-1/questions/q-1"; got != want {
		t.Errorf("URLPath = %q, want %q", got, want)
	}
}

// A parent whose jobs have all finished is equally watcherless — a supervisor
// that already exited cannot notice anything.
func TestFireUserAskNotification_ChildOfFinishedParent_Notifies(t *testing.T) {
	svc, notifier := askNotifyFixture("parent-1", []*Job{finishedJob()})
	task := &orchestrator.Task{ID: "child-1", ParentID: "parent-1", ProjectID: "p1"}

	svc.fireUserAskNotification(context.Background(), task, "send this?", "q-1")

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1 (parent's agent already exited)", len(notifier.events))
	}
}

// The case the original rule was written for, and the reason it is kept: a
// live supervisor/drive agent IS parked on the parent, so paging the user
// would duplicate what that loop is about to handle.
func TestFireUserAskNotification_ChildOfLiveParent_StaysQuiet(t *testing.T) {
	svc, notifier := askNotifyFixture("parent-1", []*Job{finishedJob(), runningJob()})
	task := &orchestrator.Task{ID: "child-1", ParentID: "parent-1", ProjectID: "p1"}

	svc.fireUserAskNotification(context.Background(), task, "send this?", "q-1")

	if len(notifier.events) != 0 {
		t.Fatalf("events = %v, want none (a live agent on the parent will notice)", notifier.events)
	}
}

// Root tasks are unchanged: they always page.
func TestFireUserAskNotification_RootTask_Notifies(t *testing.T) {
	svc, notifier := askNotifyFixture("", nil)
	task := &orchestrator.Task{ID: "root-1", Title: "root", ProjectID: "p1"}

	svc.fireUserAskNotification(context.Background(), task, "send this?", "q-1")

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1", len(notifier.events))
	}
}

// Unreadable parent state must fall back to notifying. Staying quiet on an
// error reproduces the exact failure being fixed (an ask nobody is told
// about); an extra notification is recoverable, a missed one is not. Same
// reasoning as khi's behaviors_of returning None meaning "could not read",
// not "absent".
func TestFireUserAskNotification_UnreadableParent_Notifies(t *testing.T) {
	notifier := &recordingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{tasks: map[string]*orchestrator.Task{}}, // parent lookup misses
		Jobs:   nil,                                                   // and no job store at all
		Notify: notifier,
	}
	task := &orchestrator.Task{ID: "child-1", ParentID: "ghost", ProjectID: "p1"}

	svc.fireUserAskNotification(context.Background(), task, "send this?", "q-1")

	if len(notifier.events) != 1 {
		t.Fatalf("events = %d, want 1 (unknown parent state must not silence the ask)", len(notifier.events))
	}
}
