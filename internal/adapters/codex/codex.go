// Package codex implements adapters.HarnessAdapter for the Codex CLI.
//
// Phase 3-c prototype: the goal is to validate that adapters.HarnessAdapter
// composes for a non-claude harness, not to reach feature parity with the
// claude adapter. Run() forks `codex exec` with signal forwarding and exit
// normalisation; session persistence, payload_patch.json writes and
// boid task notify integration are deliberately left out (see
// docs/plans/agent-aware-boid.md Phase 3-c).
package codex

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for the Codex CLI.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter { return &Adapter{} }

// Usage is not implemented in Phase 3-c. Usage() will be wired in Phase 4
// when the jobs table gains usage columns and each harness's per-run
// token / cost surface is finalised.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
