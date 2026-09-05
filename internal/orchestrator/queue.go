package orchestrator

import "time"

// ShouldWake evaluates the wake rule for a parked task: wake if wake_at has
// passed, OR the task referenced by wake_task_id has reached a terminal
// status. Both are decided purely from tt's fields plus facts the caller
// supplies about "now" and the referenced task — no agent judgment is involved.
//
// wakeTaskFound=false (the referenced task no longer exists — deleted or
// GC'd) is treated the same as "terminal": a vanished reference must not
// permanently strand a parked task (fail-open — daemon-side state
// loss/drift resolves toward re-surfacing, never toward silently hiding
// something forever).
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
