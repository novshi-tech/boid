package orchestrator

import "encoding/json"

// ---- docs/plans/suggestion-as-state-transition-impl.md §4: 決定15/16 廃止後
// の生き残り ----
//
// This file used to be triage_done.go, home to 決定15 (auto-done:
// ShouldAutoDone) and 決定16 (canonical source contract: HasCanonicalSource /
// MissingCanonicalSourceGuidance). PR-1 deletes both — auto-done is replaced
// by khi suggesting "done" and a human accepting it (card machine v2's
// `done: working → done` rule), and the canonical-source contract's sole
// reason to exist (feeding auto-done's second conjunct) went with it (design
// doc §3.5). See internal/api/triage_done.go's deletion in this same PR for
// the sweep-side half.
//
// SourceClosed survives: I-5b/I-5c (internal/api/attrs_set_done.go) still
// logs khi's source_closed observation landing on a done card — a read-only
// visibility aid, not a decision — and that log line reads this exact
// predicate. Nothing else in this file survives it.

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
		// error: this predicate is read by a log line on every attrs_set
		// landing on a done card, and one unparseable row must not error out
		// that path.
		return nil
	}
	return o.Attrs.Observed.SourceClosed
}

// SourceClosed reports whether khi has affirmatively observed the card's
// source as finished. Absent and false are both "not closed" — only an
// explicit true counts. The only production reader left after 決定15's
// removal is logAttrsSetOnDoneTriage (internal/api/attrs_set_done.go, I-5c) —
// a visibility log, not a gate on any transition.
func SourceClosed(detail json.RawMessage) bool {
	closed := parseObserved(detail)
	return closed != nil && *closed
}
