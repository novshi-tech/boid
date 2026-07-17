package codex

import (
	"github.com/novshi-tech/boid/internal/adapters"
)

// Bindings declares the host bind-mounts codex.Adapter.Run() needs inside
// the sandbox.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retires every entry this
// method used to return: the rw ~/.codex state dir, the resolved `codex`
// binary's parent dir (PATH), the ro ~/.volta shim tree, and the per-
// embedded-skill ~/.local/share/boid/skills/<name> -> ~/.claude/skills/<name>
// binds. All of that state now lives directly in the sandbox's $HOME, which
// Runner.Dispatch (internal/dispatcher/workspace_home.go) bind-mounts from a
// persistent per-workspace home directory instead of a fresh tmpfs — so
// ~/.codex, ~/.volta etc. simply already exist at those paths without any
// adapter-declared bind. The codex CLI binary itself is expected to be
// installed into that same workspace home by the workspace's init.sh (see
// the plan doc's init.sh 契約 section); a missing binary now fails fast with
// an explicit message from Run() (run.go) instead of silently falling back
// to a bind that no longer exists.
//
// Embedded skills are synced into the workspace home's ~/.claude/skills/ by
// skills.DeployAll (internal/skills/deploy.go), called from Runner.Dispatch
// right after the workspace home is resolved — copy-based distribution
// replaces the bind-mount this method used to declare per skill.
//
// The HarnessAdapter interface still requires this method; returning an
// empty slice keeps the contract satisfied for any future $HOME-independent
// bind a harness might need.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
