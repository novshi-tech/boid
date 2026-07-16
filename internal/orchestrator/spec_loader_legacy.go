package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LegacyProjectMeta holds the raw contents of a pre-migration project.yaml,
// including fields that have been moved to workspace.yaml and kit.yaml in the
// new schema. This type is used only by the migrate command; normal loading
// goes through ReadProjectMeta.
type LegacyProjectMeta struct {
	ID                  string                         `yaml:"id"`
	Name                string                         `yaml:"name"`
	Kits                []KitRef                       `yaml:"kits,omitempty"`
	TaskBehaviors       map[string]LegacyTaskBehavior  `yaml:"task_behaviors,omitempty"`
	BaseBranch          string                         `yaml:"base_branch,omitempty"`
	ForkPoint           string                         `yaml:"fork_point,omitempty"`
	HostCommands        HostCommands                   `yaml:"host_commands,omitempty"`
	AdditionalBindings  []BindMount                    `yaml:"additional_bindings,omitempty"`
	Env                 map[string]string              `yaml:"env,omitempty"`
	SecretNamespace     string                         `yaml:"secret_namespace,omitempty"`
	Capabilities        Capabilities                   `yaml:"capabilities,omitempty"`
	DefaultTaskBehavior string                         `yaml:"default_task_behavior,omitempty"`
}

// LegacyTaskBehavior holds a task behavior including the legacy kits field
// that appears at the behavior level in old project.yaml files.
type LegacyTaskBehavior struct {
	Readonly           *bool        `yaml:"readonly,omitempty"`
	Traits             []string     `yaml:"traits,omitempty"`
	DefaultInstruction *Instruction `yaml:"default_instruction,omitempty"`
	Kits               []KitRef     `yaml:"kits,omitempty"`
}

// ReadProjectMetaLegacy reads .boid/project.yaml (and .boid/project.local.yaml
// if present) using a raw-map first pass that does NOT reject unknown or removed
// fields. This is intentionally separate from ReadProjectMeta so that the migrate
// command can load old-schema files that would otherwise fail validation.
//
// The returned LegacyProjectMeta captures all fields that are subject to
// migration, including kits/env/host_commands/additional_bindings/secret_namespace/
// capabilities at both the project level and the task_behaviors level.
func ReadProjectMetaLegacy(dir string) (*LegacyProjectMeta, error) {
	projectYAML := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(projectYAML)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var meta LegacyProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}

	// Merge project.local.yaml on top if it exists.
	localYAML := filepath.Join(dir, ".boid", projectLocalFilename)
	localData, err := os.ReadFile(localYAML)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", projectLocalFilename, err)
	}
	if err == nil {
		var local LegacyProjectMeta
		if err := yaml.Unmarshal(localData, &local); err != nil {
			return nil, fmt.Errorf("parse %s: %w", projectLocalFilename, err)
		}
		// Merge: local overrides project.yaml for env / host_commands /
		// additional_bindings / secret_namespace (same as current merge logic).
		if local.SecretNamespace != "" {
			meta.SecretNamespace = local.SecretNamespace
		}
		for k, v := range local.Env {
			if meta.Env == nil {
				meta.Env = make(map[string]string)
			}
			meta.Env[k] = v
		}
		for k, v := range local.HostCommands {
			if meta.HostCommands == nil {
				meta.HostCommands = make(HostCommands)
			}
			meta.HostCommands[k] = v
		}
		meta.AdditionalBindings = append(meta.AdditionalBindings, local.AdditionalBindings...)
	}

	return &meta, nil
}

// HasLegacyFields reports whether the LegacyProjectMeta contains any fields
// that should be migrated to workspace.yaml or a legacy kit. This is used by
// the migrate command to detect whether migration is needed at all.
func (m *LegacyProjectMeta) HasLegacyFields() bool {
	if len(m.Kits) > 0 {
		return true
	}
	if len(m.Env) > 0 {
		return true
	}
	if len(m.HostCommands) > 0 {
		return true
	}
	if len(m.AdditionalBindings) > 0 {
		return true
	}
	if m.SecretNamespace != "" {
		return true
	}
	if m.Capabilities.Docker != nil {
		return true
	}
	for _, b := range m.TaskBehaviors {
		if len(b.Kits) > 0 {
			return true
		}
	}
	return false
}

// CollectAllKitRefs returns a deduplicated, ordered list of all kit refs
// referenced at the project level and across all task behaviors.
func (m *LegacyProjectMeta) CollectAllKitRefs() []KitRef {
	seen := make(map[string]bool)
	var result []KitRef
	add := func(ref KitRef) {
		if !seen[ref.Ref] {
			seen[ref.Ref] = true
			result = append(result, ref)
		}
	}
	for _, r := range m.Kits {
		add(r)
	}
	for _, b := range m.TaskBehaviors {
		for _, r := range b.Kits {
			add(r)
		}
	}
	return result
}
