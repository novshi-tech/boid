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
// the harness is unknown. The runner-inner-child rejects an empty/unknown
// HarnessType (the planner resolves every job to one of the built-in
// harnesses), so the nil return is kept only for forward compatibility with
// a future harness not yet wired here.
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
