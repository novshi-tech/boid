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

// currentOccupantResult reads the card's currently-active (launching or
// attached) card_requests row and renders it as an Occupied result.
func currentOccupantResult(store CardCommandLauncherStore, cardID string) (*RunCardCommandResult, error) {
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
	if s.Tasks == nil || s.CardRequests == nil || s.Exec == nil {
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

	// Optimization only: the actual safety net is CreateCardRequest's own
	// unique index below, which still catches a concurrent claim landing
	// in between.
	active, err := s.CardRequests.CountActiveCardRequests(cardID)
	if err != nil {
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	if active > 0 {
		return currentOccupantResult(s.CardRequests, cardID)
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
	if err := s.CardRequests.CreateCardRequest(req); err != nil {
		if errors.Is(err, orchestrator.ErrCardRequestSlotOccupied) {
			return currentOccupantResult(s.CardRequests, cardID)
		}
		return nil, &StatusError{Code: http.StatusInternalServerError, Message: err.Error()}
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
