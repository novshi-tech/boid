package server

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// cardContextResponse is BoidOpCardContext's reply shape — `boid card
// context`'s structured, token-authoritative input. Every field is sourced
// from the live card_requests row looked up by ctx.CardRequestID (CardWrite)
// or from the token itself (Actor), never from the request the caller sent.
type cardContextResponse struct {
	CardID      string `json:"card_id"`
	RequestID   string `json:"request_id"`
	CommandKey  string `json:"command_key"`
	Instruction string `json:"instruction"`
	// Origin is an explicit, machine-readable value derived server-side
	// from the request row's CauseID (empty = human, non-empty = an
	// internal event caused it) — never left for a caller to infer from
	// cause_id itself.
	Origin string `json:"origin"`
	// CardWrite: whether this request's continuation may write to the card,
	// independent of any task behavior's own readonly. Sourced from
	// row.Launched.CardWrite, so a caller cannot gain it by any env var or
	// CLI flag (same contract as Origin).
	CardWrite bool `json:"card_write"`
	// Actor is the kind of continuation asking — "task" or "session"
	// (orchestrator.CardRequestTargetKind{Task,Session}) — derived from
	// whether the calling token carries a TaskID, never a caller value.
	Actor string `json:"actor"`
}

const (
	cardContextOriginHuman = "human"
	cardContextOriginEvent = "event"
)

// cardRequestOrigin derives a card_requests row's origin the ONE way both
// BoidOpCardContext and BoidOpAgentStart must agree on: empty CauseID means
// a human issued the command, a non-empty CauseID means an internal event
// caused it. A single shared helper so a future origin vocabulary change
// cannot update one call site and silently leave the other behind.
func cardRequestOrigin(row *orchestrator.CardRequest) string {
	if row.CauseID != "" {
		return cardContextOriginEvent
	}
	return cardContextOriginHuman
}

// cardContextActor derives cardContextResponse.Actor from the calling
// token's own TaskID, never from anything the caller sends.
func cardContextActor(ctx sandbox.TokenContext) string {
	if ctx.TaskID != "" {
		return orchestrator.CardRequestTargetKindTask
	}
	return orchestrator.CardRequestTargetKindSession
}
