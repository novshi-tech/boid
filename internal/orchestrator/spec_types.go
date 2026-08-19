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

// Behavior names carry no built-in meaning to the loader: every key under
// task_behaviors is an ordinary project-chosen name, compared verbatim.
//
// There used to be an alias table here mapping the pre-rename names
// "plan" / "dev" onto "supervisor" / "executor", plus the mirror-entry
// machinery that kept both spellings reachable at runtime. It was removed
// because it reserved two of the most natural work-content behavior names:
// a project.yaml could not define "plan" as its own behavior at all — written
// alongside "supervisor" it was rejected as a duplicate definition, and
// written alone it was silently renamed to "supervisor". That directly
// contradicts the Track A2 free-naming contract the rest of this file
// implements. Nothing else replaced it: the daemon does not translate
// behavior names, in either direction.
//
// The only names the daemon still reacts to by spelling are "supervisor" and
// "executor", and only to emit deprecation warnings (see
// emitCanonicalBehaviorDeprecation) and to preserve executor's historical
// readonly=false default (see applyCanonicalBehaviorOverrides). Both are
// migration aids, not lookups.

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
	// time. Env, HostCommands, and AdditionalBindings are runtime-overlay
	// fields populated by ReadProjectMetaWithKits after merging kit data and
	// project-level overlays. These are not serialized to YAML.
	Hooks              []Hook            `yaml:"hooks,omitempty" json:"-"`
	Env                map[string]string `yaml:"-" json:"-"`
	HostCommands       HostCommands      `yaml:"-" json:"-"`
	AdditionalBindings []BindMount       `yaml:"-" json:"-"`
}

// SessionBehavior declares the default harness_type/model a project wants
// for a named session use-case (e.g. "shape" — see WebHandler.
// PostStartShapingSession in internal/api/web.go). Sessions are not driven
// by the task state machine's ResolveBehavior, so they cannot reuse
// TaskBehaviors' default_instruction.model — this is a deliberately
// separate, session-only free-naming dictionary (ProjectMeta.
// SessionBehaviors), not a reuse of task_behaviors' key space. Intentionally
// a minimal struct (no Traits/Hooks/agent-message concept): sessions have no
// routing or dispatch semantics to configure, only which harness/model to
// launch with.
type SessionBehavior struct {
	HarnessType string `yaml:"harness_type,omitempty" json:"harness_type,omitempty"`
	Model       string `yaml:"model,omitempty" json:"model,omitempty"`
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

// Trigger is one project.yaml `triggers[]` entry (docs/plans/
// ingestion-identity.md PR-4 / B-5, J-1/J-2). It declares WHEN a command
// runs (Every) and WHAT command runs (Run) — nothing else. daemon reads
// only these three fields; it never interprets Run's contents (J-2, this
// PR's own 不変条件: daemon は run の中身を知らない).
type Trigger struct {
	// Name identifies this trigger within its project (single-flight and
	// trigger_runs history are scoped by (project_id, trigger_name) — 12
	// 節 B-5 "single-flight の粒度は trigger 単位"). Must be non-empty and
	// unique within a project's Triggers slice — see ValidateTriggers.
	Name string `yaml:"name" json:"name"`
	// Every is a Go time.ParseDuration string (e.g. "10m", "1h") — the
	// minimum wall-clock gap between two starts of this trigger. There is
	// deliberately no time-of-day window (12 節 B-5 既定案「トリガの時間帯窓
	// はスクリプトが時刻で自制する」— J-4's「daemon はドメインに依存しない」).
	Every string `yaml:"every" json:"every"`
	// Run is a command string passed to `sh -c` inside the project's
	// sandbox — NOT a script path daemon resolves (J-2: 撤廃済みの script
	// hook 外部参照と同じ轍を踏まない). sandbox の /bin/sh は dash: bashism は
	// `bash scripts/x.sh` と明示する側 (スクリプト作者) の責任で、daemon の
	// 責任ではない。
	Run string `yaml:"run" json:"run"`
}

type ProjectMeta struct {
	ID            string                  `yaml:"id" json:"id"`
	Name          string                  `yaml:"name" json:"name"`
	TaskBehaviors map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	// Triggers is a TOP-LEVEL project.yaml field (J-1) — deliberately NOT
	// nested under any TaskBehavior. 「いつ始まるか」は task_behaviors の
	// 関心 (「どう実行するか」) とは異なる性質であり、混ぜると器の概念的な
	// 強度が落ちる (6 節「なぜ task_behaviors に入れないか」)。omitempty:
	// triggers を持たない project.yaml (大多数) は export/apply の往復で
	// `triggers: []` のノイズを出さない。
	//
	// workspace envelope の spec.* allowlist (workspace_envelope.go の
	// workspaceEnvelopeSpecFields) には**意図的に載せていない** — 理由は
	// このフィールドと同じ PR の報告を参照 (workspace-level のデフォルト
	// `run:` は特定 1 project の tracked tree にしか存在しないスクリプト
	// パスを指すことになり、workspace 内の複数 project へ一般化できる
	// task_behaviors/base_branch/fork_point とは性質が違う)。`triggers:`
	// を書けるのは project.yaml だけであり、workspace_envelope.go の
	// decodeStrictNode は今後も unknown field として拒否し続ける。
	Triggers []Trigger `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	// SessionBehaviors is a free-naming dictionary (same "any key name"
	// model as TaskBehaviors) from a use-case key (e.g. "shape") to a
	// default harness_type/model for sessions launched for that use case.
	// Sessions do NOT resolve through TaskBehaviors/ResolveBehavior — see
	// SessionBehavior's doc comment and buildShapingInstruction's doc
	// comment in internal/api/web.go for the rejected-alternatives history
	// this deliberately avoids repeating.
	SessionBehaviors map[string]SessionBehavior `yaml:"session_behaviors,omitempty" json:"session_behaviors,omitempty"`
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
	// NameSource records how Name was determined for a project.yaml-less
	// (URL-derived id, PR5) registration — "explicit" (--name given),
	// "url" (derived from the git URL), "cached" (recovered from a
	// previously-cached Name on reload), or "basename" (recovered from the
	// bare-repo WorkDir's directory name on reload). Runtime-only
	// (yaml:"-"): never read from project.yaml — a project.yaml-bearing
	// project always leaves this empty, since Name there has an
	// unambiguous single source. Surfaced by `project show --explain`
	// (docs/plans/workspace-default-project.md 論点e, PR6) as a carry-over
	// from PR5's completion report.
	NameSource string `yaml:"-" json:"name_source,omitempty"`
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
	// Status / StatusMessage reflect the daemon's in-memory ProjectStore
	// view of this project's health (docs/plans/volume-only-daemon.md
	// §論点a: StatusReady / StatusDegraded). Not a DB column — populated at
	// hydrate time (internal/api.ProjectAppService.hydrateProject) from
	// ProjectStore.Status. Omitted from JSON when "ready" (the common case)
	// to keep existing API response bodies unchanged for clients that
	// don't care.
	Status        string `json:"status,omitempty"`
	StatusMessage string `json:"status_message,omitempty"`
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

