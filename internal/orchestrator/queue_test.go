package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestUrgencyRank_Ordering(t *testing.T) {
	if !(orchestrator.UrgencyRank(orchestrator.UrgencyNow) < orchestrator.UrgencyRank(orchestrator.UrgencyToday)) {
		t.Error("now must rank before today")
	}
	if !(orchestrator.UrgencyRank(orchestrator.UrgencyToday) < orchestrator.UrgencyRank(orchestrator.UrgencyWeek)) {
		t.Error("today must rank before week")
	}
	if !(orchestrator.UrgencyRank(orchestrator.UrgencyWeek) < orchestrator.UrgencyRank(orchestrator.UrgencySomeday)) {
		t.Error("week must rank before someday")
	}
	if !(orchestrator.UrgencyRank(orchestrator.UrgencyWeek) < orchestrator.UrgencyRank("garbage")) {
		t.Error("unrecognized urgency must rank last, same as someday")
	}
}

func TestShouldWake_DateCondition(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	if !orchestrator.ShouldWake(now, &orchestrator.TaskTriage{WakeAt: &past}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at in the past must wake")
	}
	if !orchestrator.ShouldWake(now, &orchestrator.TaskTriage{WakeAt: &now}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at exactly now (<=) must wake")
	}
	if orchestrator.ShouldWake(now, &orchestrator.TaskTriage{WakeAt: &future}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at in the future must not wake")
	}
	if orchestrator.ShouldWake(now, &orchestrator.TaskTriage{}, true, orchestrator.TaskStatusExecuting) {
		t.Error("no wake condition at all must not wake")
	}
}

func TestShouldWake_TaskCondition(t *testing.T) {
	now := time.Now()
	tt := &orchestrator.TaskTriage{WakeTaskID: "some-task"}

	if orchestrator.ShouldWake(now, tt, true, orchestrator.TaskStatusExecuting) {
		t.Error("referenced task still non-terminal must not wake")
	}
	for _, terminal := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusAborted, orchestrator.TaskStatusDropped} {
		if !orchestrator.ShouldWake(now, tt, true, terminal) {
			t.Errorf("referenced task terminal (%s) must wake", terminal)
		}
	}
	if !orchestrator.ShouldWake(now, tt, false, "") {
		t.Error("referenced task not found (gone) must wake — fail-open, does not strand the parked task")
	}
}

func TestShouldWake_NilTriage(t *testing.T) {
	if orchestrator.ShouldWake(time.Now(), nil, true, orchestrator.TaskStatusDone) {
		t.Error("nil task_triage must not wake")
	}
}
