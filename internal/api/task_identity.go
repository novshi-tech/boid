package api

// TaskAppService task-identity wrappers over TaskIdentityStore, backing the
// brokered task_identity_link/_unlink/_resolve ops. Errors are intentionally
// left unwrapped (no StatusError) so callers can errors.Is against
// orchestrator.ErrTaskNotFound / ErrIdentityConflict.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// errIdentityStoreUnavailable guards the three identity ops when no identity store is configured.
var errIdentityStoreUnavailable = errors.New("identity store unavailable")

// LinkIdentity binds identity to taskID within projectID's scope. See
// TaskIdentityStore.LinkIdentity for the idempotent-same-task /
// ErrIdentityConflict-different-task contract.
//
// A link onto a card is recorded as an action in the same transaction. The
// binding is read first because the store's return value cannot distinguish
// an insert from an idempotent re-link, which records nothing.
func (s *TaskAppService) LinkIdentity(ctx context.Context, projectID, identity, taskID string) error {
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	if s.Tx == nil {
		return s.Identities.LinkIdentity(projectID, identity, taskID)
	}
	return s.Tx.WithinTx(func(tx TxStore) error {
		if existing, rerr := tx.ResolveIdentity(projectID, identity); rerr == nil && existing != nil && existing.ID == taskID {
			return nil
		} else if rerr != nil && !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
			return rerr
		}
		if err := tx.LinkIdentity(projectID, identity, taskID); err != nil {
			return err
		}
		target, gerr := tx.GetTask(taskID)
		if gerr != nil {
			return gerr
		}
		if target.Type != orchestrator.TaskTypeCard {
			return nil
		}
		payload, merr := json.Marshal(orchestrator.IdentityLinkedPayload{Identity: identity})
		if merr != nil {
			return merr
		}
		return tx.CreateAction(ctx, &orchestrator.Action{
			TaskID:  taskID,
			Type:    orchestrator.ActionTypeIdentityLinked,
			Payload: payload,
			Actor:   updateActor(ctx),
		})
	})
}

// UnlinkIdentity removes one (projectID, identity) binding, if any.
func (s *TaskAppService) UnlinkIdentity(projectID, identity string) error {
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	return s.Identities.UnlinkIdentity(projectID, identity)
}

// ResolveIdentity looks up the task bound to (projectID, identity). Returns
// orchestrator.ErrTaskNotFound when no binding exists.
func (s *TaskAppService) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	if s.Identities == nil {
		return nil, errIdentityStoreUnavailable
	}
	return s.Identities.ResolveIdentity(projectID, identity)
}
