package api

// Manual card-command launch: claims a card's single execution slot and
// dispatches its project.yaml `run:` command as a readonly exec job
// carrying card/request context, mirroring fireTrigger's shape
// (trigger_loop.go) with the single-flight unit swapped from
// (project, trigger) to the card's own card_requests row.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// CardCommandLauncherStore is the card_requests persistence surface a
// manual card command launch needs. Satisfied by *orchestrator.TaskRepository.
type CardCommandLauncherStore interface {
	CountActiveCardRequests(cardID string) (int, error)
	ListCardRequestsByCard(cardID string) ([]*orchestrator.CardRequest, error)
	CreateCardRequest(req *orchestrator.CardRequest) error
	FailCardRequest(id, errText string) error
}

// cardCommandInstructionMaxBytes matches sandbox.PayloadPatchMaxBytes, the
// cap `boid agent start --instruction` already enforces broker-side.
const cardCommandInstructionMaxBytes = sandbox.PayloadPatchMaxBytes

// RunCardCommandResult is RunCardCommand's response shape.
type RunCardCommandResult struct {
	// Occupied is true when the card's execution slot was already taken —
	// no new request was created; TargetKind/TargetID point at the current
	// occupant instead.
	Occupied bool `json:"occupied"`
	// RequestID is the card_requests row this call created (Occupied=false)
	// or the currently-occupying row (Occupied=true).
	RequestID  string `json:"request_id,omitempty"`
	JobID      string `json:"job_id,omitempty"`
	TargetKind string `json:"target_kind,omitempty"`
	TargetID   string `json:"target_id,omitempty"`
}

// cardRequestLister is the read half CardCommandLauncherStore and TxStore
// both satisfy — currentOccupantResult runs either non-transactionally
// (store) or inside the reservation transaction (tx), so it is written
// against this narrower interface rather than either concrete one.
type cardRequestLister interface {
	ListCardRequestsByCard(cardID string) ([]*orchestrator.CardRequest, error)
}

// currentOccupantResult reads the card's currently-active (launching or
// attached) card_requests row and renders it as an Occupied result.
func currentOccupantResult(store cardRequestLister, cardID string) (*RunCardCommandResult, error) {
	rows, err := store.ListCardRequestsByCard(cardID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	for _, r := range rows {
		if r.Status == orchestrator.CardRequestStatusLaunching || r.Status == orchestrator.CardRequestStatusAttached {
			return &RunCardCommandResult{
				Occupied:   true,
				RequestID:  r.ID,
				TargetKind: r.TargetKind,
				TargetID:   r.TargetID,
			}, nil
		}
	}
	return nil, &StatusError{Code: http.StatusConflict, Message: "card command: slot reported occupied but no active request found; retry"}
}

// cardWorkChildOccupantTx reports the task id of cardID's live or specced
// work child, if any — the same occupancy cardSlotOccupied checks for
// child_added, but re-read FRESH from tx (not a pre-fetched *orchestrator.Task
// snapshot) so it can run inside the same transaction as the CreateCardRequest
// INSERT that claims the slot: a stale snapshot taken before the
// transaction opened would defeat the whole point of making this atomic.
func cardWorkChildOccupantTx(tx TxStore, cardID string) (occupantID string, occupied bool, err error) {
	fresh, err := tx.GetTask(cardID)
	if err != nil {
		return "", false, err
	}
	if tt, ttErr := tx.GetTaskTriage(cardID); ttErr == nil {
		if id, derr := orchestrator.DetailOpenSlotChildID(tt.Detail); derr == nil && id != "" {
			return id, true, nil
		}
	}
	return "", fresh.OpenChildCount > 0, nil
}

// RunCardCommand fires cardID's commandKey card_commands entry as a human
// (cause_id empty) command launcher: a short-lived readonly exec job that
// runs the project.yaml `run:` command with card/request context in its
// broker token, expected to call `boid task create` or `boid agent start`
// exactly once to produce this request's continuation.
//
// When the slot is already occupied, this does not queue — it returns a
// link to the current execution instead; the caller's own UI keeps the
// typed instruction around.
func (s *TaskWorkflowService) RunCardCommand(ctx context.Context, cardID, commandKey, instruction string) (*RunCardCommandResult, error) {
	if s.Tasks == nil || s.CardRequests == nil || s.Exec == nil || s.Tx == nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: "card command: not configured"}
	}
	if len(instruction) > cardCommandInstructionMaxBytes {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: fmt.Sprintf("card command: instruction exceeds %d bytes", cardCommandInstructionMaxBytes)}
	}

	card, err := s.Tasks.GetTask(cardID)
	if err != nil {
		if errors.Is(err, orchestrator.ErrTaskNotFound) {
			return nil, &StatusError{Code: http.StatusNotFound, Message: err.Error()}
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if card.Type != orchestrator.TaskTypeCard {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "card command: target task is not a card"}
	}

	meta := s.hydrateMetaForTriggers(ctx, card.ProjectID)
	if meta == nil || len(meta.CardCommands) == 0 {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: "card command: project declares no card_commands"}
	}
	cmd, ok := meta.CardCommands[commandKey]
	if !ok {
		return nil, &StatusError{Code: http.StatusNotFound, Message: fmt.Sprintf("card command: no such command %q", commandKey)}
	}

	launcherJobID := uuid.New().String()
	def := orchestrator.CardRequestDefinition{CommandKey: commandKey, Label: cmd.Label, Run: cmd.Run}
	req := &orchestrator.CardRequest{
		CardID:        cardID,
		CommandKey:    commandKey,
		CauseID:       "", // human-issued: origin derives from an empty CauseID
		Status:        orchestrator.CardRequestStatusLaunching,
		Instruction:   instruction,
		Launched:      def,
		LauncherJobID: launcherJobID,
	}

	// The occupancy pre-check (a live/specced Go work child via
	// cardWorkChildOccupantTx, plus any already-active card_requests row)
	// and the CreateCardRequest INSERT that actually claims the slot run
	// inside ONE transaction, so a concurrent child_added (its own
	// transaction) cannot commit an open child in the gap between this
	// function's reads and its own INSERT. cardSlotOccupied (workflow_card.go)
	// closes the same gap in the other direction (child_added checking
	// CountActiveCardRequests inside its own transaction).
	var occupiedResult *RunCardCommandResult
	txErr := s.Tx.WithinTx(func(tx TxStore) error {
		occupantID, occ, operr := cardWorkChildOccupantTx(tx, cardID)
		if operr != nil {
			return &StatusError{Code: http.StatusInternalServerError, Message: operr.Error()}
		}
		if occ {
			occupiedResult = &RunCardCommandResult{Occupied: true, TargetKind: orchestrator.CardRequestTargetKindTask, TargetID: occupantID}
			return nil
		}
		if err := tx.CreateCardRequest(req); err != nil {
			if errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
				result, oerr := currentOccupantResult(tx, cardID)
				if oerr != nil {
					return oerr
				}
				occupiedResult = result
				return nil
			}
			return &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		return nil
	})
	if txErr != nil {
		var se *StatusError
		if errors.As(txErr, &se) {
			return nil, se
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: txErr.Error()}
	}
	if occupiedResult != nil {
		return occupiedResult, nil
	}

	result, err := s.Exec.StartExec(ctx, StartExecRequest{
		ProjectID:     card.ProjectID,
		JobID:         launcherJobID,
		Argv:          triggerRunArgv(cmd.Run),
		Readonly:      true,
		DisplayName:   "card:" + commandKey,
		CardID:        cardID,
		CardRequestID: req.ID,
	})
	if err != nil {
		if ferr := s.CardRequests.FailCardRequest(req.ID, fmt.Sprintf("dispatch failed: %s", err)); ferr != nil {
			slog.Warn("card command: dispatch failed and releasing the claimed slot also failed; needs an operator force-release",
				"card_id", cardID, "request_id", req.ID, "dispatch_error", err, "release_error", ferr)
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("card command: dispatch: %s", err)}
	}

	return &RunCardCommandResult{RequestID: req.ID, JobID: result.JobID}, nil
}
