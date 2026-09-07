package api

// Automatic dispatch of card_requests rows an internal event queued
// (card_event_ingest.go, orchestrator package): the queued->launching claim,
// the launcher exec dispatch, and the periodic recovery sweep. Mirrors
// RunCardCommandAsHuman's/fireTrigger's shape, with two differences: this
// never returns "occupied" to a caller — it leaves the row queued and lets a
// later attempt (another commit, or the periodic sweep) retry; and the claim
// goes through orchestrator.ClaimQueuedCardRequestsForDispatch, which
// re-checks the card's status and any force-release barrier that
// RunCardCommandAsHuman's own occupancy pre-check has no equivalent for.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// tryDispatchQueuedCardRequest is dispatchQueuedCardRequest, best-effort: an
// error here is logged, not propagated — every caller is itself a
// best-effort commit-triggered attempt, and the periodic
// CardRequestDispatchLoop sweep is the recovery path for whatever this
// misses.
func (s *TaskWorkflowService) tryDispatchQueuedCardRequest(ctx context.Context, cardID string) {
	if _, err := s.dispatchQueuedCardRequest(ctx, cardID); err != nil {
		slog.Warn("card request: automatic dispatch attempt failed; periodic sweep will retry", "card_id", cardID, "error", err)
	}
}

// dispatchQueuedCardRequest attempts to launch cardID's oldest queued
// card_requests row, if any. dispatched is true only when a launcher was
// actually started this call — false (with err possibly nil) covers every
// "nothing to do right now" outcome: no queued row, the card's slot is
// already occupied, the card is not eligible (not parked/working, drained
// instead), a force-release barrier is active, or the queued head's
// command_key drifted since it was resolved (a rare race the caller — this
// call or the periodic sweep — will simply retry).
func (s *TaskWorkflowService) dispatchQueuedCardRequest(ctx context.Context, cardID string) (dispatched bool, err error) {
	if cardID == "" || s.CardRequests == nil || s.Exec == nil || s.Tasks == nil {
		return false, nil
	}

	headID, commandKey, err := s.CardRequests.PeekOldestQueuedCardRequest(cardID)
	if err != nil {
		if errors.Is(err, orchestrator.ErrNoQueuedCardRequests) {
			return false, nil
		}
		return false, fmt.Errorf("dispatch queued card request: peek: %w", err)
	}

	card, err := s.Tasks.GetTask(cardID)
	if err != nil {
		return false, fmt.Errorf("dispatch queued card request: get card: %w", err)
	}

	meta := s.hydrateMetaForTriggers(ctx, card.ProjectID)
	cmd, ok := orchestrator.CardCommand{}, false
	if meta != nil {
		cmd, ok = meta.CardCommands[commandKey]
	}
	if !ok {
		// The command definition vanished from project.yaml while this
		// request waited — an explicit failure, never silent abandonment.
		if ferr := s.CardRequests.FailCardRequest(headID, fmt.Sprintf("card command %q is no longer declared in project.yaml", commandKey)); ferr != nil {
			return false, fmt.Errorf("dispatch queued card request: fail undeclared command: %w", ferr)
		}
		return false, nil
	}

	launcherJobID := uuid.New().String()
	def := orchestrator.CardRequestDefinition{CommandKey: commandKey, Label: cmd.Label, Run: cmd.Run, CardWrite: cmd.CardWrite}
	primary, _, err := s.CardRequests.ClaimQueuedCardRequestsForDispatch(cardID, launcherJobID, commandKey, def)
	if err != nil {
		if orchestrator.IsCardRequestDispatchSkip(err) {
			return false, nil
		}
		return false, fmt.Errorf("dispatch queued card request: claim: %w", err)
	}

	if _, err := s.Exec.StartExec(ctx, StartExecRequest{
		ProjectID:     card.ProjectID,
		JobID:         launcherJobID,
		Argv:          triggerRunArgv(cmd.Run),
		Readonly:      true,
		DisplayName:   "card:auto:" + commandKey,
		CardID:        cardID,
		CardRequestID: primary.ID,
	}); err != nil {
		if ferr := s.CardRequests.FailCardRequest(primary.ID, fmt.Sprintf("dispatch failed: %s", err)); ferr != nil {
			slog.Warn("card request auto dispatch: dispatch failed and releasing the claimed slot also failed; needs an operator force-release",
				"card_id", cardID, "request_id", primary.ID, "dispatch_error", err, "release_error", ferr)
		}
		return false, fmt.Errorf("dispatch queued card request: start exec: %w", err)
	}
	return true, nil
}

// SweepQueuedCardRequests is CardRequestDispatchLoop's per-tick work: the
// periodic recovery fallback for a missed immediate dispatch attempt
// (notification loss, a daemon restart landing between a queued row's
// creation and the commit-triggered attempt that would have fired for it).
// Never the primary path — see this package's own doc comment at the top of
// this file. Returns the card ids a launcher was actually started for.
func (s *TaskWorkflowService) SweepQueuedCardRequests(ctx context.Context, _ time.Time) ([]string, error) {
	if s.CardRequests == nil || s.Exec == nil || s.Tasks == nil {
		return nil, nil
	}
	cardIDs, err := s.CardRequests.ListCardIDsWithQueuedCardRequests()
	if err != nil {
		return nil, fmt.Errorf("sweep queued card requests: %w", err)
	}
	var dispatched []string
	for _, cardID := range cardIDs {
		ok, derr := s.dispatchQueuedCardRequest(ctx, cardID)
		if derr != nil {
			slog.Warn("card request sweep: dispatch attempt failed", "card_id", cardID, "error", derr)
			continue
		}
		if ok {
			dispatched = append(dispatched, cardID)
		}
	}
	return dispatched, nil
}
