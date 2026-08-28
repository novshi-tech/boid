package opencode

import (
	"github.com/novshi-tech/boid/internal/adapters"
)

// Bindings declares the host bind-mounts opencode.Adapter.Run() needs inside
// the sandbox.
//
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) retires every entry this
// method used to return: the rw ~/.opencode / ~/.config/opencode /
// ~/.local/share/opencode / ~/.local/state/opencode state trees, the
// resolved `opencode` binary's parent dir (PATH), the per-embedded-skill
// ~/.local/share/boid/skills/<name> -> ~/.claude/skills/<name> binds, and
// the individual ro binds for each non-embedded host skill under
// ~/.claude/skills/* (bitbucket, jira, google-*, ms-graph, ...). All of that
// state now lives directly in the sandbox's $HOME, which Runner.Dispatch
// (internal/dispatcher/workspace_home.go) bind-mounts from a persistent
// per-workspace home directory instead of a fresh tmpfs — so ~/.opencode,
// ~/.config/opencode etc. simply already exist at those paths without any
// adapter-declared bind. The opencode CLI binary itself is expected to be
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
// stages a sandbox, identical for every harness, not of what the opencode CLI needs.
// Which directories a harness then SCANS is the harness's own business, and
// the three do not agree — see skillDiscoveryRoots for the table and why a
// link is written into more than one place.
//
// Non-embedded host skills (bitbucket, jira, google-* etc.) still have no
// adapter-side exposure path at all: a workspace that wants opencode to see
// them is the workspace author's responsibility, via the workspace's init.sh
// copying them into the workspace home (e.g. `cp -r ~/.claude/skills/<name>
// "$BOID_WORKSPACE_HOME/.claude/skills/"`) — see the plan doc's dogfood
// checklist. That arrangement is exactly why PR3's binds were per skill
// rather than one bind of the whole ~/.claude/skills directory: a
// directory-wide bind would have hidden every skill copied in this way. The
// constraint outlived the binds and is now satisfied for free — a symlink at
// <root>/<name> shadows nothing else in <root>, so a hand-copied skill sitting
// beside it stays visible without anyone having to arrange for it.
//
// The HarnessAdapter interface still requires this method; returning an
// empty slice keeps the contract satisfied for any future $HOME-independent
// bind a harness might need.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
