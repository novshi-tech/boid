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
// A binding change onto a card is recorded as an action in the same
// transaction. The binding is read first because the store's return value
// cannot distinguish a write from an idempotent repeat, which records nothing.
func (s *TaskAppService) LinkIdentity(ctx context.Context, projectID, identity, taskID string) error {
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	if s.Tx == nil {
		return linkIdentityIn(ctx, s.identityWriter(), projectID, identity, taskID)
	}
	return s.Tx.WithinTx(func(tx TxStore) error {
		return linkIdentityIn(ctx, tx, projectID, identity, taskID)
	})
}

// UnlinkIdentity removes one (projectID, identity) binding, if any.
func (s *TaskAppService) UnlinkIdentity(ctx context.Context, projectID, identity string) error {
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	if s.Tx == nil {
		return unlinkIdentityIn(ctx, s.identityWriter(), projectID, identity)
	}
	return s.Tx.WithinTx(func(tx TxStore) error {
		return unlinkIdentityIn(ctx, tx, projectID, identity)
	})
}

// identityBindingWriter is the surface the two binding changes need —
// satisfied by TxStore and, without a transactor, by the service's own
// separate stores.
type identityBindingWriter interface {
	ResolveIdentity(projectID, identity string) (*orchestrator.Task, error)
	LinkIdentity(projectID, identity, taskID string) error
	UnlinkIdentity(projectID, identity string) error
	GetTask(id string) (*orchestrator.Task, error)
	CreateAction(ctx context.Context, a *orchestrator.Action) error
}

// looseIdentityWriter is identityBindingWriter assembled from the service's
// independent stores, so a missing transactor costs atomicity but never the
// record.
type looseIdentityWriter struct {
	TaskIdentityStore
	tasks   TaskStore
	actions ActionStore
}

func (w looseIdentityWriter) GetTask(id string) (*orchestrator.Task, error) {
	if w.tasks == nil {
		return nil, orchestrator.ErrTaskNotFound
	}
	return w.tasks.GetTask(id)
}

func (w looseIdentityWriter) CreateAction(ctx context.Context, a *orchestrator.Action) error {
	if w.actions == nil {
		return nil
	}
	return w.actions.CreateAction(ctx, a)
}

func (s *TaskAppService) identityWriter() identityBindingWriter {
	return looseIdentityWriter{TaskIdentityStore: s.Identities, tasks: s.Tasks, actions: s.Actions}
}

func linkIdentityIn(ctx context.Context, w identityBindingWriter, projectID, identity, taskID string) error {
	existing, rerr := w.ResolveIdentity(projectID, identity)
	if rerr != nil && !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
		return rerr
	}
	if rerr == nil && existing != nil && existing.ID == taskID {
		return nil
	}
	if err := w.LinkIdentity(projectID, identity, taskID); err != nil {
		return err
	}
	return recordIdentityBinding(ctx, w, taskID, identity, orchestrator.ActionTypeIdentityLinked)
}

func unlinkIdentityIn(ctx context.Context, w identityBindingWriter, projectID, identity string) error {
	bound, rerr := w.ResolveIdentity(projectID, identity)
	if rerr != nil && !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
		return rerr
	}
	if err := w.UnlinkIdentity(projectID, identity); err != nil {
		return err
	}
	if rerr != nil || bound == nil {
		return nil
	}
	return recordIdentityBinding(ctx, w, bound.ID, identity, orchestrator.ActionTypeIdentityUnlinked)
}

// recordIdentityBinding writes actionType against taskID when it is a card.
func recordIdentityBinding(ctx context.Context, w identityBindingWriter, taskID, identity, actionType string) error {
	target, gerr := w.GetTask(taskID)
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
	return w.CreateAction(ctx, &orchestrator.Action{
		TaskID:  taskID,
		Type:    actionType,
		Payload: payload,
		Actor:   updateActor(ctx),
	})
}

// ResolveIdentity looks up the task bound to (projectID, identity). Returns
// orchestrator.ErrTaskNotFound when no binding exists.
func (s *TaskAppService) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	if s.Identities == nil {
		return nil, errIdentityStoreUnavailable
	}
	return s.Identities.ResolveIdentity(projectID, identity)
}
