package orchestrator

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type KitResolver interface {
	Resolve(ref string) (string, error)
}

// KitRuntime holds the merged runtime fields derived from a set of kits.
// It covers env, host_commands, and additional_bindings. Hooks and directory
// metadata are excluded — those are TaskBehavior-specific and handled
// by MergeKitMetaIntoBehavior.
type KitRuntime struct {
	AdditionalBindings []BindMount
	HostCommands       HostCommands
	Env                map[string]string
}

// MergeKitRuntime merges env, host_commands, and additional_bindings from the
// given kits into a KitRuntime value. kitAgents provides display names for
// error messages (one per kit).
func MergeKitRuntime(kits []*KitMeta, kitAgents []string) (KitRuntime, error) {
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
		agent := ""
		if i < len(kitAgents) {
			agent = kitAgents[i]
		}
		for k, v := range kit.HostCommands {
			if existingAgent, ok := kitCmdSource[k]; ok {
				return rt, fmt.Errorf("host_commands: command %q is defined in both kit %q and kit %q; remove the duplicate from one kit or override it in workspace.yaml", k, existingAgent, agent)
			}
			kitCmdSource[k] = agent
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
		return nil, fmt.Errorf("%s: read: %w", yamlPath, err)
	}

	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", yamlPath, err)
	}
	if _, ok := raw["workspace_id"]; ok {
		return nil, fmt.Errorf("project.yaml: workspace_id is no longer supported; assign workspace via boid workspace assign <project-id> <workspace-id>")
	}
	for _, field := range []string{"hooks", "gates"} {
		if _, ok := raw[field]; ok {
			return nil, fmt.Errorf("project.yaml: top-level %q is no longer supported; move it into task_behaviors.<name>.kits (for kits) or define inside a local kit under .boid/kits/", field)
		}
	}

	if migErr := rejectRemovedProjectFields(dir, raw); migErr != nil {
		return nil, migErr
	}

	if err := rejectRemovedBehaviorFields("project.yaml", raw); err != nil {
		return nil, err
	}

	warnDeprecatedCommandsKey("project.yaml", dir, raw)

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

	// Resolve hook ScriptPaths from the project's .boid/hooks/ directory.
	// Non-agent hooks (Kind != "agent") require a script file; agent-kind hooks
	// may omit the script and are dispatched to the HarnessAdapter directly.
	projectHooksDir := filepath.Join(dir, ".boid", "hooks")
	for name, behavior := range meta.TaskBehaviors {
		for i := range behavior.Hooks {
			h := &behavior.Hooks[i]
			if err := validateHookKind(h); err != nil {
				return nil, fmt.Errorf("project.yaml: task_behaviors.%s: %w", name, err)
			}
			if h.Kind == HandlerKindAgent {
				continue // agent hooks do not need a script path
			}
			scriptPath, err := ResolveHookScript(projectHooksDir, h.ID)
			if err != nil {
				return nil, fmt.Errorf("project.yaml: task_behaviors.%s: hook %q: %w", name, h.ID, err)
			}
			h.ScriptPath = scriptPath
		}
		meta.TaskBehaviors[name] = behavior
	}

	if meta.ID == "" {
		return nil, fmt.Errorf("project.yaml: id is required")
	}
	if meta.Name == "" {
		return nil, fmt.Errorf("project.yaml: name is required")
	}

	normalized, err := normalizeBehaviorAliases("project.yaml", meta.TaskBehaviors)
	if err != nil {
		return nil, err
	}
	meta.TaskBehaviors = normalized

	// Add back-compat alias mirror entries so legacy lookups
	// (meta.TaskBehaviors["dev"]) continue to find the canonical entry.
	// Callers iterating the map should use the canonical entries as the
	// source of truth; ReadProjectMetaWithKits explicitly strips and re-adds
	// mirrors to avoid double-processing during kit merging.
	meta.TaskBehaviors = addAliasMirrors(meta.TaskBehaviors)

	return &meta, nil
}

// commandsDeprecationWarned tracks which (scope+dir) pairs have already received
// the Phase 3-d commands: deprecation warning this daemon run. Resets on daemon
// restart. Both project.yaml and kit.yaml share the same map so each location
// warns at most once.
var commandsDeprecationWarned sync.Map

// warnDeprecatedCommandsKey emits a Phase 3-d deprecation warning when the raw
// YAML still carries a top-level or per-task-behavior commands: key. The schema
// was removed when ProjectCommand / BehaviorCommand was retired; remaining keys
// are silently ignored at the loader level (no parse error), but users should
// be told once so they can clean up their project.yaml / kit.yaml.
//
// scope identifies the file ("project.yaml" / "kit.yaml (<dir>)") for the log
// message; dir is the deduplication key so the same project / kit does not
// re-emit on every reload.
func warnDeprecatedCommandsKey(scope, dir string, raw map[string]any) {
	emit := func(suffix string) {
		key := scope + "@" + dir + suffix
		if _, loaded := commandsDeprecationWarned.LoadOrStore(key, true); loaded {
			return
		}
		slog.Warn(scope+": 'commands:' is deprecated and ignored. Use 'boid agent <harness> -p <project>' to start an agent session, or 'boid exec -p <project> -- <argv...>' to run a one-off command.",
			"location", dir, "key", "commands"+suffix)
	}
	if _, ok := raw["commands"]; ok {
		emit("")
	}
	if behaviors, ok := raw["task_behaviors"].(map[string]any); ok {
		names := make([]string, 0, len(behaviors))
		for k := range behaviors {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			entry, ok := behaviors[name].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := entry["commands"]; ok {
				emit(" (task_behaviors." + name + ")")
			}
		}
	}
}

// removedTopLevelKeys lists the project.yaml top-level keys that have been
// removed in the new schema and must not appear in project.yaml any more.
// They are migrated to workspace.yaml (env/host_commands/additional_bindings/
// capabilities) or runtime-injected (secret_namespace).
var removedTopLevelKeys = []string{
	"kits",
	"env",
	"host_commands",
	"additional_bindings",
	"secret_namespace",
	"capabilities",
}

// migrationGuidance returns the multi-line guidance block for removed-key errors.
func migrationGuidance(dir string) string {
	return "Migration:\n" +
		"  1) Run: boid project migrate " + dir + "           (dry-run)\n" +
		"  2) Confirm the plan, then re-run with --apply\n" +
		"See docs/ja/guide/migration.md for details."
}

// rejectRemovedProjectFields scans the raw YAML map for top-level keys and
// task_behaviors-level kits that have been removed from the new project.yaml
// schema. Returns a *ProjectMigrationError (single-issue) when any violation
// is found; collects all violations so the user sees them all at once.
// Returns nil when there are no violations.
//
// The Error() output of the returned value is byte-identical to the legacy
// string-error form so existing tests that check via strings.Contains pass
// unchanged.
func rejectRemovedProjectFields(dir string, raw map[string]any) *ProjectMigrationError {
	var msgs []string

	// Check top-level removed keys.
	for _, key := range removedTopLevelKeys {
		if _, ok := raw[key]; ok {
			msgs = append(msgs, fmt.Sprintf("project.yaml: top-level %q is no longer supported.", key))
		}
	}

	// Check behavior-level kits.
	if behaviors, ok := raw["task_behaviors"].(map[string]any); ok {
		names := make([]string, 0, len(behaviors))
		for k := range behaviors {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			entry, ok := behaviors[name].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := entry["kits"]; ok {
				msgs = append(msgs, fmt.Sprintf("project.yaml: task_behaviors.%s.kits is no longer supported.", name))
			}
		}
	}

	if len(msgs) == 0 {
		return nil
	}
	return &ProjectMigrationError{
		Projects: []ProjectMigrationIssue{{
			Dir:      dir,
			Messages: msgs,
		}},
	}
}

// removedBehaviorFieldGuidance maps each removed task_behaviors.<name>.<field>
// to a human-readable message that explains the new resolution path. Keeping
// the messages in a table (rather than inline error literals) ensures that
// project.yaml and kit.yaml report the same migration guidance for the same
// field — and lets the tests assert against a single source of truth.
var removedBehaviorFieldGuidance = map[string]string{
	"worktree":        "worktree is determined by the project-top 'worktree' field combined with the behavior name (supervisor/executor)",
	"base_branch":     "base_branch is resolved from the project-top 'base_branch' field (with ${TASK_REMOTE_ID} / ${current_branch} expansion)",
	"branch_prefix":   "branch_prefix is no longer configurable; worktree branches are always created under 'boid/'",
	"default_payload": "default_payload is no longer supported; provide payload data at task creation time instead",
}

// rejectRemovedBehaviorFields scans the raw YAML map for any task_behaviors
// entry that still carries one of the fields removed in Phase 3-1
// (readonly / worktree / base_branch / branch_prefix / default_payload) and
// returns a descriptive load-time error pointing callers at the new
// resolution path. scope identifies the source ("project.yaml" / "kit.yaml
// <dir>") for error messages.
func rejectRemovedBehaviorFields(scope string, raw map[string]any) error {
	behaviors, ok := raw["task_behaviors"].(map[string]any)
	if !ok {
		return nil
	}
	// Preserve a stable key order so the same fixture always produces the
	// same error message (helpful for tests that match on substring).
	names := make([]string, 0, len(behaviors))
	for k := range behaviors {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		entry, ok := behaviors[name].(map[string]any)
		if !ok {
			continue
		}
		fields := make([]string, 0, len(entry))
		for k := range entry {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		for _, field := range fields {
			guidance, removed := removedBehaviorFieldGuidance[field]
			if !removed {
				continue
			}
			return fmt.Errorf("%s: task_behaviors.%s.%s is no longer supported; %s",
				scope, name, field, guidance)
		}
	}
	return nil
}

// normalizeBehaviorAliases rewrites the task_behaviors map so that any
// deprecated alias key (see BehaviorAliases) is renamed to its canonical
// counterpart. A deprecation warning is emitted for each alias seen. If both
// the alias and its canonical counterpart are present in the same map, an
// error is returned — authors must pick exactly one form per behavior.
//
// scope identifies the source ("project.yaml" or "kit.yaml ...") for logs and
// error messages.
//
// To keep the rename non-breaking for callers that look up behaviors by the
// legacy name, addAliasMirrors must be called on the FINAL fully-resolved map
// (after kit merging, etc.) to add back-compat mirror entries. See
// ReadProjectMetaWithKits.
func normalizeBehaviorAliases(scope string, behaviors map[string]TaskBehavior) (map[string]TaskBehavior, error) {
	if len(behaviors) == 0 {
		return behaviors, nil
	}
	// Detect duplicates first (alias and canonical both present): fail fast.
	for alias, canonical := range BehaviorAliases {
		if _, hasAlias := behaviors[alias]; !hasAlias {
			continue
		}
		if _, hasCanonical := behaviors[canonical]; hasCanonical {
			return nil, fmt.Errorf("%s: duplicate task behavior definition: %q is an alias of %q; remove one", scope, alias, canonical)
		}
	}
	result := make(map[string]TaskBehavior, len(behaviors))
	for key, behavior := range behaviors {
		canonical, isAlias := CanonicalBehaviorName(key)
		if isAlias {
			slog.Warn("task behavior name is deprecated; use canonical name instead",
				"scope", scope,
				"deprecated", key,
				"canonical", canonical,
			)
			result[canonical] = behavior
			continue
		}
		result[key] = behavior
	}
	return result, nil
}

// addAliasMirrors adds back-compat alias key mirror entries to a fully
// processed task_behaviors map. For every canonical behavior present whose
// name has a known alias, the alias key is set to the same TaskBehavior
// value. Existing alias entries are never overwritten.
//
// This is the migration-period back-compat: callers that look up by the
// legacy name (e.g. existing tests, persisted task.Behavior rows that still
// carry "dev") continue to find their behavior, while new callers can use
// the canonical name. Phase 5 will drop the mirroring step entirely.
func addAliasMirrors(behaviors map[string]TaskBehavior) map[string]TaskBehavior {
	if len(behaviors) == 0 {
		return behaviors
	}
	for alias, canonical := range BehaviorAliases {
		if _, exists := behaviors[alias]; exists {
			continue
		}
		entry, ok := behaviors[canonical]
		if !ok {
			continue
		}
		behaviors[alias] = entry
	}
	return behaviors
}

// stripAliasMirrors removes any back-compat alias key entries that have a
// matching canonical entry. It is the inverse of addAliasMirrors and is used
// before re-processing a meta map (e.g. in ReadProjectMetaWithKits, which
// iterates over the map to merge kits — alias entries would cause every
// behavior to be processed twice).
//
// Only the canonical-resolvable aliases are stripped; if the map happens to
// contain an alias key WITHOUT its canonical counterpart, the entry is left
// alone (it represents a legitimate user-authored alias-only definition that
// has not yet been canonicalized).
func stripAliasMirrors(behaviors map[string]TaskBehavior) map[string]TaskBehavior {
	if len(behaviors) == 0 {
		return behaviors
	}
	for alias, canonical := range BehaviorAliases {
		if _, hasAlias := behaviors[alias]; !hasAlias {
			continue
		}
		if _, hasCanonical := behaviors[canonical]; !hasCanonical {
			continue
		}
		delete(behaviors, alias)
	}
	return behaviors
}

// validateHookKind enforces the Hook.Kind / Hook.Agent invariants at load time:
//   - Kind must be "" or "agent"
//   - Agent can only be specified on kind: agent hooks; on non-agent hooks
//     it has no effect and likely indicates that `kind: agent` was forgotten
//
// Agent hooks without an Agent are allowed here (the kit-agent inheritance
// in MergeKitMetaIntoBehavior may still fill it in); the final "agent requires
// agent" check happens after kit merge.
func validateHookKind(h *Hook) error {
	if !h.Kind.IsValid() {
		return fmt.Errorf("hook %q: invalid kind %q (allowed: \"\" or \"agent\")", h.ID, h.Kind)
	}
	if h.Kind != HandlerKindAgent && h.Agent != "" {
		return fmt.Errorf("hook %q: 'agent' field requires 'kind: agent' (non-agent hooks must not declare agent)", h.ID)
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
	// "local/<name>" refers to a project-scoped kit in <projectDir>/.boid/kits/<name>.
	if strings.HasPrefix(ref, "local/") {
		name := strings.TrimPrefix(ref, "local/")
		localDir := filepath.Join(projectDir, ".boid", "kits", name)
		yamlPath := filepath.Join(localDir, "kit.yaml")
		if _, err := os.Stat(yamlPath); err != nil {
			return "", fmt.Errorf("local kit %q: kit.yaml not found at %s", ref, localDir)
		}
		return localDir, nil
	}

	// All other refs are resolved through the global kit registry.
	if resolver == nil {
		return "", fmt.Errorf("kit %q requires registry but none configured", ref)
	}
	return resolver.Resolve(ref)
}

// ResolveKitAgent derives the agent name for a kit reference.
// The last path segment of the ref is used.
func ResolveKitAgent(ref KitRef) string {
	parts := strings.Split(ref.Ref, "/")
	return parts[len(parts)-1]
}

// IsProjectScopable reports whether a kit may be placed in the top-level
// project.yaml kits field. Kits no longer provide hooks or task_behaviors,
// so all kits are project-scopable by definition.
func IsProjectScopable(km *KitMeta) error {
	return nil
}

// ReadProjectMetaWithKits reads project.yaml and merges project-level overlays
// into each behavior.
// Returns a ProjectMeta whose TaskBehaviors have their resolved Hooks/etc.
// populated and ready for dispatch.
//
// Note: kits are no longer supported in project.yaml (removed in the new schema).
// Workspace kits are resolved at GetWithWorkspace time via WorkspaceStore.
// project.local.yaml is deprecated; use workspace.yaml instead.
func ReadProjectMetaWithKits(dir string, resolver KitResolver) (*ProjectMeta, error) {
	meta, err := ReadProjectMeta(dir)
	if err != nil {
		return nil, err
	}

	meta = cloneProjectMeta(meta)

	// Drop alias mirror entries added by ReadProjectMeta so that the
	// behavior-merge loop below iterates each behavior only once. Mirrors are
	// re-added at the end of this function.
	meta.TaskBehaviors = stripAliasMirrors(meta.TaskBehaviors)

	if meta.TaskBehaviors == nil {
		meta.TaskBehaviors = make(map[string]TaskBehavior)
	}

	// For each behavior, merge project-level overlays.
	for name, behavior := range meta.TaskBehaviors {
		// Apply project.yaml-level overlay (env / host_commands / bindings).
		behavior.Env = mergeStringMaps(behavior.Env, meta.Env)
		behavior.HostCommands = mergeHostCommands(behavior.HostCommands, meta.HostCommands)
		behavior.AdditionalBindings = mergeBindMounts(behavior.AdditionalBindings, meta.AdditionalBindings)
		if err := validateBuiltinHostConflict(fmt.Sprintf("behavior %q", name), behavior.HostCommands); err != nil {
			return nil, err
		}
		if err := validateRejectRules(behavior.HostCommands); err != nil {
			return nil, fmt.Errorf("behavior %q: %w", name, err)
		}

		meta.TaskBehaviors[name] = behavior
	}

	// Re-add the back-compat alias mirror entries that were stripped at the
	// top of this function. After overlays, the canonical entries are fully
	// populated; mirrors must reflect the resolved state so legacy lookups
	// (e.g. meta.TaskBehaviors["dev"]) see the same data.
	meta.TaskBehaviors = addAliasMirrors(meta.TaskBehaviors)

	emitCanonicalBehaviorDeprecation(dir, meta)

	return meta, nil
}

// canonicalBehaviorWarnedProjects tracks which project directories have already
// received the canonical-name deprecation warning this daemon run (keyed by
// absolute directory path). Resets on daemon restart.
var canonicalBehaviorWarnedProjects sync.Map

// emitCanonicalBehaviorDeprecation logs deprecation warnings when the project
// uses the canonical behavior names "supervisor" or "executor". These names
// are deprecated in favour of free naming (Track A2). Fires at most once per
// project directory per daemon run. Suppressed by BOID_NO_DEPRECATION_WARN=1.
func emitCanonicalBehaviorDeprecation(dir string, meta *ProjectMeta) {
	if os.Getenv("BOID_NO_DEPRECATION_WARN") == "1" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	if _, alreadyWarned := canonicalBehaviorWarnedProjects.LoadOrStore(abs, struct{}{}); alreadyWarned {
		return
	}
	for name, behavior := range meta.TaskBehaviors {
		if IsBehaviorAliasKey(name) {
			continue // skip back-compat mirror entries
		}
		switch name {
		case "supervisor":
			slog.Warn("task behavior name 'supervisor' is deprecated; rename to a project-specific name and set default_task_behavior. See docs/ja/reference/task-behavior-migration.md",
				"project_id", meta.ID, "behavior", name)
		case "executor":
			slog.Warn("task behavior name 'executor' is deprecated; rename to a project-specific name and set default_task_behavior. See docs/ja/reference/task-behavior-migration.md",
				"project_id", meta.ID, "behavior", name)
			if behavior.Readonly == nil {
				slog.Warn("executor behavior has no explicit 'readonly: false'; applying readonly=false for backward compatibility. Set 'readonly: false' in task_behaviors.executor to silence this warning.",
					"project_id", meta.ID, "behavior", name)
			}
		}
	}
}

func ReadKitMeta(dir string) (*KitMeta, error) {
	yamlPath := filepath.Join(dir, "kit.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("read kit.yaml: %w", err)
	}

	var rawTop map[string]any
	if err := yaml.Unmarshal(data, &rawTop); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}
	if err := rejectRemovedBehaviorFields(fmt.Sprintf("kit.yaml (%s)", dir), rawTop); err != nil {
		return nil, err
	}

	warnDeprecatedCommandsKey(fmt.Sprintf("kit.yaml (%s)", dir), dir, rawTop)

	var meta KitMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse kit.yaml: %w", err)
	}

	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	warnDeprecatedStdin(fmt.Sprintf("kit.yaml (%s)", dir), dir, meta.HostCommands)

	meta.KitRoot = dir

	if _, ok := rawTop["scripts"]; ok {
		return nil, fmt.Errorf("kit.yaml: 'scripts:' section is no longer supported")
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

	warnDeprecatedStdin(projectLocalFilename+" ("+dir+")", dir, meta.HostCommands)

	if err := validateProjectLocalMeta(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// MergeKitMetaIntoBehavior merges kit-provided hooks, env, bindings, and
// host_commands into the given TaskBehavior. Kit hook IDs are prefixed
// with the agent name. The behavior is modified in place.
func MergeKitMetaIntoBehavior(behavior *TaskBehavior, kits []*KitMeta, kitAgents []string) error {
	if len(kits) == 0 {
		return nil
	}

	rt, err := MergeKitRuntime(kits, kitAgents)
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
		mounts[i].Source = interpolateBindMountField(mounts[i].Source)
		mounts[i].Target = interpolateBindMountField(mounts[i].Target)
	}
}

// interpolateBindMountField は通常の env 展開を行いつつ、
// ${WORKTREE} と ${PROJECT_WORKDIR} は dispatch 時にタスク毎に解決するため
// literal のまま温存する。
func interpolateBindMountField(s string) string {
	return os.Expand(s, func(name string) string {
		if name == "WORKTREE" || name == "PROJECT_WORKDIR" {
			return "${" + name + "}"
		}
		return os.Getenv(name)
	})
}

func interpolateEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = interpolateEnv(v)
	}
}

func interpolateHostCommands(cmds HostCommands) {
	for name, spec := range cmds {
		spec.Path = interpolateEnv(spec.Path)
		interpolateHostCommandEnvMap(spec.Env)
		cmds[name] = spec
	}
}

// interpolateHostCommandEnvMap expands ${VAR} from the host environment like
// interpolateEnvMap, but preserves ${boid:...} context variables literally —
// they are resolved per dispatch at token-registration time by
// dispatcher.ResolveHostCommands (e.g. ${boid:repo_slug} from the project's
// origin remote), not from the daemon's environment. Without this carve-out,
// os.Expand would swallow them at load time (no env var named "boid:..."
// exists) and the placeholder would silently expand to "". Same pattern as
// interpolateBindMountField's ${WORKTREE} / ${PROJECT_WORKDIR} preservation.
func interpolateHostCommandEnvMap(m map[string]string) {
	for k, v := range m {
		m[k] = os.Expand(v, func(name string) string {
			if strings.HasPrefix(name, "boid:") {
				return "${" + name + "}"
			}
			return os.Getenv(name)
		})
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

// cloneTaskBehaviorMap deep-copies the task behavior map. Runtime-overlay fields
// (Env, HostCommands, AdditionalBindings, KitRoots) are reset to nil so callers
// can reapply overlays from scratch. Hooks are preserved because they are now
// defined in project.yaml (not kit-supplied) and must survive the clone.
func cloneTaskBehaviorMap(src map[string]TaskBehavior) map[string]TaskBehavior {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]TaskBehavior, len(src))
	for k, v := range src {
		// Preserve Hooks: they come from project.yaml, not from runtime overlays.
		if len(v.Hooks) > 0 {
			hooks := make([]Hook, len(v.Hooks))
			copy(hooks, v.Hooks)
			v.Hooks = hooks
		}
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
	for _, name := range []string{"git", "boid", "fetch"} {
		if _, conflict := hostCommands[name]; conflict {
			return fmt.Errorf("%s: %q is a builtin command and cannot be declared in host_commands", scope, name)
		}
	}
	return nil
}

// validateRejectRules rejects host_commands reject entries that lack a match
// pattern or a reason. A reject rule without a reason would surface a bare
// rejection to the agent with no way to self-correct, so both fields are
// mandatory.
func validateRejectRules(hostCommands HostCommands) error {
	names := make([]string, 0, len(hostCommands))
	for name := range hostCommands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for i, rule := range hostCommands[name].Reject {
			if rule.Match == "" {
				return fmt.Errorf("host_commands.%s.reject[%d]: match is required", name, i)
			}
			if rule.Reason == "" {
				return fmt.Errorf("host_commands.%s.reject[%d]: reason is required", name, i)
			}
		}
	}
	return nil
}

// stdinDeprecationWarned tracks which (dir, command) pairs have already
// received the stdin deprecation warning this daemon run. Resets on daemon
// restart. Same idiom as commandsDeprecationWarned above.
var stdinDeprecationWarned sync.Map

// warnDeprecatedStdin emits a deprecation warning for host_commands entries
// that still declare stdin: true. The field is still parsed but has no
// effect: the broker never wires caller stdin into a host command (see
// gateHostCommand in internal/sandbox/broker.go). dir is the deduplication key
// so the same file does not re-emit on every reload.
func warnDeprecatedStdin(scope, dir string, hostCommands HostCommands) {
	for name, spec := range hostCommands {
		if !spec.Stdin {
			continue
		}
		key := dir + "@" + name
		if _, loaded := stdinDeprecationWarned.LoadOrStore(key, true); loaded {
			continue
		}
		slog.Warn(fmt.Sprintf("host_commands.%s: stdin: true is deprecated and has no effect; host commands never receive caller stdin", name),
			"location", scope)
	}
}
