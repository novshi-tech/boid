package api

// docs/plans/ingestion-identity.md PR-1 (B-1): thin TaskAppService wrappers
// over TaskIdentityStore, backing the brokered task_identity_link / _unlink
// / _resolve ops (internal/server/boid_executor.go). Deliberately NOT
// wrapped in StatusError like most TaskAppService methods (e.g. GetTask) —
// the boid_executor callers need errors.Is(err, orchestrator.ErrTaskNotFound)
// / orchestrator.ErrIdentityConflict to survive unwrapped so
// BoidOpTaskIdentityResolve can represent "not found" as a distinct exit
// code rather than a generic error (see the op's own doc comment in
// internal/sandbox/protocol.go).

import (
	"errors"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// errIdentityStoreUnavailable mirrors every other optional-dependency guard
// in this package (e.g. "boid task notify unavailable"-shaped messages),
// just scoped to the three identity ops.
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
