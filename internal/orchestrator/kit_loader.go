package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/project"
	"gopkg.in/yaml.v3"
)

// ReadKitMeta reads and validates kit.yaml from the given directory.
func ReadKitMeta(dir string) (*project.KitMeta, error) {
	yamlPath := filepath.Join(dir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read kit.yaml: %w", err)
	}

	var meta project.KitMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}

	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	hooksDir := filepath.Join(dir, "hooks")
	for i := range meta.Hooks {
		h := &meta.Hooks[i]
		if !project.ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := project.ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}
	if len(meta.Hooks) > 0 {
		meta.HooksDir = hooksDir
	}

	gatesDir := filepath.Join(dir, "gates")
	for i := range meta.Gates {
		g := &meta.Gates[i]
		if !project.ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := project.ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}
	if len(meta.Gates) > 0 {
		meta.GatesDir = gatesDir
	}

	return &meta, nil
}

// MergeKitMeta merges kit configurations into a base ProjectMeta.
func MergeKitMeta(base *project.ProjectMeta, kits []*project.KitMeta) *project.ProjectMeta {
	if len(kits) == 0 {
		return base
	}

	result := *base

	mergedEnv := make(map[string]string)
	for _, meta := range kits {
		for k, v := range meta.Env {
			mergedEnv[k] = v
		}
	}
	for k, v := range base.Env {
		mergedEnv[k] = v
	}
	result.Env = mergedEnv

	mergedBehaviors := make(map[string]project.TaskBehavior)
	for _, meta := range kits {
		for k, v := range meta.TaskBehaviors {
			mergedBehaviors[k] = v
		}
	}
	for k, v := range base.TaskBehaviors {
		mergedBehaviors[k] = v
	}
	result.TaskBehaviors = mergedBehaviors

	var allHooks []project.Hook
	for _, meta := range kits {
		allHooks = append(allHooks, meta.Hooks...)
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = dedupHooks(allHooks)

	var allGates []project.Gate
	for _, meta := range kits {
		allGates = append(allGates, meta.Gates...)
	}
	allGates = append(allGates, base.Gates...)
	result.Gates = dedupGates(allGates)

	mergedCmds := make(map[string]project.CommandDef)
	for _, meta := range kits {
		for k, v := range meta.HostCommands {
			mergedCmds[k] = v
		}
	}
	for k, v := range base.HostCommands {
		mergedCmds[k] = v
	}
	if len(mergedCmds) > 0 {
		result.HostCommands = mergedCmds
	}

	result.AdditionalBindings = unionBindMounts(kits, base.AdditionalBindings)

	for _, meta := range kits {
		if meta.HooksDir == "" || len(meta.Hooks) == 0 {
			continue
		}
		ids := make([]string, len(meta.Hooks))
		for i, h := range meta.Hooks {
			ids[i] = h.ID
		}
		result.KitHooksDirs = append(result.KitHooksDirs, project.KitHooksInfo{
			HooksDir: meta.HooksDir,
			HookIDs:  ids,
		})
	}

	for _, meta := range kits {
		if meta.GatesDir == "" || len(meta.Gates) == 0 {
			continue
		}
		ids := make([]string, len(meta.Gates))
		for i, g := range meta.Gates {
			ids[i] = g.ID
		}
		result.KitGatesDirs = append(result.KitGatesDirs, project.KitGatesInfo{
			GatesDir: meta.GatesDir,
			GateIDs:  ids,
		})
	}

	return &result
}

func dedupHooks(hooks []project.Hook) []project.Hook {
	seen := make(map[string]int)
	var result []project.Hook
	for _, hook := range hooks {
		if idx, ok := seen[hook.ID]; ok {
			result[idx] = hook
		} else {
			seen[hook.ID] = len(result)
			result = append(result, hook)
		}
	}
	return result
}

func dedupGates(gates []project.Gate) []project.Gate {
	seen := make(map[string]int)
	var result []project.Gate
	for _, gate := range gates {
		if idx, ok := seen[gate.ID]; ok {
			result[idx] = gate
		} else {
			seen[gate.ID] = len(result)
			result = append(result, gate)
		}
	}
	return result
}

func unionBindMounts(kits []*project.KitMeta, base []project.BindMount) []project.BindMount {
	seen := make(map[string]bool)
	var result []project.BindMount
	for _, meta := range kits {
		for _, binding := range meta.AdditionalBindings {
			if !seen[binding.Source] {
				seen[binding.Source] = true
				result = append(result, binding)
			}
		}
	}
	for _, binding := range base {
		if !seen[binding.Source] {
			seen[binding.Source] = true
			result = append(result, binding)
		}
	}
	return result
}

func interpolateEnv(s string) string {
	return os.Expand(s, os.Getenv)
}

func interpolateBindMounts(mounts []project.BindMount) {
	for i := range mounts {
		mounts[i].Source = interpolateEnv(mounts[i].Source)
	}
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

func interpolateHostCommands(cmds map[string]project.CommandDef) {
	for name, def := range cmds {
		def.Path = interpolateEnv(def.Path)
		interpolateEnvMap(def.Env)
		cmds[name] = def
	}
}
