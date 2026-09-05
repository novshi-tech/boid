package opencode

import (
	"github.com/novshi-tech/boid/internal/adapters"
)

// Bindings declares the host bind-mounts opencode.Adapter.Run() needs inside
// the sandbox.
//
// Returns nil: opencode CLI state and skills reach the sandbox via the
// workspace HOME volume (internal/dispatcher/workspace_home.go) and
// dispatcher-owned skill mounts (skills_overlay.go) instead of an
// adapter-declared bind. The opencode CLI binary itself is expected to be
// installed into that workspace home by the workspace's init.sh; a missing
// binary fails fast with an explicit message from Run() (run.go's
// missingCLIError).
//
// Non-embedded host skills (bitbucket, jira, google-* etc.) have no
// adapter-side exposure path: a workspace that wants opencode to see them is
// the workspace author's responsibility, via the workspace's init.sh
// copying them into the workspace home (e.g. `cp -r ~/.claude/skills/<name>
// "$BOID_WORKSPACE_HOME/.claude/skills/"`).
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
