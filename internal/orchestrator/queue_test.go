package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestQueueEligible(t *testing.T) {
	cases := []struct {
		name    string
		status  orchestrator.TaskStatus
		urgency string
		want    bool
	}{
		{"ready+now", orchestrator.TaskStatusReady, orchestrator.UrgencyNow, true},
		{"triaged+today", orchestrator.TaskStatusTriaged, orchestrator.UrgencyToday, true},
		{"triaged+week", orchestrator.TaskStatusTriaged, orchestrator.UrgencyWeek, true},
		{"triaged+someday", orchestrator.TaskStatusTriaged, orchestrator.UrgencySomeday, false},
		{"triaged+empty", orchestrator.TaskStatusTriaged, "", false},
		{"captured+now", orchestrator.TaskStatusCaptured, orchestrator.UrgencyNow, false},
		{"parked+now", orchestrator.TaskStatusParked, orchestrator.UrgencyNow, false},
		{"working+now", orchestrator.TaskStatusWorking, orchestrator.UrgencyNow, false},
		{"done+now", orchestrator.TaskStatusDone, orchestrator.UrgencyNow, false},
		{"dropped+now", orchestrator.TaskStatusDropped, orchestrator.UrgencyNow, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := orchestrator.QueueEligible(c.status, c.urgency); got != c.want {
				t.Errorf("QueueEligible(%s, %s) = %v, want %v", c.status, c.urgency, got, c.want)
			}
		})
	}
}

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

func TestStateRank_ReadyBeforeTriaged(t *testing.T) {
	if !(orchestrator.StateRank(orchestrator.TaskStatusReady) < orchestrator.StateRank(orchestrator.TaskStatusTriaged)) {
		t.Error("ready must rank before triaged")
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
