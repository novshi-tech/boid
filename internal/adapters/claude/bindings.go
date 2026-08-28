package claude

import (
	"github.com/novshi-tech/boid/internal/adapters"
)

// Bindings declares the host bind-mounts claude.Adapter.Run() needs inside
// the sandbox.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retires every entry this
// method used to return: ~/.local/bin (ro, CLI dir + PATH), ~/.local/share/claude
// (ro), ~/.claude (rw), ~/.claude.json (rw file), and the per-embedded-skill
// ~/.local/share/boid/skills/<name> -> ~/.claude/skills/<name> binds. All of
// that state now lives directly in the sandbox's $HOME, which
// Runner.Dispatch (internal/dispatcher/workspace_home.go) bind-mounts from a
// persistent per-workspace home directory instead of a fresh tmpfs — so
// ~/.claude, ~/.claude.json etc. simply already exist at those paths without
// any adapter-declared bind. The claude CLI binary itself is expected to be
// installed into that same workspace home by the workspace's init.sh (see
// the plan doc's init.sh 契約 section); a missing binary now fails fast with
// an explicit message from Run() (run.go) instead of silently falling back
// to a bind that no longer exists.
//
// Embedded skills DO reach the sandbox's $HOME, but by no mount at all, and
// the dispatcher owns that too. They are baked into the runner image
// (build/container/Dockerfile, internal/dispatcher's embeddedSkillsImageDir)
// and symlinked into each of skillDiscoveryRoots by the workspace-home init
// container's prelude. Two earlier mechanisms sat here: Phase 4 PR3 copied the
// content into the workspace home (skills.DeployAll straight into
// <home>/.claude/skills), which had to go once the home became a named volume
// the daemon cannot write to; PR3 of
// docs/plans/workspace-home-volume-persistence.md (論点 e-2) then replaced
// that with one read-only bind per skill, sourced from a host-visible
// directory the daemon re-materialized on every dispatch.
//
// Keeping skill delivery dispatcher-side rather than restoring it here is what
// keeps this method nil: how skills reach a home is a property of how boid
// stages a sandbox, identical for every harness, not of what the claude CLI needs.
// Which directories a harness then SCANS is the harness's own business, and
// the three do not agree — see skillDiscoveryRoots for the table and why a
// link is written into more than one place.
//
// The HarnessAdapter interface still requires this method; returning an
// empty slice keeps the contract satisfied for any future $HOME-independent
// bind a harness might need.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
