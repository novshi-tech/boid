package dispatcher

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// WorkspaceEnvView is the reduced "environment" that `boid task env` returns
// to an in-sandbox agent: the network egress allowlist and host-command
// policy, the only properties the agent cannot observe on its own.
//
// JSON tags define the RPC's wire schema — treat field renames/removals here
// as a breaking change to a contract skills depend on.
type WorkspaceEnvView struct {
	AllowedDomains []string                  `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty"`
	HostCommands   []WorkspaceEnvHostCommand `json:"host_commands,omitempty" yaml:"host_commands,omitempty"`
}

// WorkspaceEnvHostCommand mirrors orchestrator.CommandDef's agent-relevant
// surface: its allow/deny argument policy and reject rules (the parts an
// agent cannot infer by trying the command and reading the error). Shared
// via convertHostCommands so this and buildEnvironmentYAML's host_commands
// section can't drift apart.
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
// resolved allowedDomains and hostCommands via the shared convertHostCommands
// helper — the sole source of the `boid task env` RPC response.
func BuildWorkspaceEnvView(allowedDomains []string, hostCommands map[string]orchestrator.CommandDef) WorkspaceEnvView {
	return WorkspaceEnvView{
		AllowedDomains: append([]string(nil), allowedDomains...),
		HostCommands:   convertHostCommands(hostCommands),
	}
}
