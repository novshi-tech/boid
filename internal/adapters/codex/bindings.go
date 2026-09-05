package codex

import (
	"github.com/novshi-tech/boid/internal/adapters"
)

// Bindings declares the host bind-mounts codex.Adapter.Run() needs inside
// the sandbox.
//
// Returns nil: codex CLI state and skills reach the sandbox via the
// workspace HOME volume (internal/dispatcher/workspace_home.go) and
// dispatcher-owned skill mounts (skills_overlay.go) instead of an
// adapter-declared bind. The codex CLI binary itself is expected to be
// installed into that workspace home by the workspace's init.sh; a missing
// binary fails fast with an explicit message from Run() (run.go's
// missingCLIError).
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
