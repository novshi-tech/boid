// Package opencode implements adapters.HarnessAdapter for the opencode CLI.
//
// Run() forks `opencode run` with signal forwarding and exit normalisation.
// Unlike the claude adapter, session persistence, payload-patch application,
// and boid task notify integration are not wired.
package opencode

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for the opencode CLI.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter { return &Adapter{} }

// Usage is not yet implemented (see codex.Adapter.Usage for the same
// rationale).
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}
