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
// Embedded skills are synced into the workspace home's ~/.claude/skills/ by
// skills.DeployAll (internal/skills/deploy.go), called from Runner.Dispatch
// right after the workspace home is resolved — copy-based distribution
// replaces the bind-mount this method used to declare per skill. Non-embedded
// host skills (bitbucket, jira, google-* etc.) no longer have an adapter-side
// exposure path at all: a workspace that wants opencode to see them is now
// the workspace author's responsibility, via the workspace's init.sh copying
// them into the workspace home (e.g. `cp -r ~/.claude/skills/<name>
// "$BOID_WORKSPACE_HOME/.claude/skills/"`) — see the plan doc's dogfood
// checklist.
//
// The HarnessAdapter interface still requires this method; returning an
// empty slice keeps the contract satisfied for any future $HOME-independent
// bind a harness might need.
func (a *Adapter) Bindings(homeDir string) []adapters.BindMount {
	return nil
}
