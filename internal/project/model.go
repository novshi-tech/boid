package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BindMount describes a host path to bind-mount into the sandbox.
type BindMount struct {
	Source string `yaml:"source" json:"source"`
	Mode   string `yaml:"mode" json:"mode"`
}

// CommandDef defines a host command that can be executed inside the sandbox
// via the hostcmd broker.
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

func TraitMergeMode(t TraitType) MergeMode {
	switch t {
	case TraitVerification:
		return MergeModeShared
	default:
		return MergeModeExclusive
	}
}

func ActiveTraitTypes(raw json.RawMessage) ([]TraitType, error) {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	var traits []TraitType
	for k, v := range m {
		if string(v) != "null" {
			traits = append(traits, TraitType(k))
		}
	}
	return traits, nil
}

func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []TraitType) error {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		return nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(patch, &m); err != nil {
		return fmt.Errorf("unmarshal patch: %w", err)
	}

	allowed := make(map[TraitType]bool, len(allowedTraits))
	for _, t := range allowedTraits {
		allowed[t] = true
	}

	for k := range m {
		if !allowed[TraitType(k)] {
			return fmt.Errorf("trait %q not in requires_traits", k)
		}
	}
	return nil
}

func MergePayloadPatch(base, patch json.RawMessage, hookID string, allowedTraits []TraitType) (json.RawMessage, error) {
	if len(patch) == 0 || string(patch) == "{}" || string(patch) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}

	if allowedTraits != nil {
		if err := ValidatePayloadPatch(patch, allowedTraits); err != nil {
			return nil, err
		}
	}

	var baseMap map[string]json.RawMessage
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		baseMap = make(map[string]json.RawMessage)
	} else {
		if err := json.Unmarshal(base, &baseMap); err != nil {
			return nil, fmt.Errorf("unmarshal base: %w", err)
		}
	}

	var patchMap map[string]json.RawMessage
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return nil, fmt.Errorf("unmarshal patch: %w", err)
	}

	for k, v := range patchMap {
		trait := TraitType(k)
		switch TraitMergeMode(trait) {
		case MergeModeShared:
			var sub map[string]json.RawMessage
			if existing, ok := baseMap[k]; ok && string(existing) != "null" {
				if err := json.Unmarshal(existing, &sub); err != nil {
					sub = make(map[string]json.RawMessage)
				}
			} else {
				sub = make(map[string]json.RawMessage)
			}
			sub[hookID] = v
			merged, err := json.Marshal(sub)
			if err != nil {
				return nil, fmt.Errorf("marshal shared trait %q: %w", k, err)
			}
			baseMap[k] = merged
		default:
			baseMap[k] = v
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}

func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	if len(update) == 0 || string(update) == "{}" || string(update) == "null" {
		if len(base) == 0 {
			return json.RawMessage("{}"), nil
		}
		return base, nil
	}
	if len(base) == 0 || string(base) == "{}" || string(base) == "null" {
		return update, nil
	}

	var baseMap map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseMap); err != nil {
		return nil, fmt.Errorf("unmarshal base: %w", err)
	}

	var updateMap map[string]json.RawMessage
	if err := json.Unmarshal(update, &updateMap); err != nil {
		return nil, fmt.Errorf("unmarshal update: %w", err)
	}

	for k, v := range updateMap {
		if string(v) != "null" {
			baseMap[k] = v
		}
	}

	result, err := json.Marshal(baseMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged: %w", err)
	}
	return result, nil
}

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

var ValidHookOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

var ValidGateOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

func ResolveHookScript(hooksDir, hookID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		p := filepath.Join(hooksDir, hookID+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("script not found: %s.(sh|py)", hookID)
}

func ResolveGateScript(gatesDir, gateID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		p := filepath.Join(gatesDir, gateID+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("gate script not found: %s.(sh|py)", gateID)
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
	WorkspaceID        string                  `yaml:"workspace_id" json:"workspace_id"`
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

type Project struct {
	ID        string      `json:"id"`
	WorkDir   string      `json:"work_dir"`
	Meta      ProjectMeta `json:"meta"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}
