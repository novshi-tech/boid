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
	if err := validateBuiltinCommands("project.yaml", meta.BuiltinCommands, meta.HostCommands); err != nil {
		return nil, err
	}
	if err := resolveProjectHostCommandPaths(dir, meta.HostCommands); err != nil {
		return nil, err
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
		if len(h.On) == 0 || !h.On.AllValid(ValidHookOnValues) {
			return nil, fmt.Errorf("hook %q: invalid on value %v", h.ID, []string(h.On))
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
		if len(g.On) == 0 || !g.On.AllValid(ValidGateOnValues) {
			return nil, fmt.Errorf("gate %q: invalid on value %v", g.ID, []string(g.On))
		}
		scriptPath, err := ResolveGateScript(gatesDir, g.ID)
		if err != nil {
			return nil, fmt.Errorf("gate %q: %w", g.ID, err)
		}
		g.ScriptPath = scriptPath
	}

	return &meta, nil
}

// resolveProjectHostCommandPaths resolves relative paths in host_commands
// against the project root directory. It rejects paths that escape the project
// directory via traversal (e.g. "../../etc/passwd") or symlinks.
func resolveProjectHostCommandPaths(projectDir string, cmds HostCommands) error {
	for name, spec := range cmds {
		if spec.Path == "" || filepath.IsAbs(spec.Path) {
			continue
		}
		joined := filepath.Join(projectDir, spec.Path)
		resolved, err := filepath.EvalSymlinks(filepath.Dir(joined))
		if err != nil {
			// If the directory doesn't exist we can still detect traversal
			// via a lexical clean.
			resolved = filepath.Clean(joined)
		} else {
			resolved = filepath.Join(resolved, filepath.Base(joined))
		}
		absProject, _ := filepath.Abs(projectDir)
		if !strings.HasPrefix(resolved, absProject+string(filepath.Separator)) && resolved != absProject {
			return fmt.Errorf("project.yaml: host_commands.%s.path %q resolves outside project directory", name, spec.Path)
		}
		spec.Path = joined
		cmds[name] = spec
	}
	return nil
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

// ResolveKitConsumer derives the consumer name for a kit reference.
// If an alias is set via 'as:', that alias is returned.
// Otherwise the last path segment of the ref is used.
func ResolveKitConsumer(ref KitRef) string {
	if ref.Alias != "" {
		return ref.Alias
	}
	parts := strings.Split(ref.Ref, "/")
	return parts[len(parts)-1]
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

	// Resolve consumer names and validate uniqueness.
	consumerNames := make([]string, 0, len(kitsToLoad))
	seenConsumers := make(map[string]string) // consumer name → kit ref
	for _, kitRef := range kitsToLoad {
		consumer := ResolveKitConsumer(kitRef)
		if prev, ok := seenConsumers[consumer]; ok {
			return nil, fmt.Errorf("kit consumer %q is ambiguous: both %q and %q resolve to the same name; use 'as:' to disambiguate", consumer, prev, kitRef.Ref)
		}
		seenConsumers[consumer] = kitRef.Ref
		consumerNames = append(consumerNames, consumer)
	}

	var kits []*KitMeta
	for _, kitRef := range kitsToLoad {
		kitDir, err := resolveKitRef(kitRef.Ref, dir, resolver)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", kitRef.Ref, err)
		}
		kitMeta, err := ReadKitMeta(kitDir)
		if err != nil {
			return nil, fmt.Errorf("kit %q: %w", kitRef.Ref, err)
		}
		slog.Info("resolved kit", "ref", kitRef.Ref, "hooks", len(kitMeta.Hooks))
		kits = append(kits, kitMeta)
	}

	merged, err := MergeKitMeta(meta, kits, consumerNames)
	if err != nil {
		return nil, err
	}
	if err := validateBuiltinCommands("merged project meta", merged.BuiltinCommands, merged.HostCommands); err != nil {
		return nil, err
	}
	if local == nil {
		return merged, nil
	}
	applied := ApplyProjectLocalMeta(merged, local)
	if err := validateBuiltinCommands("effective project meta", applied.BuiltinCommands, applied.HostCommands); err != nil {
		return nil, err
	}
	return applied, nil
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
	if err := validateBuiltinCommands("kit.yaml", meta.BuiltinCommands, meta.HostCommands); err != nil {
		return nil, err
	}

	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	hooksDir := filepath.Join(dir, "hooks")
	for i := range meta.Hooks {
		h := &meta.Hooks[i]
		if len(h.On) == 0 || !h.On.AllValid(ValidHookOnValues) {
			return nil, fmt.Errorf("hook %q: invalid on value %v", h.ID, []string(h.On))
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
		if len(g.On) == 0 || !g.On.AllValid(ValidGateOnValues) {
			return nil, fmt.Errorf("gate %q: invalid on value %v", g.ID, []string(g.On))
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

	// Reject legacy scripts: section in kit.yaml.
	var rawMap map[string]any
	if err := yaml.Unmarshal(data, &rawMap); err == nil {
		if _, ok := rawMap["scripts"]; ok {
			return nil, fmt.Errorf("kit.yaml: 'scripts:' section is no longer supported; migrate scripts to gates")
		}
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

func MergeKitMeta(base *ProjectMeta, kits []*KitMeta, kitConsumers []string) (*ProjectMeta, error) {
	if len(kits) == 0 {
		return base, nil
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
	for i, meta := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for _, h := range meta.Hooks {
			h.Kit = consumer
			if consumer != "" {
				h.ID = consumer + "/" + h.ID
			}
			if h.Consumer == "" {
				h.Consumer = consumer
			}
			allHooks = append(allHooks, h)
		}
	}
	allHooks = append(allHooks, base.Hooks...)
	result.Hooks = allHooks

	var allGates []Gate
	for i, meta := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for _, g := range meta.Gates {
			g.Kit = consumer
			if consumer != "" {
				g.ID = consumer + "/" + g.ID
			}
			allGates = append(allGates, g)
		}
	}
	allGates = append(allGates, base.Gates...)
	result.Gates = allGates

	mergedCmds := make(HostCommands)
	kitCmdSource := make(map[string]string)
	for i, meta := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for k, v := range meta.HostCommands {
			if existingConsumer, ok := kitCmdSource[k]; ok {
				return nil, fmt.Errorf("host_commands: command %q is defined in both kit %q and kit %q; remove the duplicate from one kit or override it in project.local.yaml", k, existingConsumer, consumer)
			}
			kitCmdSource[k] = consumer
			mergedCmds[k] = v
		}
	}
	for k, v := range base.HostCommands {
		mergedCmds[k] = v
	}
	if len(mergedCmds) > 0 {
		result.HostCommands = mergedCmds
	}
	result.BuiltinCommands = mergeBuiltinCommands(result.BuiltinCommands, kitBuiltinCommandLists(kits)...)

	result.AdditionalBindings = unionBindMounts(kits, base.AdditionalBindings)

	for i, meta := range kits {
		if meta.HooksDir == "" || len(meta.Hooks) == 0 {
			continue
		}
		c := ""
		if i < len(kitConsumers) {
			c = kitConsumers[i]
		}
		ids := make([]string, len(meta.Hooks))
		for j, h := range meta.Hooks {
			ids[j] = h.ID
		}
		result.KitHooksDirs = append(result.KitHooksDirs, KitHooksInfo{
			HooksDir: meta.HooksDir,
			HookIDs:  ids,
			Consumer: c,
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

	return &result, nil
}

func EffectiveKitRefs(base []KitRef, local ProjectLocalKits) ([]KitRef, error) {
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

	result := make([]KitRef, 0, len(base)+len(local.Add))
	seen := make(map[string]struct{}, len(base)+len(local.Add))
	for _, kitRef := range base {
		if _, removed := removeSeen[kitRef.Ref]; removed {
			continue
		}
		if _, ok := seen[kitRef.Ref]; ok {
			continue
		}
		seen[kitRef.Ref] = struct{}{}
		result = append(result, kitRef)
	}
	for _, ref := range local.Add {
		if _, removed := removeSeen[ref]; removed {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		result = append(result, KitRef{Ref: ref})
	}
	return result, nil
}

func ApplyProjectLocalMeta(base *ProjectMeta, local *ProjectLocalMeta) *ProjectMeta {
	if local == nil {
		return base
	}

	result := cloneProjectMeta(base)
	result.Env = mergeStringMaps(result.Env, local.Env)
	result.BuiltinCommands = mergeBuiltinCommands(result.BuiltinCommands, local.BuiltinCommands)
	result.HostCommands = mergeHostCommands(result.HostCommands, local.HostCommands)
	result.AdditionalBindings = mergeBindMounts(result.AdditionalBindings, local.AdditionalBindings)
	if local.SecretNamespace != "" {
		result.SecretNamespace = local.SecretNamespace
	}
	return result
}

func unionBindMounts(kits []*KitMeta, base []BindMount) []BindMount {
	indexBySource := make(map[string]int)
	var result []BindMount
	for _, meta := range kits {
		for _, binding := range meta.AdditionalBindings {
			if idx, ok := indexBySource[binding.Source]; ok {
				if binding.Mode == "rw" {
					result[idx].Mode = "rw"
				}
			} else {
				indexBySource[binding.Source] = len(result)
				result = append(result, binding)
			}
		}
	}
	for _, binding := range base {
		if idx, ok := indexBySource[binding.Source]; ok {
			if binding.Mode == "rw" {
				result[idx].Mode = "rw"
			}
		} else {
			indexBySource[binding.Source] = len(result)
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

func interpolateHostCommands(cmds HostCommands) {
	for name, spec := range cmds {
		spec.Path = interpolateEnv(spec.Path)
		interpolateEnvMap(spec.Env)
		cmds[name] = spec
	}
}

func validateProjectLocalFields(raw map[string]any) error {
	allowed := map[string]bool{
		"version":             true,
		"kits":                true,
		"builtin_commands":    true,
		"env":                 true,
		"host_commands":       true,
		"additional_bindings": true,
		"secret_namespace":    true,
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

	for name, spec := range meta.HostCommands {
		if spec.Path != "" && !filepath.IsAbs(spec.Path) {
			return fmt.Errorf("%s: host_commands.%s.path %q must be an absolute path", projectLocalFilename, name, spec.Path)
		}
	}
	if err := validateBuiltinCommands(projectLocalFilename, meta.BuiltinCommands, meta.HostCommands); err != nil {
		return err
	}

	return nil
}

func cloneProjectMeta(meta *ProjectMeta) *ProjectMeta {
	if meta == nil {
		return nil
	}

	result := *meta
	result.Kits = append([]KitRef(nil), meta.Kits...)
	result.Hooks = append([]Hook(nil), meta.Hooks...)
	result.Gates = append([]Gate(nil), meta.Gates...)
	result.BuiltinCommands = append([]string(nil), meta.BuiltinCommands...)
	result.KitHooksDirs = append([]KitHooksInfo(nil), meta.KitHooksDirs...)
	result.KitGatesDirs = append([]KitGatesInfo(nil), meta.KitGatesDirs...)
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = cloneHostCommands(meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = mergeTaskBehaviorMaps(nil, meta.TaskBehaviors)
	return &result
}

func cloneHostCommands(cmds HostCommands) HostCommands {
	return mergeHostCommands(nil, cmds)
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

func mergeHostCommands(base, overlay HostCommands) HostCommands {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}

	result := make(HostCommands, len(base)+len(overlay))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overlay {
		result[k] = v
	}
	return result
}

func mergeBuiltinCommands(base []string, overlays ...[]string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, name := range base {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	for _, overlay := range overlays {
		for _, name := range overlay {
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			result = append(result, name)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func kitBuiltinCommandLists(kits []*KitMeta) [][]string {
	if len(kits) == 0 {
		return nil
	}
	out := make([][]string, 0, len(kits))
	for _, meta := range kits {
		if len(meta.BuiltinCommands) == 0 {
			continue
		}
		out = append(out, meta.BuiltinCommands)
	}
	return out
}

func validateBuiltinCommands(scope string, builtins []string, hostCommands HostCommands) error {
	if len(builtins) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(builtins))
	for _, name := range builtins {
		if _, ok := validBuiltinCommands[name]; !ok {
			return fmt.Errorf("%s: unsupported builtin command %q", scope, name)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("%s: duplicate builtin command %q", scope, name)
		}
		seen[name] = struct{}{}
		if _, conflict := hostCommands[name]; conflict {
			return fmt.Errorf("%s: %q cannot be declared in both builtin_commands and host_commands", scope, name)
		}
	}
	return nil
}

var validBuiltinCommands = map[string]struct{}{
	"git":  {},
	"boid": {},
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
