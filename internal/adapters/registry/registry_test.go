package registry

import (
	"testing"

	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/adapters/codex"
	"github.com/novshi-tech/boid/internal/adapters/opencode"
	"github.com/novshi-tech/boid/internal/adapters/shell"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestFor_MapsEachBuiltinHarness pins the harness→adapter table. The runner-
// inner-child dispatches every job through the adapter For() returns, so a
// silent gap here (a new HarnessType that falls through to the nil default,
// or a mis-wired case that returns the wrong adapter) would route a whole
// harness to the wrong Run() with no compile error. Assert both the non-nil
// contract and the concrete type per harness.
func TestFor_MapsEachBuiltinHarness(t *testing.T) {
	if a, ok := For(sandbox.HarnessShell).(*shell.Adapter); !ok || a == nil {
		t.Errorf("For(HarnessShell) = %T, want *shell.Adapter", For(sandbox.HarnessShell))
	}
	if a, ok := For(sandbox.HarnessClaude).(*claude.Adapter); !ok || a == nil {
		t.Errorf("For(HarnessClaude) = %T, want *claude.Adapter", For(sandbox.HarnessClaude))
	}
	if a, ok := For(sandbox.HarnessCodex).(*codex.Adapter); !ok || a == nil {
		t.Errorf("For(HarnessCodex) = %T, want *codex.Adapter", For(sandbox.HarnessCodex))
	}
	if a, ok := For(sandbox.HarnessOpenCode).(*opencode.Adapter); !ok || a == nil {
		t.Errorf("For(HarnessOpenCode) = %T, want *opencode.Adapter", For(sandbox.HarnessOpenCode))
	}
}

// TestFor_UnknownHarnessReturnsNil documents the forward-compat contract: an
// empty or unrecognised HarnessType yields nil so the runner-inner-child can
// reject it explicitly rather than crashing on a bogus adapter. Both the empty
// string (invalid spec) and an unknown value must hit the nil default.
func TestFor_UnknownHarnessReturnsNil(t *testing.T) {
	if got := For(sandbox.HarnessType("")); got != nil {
		t.Errorf("For(\"\") = %#v, want nil", got)
	}
	if got := For(sandbox.HarnessType("gemini")); got != nil {
		t.Errorf("For(\"gemini\") = %#v, want nil", got)
	}
}
