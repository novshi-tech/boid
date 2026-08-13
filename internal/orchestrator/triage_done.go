package orchestrator

import "encoding/json"

// ---- 決定15 / 決定16 (docs/plans/cross-project-issue-triage.md Phase 1 PR-5b) ----
//
// These are pure predicates in the same shape as ShouldWake (queue.go) and
// WatchdogGuidance (watchdog.go): decisions made from stored fields alone,
// with no agent judgment anywhere (決定12). They live here rather than in
// internal/api so the rule itself is unit-testable without a service, store,
// or transaction.

// observedAttrs is the slice of task_triage.detail this file interprets.
//
// Note the deliberate narrowness: the daemon reads exactly ONE key out of the
// otherwise-opaque blob. `observed` is khi's 機械観測の射影 (逆輸入3), and
// `source_closed` is its one COMMON-LANGUAGE member — "is the source finished"
// — as opposed to the channel-specific representations (Jira statusCategory,
// PR merged/declined, ...) that stay on the workspace side. Reading more of
// the blob than this would be the boundary violation 逆輸入3 forbids.
type observedAttrs struct {
	Attrs struct {
		Observed struct {
			SourceClosed *bool `json:"source_closed"`
		} `json:"observed"`
	} `json:"attrs"`
}

func parseObserved(detail json.RawMessage) *bool {
	if len(detail) == 0 || string(detail) == "null" {
		return nil
	}
	var o observedAttrs
	if err := json.Unmarshal(detail, &o); err != nil {
		// A malformed blob is treated as "nothing observed" rather than an
		// error: this predicate runs in a periodic sweep over every working
		// task, and one unparseable row must not be able to decide anything
		// about a task (least of all "done").
		return nil
	}
	return o.Attrs.Observed.SourceClosed
}

// SourceClosed reports whether khi has affirmatively observed the card's
// source as finished (決定15's second conjunct). Absent and false are both
// "not closed" — only an explicit true counts.
func SourceClosed(detail json.RawMessage) bool {
	closed := parseObserved(detail)
	return closed != nil && *closed
}

// HasCanonicalSource reports whether the card carries a source that can report
// closure at all (決定16). The test is the PRESENCE of observed.source_closed,
// not its value and not the source's type: which channels have a terminal
// state is channel knowledge, and keeping that on the workspace side is the
// whole point of 逆輸入3's claims/decisions split. A card that never reports
// the key is in breach of 決定16's contract — the watchdog surfaces it
// (MissingCanonicalSourceGuidance) rather than the create path rejecting it,
// so a broken ingestion degrades toward "visible but unfinishable" rather than
// toward "the card was never filed at all".
func HasCanonicalSource(detail json.RawMessage) bool {
	return parseObserved(detail) != nil
}

// ShouldAutoDone evaluates 決定15: 全子 closed ∧ observed.source_closed.
//
// A card with NO children at all still qualifies — 逆輸入2 defines working as
// covering both "dispatched な子あり" and "nose が手動対応中で子は open のみ",
// so a card whose whole resolution happened outside boid (a Jira reply, a
// manual fix) is finished the moment its source is. An `open` or `specced`
// child, by contrast, is an unstarted 残課題: those go back out through
// working→triaged/ready (論点8's exits), never straight to done.
func ShouldAutoDone(children []TaskTriageChild, detail json.RawMessage) bool {
	if !SourceClosed(detail) {
		return false
	}
	for i := range children {
		if children[i].Status != TaskTriageChildStatusClosed {
			return false
		}
	}
	return true
}

// MissingCanonicalSourceGuidance is 決定16's enforcement half, in the same
// pure-function shape as WatchdogGuidance (queue 節 rule 7): it returns a
// guidance string when a card that has reached triaged-or-beyond carries no
// canonical source, and "" otherwise.
//
// Enforcement is a watchdog rather than a create-time reject on purpose: a
// reject would make an ingestion bug swallow the card entirely (the direction
// that loses work), whereas a watchdog leaves the card visible and only flags
// that it can never satisfy 決定15's done rule.
//
// Statuses BEFORE triaged are exempt: captured is 起票前 (head-capture that
// has not been shaped yet), and terminal statuses have nothing left to
// finish.
func MissingCanonicalSourceGuidance(status TaskStatus, kind string, detail json.RawMessage) string {
	switch status {
	case TaskStatusTriaged, TaskStatusParked, TaskStatusReady, TaskStatusWorking:
	default:
		return ""
	}
	// kind=theme cards are 常駐 and never ride the captured→…→done lifecycle
	// (状態機械節), so they are not expected to carry a closable source.
	if kind == "theme" {
		return ""
	}
	if HasCanonicalSource(detail) {
		return ""
	}
	return "canonical source がありません (observed.source_closed が未報告) — この triage task は決定15 の done 自動落ちに到達できません"
}
