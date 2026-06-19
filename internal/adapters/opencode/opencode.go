// Package opencode implements adapters.HarnessAdapter for the opencode CLI.
//
// Phase 3-c prototype: the goal is to validate that adapters.HarnessAdapter
// composes for a non-claude harness. Run() forks `opencode run` with signal
// forwarding and exit normalisation; session persistence, payload_patch.json
// writes and boid task notify integration are deliberately left out (see
// docs/plans/agent-aware-boid.md Phase 3-c).
package opencode

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for the opencode CLI.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter { return &Adapter{} }

// Usage is not implemented in Phase 3-c (see codex/opencode.go for the
// same rationale).
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
