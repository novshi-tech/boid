// Package codex implements adapters.HarnessAdapter for the Codex CLI.
//
// Run() forks `codex exec` with signal forwarding and exit normalisation.
// Unlike the claude adapter, session persistence, payload-patch application,
// and boid task notify integration are not wired.
package codex

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for the Codex CLI.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter { return &Adapter{} }

// Usage is not yet implemented; awaits the jobs table gaining usage columns
// and a per-run token/cost surface for this harness.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
