package orchestrator

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
	// Kit slugs are resolved by KitRegistry at load time.
	Kits []string `yaml:"kits,omitempty" json:"kits,omitempty"`

	// Env holds environment variable overrides applied to every sandbox
	// launched under this workspace. Values here take precedence over
	// kit-supplied env but are overridden by project.yaml env.
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`

	// Capabilities declares optional sandbox capability flags for this
	// workspace. Uses the same Capabilities type as ProjectMeta.
	Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}
