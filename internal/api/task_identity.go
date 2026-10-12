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
var errIdentityMetadataUnavailable = errors.New("identity metadata store unavailable")

type identityMetadataWriter interface {
	UpdateIdentityMetadata(projectID, identity, taskID string, url, displayName *string) error
}

// LinkIdentity binds identity to taskID within projectID's scope. See
// TaskIdentityStore.LinkIdentity for the idempotent-same-task /
// ErrIdentityConflict-different-task contract.
//
// A binding change onto a card is recorded in the same transaction. The
// binding is read first: the store's return value cannot distinguish a write
// from an idempotent repeat, which records nothing.
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

// LinkIdentityWithMetadata links an identity and applies explicitly supplied metadata.
func (s *TaskAppService) LinkIdentityWithMetadata(ctx context.Context, projectID, identity, taskID string, url, displayName *string) error {
	if url == nil && displayName == nil {
		return s.LinkIdentity(ctx, projectID, identity, taskID)
	}
	if url != nil {
		if err := orchestrator.ValidateIdentityURL(*url); err != nil {
			return err
		}
	}
	if s.Tx == nil {
		// A metadata-bearing link is one logical write. Refuse it when no
		// transaction is available rather than leaving a binding behind if
		// the metadata update fails.
		return errIdentityMetadataUnavailable
	}
	return s.Tx.WithinTx(func(tx TxStore) error {
		r, ok := tx.(identityMetadataWriter)
		if !ok {
			return errIdentityMetadataUnavailable
		}
		if err := linkIdentityIn(ctx, tx, projectID, identity, taskID); err != nil {
			return err
		}
		return r.UpdateIdentityMetadata(projectID, identity, taskID, url, displayName)
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
	// Resolve the target before touching the index: a writer that cannot
	// read it must fail before the binding lands, not after.
	target, gerr := w.GetTask(taskID)
	if gerr != nil {
		return gerr
	}
	if err := w.LinkIdentity(projectID, identity, taskID); err != nil {
		return err
	}
	return recordIdentityBinding(ctx, w, target, identity, orchestrator.ActionTypeIdentityLinked)
}

func unlinkIdentityIn(ctx context.Context, w identityBindingWriter, projectID, identity string) error {
	bound, rerr := w.ResolveIdentity(projectID, identity)
	if rerr != nil && !errors.Is(rerr, orchestrator.ErrTaskNotFound) {
		return rerr
	}
	if err := w.UnlinkIdentity(projectID, identity); err != nil {
		return err
	}
	if bound == nil {
		return nil
	}
	return recordIdentityBinding(ctx, w, bound, identity, orchestrator.ActionTypeIdentityUnlinked)
}

// recordIdentityBinding writes actionType against target when it is a card.
func recordIdentityBinding(ctx context.Context, w identityBindingWriter, target *orchestrator.Task, identity, actionType string) error {
	if target.Type != orchestrator.TaskTypeCard {
		return nil
	}
	payload, merr := json.Marshal(orchestrator.IdentityLinkedPayload{Identity: identity})
	if merr != nil {
		return merr
	}
	return w.CreateAction(ctx, &orchestrator.Action{
		TaskID:  target.ID,
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

// scopedIdentityWriter keeps the transaction's action writer and chooses its identity namespace.
type scopedIdentityWriter struct {
	identityBindingWriter
	scope orchestrator.IdentityScope
}
type workspaceIdentityWriter interface {
	LinkIdentityInScope(orchestrator.IdentityScope, string, string) error
	UnlinkIdentityInScope(orchestrator.IdentityScope, string) error
	ResolveIdentityInScope(orchestrator.IdentityScope, string) (*orchestrator.Task, error)
	UpdateIdentityMetadataInScope(orchestrator.IdentityScope, string, string, *string, *string) error
}

func (w scopedIdentityWriter) LinkIdentity(projectID, identity, taskID string) error {
	if w.scope.ProjectID != "" {
		return w.identityBindingWriter.LinkIdentity(w.scope.ProjectID, identity, taskID)
	}
	if source, ok := w.identityBindingWriter.(workspaceIdentityWriter); ok {
		return source.LinkIdentityInScope(w.scope, identity, taskID)
	}
	return errIdentityStoreUnavailable
}
func (w scopedIdentityWriter) UnlinkIdentity(projectID, identity string) error {
	if w.scope.ProjectID != "" {
		return w.identityBindingWriter.UnlinkIdentity(w.scope.ProjectID, identity)
	}
	if source, ok := w.identityBindingWriter.(workspaceIdentityWriter); ok {
		return source.UnlinkIdentityInScope(w.scope, identity)
	}
	return errIdentityStoreUnavailable
}
func (w scopedIdentityWriter) ResolveIdentity(projectID, identity string) (*orchestrator.Task, error) {
	if w.scope.ProjectID != "" {
		return w.identityBindingWriter.ResolveIdentity(w.scope.ProjectID, identity)
	}
	if source, ok := w.identityBindingWriter.(workspaceIdentityWriter); ok {
		return source.ResolveIdentityInScope(w.scope, identity)
	}
	return nil, errIdentityStoreUnavailable
}
func (w scopedIdentityWriter) UpdateIdentityMetadata(projectID, identity, taskID string, url, displayName *string) error {
	if w.scope.ProjectID != "" {
		if source, ok := w.identityBindingWriter.(identityMetadataWriter); ok {
			return source.UpdateIdentityMetadata(w.scope.ProjectID, identity, taskID, url, displayName)
		}
	} else if source, ok := w.identityBindingWriter.(workspaceIdentityWriter); ok {
		return source.UpdateIdentityMetadataInScope(w.scope, identity, taskID, url, displayName)
	}
	return errIdentityMetadataUnavailable
}

// LinkIdentityInScope binds an identity and metadata atomically within its namespace.
func (s *TaskAppService) LinkIdentityInScope(ctx context.Context, scope orchestrator.IdentityScope, identity, taskID string, url, displayName *string) error {
	if scope.ProjectID != "" {
		return s.LinkIdentityWithMetadata(ctx, scope.ProjectID, identity, taskID, url, displayName)
	}
	if err := orchestrator.ValidWorkspaceSlug(scope.WorkspaceID); err != nil {
		return err
	}
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	if url != nil {
		if err := orchestrator.ValidateIdentityURL(*url); err != nil {
			return err
		}
	}
	write := func(w identityBindingWriter) error {
		writer := scopedIdentityWriter{w, scope}
		if err := linkIdentityIn(ctx, writer, "", identity, taskID); err != nil {
			return err
		}
		if url != nil || displayName != nil {
			return writer.UpdateIdentityMetadata("", identity, taskID, url, displayName)
		}
		return nil
	}
	if s.Tx == nil {
		if url != nil || displayName != nil {
			return errIdentityMetadataUnavailable
		}
		return write(s.identityWriter())
	}
	return s.Tx.WithinTx(func(tx TxStore) error { return write(tx) })
}
func (s *TaskAppService) UnlinkIdentityInScope(ctx context.Context, scope orchestrator.IdentityScope, identity string) error {
	if scope.ProjectID != "" {
		return s.UnlinkIdentity(ctx, scope.ProjectID, identity)
	}
	if err := orchestrator.ValidWorkspaceSlug(scope.WorkspaceID); err != nil {
		return err
	}
	if s.Tx == nil {
		return unlinkIdentityIn(ctx, scopedIdentityWriter{s.identityWriter(), scope}, "", identity)
	}
	return s.Tx.WithinTx(func(tx TxStore) error { return unlinkIdentityIn(ctx, scopedIdentityWriter{tx, scope}, "", identity) })
}
func (s *TaskAppService) ResolveIdentityInScope(scope orchestrator.IdentityScope, identity string) (*orchestrator.Task, error) {
	if scope.ProjectID != "" {
		return s.ResolveIdentity(scope.ProjectID, identity)
	}
	if err := orchestrator.ValidWorkspaceSlug(scope.WorkspaceID); err != nil {
		return nil, err
	}
	if source, ok := s.Identities.(workspaceIdentityWriter); ok {
		return source.ResolveIdentityInScope(scope, identity)
	}
	return nil, errIdentityStoreUnavailable
}
func (s *TaskAppService) LinkWorkspaceIdentity(ctx context.Context, workspaceID, identity, taskID string) error {
	return s.LinkIdentityInScope(ctx, orchestrator.IdentityScope{WorkspaceID: workspaceID}, identity, taskID, nil, nil)
}
func (s *TaskAppService) UnlinkWorkspaceIdentity(ctx context.Context, workspaceID, identity string) error {
	return s.UnlinkIdentityInScope(ctx, orchestrator.IdentityScope{WorkspaceID: workspaceID}, identity)
}
func (s *TaskAppService) ResolveWorkspaceIdentity(workspaceID, identity string) (*orchestrator.Task, error) {
	return s.ResolveIdentityInScope(orchestrator.IdentityScope{WorkspaceID: workspaceID}, identity)
}

func (w looseIdentityWriter) LinkIdentityInScope(scope orchestrator.IdentityScope, identity, taskID string) error {
	if source, ok := w.TaskIdentityStore.(workspaceIdentityWriter); ok {
		return source.LinkIdentityInScope(scope, identity, taskID)
	}
	return errIdentityStoreUnavailable
}
func (w looseIdentityWriter) UnlinkIdentityInScope(scope orchestrator.IdentityScope, identity string) error {
	if source, ok := w.TaskIdentityStore.(workspaceIdentityWriter); ok {
		return source.UnlinkIdentityInScope(scope, identity)
	}
	return errIdentityStoreUnavailable
}
func (w looseIdentityWriter) ResolveIdentityInScope(scope orchestrator.IdentityScope, identity string) (*orchestrator.Task, error) {
	if source, ok := w.TaskIdentityStore.(workspaceIdentityWriter); ok {
		return source.ResolveIdentityInScope(scope, identity)
	}
	return nil, errIdentityStoreUnavailable
}
func (w looseIdentityWriter) UpdateIdentityMetadataInScope(scope orchestrator.IdentityScope, identity, taskID string, url, displayName *string) error {
	if source, ok := w.TaskIdentityStore.(workspaceIdentityWriter); ok {
		return source.UpdateIdentityMetadataInScope(scope, identity, taskID, url, displayName)
	}
	return errIdentityMetadataUnavailable
}
