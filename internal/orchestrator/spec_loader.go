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

// KitRuntime holds the merged runtime fields derived from a set of kits.
// It covers env, host_commands, and additional_bindings. Hooks, gates, and
// directory metadata are excluded — those are TaskBehavior-specific and handled
// by MergeKitMetaIntoBehavior.
type KitRuntime struct {
	AdditionalBindings []BindMount
	HostCommands       HostCommands
	Env                map[string]string
}

// MergeKitRuntime merges env, host_commands, and additional_bindings from the
// given kits into a KitRuntime value. kitConsumers provides display names for
// error messages (one per kit).
func MergeKitRuntime(kits []*KitMeta, kitConsumers []string) (KitRuntime, error) {
	var rt KitRuntime
	if len(kits) == 0 {
		return rt, nil
	}

	// Env: later kit overrides earlier kit.
	mergedEnv := make(map[string]string)
	for _, kit := range kits {
		for k, v := range kit.Env {
			mergedEnv[k] = v
		}
	}
	if len(mergedEnv) > 0 {
		rt.Env = mergedEnv
	}

	// HostCommands: duplicate commands across kits are rejected.
	mergedCmds := make(HostCommands)
	kitCmdSource := make(map[string]string)
	for i, kit := range kits {
		consumer := ""
		if i < len(kitConsumers) {
			consumer = kitConsumers[i]
		}
		for k, v := range kit.HostCommands {
			if existingConsumer, ok := kitCmdSource[k]; ok {
				return rt, fmt.Errorf("host_commands: command %q is defined in both kit %q and kit %q; remove the duplicate from one kit or override it in project.local.yaml", k, existingConsumer, consumer)
			}
			kitCmdSource[k] = consumer
			mergedCmds[k] = v
		}
	}
	if len(mergedCmds) > 0 {
		rt.HostCommands = mergedCmds
	}

	// AdditionalBindings: union with mode promotion.
	for _, kit := range kits {
		rt.AdditionalBindings = unionBindMountSlices(rt.AdditionalBindings, kit.AdditionalBindings)
	}

	return rt, nil
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
	for _, field := range []string{"hooks", "gates"} {
		if _, ok := raw[field]; ok {
			return nil, fmt.Errorf("project.yaml: top-level %q is no longer supported; move it into task_behaviors.<name>.kits (for kits) or define inside a local kit under .boid/kits/", field)
		}
	}

	var meta ProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}
	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)
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

// validateHookKind enforces the Hook.Kind / Hook.Consumer invariants at load time:
//   - Kind must be "" or "agent"
//   - Consumer can only be specified on kind: agent hooks; on non-agent hooks
//     it has no effect and likely indicates that `kind: agent` was forgotten
//
// Agent hooks without a Consumer are allowed here (the kit-consumer inheritance
// in MergeKitMetaIntoBehavior may still fill it in); the final "agent requires
// consumer" check happens after kit merge.
func validateHookKind(h *Hook) error {
	if !h.Kind.IsValid() {
		return fmt.Errorf("hook %q: invalid kind %q (allowed: \"\" or \"agent\")", h.ID, h.Kind)
	}
	if h.Kind != HandlerKindAgent && h.Consumer != "" {
		return fmt.Errorf("hook %q: 'consumer' requires 'kind: agent' (non-agent hooks must not declare consumer)", h.ID)
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

// IsProjectScopable reports whether a kit may be placed in the top-level
// project.yaml kits field. A kit is project-scopable when it has no gates
// and all its hooks have kind == "agent" (opt-in via instructions, so they
// cannot fire unexpectedly across behaviors).
func IsProjectScopable(km *KitMeta) error {
	if len(km.Gates) > 0 {
		return fmt.Errorf("gates を持つ kit は top-level kits に指定できません (behavior スコープでのみ使用可能)")
	}
	for _, h := range km.Hooks {
		if h.Kind != HandlerKindAgent {
			return fmt.Errorf("hook %s の kind が agent 以外のため top-level kits に指定できません", h.ID)
		}
	}
	return nil
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

	// Collect unique kits across all behaviors and commands, preserving first-seen order.
	var orderedRefs []KitRef
	kitMetaByRef := make(map[string]*KitMeta)
	consumerByRef := make(map[string]string)
	loadKitRef := func(kitRef KitRef) error {
		if _, loaded := kitMetaByRef[kitRef.Ref]; loaded {
			return nil
		}
		kitDir, err := resolveKitRef(kitRef.Ref, dir, resolver)
		if err != nil {
			return fmt.Errorf("kit %q: %w", kitRef.Ref, err)
		}
		kitMeta, err := ReadKitMeta(kitDir)
		if err != nil {
			return fmt.Errorf("kit %q: %w", kitRef.Ref, err)
		}
		slog.Info("resolved kit", "ref", kitRef.Ref, "hooks", len(kitMeta.Hooks))
		kitMetaByRef[kitRef.Ref] = kitMeta
		consumerByRef[kitRef.Ref] = ResolveKitConsumer(kitRef)
		orderedRefs = append(orderedRefs, kitRef)
		return nil
	}
	for _, behavior := range meta.TaskBehaviors {
		for _, kitRef := range behavior.Kits {
			if err := loadKitRef(kitRef); err != nil {
				return nil, err
			}
		}
	}

	// Load and validate top-level project-scope kits.
	for _, kitRef := range meta.Kits {
		if err := loadKitRef(kitRef); err != nil {
			return nil, err
		}
		km := kitMetaByRef[kitRef.Ref]
		if err := IsProjectScopable(km); err != nil {
			return nil, fmt.Errorf("kit %s: %w", kitRef.Ref, err)
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

	// Kit-provided commands (from top-level project kits): field-level overlay.
	// project.Command wins when non-empty; kit.Command is inherited when project leaves it empty.
	// Conflict between two kits providing the same command name is an error.
	if meta.Commands == nil {
		meta.Commands = make(map[string]CommandSpec)
	}
	kitCmdSource := make(map[string]string) // commandName -> kit consumer (for conflict detection)
	for _, kitRef := range meta.Kits {
		kitMeta := kitMetaByRef[kitRef.Ref]
		consumer := ResolveKitConsumer(kitRef)
		for cmdName, kitCmdSpec := range kitMeta.Commands {
			if existingConsumer, ok := kitCmdSource[cmdName]; ok {
				return nil, fmt.Errorf("command %q: conflict between kits %q and %q", cmdName, existingConsumer, consumer)
			}
			kitCmdSource[cmdName] = consumer
			if projCmdSpec, exists := meta.Commands[cmdName]; !exists {
				meta.Commands[cmdName] = kitCmdSpec
			} else if len(projCmdSpec.Command) == 0 {
				projCmdSpec.Command = kitCmdSpec.Command
				meta.Commands[cmdName] = projCmdSpec
			}
		}
	}

	// For each behavior, resolve its kits and merge data into the behavior.
	for name, behavior := range meta.TaskBehaviors {
		var kits []*KitMeta
		var consumers []string
		var refs []string
		seen := make(map[string]bool)

		// Project-level kits come first and are merged into every behavior.
		for _, kitRef := range meta.Kits {
			if seen[kitRef.Ref] {
				continue
			}
			seen[kitRef.Ref] = true
			km := kitMetaByRef[kitRef.Ref]
			consumer := ResolveKitConsumer(kitRef)
			kits = append(kits, km)
			consumers = append(consumers, consumer)
			refs = append(refs, kitRef.Ref)
		}

		// Behavior-level kits follow.
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
					return nil, fmt.Errorf("behavior %q: kit consumer %q is ambiguous: both %q and %q resolve to the same name; use 'as:' to disambiguate", name, consumer, refs[i], kitRef.Ref)
				}
			}
			kits = append(kits, km)
			consumers = append(consumers, consumer)
			refs = append(refs, kitRef.Ref)
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
		if err := validateBuiltinHostConflict(fmt.Sprintf("behavior %q", name), behavior.HostCommands); err != nil {
			return nil, err
		}
		meta.TaskBehaviors[name] = behavior
	}

	// Compute project-level kit runtime once; apply to all commands.
	var projectKits []*KitMeta
	var projectKitConsumers []string
	for _, kitRef := range meta.Kits {
		km, ok := kitMetaByRef[kitRef.Ref]
		if !ok {
			continue
		}
		projectKits = append(projectKits, km)
		projectKitConsumers = append(projectKitConsumers, ResolveKitConsumer(kitRef))
	}
	projectKitRuntime, err := MergeKitRuntime(projectKits, projectKitConsumers)
	if err != nil {
		return nil, fmt.Errorf("project kits runtime: %w", err)
	}

	// Resolve Commands: apply project-level kit runtime and expand env vars in each command spec.
	for name, cmd := range meta.Commands {
		rt := projectKitRuntime

		// Apply overlays in the same order as TaskBehavior: kit → project.yaml → project.local.yaml.
		cmd.Env = mergeStringMaps(rt.Env, meta.Env)
		cmd.HostCommands = mergeHostCommands(rt.HostCommands, meta.HostCommands)
		cmd.AdditionalBindings = mergeBindMounts(rt.AdditionalBindings, meta.AdditionalBindings)
		if local != nil {
			cmd.Env = mergeStringMaps(cmd.Env, local.Env)
			cmd.HostCommands = mergeHostCommands(cmd.HostCommands, local.HostCommands)
			cmd.AdditionalBindings = mergeBindMounts(cmd.AdditionalBindings, local.AdditionalBindings)
		}

		// Expand env vars in each command argument.
		resolved := make([]string, len(cmd.Command))
		for i, arg := range cmd.Command {
			resolved[i] = interpolateEnv(arg)
		}
		cmd.ResolvedCommand = resolved

		if err := validateBuiltinHostConflict(fmt.Sprintf("command %q", name), cmd.HostCommands); err != nil {
			return nil, err
		}

		meta.Commands[name] = cmd
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
		if err := validateHookKind(h); err != nil {
			return nil, fmt.Errorf("kit.yaml: %w", err)
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

	meta.KitRoot = dir

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

// MergeKitMetaIntoBehavior merges kit-provided hooks, gates, env, bindings, and
// host_commands into the given TaskBehavior. Kit hook and gate IDs are prefixed
// with the consumer name. The behavior is modified in place.
func MergeKitMetaIntoBehavior(behavior *TaskBehavior, kits []*KitMeta, kitConsumers []string) error {
	if len(kits) == 0 {
		return nil
	}

	rt, err := MergeKitRuntime(kits, kitConsumers)
	if err != nil {
		return err
	}

	// Env: kits are lower priority than any existing behavior.Env.
	mergedEnv := make(map[string]string)
	for k, v := range rt.Env {
		mergedEnv[k] = v
	}
	for k, v := range behavior.Env {
		mergedEnv[k] = v
	}
	if len(mergedEnv) > 0 {
		behavior.Env = mergedEnv
	}

	// Hooks: prefix IDs with consumer, tag with Kit name (provenance).
	// Consumer (routing identity) is inherited from kit consumer only for
	// agent-kind hooks; non-agent hooks don't use Consumer for routing.
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
			if h.Kind == HandlerKindAgent && h.Consumer == "" {
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

	// HostCommands: behavior wins over kits.
	if len(rt.HostCommands) > 0 || len(behavior.HostCommands) > 0 {
		mergedCmds := make(HostCommands)
		for k, v := range rt.HostCommands {
			mergedCmds[k] = v
		}
		for k, v := range behavior.HostCommands {
			mergedCmds[k] = v
		}
		behavior.HostCommands = mergedCmds
	}

	behavior.AdditionalBindings = unionBindMountSlices(rt.AdditionalBindings, behavior.AdditionalBindings)

	// Collect deduplicated kit roots for sandbox bind-mounts.
	seen := make(map[string]bool)
	for _, kit := range kits {
		if kit.KitRoot == "" {
			continue
		}
		if seen[kit.KitRoot] {
			continue
		}
		seen[kit.KitRoot] = true
		behavior.KitRoots = append(behavior.KitRoots, kit.KitRoot)
	}

	// Post-merge validation: kind: agent hooks must have a Consumer. kit
	// inheritance may have filled it in; if still empty, the kit had no consumer
	// name to inherit, which is a configuration error.
	for _, h := range behavior.Hooks {
		if h.Kind == HandlerKindAgent && h.Consumer == "" {
			return fmt.Errorf("hook %q: kind: agent requires Consumer (kit has no consumer name to inherit)", h.ID)
		}
	}

	return nil
}

func unionBindMountSlices(base, extra []BindMount) []BindMount {
	indexBySource := make(map[string]int)
	var result []BindMount
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
	for _, binding := range extra {
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
		mounts[i].Target = interpolateEnv(mounts[i].Target)
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
	result.Kits = append([]KitRef(nil), meta.Kits...)
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = cloneHostCommands(meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = cloneTaskBehaviorMap(meta.TaskBehaviors)
	result.Commands = cloneCommandSpecMap(meta.Commands)
	return &result
}

// cloneCommandSpecMap deep-copies the commands map. Resolved fields are reset
// to their zero values; the caller must recompute them via ReadProjectMetaWithKits.
func cloneCommandSpecMap(src map[string]CommandSpec) map[string]CommandSpec {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]CommandSpec, len(src))
	for k, v := range src {
		v.Command = append([]string(nil), v.Command...)
		v.ResolvedCommand = nil
		v.Env = nil
		v.HostCommands = nil
		v.AdditionalBindings = nil
		result[k] = v
	}
	return result
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
		v.HostCommands = nil
		v.AdditionalBindings = nil
		v.KitRoots = nil
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

// validateBuiltinHostConflict rejects host_commands entries for names that are
// always available as builtins (git, boid). Those names are broker-mediated by
// the sandbox runtime and cannot be redirected to a host binary.
func validateBuiltinHostConflict(scope string, hostCommands HostCommands) error {
	for _, name := range []string{"git", "boid"} {
		if _, conflict := hostCommands[name]; conflict {
			return fmt.Errorf("%s: %q is a builtin command and cannot be declared in host_commands", scope, name)
		}
	}
	return nil
}
