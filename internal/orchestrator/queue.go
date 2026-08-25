package orchestrator

import "time"

// docs/plans/webui-detail-list-redesign.md PR-4 (§3.6, §5 論点2) removed this
// file's urgency vocabulary consts (UrgencyNow/Today/Week/Someday) and
// UrgencyRank: urgency dropped from every display surface (list row,
// task_tree.templ badge, queue_next ORDER BY — all gone), and UrgencyRank
// itself had zero production callers left (comments and its own test only —
// confirmed by grep before deletion). The closed vocabulary khi still WRITES
// against (attrs_set's urgency key) lives independently as a literal list in
// internal/api/workflow_card.go's promotedAttrVocabulary — that map does NOT
// reference these consts (never did), so it is unaffected by this deletion.
//
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
