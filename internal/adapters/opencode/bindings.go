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
// Embedded skills DO reach ~/.claude/skills/<name> as read-only bind mounts
// again, one per skill — but the dispatcher declares them, not this method.
// PR3 of docs/plans/workspace-home-volume-persistence.md (論点 e-2)
// materializes the embedded set under the host-visible runtimes root
// (internal/dispatcher/skills_overlay.go) and homeMounts layers it onto the
// workspace home bind. In between, Phase 4 PR3 distributed them by copying
// the content into the workspace home (skills.DeployAll straight into
// <home>/.claude/skills); that write had to go once the home became a named
// volume the daemon cannot write to.
//
// Declaring them dispatcher-side rather than restoring them here is what
// keeps this method nil: the mounts are a property of how boid stages a
// sandbox, identical for every harness, not of what the opencode CLI needs.
//
// Non-embedded host skills (bitbucket, jira, google-* etc.) still have no
// adapter-side exposure path at all: a workspace that wants opencode to see
// them is the workspace author's responsibility, via the workspace's init.sh
// copying them into the workspace home (e.g. `cp -r ~/.claude/skills/<name>
// "$BOID_WORKSPACE_HOME/.claude/skills/"`) — see the plan doc's dogfood
// checklist. That arrangement is exactly why PR3's binds are per skill
// rather than one bind of the whole ~/.claude/skills directory: a
// directory-wide bind would hide every skill copied in this way.
//
// The HarnessAdapter interface still requires this method; returning an
// empty slice keeps the contract satisfied for any future $HOME-independent
// bind a harness might need.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
