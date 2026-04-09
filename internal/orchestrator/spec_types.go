package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BindMount is a plain shared DTO across orchestration and sandbox planning.
// It carries only mount source/mode data and does not encode provider behavior.
type BindMount struct {
	Source string `yaml:"source" json:"source"`
	Mode   string `yaml:"mode" json:"mode"`
}

// HookFile describes a single hook file to bind-mount into the sandbox.
type HookFile struct {
	Source     string // host-side absolute path
	TargetName string // filename inside sandbox .boid/hooks/
}

// CommandDef is the orchestrator-side transport shape for sandbox command policy input.
// Dispatcher and sandbox mirror this shape; sandbox owns the enforcement semantics.
type CommandDef struct {
	Name               string
	Path               string
	AllowedPatterns    []string
	DeniedPatterns     []string
	AllowedSubcommands []string
	AllowStdin         bool
	Env                map[string]string
}

// HostCommandSpec is the simplified YAML DSL for declaring host commands.
type HostCommandSpec struct {
	Allow []string          `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string          `yaml:"deny,omitempty" json:"deny,omitempty"`
	Stdin bool              `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	Path  string            `yaml:"path,omitempty" json:"path,omitempty"`
	Env   map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

// ToCommandDef converts a HostCommandSpec into a CommandDef for internal use.
func (s HostCommandSpec) ToCommandDef(name string) CommandDef {
	var subcommands, patterns []string
	for _, a := range s.Allow {
		if strings.ContainsAny(a, " *?") {
			patterns = append(patterns, a)
		} else {
			subcommands = append(subcommands, a)
		}
	}
	return CommandDef{
		Name:               name,
		Path:               s.Path,
		AllowedSubcommands: subcommands,
		AllowedPatterns:    patterns,
		DeniedPatterns:     s.Deny,
		AllowStdin:         s.Stdin,
		Env:                s.Env,
	}
}

// HostCommands supports both list and map YAML forms:
//
//	host_commands: [gh, aws]
//	host_commands:
//	  gh:
//	    allow: [pr, issue]
//	  aws:
type HostCommands map[string]HostCommandSpec

func (h *HostCommands) UnmarshalYAML(value *yaml.Node) error {
	// Try list form: [gh, aws, az]
	var list []string
	if value.Kind == yaml.SequenceNode {
		if err := value.Decode(&list); err != nil {
			return fmt.Errorf("host_commands: invalid list form: %w", err)
		}
		*h = make(HostCommands, len(list))
		for _, name := range list {
			(*h)[name] = HostCommandSpec{}
		}
		return nil
	}
	// Map form: gh: {allow: [...]}
	var m map[string]HostCommandSpec
	if err := value.Decode(&m); err != nil {
		return fmt.Errorf("host_commands: %w", err)
	}
	*h = m
	return nil
}

// ToCommandDefs converts the DSL specs to internal CommandDef map.
func (h HostCommands) ToCommandDefs() map[string]CommandDef {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]CommandDef, len(h))
	for name, spec := range h {
		out[name] = spec.ToCommandDef(name)
	}
	return out
}

type InstructionType string

const (
	InstructionTypeExecution    InstructionType = "execution"
	InstructionTypeRework       InstructionType = "rework"
	InstructionTypeVerification InstructionType = "verification"
)

type Instruction struct {
	Type        InstructionType `json:"type" yaml:"type"`
	Consumer    string          `json:"consumer" yaml:"consumer"`
	Message     string          `json:"message,omitempty" yaml:"message,omitempty"`
	Interactive bool            `json:"interactive,omitempty" yaml:"interactive,omitempty"`
	Model       string          `json:"model,omitempty" yaml:"model,omitempty"`
}

type RoutedInstruction struct {
	Role        string          `json:"role"`
	Type        InstructionType `json:"type"`
	Consumer    string          `json:"consumer"`
	Message     string          `json:"message"`
	Interactive bool            `json:"interactive,omitempty"`
	Model       string          `json:"model,omitempty"`
}

type TraitType string

const (
	TraitInstructions TraitType = "instructions"
	TraitArtifact     TraitType = "artifact"
	TraitVerification TraitType = "verification"
	TraitTasks        TraitType = "tasks"
)

// IsOptional reports whether the trait is declared with a trailing "?".
func (t TraitType) IsOptional() bool {
	return strings.HasSuffix(string(t), "?")
}

// Base returns the trait name without the optional "?" suffix.
func (t TraitType) Base() TraitType {
	return TraitType(strings.TrimSuffix(string(t), "?"))
}

type HandlerTraits struct {
	Consumes []TraitType `json:"consumes,omitempty" yaml:"consumes,omitempty"`
	Produces []TraitType `json:"produces,omitempty" yaml:"produces,omitempty"`
}

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

// OnValues holds one or more task status values for hook/gate matching.
// In YAML it accepts both a scalar string ("executing") and a sequence
// (["executing", "reworking"]).
type OnValues []string

func (o *OnValues) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		*o = OnValues{node.Value}
	case yaml.SequenceNode:
		var vals []string
		if err := node.Decode(&vals); err != nil {
			return err
		}
		*o = vals
	default:
		return fmt.Errorf("on: expected string or sequence, got %v", node.Tag)
	}
	return nil
}

// Contains reports whether status is listed in this set.
func (o OnValues) Contains(status string) bool {
	for _, v := range o {
		if v == status {
			return true
		}
	}
	return false
}

// AllValid reports whether every value in o is present in valid.
func (o OnValues) AllValid(valid map[string]bool) bool {
	for _, v := range o {
		if !valid[v] {
			return false
		}
	}
	return true
}

type Hook struct {
	ID         string        `yaml:"id" json:"id"`
	On         OnValues      `yaml:"on" json:"on"`
	Traits     HandlerTraits `yaml:"traits" json:"traits"`
	Requires   []string      `yaml:"requires" json:"requires"`
	Consumer   string        `yaml:"consumer,omitempty" json:"consumer,omitempty"`
	Kit        string        `yaml:"-" json:"kit,omitempty"`
	ScriptPath string        `yaml:"-" json:"-"`
}

type Gate struct {
	ID         string        `yaml:"id" json:"id"`
	On         OnValues      `yaml:"on" json:"on"`
	Traits     HandlerTraits `yaml:"traits" json:"traits"`
	Kit        string        `yaml:"-" json:"kit,omitempty"`
	ScriptPath string        `yaml:"-" json:"-"`
}

type HookFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Hook      Hook
}

type GateFireEvent struct {
	EventID         string
	TaskID          string
	ProjectID       string
	Gate            Gate
	TaskPayloadJSON string // hook-updated payload to override DB value; empty = use DB
}

// ScriptTrigger defines events that auto-fire a script.
type ScriptTrigger string

const (
	ScriptTriggerTaskDone    ScriptTrigger = "task_done"
	ScriptTriggerTaskAborted ScriptTrigger = "task_aborted"
)

// ScriptFilter narrows which events match a script trigger.
type ScriptFilter struct {
	Behavior string `yaml:"behavior,omitempty" json:"behavior,omitempty"`
}

// Script defines a kit-bundled script that runs as an ephemeral task.
type Script struct {
	ID          string          `yaml:"id" json:"id"`
	Description string          `yaml:"description,omitempty" json:"description,omitempty"`
	On          []ScriptTrigger `yaml:"on,omitempty" json:"on,omitempty"`
	Filter      ScriptFilter    `yaml:"filter,omitempty" json:"filter,omitempty"`
	Kit         string          `yaml:"-" json:"kit,omitempty"`
	ScriptPath  string          `yaml:"-" json:"-"`
}

type KitHooksInfo struct {
	HooksDir string
	HookIDs  []string
	Consumer string
}

type KitGatesInfo struct {
	GatesDir string
	GateIDs  []string
}

type RawPayload json.RawMessage

func (p *RawPayload) UnmarshalYAML(node *yaml.Node) error {
	var v any
	if err := node.Decode(&v); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*p = RawPayload(b)
	return nil
}

func (p RawPayload) RawMessage() json.RawMessage {
	return json.RawMessage(p)
}

type TaskBehavior struct {
	Name           string     `yaml:"name" json:"name"`
	Transition     string     `yaml:"transition" json:"transition"`
	Traits         []string   `yaml:"traits" json:"traits"`
	Readonly       bool       `yaml:"readonly" json:"readonly,omitempty"`
	Worktree       bool       `yaml:"worktree" json:"worktree,omitempty"`
	BranchPrefix   string     `yaml:"branch_prefix" json:"branch_prefix,omitempty"`
	BaseBranch     string     `yaml:"base_branch" json:"base_branch,omitempty"`
	DefaultPayload RawPayload `yaml:"default_payload" json:"default_payload,omitempty"`
}

type KitRef struct {
	Ref   string `yaml:"ref" json:"ref"`
	Alias string `yaml:"as,omitempty" json:"as,omitempty"`
}

func (k *KitRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		k.Ref = value.Value
		return nil
	}
	type kitRefAlias KitRef
	return value.Decode((*kitRefAlias)(k))
}

type ProjectMeta struct {
	ID                 string                  `yaml:"id" json:"id"`
	Name               string                  `yaml:"name" json:"name"`
	Kits               []KitRef                `yaml:"kits" json:"kits,omitempty"`
	TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	Hooks              []Hook                  `yaml:"hooks" json:"hooks"`
	Gates              []Gate                  `yaml:"gates" json:"gates"`
	BuiltinCommands    []string                `yaml:"builtin_commands" json:"builtin_commands,omitempty"`
	HostCommands       HostCommands            `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string       `yaml:"env" json:"env"`
	SecretNamespace    string                  `yaml:"secret_namespace,omitempty" json:"secret_namespace,omitempty"`
	Scripts            []Script                `yaml:"-" json:"scripts,omitempty"`
	KitHooksDirs       []KitHooksInfo          `yaml:"-" json:"-"`
	KitGatesDirs       []KitGatesInfo          `yaml:"-" json:"-"`
}

type ProjectLocalMeta struct {
	Version            int                   `yaml:"version"`
	Kits               ProjectLocalKits      `yaml:"kits,omitempty"`
	BuiltinCommands    []string          `yaml:"builtin_commands,omitempty"`
	HostCommands       HostCommands      `yaml:"host_commands,omitempty"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings,omitempty"`
	Env                map[string]string `yaml:"env,omitempty"`
	SecretNamespace    string            `yaml:"secret_namespace,omitempty"`
}

type ProjectLocalKits struct {
	Add    []string `yaml:"add,omitempty"`
	Remove []string `yaml:"remove,omitempty"`
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
	Scripts            []Script                `yaml:"scripts"`
	BuiltinCommands    []string                `yaml:"builtin_commands"`
	HostCommands       HostCommands            `yaml:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings"`
	Env                map[string]string       `yaml:"env"`
	HooksDir           string                  `yaml:"-"`
	GatesDir           string                  `yaml:"-"`
	ScriptsDir         string                  `yaml:"-"`
}
