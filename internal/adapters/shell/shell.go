// Package shell implements adapters.HarnessAdapter for plain shell / exec
// jobs that do not embed an agent harness. It is the fall-through adapter
// the runner-inner-child uses when JobSpec.HarnessType resolves to "shell"
// — non-agent hook scripts (e2e fixture kits, custom user hooks without an
// `agent:` declaration) and `boid exec`-style argv launches.
//
// Phase 3-d (session 概念 + shell adapter 1 等市民化) introduced this
// adapter so every job — agent or not — flows through the same Run() pipeline
// in the runner-inner-child. The legacy `runExecArgv` branch was retired in
// the same change; HarnessType is invariant non-empty from PR1 onward.
//
// shell adapter is intentionally minimal:
//   - no session resolution (RunContext.SessionID is ignored)
//   - no payload_patch.json writes (the hook script is responsible if it
//     wants one — broker job-done still flows through PayloadPatchPath)
//   - no token accounting (Usage() returns zero — shell jobs are not
//     billable in Phase 4)
//   - no Bindings() (Phase 3-c claude / codex / opencode each declared their
//     own CLI bindings; shell relies on the base mount set the dispatcher
//     applies for every sandbox).
package shell

import (
	"context"

	"github.com/novshi-tech/boid/internal/adapters"
)

// Adapter implements adapters.HarnessAdapter for plain shell jobs.
type Adapter struct{}

// New returns a new Adapter.
func New() *Adapter { return &Adapter{} }

// Usage returns a zero Usage; shell jobs are not accounted in Phase 4.
func (a *Adapter) Usage(_ context.Context, _ string) (adapters.Usage, error) {
	return adapters.Usage{}, nil
}

// Bindings returns nil — shell adapter inherits the base mount set without
// adding any harness-specific binds.
func (a *Adapter) Bindings(_ string) []adapters.BindMount { return nil }
