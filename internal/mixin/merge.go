package mixin

import (
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
)

// MergeMixins merges mixin configurations into a base ProjectMeta.
// Mixins are applied in order. Project values take precedence.
// Returns a new ProjectMeta; the input is not mutated.
func MergeMixins(base *model.ProjectMeta, mixins []*MixinMeta) *model.ProjectMeta {
	if len(mixins) == 0 {
		return base
	}

	result := *base

	// Env: layer mixins in order, then project overrides
	mergedEnv := make(map[string]string)
	for _, m := range mixins {
		for k, v := range m.Env {
			mergedEnv[k] = v
		}
	}
	for k, v := range base.Env {
		mergedEnv[k] = v
	}
	result.Env = mergedEnv

	// TaskBehaviors: layer mixins in order, then project overrides
	mergedBehaviors := make(map[string]model.TaskBehavior)
	for _, m := range mixins {
		for k, v := range m.TaskBehaviors {
			mergedBehaviors[k] = v
		}
	}
	for k, v := range base.TaskBehaviors {
		mergedBehaviors[k] = v
	}
	result.TaskBehaviors = mergedBehaviors

	// Hooks: mixin hooks first, then project hooks. Dedup by ID (last wins).
	var allHooks []model.Hook
	for _, m := range mixins {
		allHooks = append(allHooks, m.Hooks...)
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = dedupHooks(allHooks)

	// HostCommands: layer mixins in order, then project overrides
	mergedCmds := make(map[string]hostcmd.CommandDef)
	for _, m := range mixins {
		for k, v := range m.HostCommands {
			mergedCmds[k] = v
		}
	}
	for k, v := range base.HostCommands {
		mergedCmds[k] = v
	}
	if len(mergedCmds) > 0 {
		result.HostCommands = mergedCmds
	}

	// List fields: union
	result.AdditionalBindings = unionStrings(mixins, base.AdditionalBindings, func(m *MixinMeta) []string { return m.AdditionalBindings })
	result.AllowedDomains = unionStrings(mixins, base.AllowedDomains, func(m *MixinMeta) []string { return m.AllowedDomains })

	// Collect MixinHooksDirs
	for _, m := range mixins {
		if m.HooksDir == "" || len(m.Hooks) == 0 {
			continue
		}
		ids := make([]string, len(m.Hooks))
		for i, h := range m.Hooks {
			ids[i] = h.ID
		}
		result.MixinHooksDirs = append(result.MixinHooksDirs, model.MixinHooksInfo{
			HooksDir: m.HooksDir,
			HookIDs:  ids,
		})
	}

	return &result
}

// dedupHooks keeps the last hook for each ID (project hooks come last, so they win).
func dedupHooks(hooks []model.Hook) []model.Hook {
	seen := make(map[string]int) // ID → index in result
	var result []model.Hook
	for _, h := range hooks {
		if idx, ok := seen[h.ID]; ok {
			result[idx] = h // overwrite with later definition
		} else {
			seen[h.ID] = len(result)
			result = append(result, h)
		}
	}
	return result
}

func unionStrings(mixins []*MixinMeta, base []string, extract func(*MixinMeta) []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, m := range mixins {
		for _, s := range extract(m) {
			if !seen[s] {
				seen[s] = true
				result = append(result, s)
			}
		}
	}
	for _, s := range base {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}
