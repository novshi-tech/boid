package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// WorkspaceEnvView is the reduced "environment" a Phase 5b `boid task env`
// call returns (docs/plans/phase5-shim-and-task-context.md 決定事項 4). Only
// the two properties an in-sandbox agent cannot observe on its own survive
// the 縮退 from the legacy environment.yaml: network egress allowlist and
// host-command policy. Every other section the legacy doc had
// (sandbox.*/filesystem.*/worktree/tools/session.*/network.restricted/notes)
// is either hard-coded scenery or directly observable from inside the
// container, so it is dropped rather than carried over.
//
// JSON tags define the RPC's wire schema; per the plan doc's "broker RPC の
// スキーマ安定性契約" open question, treat field renames/removals here as a
// breaking change to the `boid task env` contract skills depend on.
type WorkspaceEnvView struct {
	AllowedDomains []string                  `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty"`
	HostCommands   []WorkspaceEnvHostCommand `json:"host_commands,omitempty" yaml:"host_commands,omitempty"`
}

// WorkspaceEnvHostCommand mirrors orchestrator.CommandDef's agent-relevant
// surface (the parts an agent cannot infer by trying the command and reading
// the error): its allow/deny argument policy and reject rules. Renamed and
// exported from the former environmentHostCommand (Phase 5b PR1) so
// buildEnvironmentYAML's host_commands section and `boid task env`'s RPC
// response share one conversion (convertHostCommands) instead of two
// independently-maintained shapes that could silently drift apart.
type WorkspaceEnvHostCommand struct {
	Name   string                   `json:"name" yaml:"name"`
	Allow  []string                 `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny   []string                 `json:"deny,omitempty" yaml:"deny,omitempty"`
	Reject []WorkspaceEnvRejectRule `json:"reject,omitempty" yaml:"reject,omitempty"`
}

// WorkspaceEnvRejectRule mirrors orchestrator.RejectRule so agents can read,
// per host command, which arg shapes are rejected and what to do instead.
type WorkspaceEnvRejectRule struct {
	Match  string `json:"match" yaml:"match"`
	Reason string `json:"reason" yaml:"reason"`
}

// BuildWorkspaceEnvView derives the reduced env view from the dispatcher's
// resolved allowedDomains and hostCommands, via the shared convertHostCommands
// helper — the sole source of the `boid task env` RPC response as of the
// Phase 5b PR6 cutover (docs/plans/phase5-shim-and-task-context.md), which
// retired the parallel dispatch-time environment.yaml file this function used
// to also feed.
func BuildWorkspaceEnvView(allowedDomains []string, hostCommands map[string]orchestrator.CommandDef) WorkspaceEnvView {
	return WorkspaceEnvView{
		AllowedDomains: append([]string(nil), allowedDomains...),
		HostCommands:   convertHostCommands(hostCommands),
	}
}
