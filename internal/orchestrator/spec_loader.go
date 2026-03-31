package orchestrator

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type KitResolver interface {
	Resolve(ref string) (string, error)
}

func ReadProjectMeta(dir string) (*ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var meta ProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}

	if meta.ID == "" {
		return nil, fmt.Errorf("project.yaml: id is required")
	}
	if meta.Name == "" {
		return nil, fmt.Errorf("project.yaml: name is required")
	}

	hooksDir := filepath.Join(dir, ".boid", "hooks")
	for i := range meta.Hooks {
		h := &meta.Hooks[i]
		if !ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := ResolveHookScript(hooksDir, h.ID)
		if err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.ID, err)
		}
		h.ScriptPath = scriptPath
	}

	gatesDir := filepath.Join(dir, ".boid", "gates")
	for i := range meta.Gates {
		g := &meta.Gates[i]
		if !ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}

	return &meta, nil
}

func resolveKitRef(ref, projectDir string, resolver KitResolver) (string, error) {
	parts := strings.Split(ref, "/")
	if len(parts) < 4 {
		localDir := filepath.Join(projectDir, ".boid", "kits", ref)
		yamlPath := filepath.Join(localDir, "kit.yaml")
		if _, err := os.Stat(yamlPath); err != nil {
			return "", fmt.Errorf("local kit %q: kit.yaml not found at %s", ref, localDir)
		}
		return localDir, nil
	}

	if resolver == nil {
		return "", fmt.Errorf("kit %q requires registry but none configured", ref)
	}
	return resolver.Resolve(ref)
}

func ReadProjectMetaWithKits(dir string, resolver KitResolver) (*ProjectMeta, error) {
	meta, err := ReadProjectMeta(dir)
	if err != nil {
		return nil, err
	}
	if len(meta.Kits) == 0 {
		return meta, nil
	}

	var kits []*KitMeta
	for _, ref := range meta.Kits {
		kitDir, err := resolveKitRef(ref, dir, resolver)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		kitMeta, err := ReadKitMeta(kitDir)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", ref, err)
		}
		slog.Info("resolved kit", "ref", ref, "hooks", len(kitMeta.Hooks))
		kits = append(kits, kitMeta)
	}

	return MergeKitMeta(meta, kits), nil
}

func ReadKitMeta(dir string) (*KitMeta, error) {
	yamlPath := filepath.Join(dir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read kit.yaml: %w", err)
	}

	var meta KitMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}

	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	hooksDir := filepath.Join(dir, "hooks")
	for i := range meta.Hooks {
		h := &meta.Hooks[i]
		if !ValidHookOnValues[h.On] {
			return nil, fmt.Errorf("hook %q: invalid on value %q", h.ID, h.On)
		}
		scriptPath, err := ResolveHookScript(hooksDir, h.ID)
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
		if !ValidGateOnValues[g.On] {
			return nil, fmt.Errorf("gate %q: invalid on value %q", g.ID, g.On)
		}
		scriptPath, err := ResolveGateScript(gatesDir, g.ID)
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

func MergeKitMeta(base *ProjectMeta, kits []*KitMeta) *ProjectMeta {
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

	mergedBehaviors := make(map[string]TaskBehavior)
	for _, meta := range kits {
		for k, v := range meta.TaskBehaviors {
			mergedBehaviors[k] = v
		}
	}
	for k, v := range base.TaskBehaviors {
		mergedBehaviors[k] = v
	}
	result.TaskBehaviors = mergedBehaviors

	var allHooks []Hook
	for _, meta := range kits {
		allHooks = append(allHooks, meta.Hooks...)
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = dedupHooks(allHooks)

	var allGates []Gate
	for _, meta := range kits {
		allGates = append(allGates, meta.Gates...)
	}
	allGates = append(allGates, base.Gates...)
	result.Gates = dedupGates(allGates)

	mergedCmds := make(map[string]CommandDef)
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
		result.KitHooksDirs = append(result.KitHooksDirs, KitHooksInfo{
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
		result.KitGatesDirs = append(result.KitGatesDirs, KitGatesInfo{
			GatesDir: meta.GatesDir,
			GateIDs:  ids,
		})
	}

	return &result
}

func dedupHooks(hooks []Hook) []Hook {
	seen := make(map[string]int)
	var result []Hook
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

func dedupGates(gates []Gate) []Gate {
	seen := make(map[string]int)
	var result []Gate
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

func unionBindMounts(kits []*KitMeta, base []BindMount) []BindMount {
	seen := make(map[string]bool)
	var result []BindMount
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

func interpolateBindMounts(mounts []BindMount) {
	for i := range mounts {
		mounts[i].Source = interpolateEnv(mounts[i].Source)
	}
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

func interpolateHostCommands(cmds map[string]CommandDef) {
	for name, def := range cmds {
		def.Path = interpolateEnv(def.Path)
		interpolateEnvMap(def.Env)
		cmds[name] = def
	}
}
