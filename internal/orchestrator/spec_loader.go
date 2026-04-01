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

const projectLocalFilename = "project.local.yaml"

func ReadProjectMeta(dir string) (*ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read project.yaml: %w", err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}
	if _, ok := raw["workspace_id"]; ok {
		return nil, fmt.Errorf("project.yaml: workspace_id is no longer supported; assign workspace via boid workspace assign <project-id> <workspace-id>")
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
	if strings.HasPrefix(ref, "local/") {
		if resolver == nil {
			return "", fmt.Errorf("kit %q requires registry but none configured", ref)
		}
		return resolver.Resolve(ref)
	}

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

	local, err := ReadProjectLocalMeta(dir)
	if err != nil {
		return nil, err
	}

	kitsToLoad := meta.Kits
	if local != nil {
		kitsToLoad, err = EffectiveKitRefs(meta.Kits, local.Kits)
		if err != nil {
			return nil, err
		}
		meta = cloneProjectMeta(meta)
		meta.Kits = kitsToLoad
	}

	var kits []*KitMeta
	for _, ref := range kitsToLoad {
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

	merged := MergeKitMeta(meta, kits)
	if local == nil {
		return merged, nil
	}
	return ApplyProjectLocalMeta(merged, local), nil
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

func ReadProjectLocalMeta(dir string) (*ProjectLocalMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", projectLocalFilename)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", projectLocalFilename, err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", projectLocalFilename, err)
	}
	if err := validateProjectLocalFields(raw); err != nil {
		return nil, err
	}

	var meta ProjectLocalMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse %s: %w", projectLocalFilename, err)
	}
	if meta.Version == 0 {
		meta.Version = 1
	}
	if meta.Version != 1 {
		return nil, fmt.Errorf("%s: unsupported version %d", projectLocalFilename, meta.Version)
	}

	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	if err := validateProjectLocalMeta(&meta); err != nil {
		return nil, err
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

func EffectiveKitRefs(base []string, local ProjectLocalKits) ([]string, error) {
	addSeen := make(map[string]struct{}, len(local.Add))
	for _, ref := range local.Add {
		if _, ok := addSeen[ref]; ok {
			return nil, fmt.Errorf("%s: kits.add contains duplicate ref %q", projectLocalFilename, ref)
		}
		addSeen[ref] = struct{}{}
	}

	removeSeen := make(map[string]struct{}, len(local.Remove))
	for _, ref := range local.Remove {
		if _, ok := removeSeen[ref]; ok {
			return nil, fmt.Errorf("%s: kits.remove contains duplicate ref %q", projectLocalFilename, ref)
		}
		if _, ok := addSeen[ref]; ok {
			return nil, fmt.Errorf("%s: kit %q cannot be present in both kits.add and kits.remove", projectLocalFilename, ref)
		}
		removeSeen[ref] = struct{}{}
	}

	result := make([]string, 0, len(base)+len(local.Add))
	seen := make(map[string]struct{}, len(base)+len(local.Add))
	for _, ref := range base {
		if _, removed := removeSeen[ref]; removed {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		result = append(result, ref)
	}
	for _, ref := range local.Add {
		if _, removed := removeSeen[ref]; removed {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		result = append(result, ref)
	}
	return result, nil
}

func ApplyProjectLocalMeta(base *ProjectMeta, local *ProjectLocalMeta) *ProjectMeta {
	if local == nil {
		return base
	}

	result := cloneProjectMeta(base)
	result.Env = mergeStringMaps(result.Env, local.Env)
	result.HostCommands = mergeCommandMaps(result.HostCommands, local.HostCommands)
	result.AdditionalBindings = mergeBindMounts(result.AdditionalBindings, local.AdditionalBindings)
	return result
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

func mergeBindMounts(base, overlay []BindMount) []BindMount {
	if len(overlay) == 0 {
		return cloneBindMounts(base)
	}

	result := cloneBindMounts(base)
	indexBySource := make(map[string]int, len(result))
	for i, binding := range result {
		indexBySource[binding.Source] = i
	}

	for _, binding := range overlay {
		if idx, ok := indexBySource[binding.Source]; ok {
			result[idx] = binding
			continue
		}
		indexBySource[binding.Source] = len(result)
		result = append(result, binding)
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

func validateProjectLocalFields(raw map[string]any) error {
	allowed := map[string]bool{
		"version":             true,
		"kits":                true,
		"env":                 true,
		"host_commands":       true,
		"additional_bindings": true,
	}
	for key := range raw {
		if !allowed[key] {
			return fmt.Errorf("%s: unsupported field %q", projectLocalFilename, key)
		}
	}
	return nil
}

func validateProjectLocalMeta(meta *ProjectLocalMeta) error {
	for _, binding := range meta.AdditionalBindings {
		if binding.Source == "" {
			return fmt.Errorf("%s: additional_bindings.source is required", projectLocalFilename)
		}
		if !filepath.IsAbs(binding.Source) {
			return fmt.Errorf("%s: additional_bindings source %q must be an absolute path", projectLocalFilename, binding.Source)
		}
		if binding.Mode != "ro" && binding.Mode != "rw" {
			return fmt.Errorf("%s: additional_bindings source %q has invalid mode %q", projectLocalFilename, binding.Source, binding.Mode)
		}
	}

	for name, def := range meta.HostCommands {
		if def.Path == "" {
			return fmt.Errorf("%s: host_commands.%s.path is required", projectLocalFilename, name)
		}
		if !filepath.IsAbs(def.Path) {
			return fmt.Errorf("%s: host_commands.%s.path %q must be an absolute path", projectLocalFilename, name, def.Path)
		}
	}

	return nil
}

func cloneProjectMeta(meta *ProjectMeta) *ProjectMeta {
	if meta == nil {
		return nil
	}

	result := *meta
	result.Kits = append([]string(nil), meta.Kits...)
	result.Hooks = append([]Hook(nil), meta.Hooks...)
	result.Gates = append([]Gate(nil), meta.Gates...)
	result.KitHooksDirs = append([]KitHooksInfo(nil), meta.KitHooksDirs...)
	result.KitGatesDirs = append([]KitGatesInfo(nil), meta.KitGatesDirs...)
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = mergeCommandMaps(nil, meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = mergeTaskBehaviorMaps(nil, meta.TaskBehaviors)
	return &result
}

func cloneBindMounts(mounts []BindMount) []BindMount {
	if len(mounts) == 0 {
		return nil
	}
	result := make([]BindMount, len(mounts))
	copy(result, mounts)
	return result
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}

func mergeCommandMaps(base, overlay map[string]CommandDef) map[string]CommandDef {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(map[string]CommandDef, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}

func mergeTaskBehaviorMaps(base, overlay map[string]TaskBehavior) map[string]TaskBehavior {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(map[string]TaskBehavior, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}
