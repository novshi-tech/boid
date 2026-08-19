package orchestrator

import "encoding/json"

// ---- docs/plans/ingestion-identity.md PR-5 (B-6): auto-reopen と done への着地 ----
//
// This is 決定15's mirror (I-5): where ShouldAutoDone closes a working triage
// task the moment its source reports closed, ShouldAutoReopen reopens a DONE
// one the moment the SAME source reports open again. Same shape as
// ShouldAutoDone/ShouldWake — a pure predicate over stored fields, no agent
// judgment (決定12) — and it lives here, next to triage_done.go's
// ShouldAutoDone/SourceClosed, because it reads the identical single key
// those already read.

// ShouldAutoReopen evaluates I-5: "observed.source_closed が true → false に
// 戻ったら reopen_triaged". The daemon does NOT read a second key to detect
// the flip — a done triage task can only have REACHED done via triage_done
// (machine.go), which itself requires SourceClosed(detail)==true at that
// moment (ShouldAutoDone's own first conjunct), so simply re-reading the
// SAME key now and finding it false again already IS the true→false
// transition. This is exactly what I-5's own text promises: "読むキーは
// 増やさない".
//
// priorAutoReopens is 12節 B-6 の既定案 (フラップ対策): once the daemon has
// already auto-reopened this exact task once, a further flip does not
// auto-reopen again — see CountAutoReopens's doc comment for how that count
// is derived (決定13: no dedicated counter column). A priorAutoReopens of 0
// is "never auto-reopened before" and is the only value that admits a
// reopen; everything else falls to the caller's フラップ通知 path instead.
func ShouldAutoReopen(detail json.RawMessage, priorAutoReopens int) bool {
	if SourceClosed(detail) {
		return false
	}
	return priorAutoReopens == 0
}

// CountAutoReopens derives 「この task を daemon が何回自動 reopen したか」
// from the task's own action history (決定13: state は導出、専用カウンタ列を
// 作らない) — no persisted counter, just a filter over already-recorded
// actions.
//
// Only Type=="reopen_triaged" actions stamped Actor==ActorDaemon count. A
// human's own manual reopen of a done triage task (the Web UI button;
// resolveReopenVariant routes it through the identical reopen_triaged verb,
// workflow_action.go) is a deliberate human judgment, not automation
// flapping, so it must not consume the automatic budget — a human who
// reopens a card by hand can do so as many times as they like.
func CountAutoReopens(actions []*Action) int {
	n := 0
	for _, a := range actions {
		if a != nil && a.Type == "reopen_triaged" && a.Actor == ActorDaemon {
			n++
		}
	}
	return n
}
