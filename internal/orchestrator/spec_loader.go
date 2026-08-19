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

const projectLocalFilename = "project.local.yaml"

// removedTopLevelKeyGuidance maps a top-level project.yaml key rejected by
// the loop below to a field-specific migration message. Both keys predate
// the current schema and were removed for different reasons, so a single
// shared message (as this used to say — "move it into
// task_behaviors.<name>.kits ... or define inside a local kit") is wrong for
// either of them: "hooks" never had anything to do with kits (kits do not
// supply hooks — see kit.Meta's "Kits do not provide hooks or task_behaviors"
// doc comment; the authoritative location is task_behaviors.<name>.hooks,
// which project.yaml itself already supports), and "gates" named the Gate
// mechanism, which was retired outright with no replacement to move to.
var removedTopLevelKeyGuidance = map[string]string{
	"hooks": "move each hook into task_behaviors.<name>.hooks instead (project.yaml is the authoritative source for hooks; kits do not supply them)",
	"gates": "the Gate mechanism has been retired entirely (dispatch is hook-only now); there is no replacement field, just remove it",
}

func ReadProjectMeta(dir string) (*ProjectMeta, error) {
	yamlPath := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("%s: read: %w", yamlPath, err)
	}

	meta, err := parseProjectMetaBytes(dir, false, data)
	if err != nil {
		return nil, err
	}

	return meta, nil
}

// parseProjectMetaBytes runs the project.yaml validation/unmarshal pipeline
// shared between the filesystem-based loader (ReadProjectMeta, above) and
// the daemon-managed bare-repo loader (ReadProjectMetaFromBareRepo,
// project_bare_repo.go — docs/plans/volume-only-daemon.md §論点a: "project.yaml
// は bare repo の HEAD (default branch) から `git show HEAD:.boid/project.yaml`
// で読む"). dirLabel is used for error text and ProjectMigrationIssue.Dir
// (rejectRemovedProjectFields) either way, but isBareRepo (codex round-9
// review of PR5, Blocker 1) tells rejectRemovedProjectFields/
// migrationGuidance which of the two callers this is: a real filesystem
// directory the caller can `boid project migrate` directly (isBareRepo
// false), or the DAEMON's own internal bare-repository path for a git-URL-
// registered project (isBareRepo true) — nothing the CLI can run
// `boid project migrate` against directly at all (it reads
// <dir>/.boid/project.yaml off a real filesystem, cmd/project_migrate.go),
// so the guidance for that case must say so instead of asserting a command
// against a path the user cannot use.
func parseProjectMetaBytes(dirLabel string, isBareRepo bool, data []byte) (*ProjectMeta, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: parse: %w", dirLabel, err)
	}
	if _, ok := raw["workspace_id"]; ok {
		return nil, fmt.Errorf("project.yaml: workspace_id is no longer supported; assign workspace via boid workspace assign <project-id> <workspace-id>")
	}
	for _, field := range []string{"hooks", "gates"} {
		if _, ok := raw[field]; ok {
			return nil, fmt.Errorf("project.yaml: top-level %q is no longer supported; %s", field, removedTopLevelKeyGuidance[field])
		}
	}

	if migErr := rejectRemovedProjectFields(dirLabel, isBareRepo, raw); migErr != nil {
		return nil, migErr
	}

	if err := rejectRemovedBehaviorFields("project.yaml", raw); err != nil {
		return nil, err
	}

	warnDeprecatedCommandsKey("project.yaml", dirLabel, raw)

	var meta ProjectMeta
	if err := yaml.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse project.yaml: %w", err)
	}
	interpolateBindMounts(meta.AdditionalBindings)
	interpolateHostCommands(meta.HostCommands)
	interpolateEnvMap(meta.Env)

	// Validate hook kind/agent/command invariants at load time. This is
	// defense-in-depth alongside the runtime check in
	// DispatchPlanner.validateHookCommandFields (see validateHookKind's doc
	// comment): load-time rejects malformed YAML shapes, runtime catches
	// programmatic construction / kit-merge drift. Prior to
	// docs/plans/script-hook-removal.md PR3 this loop also resolved each
	// hook's backing .boid/hooks/<id>.(sh|py) script into a runtime-only
	// field on Hook; that field and its resolution were removed once every
	// hook had migrated to the inline `command:` field or agent-kind
	// dispatch.
	for name, behavior := range meta.TaskBehaviors {
		for i := range behavior.Hooks {
			if err := validateHookKind(&behavior.Hooks[i]); err != nil {
				return nil, fmt.Errorf("project.yaml: task_behaviors.%s: %w", name, err)
			}
		}
	}

	// docs/plans/ingestion-identity.md PR-4 (B-5): same load-time posture as
	// the hook validation loop above — a malformed `triggers[]` entry (empty
	// name/run, unparseable/non-positive every, duplicate name) must fail
	// `boid project add`/`boid project fetch` loudly rather than silently
	// never firing at runtime. An OLDER daemon binary (pre-PR-4) parses
	// project.yaml non-strictly (yaml.Unmarshal above has no KnownFields) and
	// has no Triggers field to decode into at all, so it ignores `triggers:`
	// with no warning — accepted (see spec_types.go's Trigger doc comment).
	if err := ValidateTriggers(meta.Triggers); err != nil {
		return nil, err
	}

	// docs/plans/workspace-default-project.md 論点h 案1 (PR7): `id:` is
	// optional. project.yaml declaring one is validated no further here
	// (the PK-uniqueness / id-drift checks live at the registration/reload
	// call sites, orchestrator.ProjectStore.LoadBareRepoExpectingID /
	// LoadExpectingID and internal/api's CreateProjectFromGitURL); omitting
	// it entirely falls through to the caller's URL-derived id (the exact
	// same derivation a wholly missing project.yaml already gets — see
	// internal/api/project_no_yaml.go's deriveProjectIDFromURL). This used
	// to be a hard requirement ("project.yaml: id is required"); removing
	// it is safe for the overwhelming majority of existing project.yaml
	// files, which declare `id:` explicitly and are completely unaffected.
	//
	// A NON-empty id is still validated against the URLDerivedProjectIDPrefix
	// reservation below (Codex review round 2 Major, PR6, 論点e) — PR7 only
	// widens the empty-id case to "optional, not an error"; it does not
	// relax PR6's rule that a hand-authored id may never collide with the
	// prefix reserved for URL-derived ids. This is additional, load-time
	// defense-in-depth alongside PR7's own provenance verification
	// (ProjectStore.reconcileExpectedProjectID / isVerifiedURLDerivedID,
	// which confirm a registered url-derived id actually came from the
	// project's UpstreamURL rather than trusting the prefix alone) —
	// belt-and-suspenders against the same hand-authored-`url-`-id class of
	// bug, enforced at two different points in the system.
	if meta.ID != "" && IsURLDerivedProjectID(meta.ID) {
		return nil, fmt.Errorf("project.yaml: id must not start with %q (reserved for project.yaml-less registrations, docs/plans/workspace-default-project.md 決定3)", URLDerivedProjectIDPrefix)
	}
	if strings.TrimSpace(meta.Name) == "" {
		// PR-1d codex round-5 Minor: a bare `meta.Name == ""` check let a
		// whitespace-only name (e.g. `name: "  "`) through project.yaml
		// validation, while workspace apply's resolveWorkspaceApplyProjectNames
		// (internal/api/project_service.go) already refuses a
		// whitespace-only spec.projects[].name via
		// strings.TrimSpace(ep.Name) == "" — a project registered with such a
		// name could be exported successfully (ExportWorkspaceEnvelopes only
		// checked the same bare == "" before this fix) but its own export
		// could never be applied back: apply would fold the whitespace name
		// into "missing" and silently detach the project. Rejecting it here,
		// at the source, closes the round-trip gap at its root instead of
		// only patching each downstream consumer separately.
		return nil, fmt.Errorf("project.yaml: name is required")
	}

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

// migrationGuidance returns the multi-line guidance block for removed-key
// errors. docs/plans/release-onboarding.md 決定2/PR5 — this went through
// SEVEN rounds of codex review (round-2 through round-7). Every attempt
// at a step-by-step recovery recipe (automated via --apply, or a manual
// command sequence promising to reach the same end state) turned out
// wrong on closer inspection somewhere — a daemon-topology hole, a
// format mismatch between two commands, a step that silently loses data,
// or (round-7) cases this project's own registration model (git-URL
// bare-repo caching, host_commands definitions with no create/edit API
// at all) has no fully general answer for yet at all. See
// cmd/project_migrate.go's guardApply for the detailed history of what
// was tried and why each attempt was rejected.
//
// This function has stopped trying to promise a recipe it cannot
// guarantee. It states the fact (which fields are invalid) and points
// at the docs/command that own the actual up-to-date answer, rather than
// asserting specific steps here that static review keeps finding holes
// in only after the fact.
func migrationGuidance(dir string, isBareRepo bool) string {
	if isBareRepo {
		// codex round-9 review of PR5, Blocker 1: dir here is the
		// DAEMON's own internal bare-repository path for a git-URL-
		// registered project (project_bare_repo.go's
		// ReadProjectMetaFromBareRepo) — not a path the user has
		// filesystem access to, and not something `boid project migrate`
		// (which reads <dir>/.boid/project.yaml off a real, user-owned
		// filesystem directory) can be run against at all. Telling the
		// user to run it against dir would be flatly wrong, not just
		// "less convenient" — point at their OWN clone instead.
		return "Migration:\n" +
			"  project.yaml uses fields removed in the new schema (listed above).\n" +
			"  This project is registered via a git URL; the daemon reads project.yaml\n" +
			"  from its own managed bare repository (" + dir + "), which is NOT a path\n" +
			"  you can run `boid project migrate` against. Run it against YOUR OWN local\n" +
			"  clone of this project's repository instead, then push the fix — see\n" +
			"  docs/ja/guide/migration.md."
	}
	// codex round-9 review of PR5, Minor: "shows exactly what would move"
	// overstated it after cmd/project_migrate.go's dry-run stopped
	// touching the daemon's DB at all (round-8 fix) — dry-run now reads
	// only project.yaml and explicitly SKIPS the workspace-assignment and
	// secret-collision checks that used to make that claim true, printing
	// its own "(dry-run) skipping ..." notes when it does. Weakened to
	// describe what dry-run actually shows: a plan derived from
	// project.yaml alone.
	return "Migration:\n" +
		"  project.yaml uses fields removed in the new schema (listed above).\n" +
		"  `boid project migrate " + dir + "` (dry-run) shows the migration plan derived\n" +
		"  from project.yaml (it does not check the daemon's DB — see its own output\n" +
		"  for what it skips as a result).\n" +
		"  Automated --apply is a legacy, pre-compose, bare-metal-only path (requires\n" +
		"  --legacy-bare-metal) — see `boid project migrate --help` and\n" +
		"  docs/ja/guide/migration.md for what it does and does not cover for a\n" +
		"  project registered with a compose daemon."
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
func rejectRemovedProjectFields(dir string, isBareRepo bool, raw map[string]any) *ProjectMigrationError {
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
			Dir:        dir,
			IsBareRepo: isBareRepo,
			Messages:   msgs,
		}},
	}
}

// removedBehaviorFieldGuidance maps each removed task_behaviors.<name>.<field>
// to a human-readable message that explains the new resolution path. Keeping
// the messages in a table (rather than inline error literals) ensures that
// project.yaml and kit.yaml report the same migration guidance for the same
// field — and lets the tests assert against a single source of truth.
var removedBehaviorFieldGuidance = map[string]string{
	"worktree":        "worktree is no longer configurable; every project-visible job runs in a fresh sandbox clone (branch-policy-simplification Phase 2)",
	"base_branch":     "base_branch is resolved from the project-top 'base_branch' field (with ${TASK_REMOTE_ID} / ${current_branch} expansion)",
	"branch_prefix":   "branch_prefix is no longer configurable; the per-task branch concept was retired (branch-policy-simplification Phase 1)",
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

// validateWorkspaceDefaultTaskBehaviors runs the load-time validation
// project.yaml's task_behaviors goes through (parseProjectMetaBytes above:
// validateHookKind per hook) against a workspace's default project definition
// task_behaviors (docs/plans/workspace-default-project.md 決定4, 論点j).
// scope identifies the caller for error messages (e.g. "workspace default"
// or "workspace %q").
//
// The map is returned unchanged: behavior names are compared verbatim
// everywhere, so there is nothing to normalize. This function used to also
// run alias canonicalization, and its two call sites (envelope decode AND DB
// save) are the reason the alias machinery had to be kept out of persisted
// and exported storage — a mirror entry written to the workspaces
// .task_behaviors column made the workspace's own export un-re-appliable,
// because decode then saw both spellings and rejected the pair as ambiguous.
// With the alias table gone that hazard no longer exists in any form, but the
// invariant it forced is worth keeping on purpose: what this function returns
// is exactly what the user wrote, and exactly what gets persisted and
// re-exported.
func validateWorkspaceDefaultTaskBehaviors(scope string, behaviors map[string]TaskBehavior) (map[string]TaskBehavior, error) {
	if len(behaviors) == 0 {
		return behaviors, nil
	}
	for name, behavior := range behaviors {
		for i := range behavior.Hooks {
			if err := validateHookKind(&behavior.Hooks[i]); err != nil {
				return nil, fmt.Errorf("%s: task_behaviors.%s: %w", scope, name, err)
			}
		}
	}
	return behaviors, nil
}

// validateHookKind enforces the Hook.Kind / Hook.Agent / Hook.Command
// invariants at load time:
//   - Kind must be "" or "agent"
//   - Agent can only be specified on kind: agent hooks; on non-agent hooks
//     it has no effect and likely indicates that `kind: agent` was forgotten
//   - Command must NOT be specified on kind: agent hooks; agent hooks are
//     dispatched to a HarnessAdapter, which builds its own argv, so an
//     inline command has nowhere to run (script-hook-removal PR1,
//     docs/plans/script-hook-removal.md). This mirrors the runtime check in
//     DispatchPlanner.validateHookCommandFields; keeping both is intentional
//     defense-in-depth (load-time rejects YAML shapes, runtime catches
//     programmatic construction / kit-merge drift).
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
	if h.Kind == HandlerKindAgent && h.Command != "" {
		return fmt.Errorf("hook %q: kind %q does not allow 'command:' (agent hooks are dispatched to a HarnessAdapter, which builds its own argv)", h.ID, h.Kind)
	}
	return nil
}

// ReadProjectMetaWithKits reads project.yaml and merges project-level overlays
// into each behavior.
// Returns a ProjectMeta whose TaskBehaviors have their resolved Hooks/etc.
// populated and ready for dispatch.
//
// Note: kits are no longer supported in project.yaml (removed in the new
// schema); the kit mechanism itself was retired in Phase 2.5 PR6
// (docs/plans/workspace-db-consolidation.md). This function used to accept a
// KitResolver parameter for call-site compatibility even though it was never
// used; PR7 removed that type and parameter outright (decision 12). The
// "WithKits" name is now a historical artifact — it's kept as-is since it's
// a widely-used, purely-internal function name, not worth a rename-only
// churn across every call site. project.local.yaml is deprecated; use
// workspace.yaml instead.
func ReadProjectMetaWithKits(dir string) (*ProjectMeta, error) {
	meta, err := ReadProjectMeta(dir)
	if err != nil {
		return nil, err
	}

	meta = cloneProjectMeta(meta)

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

func cloneProjectMeta(meta *ProjectMeta) *ProjectMeta {
	if meta == nil {
		return nil
	}

	result := *meta
	result.Env = mergeStringMaps(nil, meta.Env)
	result.HostCommands = cloneHostCommands(meta.HostCommands)
	result.AdditionalBindings = cloneBindMounts(meta.AdditionalBindings)
	result.TaskBehaviors = cloneTaskBehaviorMap(meta.TaskBehaviors)
	result.SessionBehaviors = cloneSessionBehaviorMap(meta.SessionBehaviors)
	return &result
}

// cloneTaskBehaviorMap deep-copies the task behavior map. Runtime-overlay fields
// (Env, HostCommands, AdditionalBindings) are reset to nil so callers
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
		result[k] = v
	}
	return result
}

// cloneSessionBehaviorMap deep-copies the session behavior map. Unlike
// TaskBehavior, SessionBehavior has no runtime-overlay fields to reset —
// every field is plain project.yaml data (two strings) — so a straight
// value copy of the map entries is already a deep copy.
func cloneSessionBehaviorMap(src map[string]SessionBehavior) map[string]SessionBehavior {
	if len(src) == 0 {
		return nil
	}
	result := make(map[string]SessionBehavior, len(src))
	for k, v := range src {
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

// validateBuiltinHostConflict rejects host_commands entries for names that
// the sandbox reserves. "boid" is a broker-mediated builtin with a dedicated
// bind. "fetch" is a broker builtin (`FetchRequest`). "git" is neither — it
// is a real binary reached via the base rbind of /usr — but the name stays
// reserved because a user `host_commands.git:` entry would try to overlay a
// shim onto that path and break the sandbox-side git the git gateway clone
// flow depends on. Redirecting any of these to a host binary is rejected at
// config load.
func validateBuiltinHostConflict(scope string, hostCommands HostCommands) error {
	for _, name := range []string{"git", "boid", "fetch"} {
		if _, conflict := hostCommands[name]; conflict {
			return fmt.Errorf("%s: %q is a reserved builtin/sandbox name and cannot be declared in host_commands", scope, name)
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

