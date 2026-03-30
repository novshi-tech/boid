package kit

import (
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
)

// MergeKits merges kit configurations into a base ProjectMeta.
// Kits are applied in order. Project values take precedence.
// Returns a new ProjectMeta; the input is not mutated.
func MergeKits(base *model.ProjectMeta, kits []*KitMeta) *model.ProjectMeta {
	if len(kits) == 0 {
		return base
	}

	result := *base

	// Env: layer kits in order, then project overrides
	mergedEnv := make(map[string]string)
	for _, m := range kits {
		for k, v := range m.Env {
			mergedEnv[k] = v
		}
	}
	for k, v := range base.Env {
		mergedEnv[k] = v
	}
	result.Env = mergedEnv

	// TaskBehaviors: layer kits in order, then project overrides
	mergedBehaviors := make(map[string]model.TaskBehavior)
	for _, m := range kits {
		for k, v := range m.TaskBehaviors {
			mergedBehaviors[k] = v
		}
	}
	for k, v := range base.TaskBehaviors {
		mergedBehaviors[k] = v
	}
	result.TaskBehaviors = mergedBehaviors

	// Hooks: kit hooks first, then project hooks. Dedup by ID (last wins).
	var allHooks []model.Hook
	for _, m := range kits {
		allHooks = append(allHooks, m.Hooks...)
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = dedupHooks(allHooks)

	// HostCommands: layer kits in order, then project overrides
	mergedCmds := make(map[string]hostcmd.CommandDef)
	for _, m := range kits {
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

	// List fields: union by source path
	result.AdditionalBindings = unionBindMounts(kits, base.AdditionalBindings)

	// Collect KitHooksDirs
	for _, m := range kits {
		if m.HooksDir == "" || len(m.Hooks) == 0 {
			continue
		}
		ids := make([]string, len(m.Hooks))
		for i, h := range m.Hooks {
			ids[i] = h.ID
		}
		result.KitHooksDirs = append(result.KitHooksDirs, model.KitHooksInfo{
			HooksDir: m.HooksDir,
			HookIDs:  ids,
		})
	}

	return &result
}

// dedupHooks keeps the last hook for each ID (project hooks come last, so they win).
func dedupHooks(hooks []model.Hook) []model.Hook {
	seen := make(map[string]int) // ID -> index in result
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

func unionBindMounts(kits []*KitMeta, base []model.BindMount) []model.BindMount {
	seen := make(map[string]bool)
	var result []model.BindMount
	for _, m := range kits {
		for _, b := range m.AdditionalBindings {
			if !seen[b.Source] {
				seen[b.Source] = true
				result = append(result, b)
			}
		}
	}
	for _, b := range base {
		if !seen[b.Source] {
			seen[b.Source] = true
			result = append(result, b)
		}
	}
	return result
}
