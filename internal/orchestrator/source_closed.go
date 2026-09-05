package orchestrator

import "encoding/json"

// observedAttrs is the slice of task_triage.detail this file interprets.
//
// Note the deliberate narrowness: the daemon reads exactly ONE key out of the
// otherwise-opaque blob. `observed` is khi's projection of its own
// mechanical observation, and `source_closed` is its one common-language
// member — "is the source finished" — as opposed to the channel-specific
// representations (Jira statusCategory, PR merged/declined, ...) that stay
// on the workspace side. Reading more of the blob than this would be a
// boundary violation.
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
// explicit true counts. The only production reader is
// logAttrsSetOnDoneTriage (internal/api/attrs_set_done.go) — a visibility
// log, not a gate on any transition.
func SourceClosed(detail json.RawMessage) bool {
	closed := parseObserved(detail)
	return closed != nil && *closed
}
