package api

// TaskAppService task-identity wrappers over TaskIdentityStore, backing the
// brokered task_identity_link/_unlink/_resolve ops. Errors are intentionally
// left unwrapped (no StatusError) so callers can errors.Is against
// orchestrator.ErrTaskNotFound / ErrIdentityConflict.

import (
	"errors"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// errIdentityStoreUnavailable guards the three identity ops when no identity store is configured.
var errIdentityStoreUnavailable = errors.New("identity store unavailable")

// LinkIdentity binds identity to taskID within projectID's scope. See
// TaskIdentityStore.LinkIdentity for the idempotent-same-task /
// ErrIdentityConflict-different-task contract.
func (s *TaskAppService) LinkIdentity(projectID, identity, taskID string) error {
	if s.Identities == nil {
		return errIdentityStoreUnavailable
	}
	return s.Identities.LinkIdentity(projectID, identity, taskID)
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
