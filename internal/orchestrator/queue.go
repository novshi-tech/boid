package orchestrator

import "time"

// Urgency vocabulary for task_triage.urgency (queue の決定論的評価 節、論点d 語彙は
// Phase 0 実運用からの逆輸入 — Phase 0 の khi-task-collector dogfood がすでに
// now/today/week/someday を使っている)。
const (
	UrgencyNow     = "now"
	UrgencyToday   = "today"
	UrgencyWeek    = "week"
	UrgencySomeday = "someday"
)

// queueUrgencies is the set of urgency values eligible for queue membership
// (queue 節 rule 2). someday (and empty/unset urgency) is deliberately
// excluded — 決定9: someday only resurfaces via the periodic 棚卸し (UC-5),
// never via the queue itself.
var queueUrgencies = map[string]bool{
	UrgencyNow:   true,
	UrgencyToday: true,
	UrgencyWeek:  true,
}

// QueueEligible reports whether a (status, urgency) pair belongs in the
// queue view per queue 節 rule 2: 「state ∈ {ready, triaged} かつ urgency ∈
// {now, today, week}」。
//
// captured is deliberately excluded even though it is a pre-execution
// status: per UC-4 (head-capture), a captured triage task has not been
// triaged into an urgency yet and gets its own confirmation section, not
// the main queue. parked/someday/dropped/working and the execution-lifecycle
// statuses are excluded by construction — none of them are ready/triaged.
func QueueEligible(status TaskStatus, urgency string) bool {
	if status != TaskStatusReady && status != TaskStatusTriaged {
		return false
	}
	return queueUrgencies[urgency]
}

// UrgencyRank / StateRank back queue 節 rule 3's ordering (urgency: now >
// today > week; state: ready before triaged — 「Go 一発で捌けるものを上に」)。
// Lower rank sorts first. Unrecognized urgency values sort last (rank 3),
// same treatment as someday.
func UrgencyRank(urgency string) int {
	switch urgency {
	case UrgencyNow:
		return 0
	case UrgencyToday:
		return 1
	case UrgencyWeek:
		return 2
	default:
		return 3
	}
}

// StateRank implements the "state (ready が先)" half of rule 3.
func StateRank(status TaskStatus) int {
	if status == TaskStatusReady {
		return 0
	}
	return 1
}

// ShouldWake evaluates queue 節 rule 1 (wake 評価) for a parked task: wake
// if wake_at has passed, OR the task referenced by wake_task_id has reached
// a terminal status. Both are decided purely from tt's fields plus facts
// the caller supplies about "now" and the referenced task — no agent
// judgment is involved (決定12).
//
// wakeTaskFound=false (the referenced task no longer exists — deleted or
// GC'd) is treated the same as "terminal": a vanished reference must not
// permanently strand a parked task (fail-open, ストレージ節 原則2's same
// direction — daemon-side state loss/drift resolves toward re-surfacing,
// never toward silently hiding something forever).
func ShouldWake(now time.Time, tt *TaskTriage, wakeTaskFound bool, wakeTaskStatus TaskStatus) bool {
	if tt == nil {
		return false
	}
	if tt.WakeAt != nil && !tt.WakeAt.After(now) {
		return true
	}
	if tt.WakeTaskID != "" {
		if !wakeTaskFound || IsTerminalStatus(wakeTaskStatus) {
			return true
		}
	}
	return false
}
