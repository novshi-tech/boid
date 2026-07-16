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

// UnmarshalYAML accepts two equivalent forms so handwritten and
// generated kit.yaml can use whichever is more convenient:
//
//	additional_bindings:
//	  - /host/path              # short form: equivalent to {source: "/host/path"}
//	  - source: /host/path      # struct form: required when mode/target/is_file/etc. are set
//	    mode: rw
//
// Without this, yaml.v3 rejects the short form with
// "cannot unmarshal !!str into orchestrator.BindMount" and the single
// kit's parse error cascades into project meta hydration falling back to
// raw meta, silently dropping host_commands from *unrelated* kits.
func (b *BindMount) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		b.Source = node.Value
		return nil
	}
	type bindMountAlias BindMount
	var aux bindMountAlias
	if err := node.Decode(&aux); err != nil {
		return err
	}
	*b = BindMount(aux)
	return nil
}

// RejectRule declares a pattern that rejects an invocation with a
// human-readable reason. Match is a glob over the joined args string with the
// same semantics as allow/deny patterns (globMatch in internal/sandbox/policy.go).
// Reason is surfaced to the agent so it can self-correct. This type is
// vocabulary/transport only for now; enforcement is wired up separately.
type RejectRule struct {
	Match  string `yaml:"match" json:"match"`
	Reason string `yaml:"reason" json:"reason"`
}

// CommandDef is the orchestrator-side transport shape for sandbox command policy input.
// Dispatcher and sandbox mirror this shape; sandbox owns the enforcement semantics.
type CommandDef struct {
	Name               string            `json:"name,omitempty"`
	Path               string            `json:"path,omitempty"`
	AllowedPatterns    []string          `json:"allowed_patterns,omitempty"`
	DeniedPatterns     []string          `json:"denied_patterns,omitempty"`
	AllowedSubcommands []string          `json:"allowed_subcommands,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	RejectRules        []RejectRule      `json:"reject_rules,omitempty"`
}

// HostCommandSpec is the simplified YAML DSL for declaring host commands.
type HostCommandSpec struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
	// Stdin is deprecated: it is still parsed for backward compatibility but
	// will be ignored in a future release (loading a spec with stdin: true
	// emits a deprecation warning).
	Stdin  bool              `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	Path   string            `yaml:"path,omitempty" json:"path,omitempty"`
	Env    map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Reject []RejectRule      `yaml:"reject,omitempty" json:"reject,omitempty"`
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
		Env:                s.Env,
		RejectRules:        s.Reject,
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

type Instruction struct {
	Agent   string `json:"agent" yaml:"agent"`
	Name    string `json:"name,omitempty" yaml:"name,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	Model   string `json:"model,omitempty" yaml:"model,omitempty"`
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
	Role    string `json:"role"`
	Agent   string `json:"agent"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
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
)

type Hook struct {
	ID         string        `yaml:"id" json:"id"`
	Name       string        `yaml:"name,omitempty" json:"name,omitempty"`
	Kind       HandlerKind   `yaml:"kind,omitempty" json:"kind,omitempty"`
	Traits     HandlerTraits `yaml:"traits" json:"traits"`
	Requires   []string      `yaml:"requires" json:"requires"`
	Agent      string        `yaml:"agent,omitempty" json:"agent,omitempty"`
	Kit        string        `yaml:"-" json:"kit,omitempty"`
	// Command is an inline shell command, run via `sh -c`
	// (docs/plans/script-hook-removal.md). Mutually exclusive with Agent, and
	// not allowed on agent-kind hooks. See DispatchPlanner.PlanHook for the
	// argv selection and validateHookCommandFields for the exclusivity rules.
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
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

type HookFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Hook      Hook
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
	// Readonly controls whether the sandbox working directory is mounted read-only
	// for tasks using this behavior. When nil (unset), the daemon applies a
	// fail-safe default: readonly=true for all behaviors except the canonical
	// "executor" (which retains readonly=false during the compat period).
	// Set explicitly to override: readonly: false in project.yaml.
	Readonly           *bool        `yaml:"readonly,omitempty" json:"readonly,omitempty"`
	Traits             []string     `yaml:"traits" json:"traits"`
	DefaultInstruction *Instruction `yaml:"default_instruction,omitempty" json:"default_instruction,omitempty"`

	// Hooks is parsed from project.yaml task_behaviors.<name>.hooks at load
	// time. Env, HostCommands, AdditionalBindings, and KitRoots are
	// runtime-overlay fields populated by ReadProjectMetaWithKits after merging
	// kit data and project-level overlays. These are not serialized to YAML.
	Hooks              []Hook            `yaml:"hooks,omitempty" json:"-"`
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
	Ref string `yaml:"ref" json:"ref"`
}

func (k *KitRef) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		k.Ref = value.Value
		return nil
	}
	type kitRefAlias KitRef
	return value.Decode((*kitRefAlias)(k))
}

// DockerCapability is the opt-in marker for the native docker proxy.
// Presence (non-nil pointer in Capabilities) enables the proxy; the empty
// struct is a placeholder for future per-project policy fields.
type DockerCapability struct{}

// Capabilities declares optional sandbox capabilities declared in project.yaml.
type Capabilities struct {
	// Docker, when non-nil, enables the per-sandbox native docker proxy.
	// Declared as capabilities.docker: {} in project.yaml.
	Docker *DockerCapability `yaml:"docker,omitempty" json:"docker,omitempty"`
}

type ProjectMeta struct {
	ID            string                  `yaml:"id" json:"id"`
	Name          string                  `yaml:"name" json:"name"`
	TaskBehaviors map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	// BaseBranch is the default git base branch for worktrees created by
	// tasks in this project. It is resolved at task creation time (with
	// ${TASK_REMOTE_ID} / ${current_branch} expansion) and persisted on
	// each task row.
	BaseBranch string `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	// ForkPoint is the git ref used as the start point when creating a
	// base branch that does not yet exist (ClassifyBaseBranch case 3).
	// Accepts any ref that `git rev-parse --verify` resolves (e.g. "main",
	// "origin/main", a tag, or a commit SHA). When empty, the dispatcher
	// falls back to "refs/remotes/origin/HEAD"; if that is also unset the
	// case-3 worktree creation fails. The project root's working-tree HEAD
	// is intentionally never consulted, since it can drift to an
	// unexpected branch between task creation and dispatch.
	ForkPoint          string            `yaml:"fork_point,omitempty" json:"fork_point,omitempty"`
	HostCommands       HostCommands      `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []BindMount       `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string `yaml:"env" json:"env"`
	// SecretNamespace is a runtime-only field injected at hydration time from the
	// linked workspace ID. It is intentionally not read from project.yaml (yaml:"-").
	SecretNamespace string `yaml:"-" json:"secret_namespace,omitempty"`
	// Capabilities declares optional sandbox capabilities. This is a runtime-only
	// field injected from workspace.yaml at hydration time (yaml:"-").
	Capabilities Capabilities `yaml:"-" json:"capabilities,omitempty"`
	// DefaultTaskBehavior names the behavior to use when a CreateTaskRequest
	// omits both behavior and behavior_spec. When empty, the daemon falls back
	// to "supervisor" if that behavior exists (with a deprecation warning);
	// if neither is set, CreateTask returns an error.
	DefaultTaskBehavior string `yaml:"default_task_behavior,omitempty" json:"default_task_behavior,omitempty"`
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
	// UpstreamURL is the project's git remote origin, captured and normalized
	// to HTTPS (SSH → HTTPS) at `project add` / `project reload` time (see
	// docs/plans/git-gateway-cutover.md PR2). Empty until captured; daemon
	// startup backfills it for projects registered before this field existed.
	// Not read from project.yaml — this is DB-only, machine-local state.
	UpstreamURL string    `json:"upstream_url,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type WorkspaceSummary struct {
	ID           string `json:"id"`
	ProjectCount int    `json:"project_count"`
	// Revision is an opaque ETag-like token derived from the workspaces row's
	// updated_at column (RFC3339), used by the PUT /api/workspaces/{slug}
	// If-Match optimistic-concurrency check (docs/plans/
	// workspace-db-consolidation.md decision 17). Empty when the summary was
	// built from a project_workspaces reference with no corresponding
	// workspaces row (should not happen once PR4's ListWorkspaces query is
	// workspaces-table-based, but callers should not assume non-empty).
	Revision string `json:"revision,omitempty"`
}

