package templates

// Task list row activity state: a signal separate from card status and the
// suggestion's transition edge, telling the reader whether a card's sole
// work child is ready/running/blocked and whether a card command is
// currently in flight — without inferring a semantic use ("Reviewing"/
// "Discussing") the daemon has no way to know.

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// CardActivityState is one card row's two independent activity axes, both
// possibly empty: WorkLabel from the card's sole open/specced/dispatched
// child, CommandLabel from its single active card command. A card can show
// either, neither, or both at once (a card command and a specced-but-not-yet
// -Go'd child can coexist).
type CardActivityState struct {
	WorkLabel    string
	CommandLabel string
}

// ActiveChildFromDetail returns the first non-closed entry in a task_triage
// detail blob's children list — the sole open/specced/dispatched work child
// the card model allows at a time. Returns nil for no children, an
// all-closed history, or malformed detail.
func ActiveChildFromDetail(detail json.RawMessage) *orchestrator.TaskTriageChild {
	children, err := orchestrator.DetailChildren(detail)
	if err != nil {
		return nil
	}
	for i := range children {
		if children[i].Status != orchestrator.TaskTriageChildStatusClosed {
			return &children[i]
		}
	}
	return nil
}

// WorkActivityLabel renders the work-child vocabulary: "Draft" (open, spec
// incomplete), "Ready to run" (specced, not yet dispatched), or —once
// dispatched— "Queued"/"Running"/"Needs input" keyed off the child's OWN
// task row's real status via statuses (never inferred from the JSON
// "dispatched" status alone). Returns "" for no active child, or a
// dispatched child whose real task is missing or already terminal.
func WorkActivityLabel(child *orchestrator.TaskTriageChild, statuses map[string]orchestrator.TaskStatus) string {
	if child == nil {
		return ""
	}
	switch child.Status {
	case orchestrator.TaskTriageChildStatusOpen:
		return "Draft"
	case orchestrator.TaskTriageChildStatusSpecced:
		return "Ready to run"
	case orchestrator.TaskTriageChildStatusDispatched:
		switch statuses[child.TaskRef] {
		case orchestrator.TaskStatusPending:
			return "Queued"
		case orchestrator.TaskStatusExecuting:
			return "Running"
		case orchestrator.TaskStatusAwaiting:
			return "Needs input"
		default:
			return ""
		}
	default:
		return ""
	}
}

// CommandActivityLabel renders a card's single active card_requests row
// (orchestrator.PickActiveCardRequest) as "<label>: Queued" / "<label>:
// Launching" / "<label>: Running". A queued row has no launched_label
// snapshot yet, so it falls back to the raw command_key. Returns "" for a
// nil request or a terminal/unknown status.
func CommandActivityLabel(req *orchestrator.CardRequest) string {
	if req == nil {
		return ""
	}
	name := req.Launched.Label
	if name == "" {
		name = req.CommandKey
	}
	switch req.Status {
	case orchestrator.CardRequestStatusQueued:
		return name + ": Queued"
	case orchestrator.CardRequestStatusLaunching:
		return name + ": Launching"
	case orchestrator.CardRequestStatusAttached:
		return name + ": Running"
	default:
		return ""
	}
}

// BuildCardActivityStates fans out over three already-batched inputs (no DB
// access here) into one CardActivityState per card. A card with neither
// axis active is omitted from the result.
func BuildCardActivityStates(cardIDs []string, activeChildren map[string]*orchestrator.TaskTriageChild, childStatuses map[string]orchestrator.TaskStatus, activeRequests map[string]*orchestrator.CardRequest) map[string]CardActivityState {
	out := make(map[string]CardActivityState, len(cardIDs))
	for _, id := range cardIDs {
		state := CardActivityState{
			WorkLabel:    WorkActivityLabel(activeChildren[id], childStatuses),
			CommandLabel: CommandActivityLabel(activeRequests[id]),
		}
		if state.WorkLabel == "" && state.CommandLabel == "" {
			continue
		}
		out[id] = state
	}
	return out
}
