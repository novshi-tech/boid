package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// agentStartResponse is BoidOpAgentStart's reply shape — a launcher script
// parses this off stdout to learn where its session landed. Kind is always
// "session": this op only ever starts a session (the task continuation goes
// through the existing `boid task create`, not this op).
type agentStartResponse struct {
	Kind  string `json:"kind"`
	JobID string `json:"job_id"`
}

func agentStartSuccess(jobID string) *sandbox.ExecResponse {
	return marshalTaskContextResponse(agentStartResponse{Kind: "session", JobID: jobID})
}

// executeAgentStart backs BoidOpAgentStart: the launcher-side half of
// creating a session continuation for a card command. It reads the
// launcher's own card_requests row (never a caller-supplied id — see
// TokenContext.CardRequestID's doc comment), refuses an internal-event
// origin, and — for the ordinary human-command case — dispatches the
// session through the SAME path the Web UI / `boid agent <harness>` use,
// then records the continuation before replying.
func (e *boidBuiltinExecutor) executeAgentStart(goCtx context.Context, ctx sandbox.TokenContext, req *sandbox.BoidRequest) *sandbox.ExecResponse {
	// ctx.CardRequestID is already broker-verified non-empty (broker.go's
	// BoidOpAgentStart case) — the check below is defense in depth for a
	// handwritten request that bypassed the broker.
	if ctx.CardRequestID == "" {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: no card context for this job"}
	}
	if e.cardRequests == nil || e.sessionStarter == nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start unavailable"}
	}

	row, err := e.cardRequests.GetCardRequest(ctx.CardRequestID)
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}
	// Same fail-closed cross-check as BoidOpCardContext: a token stamped
	// inconsistently must not be papered over.
	if ctx.CardID == "" || row.CardID != ctx.CardID {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: token card_id does not match the request's card_id"}
	}
	// A request caused by an internal event must never reach a session: an
	// unattended session never terminates on its own, so it would hold the
	// card's single execution slot forever. Only a human-issued command may
	// take this path.
	if row.CauseID != "" {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: rejected — this request originated from an internal event, and only a human-issued command may start a session"}
	}

	switch row.Status {
	case orchestrator.CardRequestStatusAttached:
		// Idempotent retry: the launcher (or a script bug) called this
		// twice for the same request. Return the SAME continuation rather
		// than starting a second session.
		if row.TargetKind == orchestrator.CardRequestTargetKindSession {
			return agentStartSuccess(row.TargetID)
		}
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: this request is already attached to a %s, not a session", row.TargetKind)}
	case orchestrator.CardRequestStatusLaunching:
		// expected state — fall through to dispatch below.
	default:
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: card request is %q, not launchable", row.Status)}
	}

	if msg := api.ValidateHarnessType(req.HarnessType); msg != "" {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: " + msg}
	}

	result, err := e.sessionStarter.StartSession(goCtx, api.StartSessionRequest{
		ProjectID:   req.ProjectID,
		HarnessType: req.HarnessType,
		Instruction: req.Instruction,
		Readonly:    req.Readonly,
		Model:       req.Model,
		DisplayName: req.DisplayName,
	})
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: %s", err)}
	}

	if attachErr := e.cardRequests.AttachCardRequest(ctx.CardRequestID, orchestrator.CardRequestTargetKindSession, result.JobID); attachErr != nil {
		if errors.Is(attachErr, orchestrator.ErrCardRequestInvalidTransition) {
			// A concurrent call already attached this request first — the
			// session we just started is an orphan. Report the WINNING
			// continuation rather than a target the store never recorded.
			if existing, gerr := e.cardRequests.GetCardRequest(ctx.CardRequestID); gerr == nil && existing.TargetKind == orchestrator.CardRequestTargetKindSession {
				return agentStartSuccess(existing.TargetID)
			}
		}
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: session %s started but recording its association failed: %s", result.JobID, attachErr)}
	}
	return agentStartSuccess(result.JobID)
}
