package model

import (
	"time"
)

// BindMount describes a host path to bind-mount into the sandbox.
type BindMount struct {
	Source string `yaml:"source" json:"source"`
	Mode   string `yaml:"mode" json:"mode"` // "ro" (default) or "rw"
}

// CommandDef defines a host command that can be executed inside the sandbox
// via the hostcmd broker.
// Deprecated: use project.CommandDef instead. This exists only for transitional compatibility.
type CommandDef struct {
	Name                string            `yaml:"name" json:"name"`
	Path                string            `yaml:"path" json:"path"`
	AllowedPatterns     []string          `yaml:"allowed_patterns" json:"allowed_patterns"`
	DeniedPatterns      []string          `yaml:"denied_patterns" json:"denied_patterns"`
	AllowedSubcommands  []string          `yaml:"allowed_subcommands" json:"allowed_subcommands"`
	AllowStdin          bool              `yaml:"allow_stdin" json:"allow_stdin"`
	Env                 map[string]string `yaml:"env" json:"env"`
	ExtractSubcommandFn string            `yaml:"extract_subcommand_fn" json:"extract_subcommand_fn"`
	RequireCwd          bool              `yaml:"require_cwd" json:"require_cwd"`
	AllowedCwdPrefixes  []string          `yaml:"allowed_cwd_prefixes" json:"allowed_cwd_prefixes"`
}

type ProjectMeta struct {
	ID                 string                   `yaml:"id" json:"id"`
	WorkspaceID        string                   `yaml:"workspace_id" json:"workspace_id"`
	Name               string                   `yaml:"name" json:"name"`
	Kits               []string                 `yaml:"kits" json:"kits,omitempty"`
	TaskBehaviors      map[string]TaskBehavior  `yaml:"task_behaviors" json:"task_behaviors"`
	Hooks              []Hook                   `yaml:"hooks" json:"hooks"`
	Gates              []Gate                   `yaml:"gates" json:"gates"`
	HostCommands       map[string]CommandDef    `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []BindMount              `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string        `yaml:"env" json:"env"`

	// Populated at load time after kit resolution; not from YAML.
	KitHooksDirs []KitHooksInfo `yaml:"-" json:"-"`
	KitGatesDirs []KitGatesInfo `yaml:"-" json:"-"`
}

// Project represents a registered project.
// DB stores only ID, WorkDir, and timestamps.
// Meta is loaded from project.yaml at runtime via project.Store.
type Project struct {
	ID        string      `json:"id"`
	WorkDir   string      `json:"work_dir"`
	Meta      ProjectMeta `json:"meta"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}
