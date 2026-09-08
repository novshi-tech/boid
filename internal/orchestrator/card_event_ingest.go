package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/novshi-tech/boid/internal/db"
)

// CardEventResolver answers, for a project, the card_commands key its
// card_events.command declares — "" (ok=false) when it declares none.
type CardEventResolver interface {
	CardEventCommand(projectID string) (commandKey string, ok bool)
}

// Compile-time proof that *ProjectStore satisfies this interface.
var _ CardEventResolver = (*ProjectStore)(nil)

// cardEventIngestActionTypes is the closed allowlist of action types that
// may auto-start a card_events command. Every action type not listed here
// is a no-op, including future additions to the card machine's vocabulary —
// this stays a deliberate allowlist, not a derived exclusion.
var cardEventIngestActionTypes = map[string]bool{
	"child_closed": true,
	"wake_due":     true,
	"answered":     true,
	"noted":        true,
	"attrs_set":    true,
	// The daemon's own state-change records: a new card, an edited one and a
	// changed identity binding are each a change the next decision must see.
	ActionTypeCardCreated:      true,
	ActionTypeCardEdited:       true,
	ActionTypeIdentityLinked:   true,
	ActionTypeIdentityUnlinked: true,
}

// IngestCardEventRequest is CreateAction's card-event ingest step: it
// queues a card_requests row for an eligible action.
//
// Unlike IngestActionSignal, a genuine failure here fails the caller's
// transaction — only ineligibility and ErrCardRequestDuplicateCause
// (redelivery) are no-ops; every other error propagates.
func IngestCardEventRequest(ctx context.Context, dbtx db.DBTX, a *Action, resolver CardEventResolver) error {
	if resolver == nil || a == nil || a.TaskID == "" {
		return nil
	}
	if !cardEventIngestActionTypes[a.Type] {
		return nil
	}

	taskType, projectID, status, err := cardEventTaskContext(dbtx, a.TaskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("card event ingest: resolve target task: %w", err)
	}
	if taskType != TaskTypeCard {
		return nil
	}
	if status != TaskStatusParked && status != TaskStatusWorking {
		return nil
	}

	commandKey, ok := resolver.CardEventCommand(projectID)
	if !ok || commandKey == "" {
		return nil
	}

	if selfLoop, err := writerHoldsCardsLiveRequest(dbtx, ctx, a.TaskID); err != nil {
		return fmt.Errorf("card event ingest: resolve writer's card request: %w", err)
	} else if selfLoop {
		return nil
	}

	err = CreateCardRequest(dbtx, &CardRequest{
		CardID:     a.TaskID,
		CommandKey: commandKey,
		CauseID:    a.ID,
	})
	if err != nil {
		if errors.Is(err, ErrCardRequestDuplicateCause) {
			return nil
		}
		return fmt.Errorf("card event ingest: create card request: %w", err)
	}
	return nil
}

// writerHoldsCardsLiveRequest reports whether ctx's writer card_requests id
// is cardID's OWN currently launching/attached request — the self-loop this
// ingest step must exclude.
func writerHoldsCardsLiveRequest(dbtx db.DBTX, ctx context.Context, cardID string) (bool, error) {
	writerRequestID, hasWriter := WriterCardRequestIDFromContext(ctx)
	if !hasWriter || writerRequestID == "" {
		return false, nil
	}
	row, err := GetCardRequest(dbtx, writerRequestID)
	if err != nil {
		if errors.Is(err, ErrCardRequestNotFound) {
			return false, nil
		}
		return false, err
	}
	live := row.Status == CardRequestStatusLaunching || row.Status == CardRequestStatusAttached
	return row.CardID == cardID && live, nil
}

// cardEventTaskContext is a minimal, single-row lookup (type + project id +
// status only), avoiding GetTask's extra child-count rollup cost.
func cardEventTaskContext(dbtx db.DBTX, taskID string) (taskType TaskType, projectID string, status TaskStatus, err error) {
	var t, s string
	err = dbtx.QueryRow(`SELECT type, project_id, status FROM tasks WHERE id = ?`, taskID).Scan(&t, &projectID, &s)
	return TaskType(t), projectID, TaskStatus(s), err
}
