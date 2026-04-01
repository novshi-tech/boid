package orchestrator

import "time"

// BindMount is a plain shared DTO across orchestration and sandbox planning.
// It carries only mount source/mode data and does not encode provider behavior.
type BindMount struct {
	Source string `yaml:"source" json:"source"`
	Mode   string `yaml:"mode" json:"mode"`
}

// CommandDef is the project-spec transport shape for sandbox command policy input.
// It is mirrored into dispatcher-owned transport data, while sandbox remains the
// canonical owner of how these policy fields are enforced.
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

type TraitType string

const (
	TraitPrompt       TraitType = "prompt"
	TraitArtifact     TraitType = "artifact"
	TraitVerification TraitType = "verification"
	TraitTasks        TraitType = "tasks"
)

type MergeMode string

const (
	MergeModeExclusive MergeMode = "exclusive"
	MergeModeShared    MergeMode = "shared"
)

type Role string

const (
	RoleHook Role = "hook"
	RoleGate Role = "gate"
)

type Hook struct {
	ID             string      `yaml:"id" json:"id"`
	On             string      `yaml:"on" json:"on"`
	RequiresTraits []TraitType `yaml:"requires_traits" json:"requires_traits"`
	Requires       []string    `yaml:"requires" json:"requires"`
	ScriptPath     string      `yaml:"-" json:"-"`
}

type Gate struct {
	ID             string      `yaml:"id" json:"id"`
	On             string      `yaml:"on" json:"on"`
	RequiresTraits []TraitType `yaml:"requires_traits" json:"requires_traits"`
	ScriptPath     string      `yaml:"-" json:"-"`
}

type HookFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Hook      Hook
}

type GateFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Gate      Gate
}

type KitHooksInfo struct {
	HooksDir string
	HookIDs  []string
}

type KitGatesInfo struct {
	GatesDir string
	GateIDs  []string
}

type TaskBehavior struct {
	Name         string   `yaml:"name" json:"name"`
	Transition   string   `yaml:"transition" json:"transition"`
	Traits       []string `yaml:"traits" json:"traits"`
	Readonly     bool     `yaml:"readonly" json:"readonly,omitempty"`
	Worktree     bool     `yaml:"worktree" json:"worktree,omitempty"`
	BranchPrefix string   `yaml:"branch_prefix" json:"branch_prefix,omitempty"`
	BaseBranch   string   `yaml:"base_branch" json:"base_branch,omitempty"`
}

type ProjectMeta struct {
	ID                 string                  `yaml:"id" json:"id"`
	Name               string                  `yaml:"name" json:"name"`
	Kits               []string                `yaml:"kits" json:"kits,omitempty"`
	TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	Hooks              []Hook                  `yaml:"hooks" json:"hooks"`
	Gates              []Gate                  `yaml:"gates" json:"gates"`
	HostCommands       map[string]CommandDef   `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string       `yaml:"env" json:"env"`
	KitHooksDirs       []KitHooksInfo          `yaml:"-" json:"-"`
	KitGatesDirs       []KitGatesInfo          `yaml:"-" json:"-"`
}

type ProjectLocalMeta struct {
	Version            int                   `yaml:"version"`
	Kits               ProjectLocalKits      `yaml:"kits"`
	HostCommands       map[string]CommandDef `yaml:"host_commands"`
	AdditionalBindings []BindMount           `yaml:"additional_bindings"`
	Env                map[string]string     `yaml:"env"`
}

type ProjectLocalKits struct {
	Add    []string `yaml:"add"`
	Remove []string `yaml:"remove"`
}

type Project struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspace_id"`
	WorkDir     string      `json:"work_dir"`
	Meta        ProjectMeta `json:"meta"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type WorkspaceSummary struct {
	ID           string `json:"id"`
	ProjectCount int    `json:"project_count"`
}

// KitMeta holds the parsed content of a kit.yaml file.
type KitMeta struct {
	TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors"`
	Hooks              []Hook                  `yaml:"hooks"`
	Gates              []Gate                  `yaml:"gates"`
	HostCommands       map[string]CommandDef   `yaml:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings"`
	Env                map[string]string       `yaml:"env"`
	HooksDir           string                  `yaml:"-"`
	GatesDir           string                  `yaml:"-"`
}
