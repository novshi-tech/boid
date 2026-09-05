package orchestrator

import "strings"

// WorkspaceMeta holds the machine-local workspace configuration that is
// stored in ~/.config/boid/workspaces/<slug>.yaml.
//
// A workspace defines host_command references, environment variable
// overrides, and optional capability flags for sandboxes. Fields that are
// project-specific (secret_namespace, host_commands, additional_bindings,
// name, description, version) are intentionally absent; they remain in
// project.yaml.
//
// A Kits field used to live here (an ordered list of kit slugs, resolved and
// merged in at hydration time); the kit mechanism itself was retired and the
// field removed. Two client-side call sites still resolve a legacy `kits:`
// reference list against MaterializeWorkspaceKitsForPersist, but source it
// from outside this type now — see cmd/workspace.go's
// ensureWorkspaceExistsForAssign and cmd/project_migrate.go.
type WorkspaceMeta struct {
	// Env holds environment variable overrides applied to every sandbox
	// launched under this workspace. Values here take precedence over
	// kit-supplied env but are overridden by project.yaml env.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// Capabilities declares optional sandbox capability flags for this
	// workspace. Uses the same Capabilities type as ProjectMeta.
	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`

	// AllowedDomains is the workspace-scoped HTTP(S) proxy egress allowlist.
	// Domains listed here are ADDED to the daemon-wide allowlist
	// (config.yaml sandbox.allowed_domains); the workspace cannot remove
	// entries from the global floor — that floor exists to keep
	// pypi/github/etc reachable for tool installation.
	//
	// Same matching rules as the global list (see sandbox.Proxy):
	//   - "registry-1.docker.io"  exact match
	//   - ".cosmos.azure.com"     suffix match (matches "<sub>.cosmos.azure.com")
	AllowedDomains []string `yaml:"allowed_domains,omitempty" json:"allowed_domains,omitempty"`

	// ExtraRepos is the workspace-scoped, read-only allowlist of additional
	// git repositories outside this workspace's own projects that jobs may
	// fetch (never push) via the git gateway. Entries are upstream URLs in
	// any form dispatcher.NormalizeOriginURL accepts (HTTPS or SSH);
	// dispatcher resolves each into a gitgateway.RepoKey at dispatch time and
	// grants it PermFetch alongside this workspace's own projects and peers.
	ExtraRepos []string `yaml:"extra_repos,omitempty" json:"extra_repos,omitempty"`

	// Services is the workspace-scoped list of API gateway logical service
	// names enabled for this workspace, on top of config.yaml's
	// services_floor — additive only, a workspace can never remove a floor
	// entry. An entry naming a service config.yaml's `services:` map does
	// not declare is not rejected here; it simply never resolves to
	// anything reachable at dispatch time — see ResolveEnabledServices.
	Services []string `yaml:"services,omitempty" json:"services,omitempty"`

	// HostCommands is the reference-name list into the aggregated
	// host_commands config assembled at daemon startup (see
	// host_commands_config.go's LoadHostCommandsFromKits /
	// WriteHostCommandsConfig). This field carries only names — no
	// path/allow/env/reject definitions travel with the workspace itself;
	// those live in the daemon-wide ~/.config/boid/host_commands.yaml and
	// are resolved by name at GetWithWorkspace hydration time.
	HostCommands []string `yaml:"host_commands,omitempty" json:"host_commands,omitempty"`

	// ContainerImage is a nullable field reserved for the container backend.
	// Dispatch ignores it entirely until then.
	ContainerImage string `yaml:"container_image,omitempty" json:"container_image,omitempty"`

	// AdditionalBindings was retired outright: the $HOME workspace volume
	// contract replaces the need for a workspace to declare extra host bind
	// mounts. There is deliberately no field here any more; an existing
	// `additional_bindings:` key still parses without error (unknown to this
	// struct, silently ignored) but its value is discarded rather than
	// materialized into a sandbox mount.

	// TaskBehaviors / BaseBranch / ForkPoint / DefaultTaskBehavior are the
	// workspace's "default project" definition — a project.yaml-less or
	// partial project.yaml inherits these. Mirrors the identically-named
	// ProjectMeta fields field-for-field, both in shape and in the
	// validation/normalization pipeline they go through
	// (normalizeWorkspaceDefaultTaskBehaviors, spec_loader.go). See
	// docs/plans/workspace-default-project.md for the hydration-merge rules
	// (project.yaml wins per-field over these workspace defaults).
	TaskBehaviors map[string]TaskBehavior `yaml:"task_behaviors,omitempty" json:"task_behaviors,omitempty"`
	// BaseBranch is the workspace default's base_branch. Empty means
	// "unspecified" — there is deliberately no way to explicitly un-set an
	// inherited value.
	BaseBranch string `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	// ForkPoint is the workspace default's fork_point. Same empty-means-
	// unspecified rule as BaseBranch.
	ForkPoint string `yaml:"fork_point,omitempty" json:"fork_point,omitempty"`
	// DefaultTaskBehavior is the workspace default's default_task_behavior.
	// Same empty-means-unspecified rule as BaseBranch.
	DefaultTaskBehavior string `yaml:"default_task_behavior,omitempty" json:"default_task_behavior,omitempty"`
}

// ResolveAllowedDomains returns the effective proxy egress allowlist for a
// sandbox launched under workspace. The result is the additive union of the
// daemon-wide floor (config.yaml sandbox.allowed_domains, plus boid built-in
// defaults) and the workspace's AllowedDomains. The workspace cannot remove
// entries from the floor: that guarantee keeps tool-install endpoints
// (pypi.org, github.com, …) reachable across every workspace.
//
// Duplicate entries are de-duplicated (case-insensitive) while preserving
// first-seen order. The function is a free function (rather than a method on
// WorkspaceMeta) so that callers may pass a nil workspace to mean "no
// workspace overrides" without having to construct an empty struct.
//
// Future extension point: a third parameter for kit-supplied domains is
// expected here (see [[project-workspace-allowed-domains]]); when added it
// will slot in between the floor and the workspace overrides with the same
// additive semantics.
// expandWorkspaceRuntimeForDispatch returns a clone of meta with Env
// host-environment-expanded (${VAR}) for dispatch, leaving meta itself
// completely untouched.
//
// DB/yaml-stored WorkspaceMeta values are intentionally raw/unexpanded —
// expanding at rest would bake resolved, possibly secret-shaped values into
// storage and would be subject to TOCTOU (the daemon's own environment can
// change between materialization and dispatch). ProjectStore.GetWithWorkspace
// calls this once per hydration, right after WorkspaceStore.Load, so every
// dispatch sees the current expansion of a ${VAR} placeholder.
//
// Mirrors ExpandHostCommandsForDispatch (host_commands_config.go), which
// performs the identical clone-then-expand step for
// workspace.HostCommands' resolved definitions.
//
// meta is never mutated: the returned value's Env map is an independent copy
// before interpolateEnvMap runs in place on it.
func expandWorkspaceRuntimeForDispatch(meta *WorkspaceMeta) *WorkspaceMeta {
	if meta == nil {
		return nil
	}
	clone := *meta
	clone.Env = mergeStringMaps(nil, meta.Env)
	interpolateEnvMap(clone.Env)
	return &clone
}

func ResolveAllowedDomains(globalFloor []string, workspace *WorkspaceMeta) []string {
	seen := make(map[string]struct{}, len(globalFloor))
	out := make([]string, 0, len(globalFloor))
	add := func(d string) {
		key := strings.ToLower(strings.TrimSpace(d))
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}
	for _, d := range globalFloor {
		add(d)
	}
	if workspace != nil {
		for _, d := range workspace.AllowedDomains {
			add(d)
		}
	}
	return out
}

// ResolveEnabledServices returns the effective API gateway service allowlist
// for a job dispatched under workspace: the additive union of the
// daemon-wide floor (config.yaml services_floor) and the workspace's own
// Services list (docs/plans/api-gateway.md §3). The workspace cannot remove
// entries from the floor.
//
// Unlike ResolveAllowedDomains, matching is case-SENSITIVE: a service name
// is an arbitrary config.yaml map key (an identifier), not a domain name, so
// there is no DNS-style case-insensitivity convention to honor here.
// Duplicate entries are de-duplicated while preserving first-seen order,
// same as ResolveAllowedDomains. workspace may be nil (no workspace
// overrides).
func ResolveEnabledServices(floor []string, workspace *WorkspaceMeta) []string {
	seen := make(map[string]struct{}, len(floor))
	out := make([]string, 0, len(floor))
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range floor {
		add(s)
	}
	if workspace != nil {
		for _, s := range workspace.Services {
			add(s)
		}
	}
	return out
}
