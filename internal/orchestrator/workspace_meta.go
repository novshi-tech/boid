package orchestrator

import "strings"

// WorkspaceMeta holds the machine-local workspace configuration that is
// stored in ~/.config/boid/workspaces/<slug>.yaml.
//
// A workspace defines which kits are active, environment variable overrides,
// and optional capability flags for sandboxes. Fields that are
// project-specific (secret_namespace, host_commands, additional_bindings,
// name, description, version) are intentionally absent; they remain in
// project.yaml.
type WorkspaceMeta struct {
	// Kits is the ordered list of kit slugs active in this workspace.
	// Kit slugs are resolved by KitRegistry at load time. Kits supply
	// host_commands / env / additional_bindings and are merged into every
	// behavior (kits do not provide hooks under the current schema).
	Kits []string `yaml:"kits,omitempty" json:"kits,omitempty"`

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
	// fetch (never push) via the git gateway — e.g. a private go module
	// dependency (docs/plans/git-gateway-cutover.md 「workspace peer」節:
	// 「workspace 設定に read-only の追加許可 repo」). Entries are upstream
	// URLs in any form dispatcher.NormalizeOriginURL accepts (HTTPS or SSH);
	// dispatcher resolves each into a gitgateway.RepoKey at dispatch time and
	// grants it PermFetch alongside this workspace's own projects and peers.
	// Same additive, floor-cannot-shrink relationship to the (currently
	// nonexistent) global floor as AllowedDomains — there is no write grant
	// here, fetch-only by construction.
	ExtraRepos []string `yaml:"extra_repos,omitempty" json:"extra_repos,omitempty"`

	// HostCommands is the reference-name list into the aggregated
	// host_commands config assembled at daemon startup (see
	// host_commands_config.go's LoadHostCommandsFromKits /
	// WriteHostCommandsConfig and docs/plans/workspace-db-consolidation.md
	// 「host_commands 実定義の集約先」). Unlike Kits, this field carries only
	// names — no path/allow/env/reject definitions travel with the
	// workspace itself; those live in the daemon-wide
	// ~/.config/boid/host_commands.yaml and are resolved by name at
	// GetWithWorkspace hydration time (ProjectStore.hostCommands).
	// Populated by MigrateWorkspaceYAMLToDB from each workspace's former
	// Kits list, unioned with any host_commands the workspace already had.
	HostCommands []string `yaml:"host_commands,omitempty" json:"host_commands,omitempty"`

	// ContainerImage is a nullable field reserved for the Phase 6 container
	// backend (decision 7, docs/plans/workspace-db-consolidation.md).
	// Dispatch ignores it entirely until then — the field only exists so
	// the schema migration for it does not need to be revisited later.
	ContainerImage string `yaml:"container_image,omitempty" json:"container_image,omitempty"`

	// AdditionalBindings is a workspace-scoped vestige of the kit mechanism
	// (decision 4): additional bind mounts merged into every sandbox
	// launched under this workspace, the same way kit-provided
	// AdditionalBindings are merged today. Retained until Phase 4 replaces
	// it with the $HOME workspace volume contract.
	AdditionalBindings []BindMount `yaml:"additional_bindings,omitempty" json:"additional_bindings,omitempty"`
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
// expandWorkspaceRuntimeForDispatch returns a clone of meta with Env and
// AdditionalBindings host-environment-expanded (${VAR}) for dispatch,
// leaving meta itself completely untouched.
//
// DB/yaml-stored WorkspaceMeta values are intentionally raw/unexpanded —
// expanding at rest would bake resolved, possibly secret-shaped values into
// storage and would be subject to TOCTOU (the daemon's own environment can
// change between materialization and dispatch). ProjectStore.GetWithWorkspace
// calls this once per hydration, right after WorkspaceStore.Load, so every
// dispatch sees the current expansion of a ${VAR} placeholder rather than
// whatever happened to be baked in at migration/save time.
//
// This mirrors ExpandHostCommandsForDispatch (host_commands_config.go),
// which performs the identical clone-then-expand step for
// workspace.HostCommands' resolved definitions. Before this function
// existed, workspace/kit-materialized Env and AdditionalBindings never got
// the equivalent treatment: a placeholder such as ${E2E_WORKSPACE_DIR} or
// ${XDG_DATA_HOME} was carried into the DB unexpanded by
// materializeKitRuntimeIntoWorkspace and never expanded again, a silent
// regression versus the pre-cutover yaml-mode path.
//
// meta is never mutated: the returned value's Env map and AdditionalBindings
// slice are independent copies before interpolateEnvMap /
// interpolateBindMounts run in place on those copies. This matters even
// though WorkspaceStore.Load (both the yaml and DB-repository backends)
// currently always allocates a fresh *WorkspaceMeta per call — the
// no-mutation contract should not rely on that happening to be true.
func expandWorkspaceRuntimeForDispatch(meta *WorkspaceMeta) *WorkspaceMeta {
	if meta == nil {
		return nil
	}
	clone := *meta
	clone.Env = mergeStringMaps(nil, meta.Env)
	clone.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	interpolateEnvMap(clone.Env)
	interpolateBindMounts(clone.AdditionalBindings)
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
