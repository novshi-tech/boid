package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// BindMount is a plain shared DTO across orchestration and sandbox planning.
// It carries mount source/target/mode data and does not encode provider behavior.
type BindMount struct {
	Source   string `yaml:"source" json:"source"`
	Target   string `yaml:"target,omitempty" json:"target,omitempty"` // if empty, defaults to Source
	Mode     string `yaml:"mode" json:"mode"`                         // "rw" | "" (ro default)
	IsFile   bool   `yaml:"is_file,omitempty" json:"is_file,omitempty"`
	Optional bool   `yaml:"optional,omitempty" json:"optional,omitempty"` // if true, skip mount when Source does not exist on the host
}

// HookFile describes a single hook file to bind-mount into the sandbox.
type HookFile struct {
	Source     string // host-side absolute path
	TargetName string // filename inside sandbox .boid/hooks/
}

// CommandDef is the orchestrator-side transport shape for sandbox command policy input.
// Dispatcher and sandbox mirror this shape; sandbox owns the enforcement semantics.
type CommandDef struct {
	Name               string            `json:"name,omitempty"`
	Path               string            `json:"path,omitempty"`
	AllowedPatterns    []string          `json:"allowed_patterns,omitempty"`
	DeniedPatterns     []string          `json:"denied_patterns,omitempty"`
	AllowedSubcommands []string          `json:"allowed_subcommands,omitempty"`
	AllowStdin         bool              `json:"allow_stdin,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
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
	InstructionTypeExecution InstructionType = "execution"
)

type Instruction struct {
	Type    InstructionType `json:"type" yaml:"type"`
	Agent   string          `json:"agent" yaml:"agent"`
	Name    string          `json:"name,omitempty" yaml:"name,omitempty"`
	Message string          `json:"message,omitempty" yaml:"message,omitempty"`
	Model   string          `json:"model,omitempty" yaml:"model,omitempty"`
}

// Instructions is the persisted instruction history for a task. The most
// recent entry is the "active" instruction passed to the agent on dispatch;
// earlier entries are kept as history (e.g. for reopen tracking).
//
// JSON shape on the wire is an array. For backward compatibility, the legacy
// single-instruction map form ({"main": {...}}) is also accepted on
// unmarshal and converted to a single-element array.
type Instructions []Instruction

// Active returns the currently-active instruction (the last entry), or nil if
// the list is empty.
func (is Instructions) Active() *Instruction {
	if len(is) == 0 {
		return nil
	}
	return &is[len(is)-1]
}

// UnmarshalJSON accepts both the new array form and the legacy
// {"main": {...}, "verify": {...}} map form. For the map form, only the
// "main" entry is preserved (verifying/reworking variants were removed).
func (is *Instructions) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*is = nil
		return nil
	}
	// Try array first.
	if data[0] == '[' {
		var arr []Instruction
		if err := json.Unmarshal(data, &arr); err != nil {
			return err
		}
		*is = arr
		return nil
	}
	// Legacy map: {"main": {...}, ...}
	var m map[string]Instruction
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("instructions: expected array or legacy map: %w", err)
	}
	if main, ok := m["main"]; ok {
		*is = Instructions{main}
		return nil
	}
	// Fallback: take any single entry (deterministic by sorted keys is unnecessary here).
	for _, v := range m {
		*is = Instructions{v}
		return nil
	}
	*is = nil
	return nil
}

type RoutedInstruction struct {
	Role    string          `json:"role"`
	Type    InstructionType `json:"type"`
	Agent   string          `json:"agent"`
	Name    string          `json:"name,omitempty"`
	Message string          `json:"message"`
	Model   string          `json:"model,omitempty"`
}

type TraitType string

const (
	TraitArtifact     TraitType = "artifact"
	TraitVerification TraitType = "verification"
	TraitAwaiting     TraitType = "awaiting"
)

// HandlerKind distinguishes the role a hook plays.
// An empty kind means a generic hook (no instructions routing).
// Only agent-kind hooks participate in instructions routing.
type HandlerKind string

const (
	HandlerKindAgent HandlerKind = "agent"
)

// IsValid reports whether the kind value is recognized.
func (k HandlerKind) IsValid() bool {
	return k == "" || k == HandlerKindAgent
}

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

type Hook struct {
	ID         string        `yaml:"id" json:"id"`
	Name       string        `yaml:"name,omitempty" json:"name,omitempty"`
	Kind       HandlerKind   `yaml:"kind,omitempty" json:"kind,omitempty"`
	Traits     HandlerTraits `yaml:"traits" json:"traits"`
	Requires   []string      `yaml:"requires" json:"requires"`
	Agent      string        `yaml:"agent,omitempty" json:"agent,omitempty"`
	Kit        string        `yaml:"-" json:"kit,omitempty"`
	ScriptPath string        `yaml:"-" json:"-"`
}

// UnmarshalYAML rejects legacy `on:` entries to surface migration breakage clearly.
func (h *Hook) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == "on" {
				return fmt.Errorf("hook %q: 'on:' is no longer supported (hooks always run during executing state)", hookIDFromNode(node))
			}
		}
	}
	type hookAlias Hook
	var alias hookAlias
	if err := node.Decode(&alias); err != nil {
		return err
	}
	*h = Hook(alias)
	return nil
}

func hookIDFromNode(node *yaml.Node) string {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "id" {
			return node.Content[i+1].Value
		}
	}
	return "<unknown>"
}

// GatePhase determines when a gate fires relative to a state transition.
type GatePhase string

const (
	GatePhaseEntry GatePhase = "entry"
	GatePhaseExit  GatePhase = "exit"
)

type Gate struct {
	ID         string        `yaml:"id" json:"id"`
	Phase      GatePhase     `yaml:"phase,omitempty" json:"phase,omitempty"`
	Traits     HandlerTraits `yaml:"traits" json:"traits"`
	Kit        string        `yaml:"-" json:"kit,omitempty"`
	ScriptPath string        `yaml:"-" json:"-"`
}

// UnmarshalYAML defaults Phase to GatePhaseExit when omitted.
// Rejects `kind:` because gates cannot participate in instructions routing
// (project directory is not mounted, so no agent can do meaningful work).
// Rejects `on:` since gates are scoped to task entry/exit, not per-state.
func (g *Gate) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			switch node.Content[i].Value {
			case "kind":
				return fmt.Errorf("gate %q: 'kind' is not supported on gates", gateIDFromNode(node))
			case "on":
				return fmt.Errorf("gate %q: 'on:' is no longer supported (gates run on task entry/exit, set 'phase: entry|exit' instead)", gateIDFromNode(node))
			}
		}
	}
	type gateAlias Gate
	var alias gateAlias
	if err := node.Decode(&alias); err != nil {
		return err
	}
	*g = Gate(alias)
	if g.Phase == "" {
		g.Phase = GatePhaseExit
	}
	return nil
}

// gateIDFromNode extracts the id value from a gate YAML mapping, if present.
// Used only for error messages.
func gateIDFromNode(node *yaml.Node) string {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "id" {
			return node.Content[i+1].Value
		}
	}
	return "<unknown>"
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

// UnmarshalJSON / MarshalJSON は json.RawMessage と同じ振る舞いを named type に
// 改めて再実装する。 named type はメソッドを継承しないため、 これが無いと
// encoding/json は underlying []byte 扱いで base64 文字列を要求してしまい、
// JSON object/array 形式の default_payload を弾いてしまう。
func (p *RawPayload) UnmarshalJSON(data []byte) error {
	if p == nil {
		return fmt.Errorf("orchestrator.RawPayload: UnmarshalJSON on nil pointer")
	}
	*p = append((*p)[0:0], data...)
	return nil
}

func (p RawPayload) MarshalJSON() ([]byte, error) {
	if len(p) == 0 {
		return []byte("null"), nil
	}
	return []byte(p), nil
}

func (p RawPayload) RawMessage() json.RawMessage {
	return json.RawMessage(p)
}

// BehaviorAliases maps legacy behavior names to their canonical counterparts.
// This is the alias table used during the "task_behavior simplification" rename:
// project.yaml files written before the rename keep using "plan" / "dev"; on
// load they are normalized to "supervisor" / "executor" and a deprecation
// warning is emitted. The map is intentionally not exported as mutable state —
// callers should go through CanonicalBehaviorName.
var BehaviorAliases = map[string]string{
	"plan": "supervisor",
	"dev":  "executor",
}

// CanonicalBehaviorName returns the canonical behavior name for the given
// (possibly aliased) name. If the input is a deprecated alias, the returned
// canonical name and isAlias=true are returned. Otherwise the input is
// returned unchanged with isAlias=false.
func CanonicalBehaviorName(name string) (canonical string, isAlias bool) {
	if c, ok := BehaviorAliases[name]; ok {
		return c, true
	}
	return name, false
}

// IsBehaviorAliasKey reports whether the given key is a deprecated alias key.
// Display code that needs to suppress mirror entries for the migration period
// (so user-facing output does not show the same behavior twice) can skip keys
// where IsBehaviorAliasKey returns true and the canonical counterpart is also
// present in the same map.
func IsBehaviorAliasKey(key string) bool {
	_, ok := BehaviorAliases[key]
	return ok
}

type TaskBehavior struct {
	Traits             []string               `yaml:"traits" json:"traits"`
	DefaultInstruction *Instruction           `yaml:"default_instruction,omitempty" json:"default_instruction,omitempty"`
	Kits               []KitRef               `yaml:"kits,omitempty" json:"kits,omitempty"`
	Commands           map[string]CommandSpec `yaml:"commands,omitempty" json:"commands,omitempty"`

	// Resolved fields populated by ReadProjectMetaWithKits after merging kit data
	// and project-level overlays. These are not serialized to YAML.
	Hooks              []Hook            `yaml:"-" json:"-"`
	Gates              []Gate            `yaml:"-" json:"-"`
	Env                map[string]string `yaml:"-" json:"-"`
	HostCommands       HostCommands      `yaml:"-" json:"-"`
	AdditionalBindings []BindMount       `yaml:"-" json:"-"`
	// KitRoots holds the deduplicated list of kit root directories to bind-mount
	// in the sandbox at their original host paths. Populated by MergeKitMetaIntoBehavior.
	KitRoots []string `yaml:"-" json:"-"`
}

// BehaviorSpec is an inline behavior specification that can be used instead of
// referencing a named behavior from project.yaml task_behaviors. This allows
// kits to self-describe the behavior they need without depending on project config.
type BehaviorSpec struct {
	Name               string       `yaml:"name" json:"name"`
	Traits             []string     `yaml:"traits,omitempty" json:"traits,omitempty"`
	DefaultInstruction *Instruction `yaml:"default_instruction,omitempty" json:"default_instruction,omitempty"`
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

// CommandSpec defines a named sandbox command available via `boid exec`.
// The Command slice is expanded with os.ExpandEnv at load time and stored in ResolvedCommand.
type CommandSpec struct {
	Command  []string `yaml:"command" json:"command"`
	Readonly bool     `yaml:"readonly,omitempty" json:"readonly,omitempty"`

	// Resolved fields populated by ReadProjectMetaWithKits.
	ResolvedCommand    []string          `yaml:"-" json:"-"`
	Env                map[string]string `yaml:"-" json:"-"`
	HostCommands       HostCommands      `yaml:"-" json:"-"`
	AdditionalBindings []BindMount       `yaml:"-" json:"-"`
}

type ProjectMeta struct {
	ID            string                  `yaml:"id" json:"id"`
	Name          string                  `yaml:"name" json:"name"`
	Kits          []KitRef                `yaml:"kits,omitempty" json:"kits,omitempty"`
	TaskBehaviors map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	Commands      map[string]CommandSpec  `yaml:"commands,omitempty" json:"commands,omitempty"`
	// Worktree controls whether tasks in this project run in a per-task git
	// worktree by default. For the canonical "executor" behavior the value
	// is used as-is; for "supervisor" the worktree decision is governed by
	// the base_branch state classification (see ClassifyBaseBranch). For
	// non-canonical behaviors, this is the worktree flag verbatim.
	Worktree bool `yaml:"worktree,omitempty" json:"worktree,omitempty"`
	// BaseBranch is the default git base branch for worktrees created by
	// tasks in this project. It is resolved at task creation time (with
	// ${TASK_REMOTE_ID} / ${current_branch} expansion) and persisted on
	// each task row.
	BaseBranch         string            `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	HostCommands       HostCommands      `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string `yaml:"env" json:"env"`
	SecretNamespace    string            `yaml:"secret_namespace,omitempty" json:"secret_namespace,omitempty"`
}

type ProjectLocalMeta struct {
	Version            int               `yaml:"version"`
	HostCommands       HostCommands      `yaml:"host_commands,omitempty"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings,omitempty"`
	Env                map[string]string `yaml:"env,omitempty"`
	SecretNamespace    string            `yaml:"secret_namespace,omitempty"`
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
	Commands           map[string]CommandSpec  `yaml:"commands,omitempty"`
	Hooks              []Hook                  `yaml:"hooks"`
	Gates              []Gate                  `yaml:"gates"`
	HostCommands       HostCommands            `yaml:"host_commands"`
	AdditionalBindings []BindMount             `yaml:"additional_bindings"`
	Env                map[string]string       `yaml:"env"`
	HooksDir           string                  `yaml:"-"`
	GatesDir           string                  `yaml:"-"`
	KitRoot            string                  `yaml:"-"` // directory containing kit.yaml

	// Init-time metadata — not merged into runtime spec by MergeKitMeta.
	Meta             *KitMetaInfo `yaml:"meta,omitempty"`
	Detect           *KitDetect   `yaml:"detect,omitempty"`
	Requires         *KitRequires `yaml:"requires,omitempty"`
	Scaffold         *KitScaffold `yaml:"scaffold,omitempty"`
	ProvidesAgent    string       `yaml:"provides_agent,omitempty"`
}

// KitMetaInfo holds human-readable metadata for a kit.
type KitMetaInfo struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Category    string `yaml:"category"` // language / vcs / ci / agent / workflow / utility
}

// KitDetect declares how to determine whether a kit is applicable to a
// project. The referenced script is executed with POSIX sh in the project
// directory; its first line of stdout — "required", "optional", or empty
// — indicates the detection outcome.
type KitDetect struct {
	// Script is a path (relative to the kit directory) to a POSIX sh
	// script. boid init runs it with sh(1) using projectDir as CWD and a
	// 5-second timeout. The first trimmed line of stdout is interpreted:
	//   "required" → kit is auto-selected
	//   "optional" → kit is shown as a candidate but not auto-selected
	//   other / empty / non-zero exit → not applicable
	Script string `yaml:"script"`
}

// KitRequires declares host commands that must be present in PATH.
type KitRequires struct {
	Commands []string `yaml:"commands"`
}

// KitScaffold declares scaffold templates bundled with this kit.
type KitScaffold struct {
	TaskBehaviors *ScaffoldTemplate `yaml:"task_behaviors,omitempty"`
}

// ScaffoldTemplate points to a template file relative to the kit directory.
type ScaffoldTemplate struct {
	Description string `yaml:"description"`
	Template    string `yaml:"template"`
}
