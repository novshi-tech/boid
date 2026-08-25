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

// UrgencyRank backs the queue_next view's ordering (store.go): urgency now >
// today > week, someday/unrecognized sort last. Lower rank sorts first. This
// is a pure-Go mirror of the CASE expression in store.go's "queue_next"
// ORDER BY, kept for unit testing the ranking in isolation — the CASE
// expression itself must stay in lockstep with this function.
//
// PR-2 (docs/plans/suggestion-as-state-transition-impl.md §4.1) removed this
// file's QueueEligible and StateRank: queue membership is no longer a
// (status, urgency) predicate at all (it's `suggestion_verb != ”`, any
// status — design doc §3.6, 「一覧は suggestion で駆動する」), and the v1
// "state (ready が先)" ordering tier StateRank backed has no SQL counterpart
// left to mirror — card machine v2 has no "ready" status to rank. Keeping
// either function around describing a rule store.go no longer implements
// would be actively misleading, not merely unused.
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
func ShouldWake(now time.Time, tt *CardAttrs, wakeTaskFound bool, wakeTaskStatus TaskStatus) bool {
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
