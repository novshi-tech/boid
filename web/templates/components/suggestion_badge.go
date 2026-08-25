package components

import (
	"fmt"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// knownSuggestionVerbs is the single source of truth for suggestion.verb's
// UI vocabulary — card machine v2's own six transition verbs
// (orchestrator.IsCardTransitionAction; docs/plans/
// suggestion-as-state-transition.md §3.1: suggestion.verb is boid's own
// state-machine vocabulary now, not a free-form workspace word like the old
// go/shape/manual/park/drop/wake set). Deliberately NOT
// orchestrator.promotedAttrVocabulary: that guard exists so the daemon can
// trust its own SQL predicates against attrs_set writes (see its doc
// comment) — verb is neither a stored column nor a query target, and the
// daemon does not interpret/judge suggestions at all beyond validating the
// verb itself (attrs_set's own validateSuggestionAttr, internal/api). An
// unknown verb is therefore a UI-only rendering concern, handled by
// VerbBadgeClass below rather than by validating/rejecting the value
// anywhere upstream.
var knownSuggestionVerbs = map[string]bool{
	"go":      true,
	"working": true,
	"park":    true,
	"drop":    true,
	"done":    true,
	"reopen":  true,
}

// VerbBadgeClass maps a suggestion.verb to its badge CSS class (style.css).
// Kept in exactly one place and called from every rendering site — the card
// detail page's suggestion section (web/templates/tasks.templ's
// TaskDetailSuggestionSection) and the list row's movement line
// (web/templates/task_list_row.templ, docs/plans/
// webui-detail-list-redesign.md PR-4) — so a future 7th verb, or a change to
// the unknown-verb fallback, only needs updating here. An unknown verb still
// gets a class (badge-verb-unknown, style.css) rather than no badge at all:
// the verb text itself always renders regardless (rule 5, 隠さない) — only
// the color is what falls back to neutral gray.
//
// Prior to PR-4 this file was task_tree.templ, alongside the list-page tree
// row (taskTreeRow) that PR-4 deleted (タブ/ツリー撤廃). This function and
// its two neighbors below survive the deletion — plain Go, no templ syntax
// — because the card detail page (a caller PR-4 does not touch) still needs
// them, and the new flat list row (PR-4) needs them too.
func VerbBadgeClass(verb string) string {
	if !knownSuggestionVerbs[verb] {
		return "badge-verb-unknown"
	}
	return "badge-verb-" + verb
}

// SuggestionInapplicable reports whether a suggestion's own verb cannot fire
// a card transition from status right now — orchestrator.StateMachine.
// CanApplyTransitionAction (PR-3, suggestion 状態遷移化 follow-up), which
// reads card machine v2's own rule table (each of the six verbs admits
// exactly ONE FromStatus — e.g. "done" only fires from "working",
// machine_card.go's own doc comment). NewCardMachine (not machineFor's
// dynamic task-based selection) is correct here for the same reason
// TaskDetailSuggestionSection already establishes: a Suggestion only ever
// exists on a task_triage sidecar row (a card) in the first place.
func SuggestionInapplicable(verb string, status orchestrator.TaskStatus) bool {
	return verb != "" && !orchestrator.NewCardMachine().CanApplyTransitionAction(verb, status)
}

// SuggestionInapplicableReason is the reader-facing "why is Accept hidden /
// why is this verb badge marked inapplicable" text, naming both the verb and
// the current status plainly (rule 5, 隠さない — never a bare "cannot apply"
// with no context to act on). Both the Web UI and the API pull the "what CAN
// be applied" clause from the SAME single source —
// orchestrator.StateMachine.AvailableActionsHint — so they can never say two
// different things about the same status.
func SuggestionInapplicableReason(verb string, status orchestrator.TaskStatus) string {
	return fmt.Sprintf(
		"This suggestion (verb=%s) cannot be applied from the current status (status=%s); %s.",
		verb, status, orchestrator.NewCardMachine().AvailableActionsHint(status),
	)
}
