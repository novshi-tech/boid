package orchestrator

import (
	"encoding/json"
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
	for _, field := range []string{"hooks", "gates", "kits", "builtin_commands"} {
		if _, ok := raw[field]; ok {
			return nil, fmt.Errorf("project.yaml: top-level %q is no longer supported; move it into task_behaviors.<name>.kits (for kits) or define inside a local kit under .boid/kits/", field)
		}
	}

	var meta ProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
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

	for name, behavior := range meta.TaskBehaviors {
		if err := ValidateDefaultPayloadNoInstructions(behavior.DefaultPayload); err != nil {
			return nil, fmt.Errorf("project.yaml: task_behaviors.%s.default_payload: %w", name, err)
		}
	}

	return &meta, nil
}

// ValidateDefaultPayloadNoInstructions rejects "instructions" as a top-level key
// in default_payload. instructions live on Task.Instructions via default_instructions.
func ValidateDefaultPayloadNoInstructions(p RawPayload) error {
	raw := json.RawMessage(p)
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if _, ok := m["instructions"]; ok {
		return fmt.Errorf(`"instructions" is no longer allowed here; use "default_instructions" instead`)
	}
	return nil
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

// ReadProjectMetaWithKits reads project.yaml and project.local.yaml, resolves
// kits referenced by each task behavior, and merges kit data into each behavior.
// Returns a ProjectMeta whose TaskBehaviors have their resolved Hooks/Gates/etc.
// populated and ready for dispatch.
func ReadProjectMetaWithKits(dir string, resolver KitResolver) (*ProjectMeta, error) {
	meta, err := ReadProjectMeta(dir)
	if err != nil {
		return nil, err
	}

	local, err := ReadProjectLocalMeta(dir)
	if err != nil {
		return nil, err
	}

	meta = cloneProjectMeta(meta)

	// Collect unique kits across all behaviors, preserving first-seen order.
	var orderedRefs []KitRef
	kitMetaByRef := make(map[string]*KitMeta)
	consumerByRef := make(map[string]string)
	for _, behavior := range meta.TaskBehaviors {
		for _, kitRef := range behavior.Kits {
			if _, loaded := kitMetaByRef[kitRef.Ref]; loaded {
				continue
			}
			kitDir, err := resolveKitRef(kitRef.Ref, dir, resolver)
			if err != nil {
				return nil, fmt.Errorf("kit %q: %w", kitRef.Ref, err)
			}
			kitMeta, err := ReadKitMeta(kitDir)
			if err != nil {
				return nil, fmt.Errorf("kit %q: %w", kitRef.Ref, err)
			}
			slog.Info("resolved kit", "ref", kitRef.Ref, "hooks", len(kitMeta.Hooks))
			kitMetaByRef[kitRef.Ref] = kitMeta
			consumerByRef[kitRef.Ref] = ResolveKitConsumer(kitRef)
			orderedRefs = append(orderedRefs, kitRef)
		}
	}

	// Kit-provided task_behaviors act as defaults; project behaviors take precedence.
	if meta.TaskBehaviors == nil {
		meta.TaskBehaviors = make(map[string]TaskBehavior)
	}
	for _, kitRef := range orderedRefs {
		kitMeta := kitMetaByRef[kitRef.Ref]
		for k, v := range kitMeta.TaskBehaviors {
			if _, exists := meta.TaskBehaviors[k]; !exists {
				meta.TaskBehaviors[k] = v
			}
		}
	}

	// For each behavior, resolve its kits and merge data into the behavior.
	for name, behavior := range meta.TaskBehaviors {
		var kits []*KitMeta
		var consumers []string
		seen := make(map[string]bool)
		for _, kitRef := range behavior.Kits {
			if seen[kitRef.Ref] {
				continue
			}
			seen[kitRef.Ref] = true
			km, ok := kitMetaByRef[kitRef.Ref]
			if !ok {
				continue
			}
			consumer := ResolveKitConsumer(kitRef)
			// Within this behavior, consumer names must be unique because hook
			// IDs are prefixed with the consumer name.
			for i, c := range consumers {
				if c == consumer {
					return nil, fmt.Errorf("behavior %q: kit consumer %q is ambiguous: both %q and %q resolve to the same name; use 'as:' to disambiguate", name, consumer, behavior.Kits[i].Ref, kitRef.Ref)
				}
			}
			kits = append(kits, km)
			consumers = append(consumers, consumer)
		}
		if err := MergeKitMetaIntoBehavior(&behavior, kits, consumers); err != nil {
			return nil, fmt.Errorf("behavior %q: %w", name, err)
		}
		// Apply project.yaml-level overlay (env / host_commands / bindings).
		behavior.Env = mergeStringMaps(behavior.Env, meta.Env)
		behavior.HostCommands = mergeHostCommands(behavior.HostCommands, meta.HostCommands)
		behavior.AdditionalBindings = mergeBindMounts(behavior.AdditionalBindings, meta.AdditionalBindings)
		// Apply project.local.yaml overlay on top.
		if local != nil {
			behavior.Env = mergeStringMaps(behavior.Env, local.Env)
			behavior.HostCommands = mergeHostCommands(behavior.HostCommands, local.HostCommands)
			behavior.AdditionalBindings = mergeBindMounts(behavior.AdditionalBindings, local.AdditionalBindings)
		}
		if err := validateBuiltinCommands(fmt.Sprintf("behavior %q", name), behavior.BuiltinCommands, behavior.HostCommands); err != nil {
			return nil, err
		}
		meta.TaskBehaviors[name] = behavior
	}

	if local != nil && local.SecretNamespace != "" {
		meta.SecretNamespace = local.SecretNamespace
	}

	return meta, nil
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
	for name, behavior := range meta.TaskBehaviors {
		if err := ValidateDefaultPayloadNoInstructions(behavior.DefaultPayload); err != nil {
			return nil, fmt.Errorf("kit.yaml: task_behaviors.%s.default_payload: %w", name, err)
		}
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

// MergeKitMetaIntoBehavior merges kit-provided hooks, gates, env, bindings,
// builtin_commands, and host_commands into the given TaskBehavior. Kit hook and
// gate IDs are prefixed with the consumer name. The behavior is modified in place.
func MergeKitMetaIntoBehavior(behavior *TaskBehavior, kits []*KitMeta, kitConsumers []string) error {
	if len(kits) == 0 {
		return nil
	}

	// Env: kits are lower priority than any existing behavior.Env.
	mergedEnv := make(map[string]string)
	for _, kit := range kits {
		for k, v := range kit.Env {
			mergedEnv[k] = v
		}
	}
	for k, v := range behavior.Env {
		mergedEnv[k] = v
	}
	if len(mergedEnv) > 0 {
		behavior.Env = mergedEnv
	}

	// Hooks: prefix IDs with consumer, tag with Kit name, inherit Consumer.
	var allHooks []Hook
	allHooks = append(allHooks, behavior.Hooks...)
	for i, kit := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for _, h := range kit.Hooks {
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
	behavior.Hooks = allHooks

	// Gates: prefix IDs with consumer.
	var allGates []Gate
	allGates = append(allGates, behavior.Gates...)
	for i, kit := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for _, g := range kit.Gates {
			g.Kit = consumer
			if consumer != "" {
				g.ID = consumer + "/" + g.ID
			}
			allGates = append(allGates, g)
		}
	}
	behavior.Gates = allGates

	// HostCommands: reject duplicates across kits.
	mergedCmds := make(HostCommands)
	for k, v := range behavior.HostCommands {
		mergedCmds[k] = v
	}
	kitCmdSource := make(map[string]string)
	for i, kit := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for k, v := range kit.HostCommands {
			if existingConsumer, ok := kitCmdSource[k]; ok {
				return fmt.Errorf("host_commands: command %q is defined in both kit %q and kit %q; remove the duplicate from one kit or override it in project.local.yaml", k, existingConsumer, consumer)
			}
			kitCmdSource[k] = consumer
			if _, preset := mergedCmds[k]; !preset {
				mergedCmds[k] = v
			}
		}
	}
	if len(mergedCmds) > 0 {
		behavior.HostCommands = mergedCmds
	}

	behavior.BuiltinCommands = mergeBuiltinCommands(behavior.BuiltinCommands, kitBuiltinCommandLists(kits)...)
	behavior.AdditionalBindings = unionBindMounts(kits, behavior.AdditionalBindings)

	for i, kit := range kits {
		if kit.HooksDir == "" || len(kit.Hooks) == 0 {
			continue
		}
		c := ""
		if i < len(kitConsumers) {
			c = kitConsumers[i]
		}
		ids := make([]string, len(kit.Hooks))
		for j, h := range kit.Hooks {
			ids[j] = h.ID
		}
		behavior.KitHooksDirs = append(behavior.KitHooksDirs, KitHooksInfo{
			HooksDir: kit.HooksDir,
			HookIDs:  ids,
			Consumer: c,
		})
	}

	for _, kit := range kits {
		if kit.GatesDir == "" || len(kit.Gates) == 0 {
			continue
		}
		ids := make([]string, len(kit.Gates))
		for i, g := range kit.Gates {
			ids[i] = g.ID
		}
		behavior.KitGatesDirs = append(behavior.KitGatesDirs, KitGatesInfo{
			GatesDir: kit.GatesDir,
			GateIDs:  ids,
		})
	}

	return nil
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
	return nil
}

func cloneProjectMeta(meta *ProjectMeta) *ProjectMeta {
	if meta == nil {
		return nil
	}

	result := *meta
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = cloneHostCommands(meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = cloneTaskBehaviorMap(meta.TaskBehaviors)
	return &result
}

// cloneTaskBehaviorMap deep-copies the task behavior map including each
// behavior's kit refs. Resolved fields are reset to nil; the caller is expected
// to recompute them via MergeKitMetaIntoBehavior.
func cloneTaskBehaviorMap(src map[string]TaskBehavior) map[string]TaskBehavior {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]TaskBehavior, len(src))
	for k, v := range src {
		v.Kits = append([]KitRef(nil), v.Kits...)
		v.Hooks = nil
		v.Gates = nil
		v.Env = nil
		v.BuiltinCommands = nil
		v.HostCommands = nil
		v.AdditionalBindings = nil
		v.KitHooksDirs = nil
		v.KitGatesDirs = nil
		result[k] = v
	}
	return result
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
