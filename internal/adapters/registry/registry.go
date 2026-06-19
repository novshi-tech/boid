// Package registry maps sandbox.HarnessType to a HarnessAdapter implementation.
//
// adapters/ is the interface package and each adapters/<harness>/ package
// imports it for Usage / Result / RunContext / HarnessAdapter — so adapters/
// itself cannot import the sub-packages without an import cycle. The registry
// lives one level out (it imports the sub-packages and the interface package)
// and is consumed by callers that need a harness → adapter mapping in one
// place: the dispatcher (for Bindings()) and the runner-inner-child
// (for Run()).
package registry

import (
	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/adapters/codex"
	"github.com/novshi-tech/boid/internal/adapters/opencode"
	"github.com/novshi-tech/boid/internal/adapters/shell"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// For returns the HarnessAdapter that owns the given HarnessType, or nil if
// the harness is unknown.
//
// Phase 3-d made the four built-in harnesses (shell / claude / codex /
// opencode) authoritative; the runner-inner-child rejects an empty / unknown
// HarnessType because the planner now resolves every job to one of these.
// The nil return path is kept for forward compatibility (a future harness
// the caller has not yet wired) but is no longer expected on any live path.
func For(harness sandbox.HarnessType) adapters.HarnessAdapter {
	switch harness {
	case sandbox.HarnessShell:
		return shell.New()
	case sandbox.HarnessClaude:
		return claude.New()
	case sandbox.HarnessCodex:
		return codex.New()
	case sandbox.HarnessOpenCode:
		return opencode.New()
	default:
		return nil
	}
}
