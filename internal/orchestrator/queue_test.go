package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestUrgencyRank_Ordering / UrgencyNow / UrgencyRank were removed by
// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6, §5 論点2) alongside
// their production counterparts in queue.go — urgency dropped from every
// display surface and UrgencyRank had zero production callers left.

func TestShouldWake_DateCondition(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	if !orchestrator.ShouldWake(now, &orchestrator.CardAttrs{WakeAt: &past}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at in the past must wake")
	}
	if !orchestrator.ShouldWake(now, &orchestrator.CardAttrs{WakeAt: &now}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at exactly now (<=) must wake")
	}
	if orchestrator.ShouldWake(now, &orchestrator.CardAttrs{WakeAt: &future}, true, orchestrator.TaskStatusExecuting) {
		t.Error("wake_at in the future must not wake")
	}
	if orchestrator.ShouldWake(now, &orchestrator.CardAttrs{}, true, orchestrator.TaskStatusExecuting) {
		t.Error("no wake condition at all must not wake")
	}
}

func TestShouldWake_TaskCondition(t *testing.T) {
	now := time.Now()
	tt := &orchestrator.CardAttrs{WakeTaskID: "some-task"}

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
