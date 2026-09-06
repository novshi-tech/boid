package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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
	// A stale launcher from a superseded attempt (e.g. one RetryCardRequest
	// re-queued and re-claimed under a fresh launcher job after this one
	// timed out but kept running) must not attach to the CURRENT attempt
	// just because it still holds a token naming the same request id.
	// LauncherJobID is now stamped atomically with the queued->launching
	// promotion (ClaimQueuedCardRequests / the launching fast path in
	// CreateCardRequest), so an empty value here can only mean a row that
	// bypassed that invariant — reject rather than treat it as unclaimed.
	if row.LauncherJobID == "" || row.LauncherJobID != ctx.JobID {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: this job is no longer the request's current launcher"}
	}
	// A request caused by an internal event must never reach a session: an
	// unattended session never terminates on its own, so it would hold the
	// card's single execution slot forever. Only a human-issued command may
	// take this path.
	if cardRequestOrigin(row) == cardContextOriginEvent {
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
		ProjectID:     req.ProjectID,
		HarnessType:   req.HarnessType,
		Instruction:   req.Instruction,
		Readonly:      req.Readonly,
		Model:         req.Model,
		DisplayName:   req.DisplayName,
		CardID:        ctx.CardID,
		CardRequestID: ctx.CardRequestID,
	})
	if err != nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: %s", err)}
	}
	if result == nil {
		return &sandbox.ExecResponse{ExitCode: 1, Stderr: "boid agent start: session dispatch returned no result"}
	}

	if attachErr := e.cardRequests.AttachCardRequest(ctx.CardRequestID, orchestrator.CardRequestTargetKindSession, result.JobID); attachErr != nil {
		return e.handleAgentStartAttachFailure(ctx.CardRequestID, result.JobID, attachErr)
	}
	return agentStartSuccess(result.JobID)
}

// handleAgentStartAttachFailure runs when the session this call just
// dispatched (orphanJobID, now referenced by no card_requests row) lost the
// race to record itself. It reports the WINNING continuation when one
// exists, and otherwise a message naming both the orphan and the actual
// cause rather than the generic store error, and always logs the orphan
// since nothing else references it.
//
// KNOWN GAP: this only logs — it never stops orphanJobID's container. A
// session that loses the attach race keeps running unreferenced by any
// card_requests row until whatever normally ends that session (a person
// attaching and quitting, or the harness itself exiting) does. Cleanup would
// mean calling the equivalent of `boid agent stop` here, which this op does
// not yet do — tracked as follow-up work, not silently unspecified.
func (e *boidBuiltinExecutor) handleAgentStartAttachFailure(requestID, orphanJobID string, attachErr error) *sandbox.ExecResponse {
	if errors.Is(attachErr, orchestrator.ErrCardRequestInvalidTransition) {
		existing, gerr := e.cardRequests.GetCardRequest(requestID)
		switch {
		case gerr != nil:
			slog.Warn("boid agent start: session orphaned; re-fetching the request that beat it also failed", "job_id", orphanJobID, "request_id", requestID, "error", gerr)
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: session %s started but lost the attach race, and the winning request could not be re-read: %s", orphanJobID, gerr)}
		case existing.TargetKind == orchestrator.CardRequestTargetKindSession:
			slog.Warn("boid agent start: session orphaned by a concurrent attach", "job_id", orphanJobID, "request_id", requestID, "winning_job_id", existing.TargetID)
			return agentStartSuccess(existing.TargetID)
		default:
			slog.Warn("boid agent start: session orphaned; the request was already attached to a different kind of continuation", "job_id", orphanJobID, "request_id", requestID, "target_kind", existing.TargetKind)
			return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: session %s started but the request is already attached to a %s, not a session", orphanJobID, existing.TargetKind)}
		}
	}
	slog.Warn("boid agent start: session orphaned; recording its association failed", "job_id", orphanJobID, "request_id", requestID, "error", attachErr)
	return &sandbox.ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid agent start: session %s started but recording its association failed: %s", orphanJobID, attachErr)}
}
