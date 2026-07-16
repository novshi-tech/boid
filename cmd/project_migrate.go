package cmd

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/daemon"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// projectMigrateCmd implements "boid project migrate <dir>".
//
// This command converts an old-schema project.yaml (which may contain
// kits/env/host_commands/additional_bindings/secret_namespace/capabilities at the
// top level, or kits at the behavior level) to the new schema where those fields
// live in workspace.yaml and/or a legacy kit.yaml. It does NOT require the daemon
// to be running — project.yaml, the legacy kit, and the DB project/secret rows
// are all written via direct file/DB access.
//
// The one exception is the workspace content itself: since Phase 2.5's
// yaml->DB migration cut over, a running daemon only ever reads workspaces
// from the `workspaces` table, so --apply always applies the migrated
// content to that table too — see pushMigratedWorkspaceToDaemon. When the
// daemon is reachable, this happens over HTTP: a brand-new slug is created
// outright (POST /api/workspaces); a slug that already has a DB row has its
// current content GET-fetched and merged with the migrated fields (existing
// content is preserved, migration-sourced values win on a key collision),
// then PUT back with If-Match (MAJOR 2, codex review — a create-only POST
// used to just 409 and leave an already-existing workspace's real content
// untouched). When the daemon is NOT reachable — including `boid start
// --auto-migrate`'s call site (cmd/start.go), which by construction always
// runs after the daemon has just failed to start, so it is never reachable
// there — the same merge runs directly against the DB via
// orchestrator.WorkspaceRepository instead (MAJOR 1, codex review,
// docs/plans/workspace-db-consolidation.md): before this, --apply/auto-migrate
// silently fell back to writing only the (daemon-unread) legacy per-slug
// shadow yaml in this case, so a project already cut over to the DB-backed
// `workspaces` table never actually had its migrated fields land anywhere a
// subsequent successful daemon start would read from. The shadow yaml is
// still written either way, for review/rollback (`boid workspace
// import`/`edit --from-file`). This closes the gap identified in codex
// review MAJOR 4 (docs/plans/workspace-db-consolidation.md): before that
// fix, --apply always looked like it succeeded but never touched what a
// live daemon uses at all.
var projectMigrateCmd = &cobra.Command{
	Use:   "migrate <dir>",
	Short: "Migrate a project from the old project.yaml schema to the new workspace+kit schema (no daemon required)",
	Long: `Migrate reads the project.yaml under <dir>/.boid/ using the legacy schema,
generates a workspace.yaml update and optionally a legacy kit.yaml, and rewrites
project.yaml to remove the fields that have moved.

By default the command runs in dry-run mode: it prints what it would do without
writing anything. Pass --apply to actually perform the migration.

The daemon does not need to be running for project.yaml, the legacy kit, or
secrets to migrate. The workspace's content is always applied to the
workspaces DB table: if the daemon is reachable, --apply applies it there
over HTTP — a brand-new workspace slug is created (POST /api/workspaces),
and a slug that already exists has its current content merged with the
migrated fields and updated in place (existing content is preserved;
migration-sourced values win on a key collision). If the daemon isn't
running (e.g. boid start --auto-migrate, which runs this after the daemon
has already failed to start), the same merge is applied directly to the
database instead. Either way, a reviewable yaml file is also written, and
can still be applied by hand if wanted:
  boid workspace import <file> --slug <slug>       (brand-new slug)
  boid workspace edit <slug> --from-file <file>    (slug already exists)

Secret migration copies secrets from the old namespace (secret_namespace field)
to the workspace namespace. Use --on-collision to control behaviour when a
key already exists in the new namespace (default: refuse and list collisions).`,
	Args: cobra.ExactArgs(1),
	Annotations: map[string]string{
		annotationSkipAutostart: "skip",
		// scopeLocal: this command is explicitly "daemon-free" (see the
		// doc comment above) — all I/O is direct file/DB access.
		scopeAnnotationKey: scopeLocal,
	},
	RunE: runProjectMigrate,
}

var (
	migrateWorkspace    string
	migrateApply        bool
	migrateOnCollision  string
	migrateDBPath       string
	migrateKeyFilePath  string
)

func init() {
	projectMigrateCmd.Flags().StringVar(&migrateWorkspace, "workspace", "", "Workspace slug to assign (required if no existing project_workspaces entry)")
	projectMigrateCmd.Flags().BoolVar(&migrateApply, "apply", false, "Actually perform the migration (default: dry-run)")
	projectMigrateCmd.Flags().StringVar(&migrateOnCollision, "on-collision", "refuse", "How to handle secret key collisions: refuse (default), skip, overwrite")
	projectMigrateCmd.Flags().StringVar(&migrateDBPath, "db-path", "", "Path to the SQLite database (default: XDG data dir)")
	projectMigrateCmd.Flags().StringVar(&migrateKeyFilePath, "key-file-path", "", "Path to the secret key file (default: XDG data dir)")

	projectCmd.AddCommand(projectMigrateCmd)
}

// migratePlan holds the computed migration intent. It is populated during
// phase 1+2 (read + compute) and consumed by phase 3 (display) and phase 4 (apply).
type migratePlan struct {
	// Source
	dir     string
	meta    *orchestrator.LegacyProjectMeta
	project *orchestrator.Project // from DB (may be nil if project not registered)

	// Resolved workspace slug
	workspaceSlug string

	// Generated workspace.yaml content
	workspaceMeta *orchestrator.WorkspaceMeta

	// kitRefStrs is the normalised (last-segment) kit names collected from
	// the legacy project.yaml's top-level and behavior-level `kits:` refs
	// (collectKitNames). Stored on the plan (not just a computeMigratePlan
	// local) so pushMigratedWorkspaceToDaemon can re-run
	// mergeLegacyFieldsIntoWorkspace against a freshly-fetched live
	// workspace base (MAJOR 2, codex review).
	kitRefStrs []string

	// Whether to generate a legacy kit
	needsLegacyKit bool
	legacyKitDir   string
	legacyKitMeta  *legacyKitYAML

	// Keys to remove from project.yaml
	removeKeys []string

	// Secret migration
	oldNamespace      string
	newNamespace      string // == workspaceSlug
	secretKeys        []string
	secretCollisions  []secretCollision
}

type secretCollision struct {
	Key         string
	OldHashSame bool // true when old and new values hash to the same sha256
}

// legacyKitYAML is the shape we write for the generated legacy kit.
//
// Per the migration transformation table in docs/plans/kit-workspace-project-reorg.md,
// only host_commands + additional_bindings move into the legacy kit; env is
// migrated to workspace.yaml. We therefore intentionally do not carry an Env
// field on this struct.
type legacyKitYAML struct {
	Meta               legacyKitMeta            `yaml:"meta"`
	HostCommands       orchestrator.HostCommands `yaml:"host_commands,omitempty"`
	AdditionalBindings []orchestrator.BindMount  `yaml:"additional_bindings,omitempty"`
}

type legacyKitMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Category    string `yaml:"category"`
	// SourceProjectID records which project this auto-generated legacy kit was
	// produced from. It lets a re-run of `boid project migrate` recognise the
	// same kit dir as its own (idempotent overwrite) and avoid colliding with
	// another project that happened to slugify to the same name.
	SourceProjectID string `yaml:"source_project_id,omitempty"`
}

// MigrateProjectOptions controls a single project migration run. It is the
// daemon-free surface that `cmd/start.go --auto-migrate` calls in-process
// once per affected project, and that `runProjectMigrate` populates from
// cobra flags.
type MigrateProjectOptions struct {
	// Dir is the absolute project root path (parent of .boid/).
	Dir string
	// Workspace overrides the workspace slug. Empty falls back to the
	// project's existing project_workspaces entry, then to the implicit
	// default workspace.
	Workspace string
	// Apply switches from dry-run (false) to actually writing files.
	Apply bool
	// OnCollision controls secret-key collisions when copying from the old
	// namespace into the new one. Empty defaults to "refuse".
	OnCollision string
	// DBPath / KeyFilePath override the XDG-derived defaults; empty uses
	// the defaults so production callers can pass zero-value options.
	DBPath      string
	KeyFilePath string
	// Out receives the dry-run / progress text. nil → io.Discard.
	Out io.Writer
}

// MigrateProject is the daemon-free implementation of `boid project migrate`.
// It can be called in-process from the boid start auto-migrate path or from
// cobra's runProjectMigrate (which is now a thin wrapper).
func MigrateProject(opts MigrateProjectOptions) error {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}
	dir := opts.Dir
	if dir == "" {
		return fmt.Errorf("migrate: Dir is required")
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("migrate: Dir must be absolute, got %q", dir)
	}
	onCollision := opts.OnCollision
	if onCollision == "" {
		onCollision = "refuse"
	}
	switch onCollision {
	case "refuse", "skip", "overwrite":
	default:
		return fmt.Errorf("--on-collision must be one of: refuse, skip, overwrite (got %q)", onCollision)
	}

	// ---- Phase 1: Read legacy project.yaml -----------------------------------
	meta, err := orchestrator.ReadProjectMetaLegacy(dir)
	if err != nil {
		return fmt.Errorf("migrate: read project: %w", err)
	}
	if meta.ID == "" {
		return fmt.Errorf("migrate: project.yaml has no 'id' field; cannot proceed")
	}
	if meta.Name == "" {
		return fmt.Errorf("migrate: project.yaml has no 'name' field; cannot proceed")
	}

	// Open DB (direct, no daemon).
	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = defaultDBPath()
	}
	boidDB, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("migrate: open db: %w", err)
	}
	defer boidDB.Close()

	// ---- Phase 2: Resolve workspace slug ------------------------------------
	plan := &migratePlan{
		dir:  dir,
		meta: meta,
	}

	// Look up project in DB to find existing workspace assignment.
	project, err := orchestrator.GetProject(boidDB.Conn, meta.ID)
	if err != nil && !errors.Is(err, errProjectNotFound(err)) {
		// Only hard-fail on unexpected errors. Missing project means it hasn't
		// been registered yet; we still allow migration.
		if !strings.Contains(err.Error(), "no rows") {
			return fmt.Errorf("migrate: query project: %w", err)
		}
		project = nil
	}
	plan.project = project

	workspaceSlug := opts.Workspace
	if workspaceSlug == "" && project != nil && project.WorkspaceID != "" {
		workspaceSlug = project.WorkspaceID
	}
	if workspaceSlug == "" {
		// Fall back to the implicit default workspace so every project ends
		// up linked after migration. Surface the choice in the dry-run plan
		// so the user notices and can override with --workspace if wanted.
		workspaceSlug = orchestrator.DefaultWorkspaceSlug
		fmt.Fprintf(out,
			"migrate: no --workspace flag and no existing assignment; defaulting to %q workspace.\n",
			workspaceSlug)
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceSlug); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	plan.workspaceSlug = workspaceSlug

	// ---- Compute what changes will be made -----------------------------------
	if err := computeMigratePlan(plan, boidDB, opts.KeyFilePath); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// ---- Phase 3: Print plan (dry-run output) --------------------------------
	printMigratePlan(out, plan, opts.Apply)

	// Check for collisions before apply.
	if len(plan.secretCollisions) > 0 && onCollision == "refuse" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "ERROR: secret collisions detected. Migration aborted.")
		fmt.Fprintln(out, "  Use --on-collision skip  to copy non-conflicting keys")
		fmt.Fprintln(out, "  Use --on-collision overwrite  to overwrite all (DANGEROUS)")
		return fmt.Errorf("secret collisions require --on-collision flag")
	}

	// ---- Phase 4: Apply (only when --apply is set) --------------------------
	if !opts.Apply {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "(dry-run) Pass --apply to execute the migration.")
		return nil
	}

	if err := applyMigratePlan(plan, boidDB, opts.KeyFilePath, onCollision, out); err != nil {
		return fmt.Errorf("migrate: apply: %w", err)
	}

	fmt.Fprintln(out, "Migration complete.")
	return nil
}

// runProjectMigrate is the cobra adapter for the daemon-free MigrateProject
// implementation. It parses CLI flags into MigrateProjectOptions and
// delegates.
func runProjectMigrate(cmd *cobra.Command, args []string) error {
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	return MigrateProject(MigrateProjectOptions{
		Dir:         dir,
		Workspace:   migrateWorkspace,
		Apply:       migrateApply,
		OnCollision: migrateOnCollision,
		DBPath:      migrateDBPath,
		KeyFilePath: migrateKeyFilePath,
		Out:         cmd.OutOrStdout(),
	})
}

// errProjectNotFound is a helper to detect "not found" errors from GetProject.
// GetProject returns sql.ErrNoRows wrapped in a message, so we match the string.
func errProjectNotFound(err error) error {
	return err // used only in errors.Is chain
}

// computeMigratePlan populates the migration plan fields based on the legacy
// meta and the existing workspace state.
func computeMigratePlan(plan *migratePlan, boidDB *db.DB, keyFilePath string) error {
	meta := plan.meta
	wsStore := orchestrator.NewWorkspaceStore("")

	// Load existing workspace.yaml (may not exist yet). This shadow yaml is
	// the merge base for the dry-run preview and the apply-time shadow-file
	// artifact (plan.workspaceMeta below); it is a separate, inert snapshot
	// from whatever the live daemon's DB row for this slug holds — see
	// pushMigratedWorkspaceToDaemon's own live-DB merge (MAJOR 2, codex
	// review, docs/plans/workspace-db-consolidation.md), which reuses
	// mergeLegacyFieldsIntoWorkspace below against a freshly GET-fetched
	// base instead of this one when the daemon is reachable at apply time.
	existingWS, err := wsStore.Load(plan.workspaceSlug)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load workspace %q: %w", plan.workspaceSlug, err)
	}
	if existingWS == nil {
		existingWS = &orchestrator.WorkspaceMeta{}
	}

	// Collect all kit refs from project top level and all behaviors.
	// Normalize to simple name form (last segment). Invalid refs (whose
	// derived name fails ValidKitName, e.g. a bare "github.com") fail the
	// migration outright rather than silently producing an unloadable kit
	// slug in workspace.yaml — past versions did the latter and left
	// invalid kit dirs that later tripped boid-kit-init cleanup.
	//
	// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12):
	// these external kit refs are validated and recorded on the plan (for
	// printMigratePlan's informational display) but are no longer resolved
	// or folded into the workspace at all — the kit mechanism itself was
	// retired in PR6, and any external kit ref here (as opposed to this
	// migration's own auto-generated legacy kit just below, whose fields are
	// fully known and folded directly) has no automatic resolution path any
	// more. If such a kit is still needed, add its host_commands/env/
	// additional_bindings to workspace.yaml by hand after migrating.
	kitRefStrs, err := collectKitNames(meta)
	if err != nil {
		return fmt.Errorf("collect kit names: %w", err)
	}
	plan.kitRefStrs = kitRefStrs

	// Legacy kit (host_commands + additional_bindings). Computed before the
	// merge below so mergeLegacyFieldsIntoWorkspace can fold its
	// host_commands names + additional_bindings directly into the workspace
	// in the same pass (no kit-directory round trip needed for this: the
	// data is this project's own project.yaml fields, already fully known
	// here).
	needsLegacyKit := len(meta.HostCommands) > 0 || len(meta.AdditionalBindings) > 0
	legacyKitName := ""
	if needsLegacyKit {
		kitsBaseDir := defaultKitsDir()
		kitName := legacyKitNameFor(meta.Name, meta.ID, kitsBaseDir)
		legacyKitDir := filepath.Join(kitsBaseDir, kitName)

		kitMeta := &legacyKitYAML{
			Meta: legacyKitMeta{
				Name:            kitName,
				Description:     "Auto-migrated from project.yaml of " + meta.Name,
				Category:        "legacy",
				SourceProjectID: meta.ID,
			},
			HostCommands:       meta.HostCommands,
			AdditionalBindings: meta.AdditionalBindings,
		}

		plan.needsLegacyKit = true
		plan.legacyKitDir = legacyKitDir
		plan.legacyKitMeta = kitMeta
		legacyKitName = kitName
	}

	plan.workspaceMeta = mergeLegacyFieldsIntoWorkspace(existingWS, legacyKitName, meta)

	// Build list of keys to remove from project.yaml.
	plan.removeKeys = computeRemoveKeys(meta)

	// Secret migration plan.
	plan.oldNamespace = meta.SecretNamespace
	plan.newNamespace = plan.workspaceSlug

	if plan.oldNamespace != "" && plan.oldNamespace != plan.newNamespace {
		// Load secret store to enumerate keys.
		kfPath := keyFilePath
		if kfPath == "" {
			kfPath = defaultKeyFilePath()
		}
		key, err := loadKeyFileIfExists(kfPath)
		if err != nil {
			return fmt.Errorf("load key file: %w", err)
		}
		if key != nil {
			store, err := dispatcher.NewSecretStore(boidDB.Conn, key)
			if err != nil {
				return fmt.Errorf("open secret store: %w", err)
			}
			keys, err := store.List(plan.oldNamespace)
			if err != nil {
				return fmt.Errorf("list secrets in namespace %q: %w", plan.oldNamespace, err)
			}
			plan.secretKeys = keys

			// Detect collisions.
			newKeys, err := store.List(plan.newNamespace)
			if err != nil {
				return fmt.Errorf("list secrets in namespace %q: %w", plan.newNamespace, err)
			}
			newKeySet := stringSet(newKeys)
			for _, k := range keys {
				if !newKeySet[k] {
					continue
				}
				// Collision: check if values are identical.
				oldVal, _ := store.Get(plan.oldNamespace, k)
				newVal, _ := store.Get(plan.newNamespace, k)
				same := sha256.Sum256([]byte(oldVal)) == sha256.Sum256([]byte(newVal))
				plan.secretCollisions = append(plan.secretCollisions, secretCollision{
					Key:         k,
					OldHashSame: same,
				})
			}
		}
	}

	return nil
}

// mergeLegacyFieldsIntoWorkspace merges migrated legacy project.yaml fields
// (env, capabilities, and — if this migration generated one — the legacy
// kit's own host_commands names + additional_bindings) into base, returning
// a new *WorkspaceMeta. base itself is not mutated: base.Env/HostCommands/
// AdditionalBindings, when non-nil, are copied into fresh containers before
// any write.
//
// This is called from three places: computeMigratePlan, against the (inert)
// shadow-yaml base, to build plan.workspaceMeta (the dry-run preview and the
// shadow-file artifact applyMigratePlan writes to disk); and both
// mergeMigratedWorkspaceIntoDaemon (MAJOR 2, codex review, docs/plans/
// workspace-db-consolidation.md) and applyMigratedWorkspaceOffline, each
// against a freshly-fetched *live* base when the workspace slug already has
// a DB row — so an existing workspace's real content is merged into instead
// of being silently dropped by a create that used to just warn.
//
// Precedence on a key collision: env entries from meta overwrite base's on a
// matching key ("merge で新値が優先"), capabilities.docker is overwritten
// when meta sets it, host_commands names are unioned (never removed), and
// additional_bindings are merged by Source with meta's own entry winning on
// a conflict — the same "new value wins" precedence the Env merge already
// uses. Every other WorkspaceMeta field is carried over from base untouched.
//
// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12):
// this used to also fold every kit ref (external refs collected from the
// legacy project.yaml's kits:, plus legacyKitName) into workspace.Kits,
// materialized later via orchestrator.MaterializeWorkspaceKitsForPersist at
// push/write time. WorkspaceMeta has no Kits field any more, and this
// migration's own auto-generated legacy kit's host_commands/
// additional_bindings are already fully known here — they ARE
// meta.HostCommands/meta.AdditionalBindings, this project's own
// project.yaml fields being relocated — so they are folded directly below,
// with no kit-directory round trip needed at all. External kit refs
// (computeMigratePlan's kitRefStrs) are no longer resolved here at all —
// see that collection's own doc comment for why.
func mergeLegacyFieldsIntoWorkspace(base *orchestrator.WorkspaceMeta, legacyKitName string, meta *orchestrator.LegacyProjectMeta) *orchestrator.WorkspaceMeta {
	merged := *base

	// Env: merge, new values take precedence (match spec: "merge で新値が優先").
	if len(meta.Env) > 0 {
		newEnv := make(map[string]string, len(base.Env)+len(meta.Env))
		for k, v := range base.Env {
			newEnv[k] = v
		}
		for k, v := range meta.Env {
			newEnv[k] = v
		}
		merged.Env = newEnv
	}

	// Capabilities.
	if meta.Capabilities.Docker != nil {
		merged.Capabilities.Docker = meta.Capabilities.Docker
	}

	// This migration's own auto-generated legacy kit (see needsLegacyKit in
	// computeMigratePlan): its host_commands (folded in as reference
	// names — the daemon's aggregated host_commands.yaml gets the full
	// definitions separately via ensureLegacyKitHostCommandsKnownToDaemon)
	// and additional_bindings come straight from meta, so no kit.yaml round
	// trip is needed to resolve them.
	if legacyKitName != "" {
		if len(meta.HostCommands) > 0 {
			names := make([]string, 0, len(meta.HostCommands))
			for name := range meta.HostCommands {
				names = append(names, name)
			}
			sort.Strings(names)
			merged.HostCommands = unionStringSlices(base.HostCommands, names)
		}
		if len(meta.AdditionalBindings) > 0 {
			merged.AdditionalBindings = mergeBindMountsBySourceNewWins(base.AdditionalBindings, meta.AdditionalBindings)
		}
	}

	return &merged
}

// unionStringSlices returns the sorted, deduplicated union of a and b.
func unionStringSlices(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	var out []string
	for _, s := range a {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range b {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// mergeBindMountsBySourceNewWins merges base and overlay by Source, with
// overlay's entry winning on a Source collision — the same "new value wins"
// precedence mergeLegacyFieldsIntoWorkspace's Env merge already uses.
func mergeBindMountsBySourceNewWins(base, overlay []orchestrator.BindMount) []orchestrator.BindMount {
	result := make([]orchestrator.BindMount, len(base))
	copy(result, base)
	indexBySource := make(map[string]int, len(result))
	for i, b := range result {
		indexBySource[b.Source] = i
	}
	for _, b := range overlay {
		if idx, ok := indexBySource[b.Source]; ok {
			result[idx] = b
			continue
		}
		indexBySource[b.Source] = len(result)
		result = append(result, b)
	}
	return result
}

// loadKeyFileIfExists returns nil (no error) when the key file does not exist.
// This allows running migrate without secrets when no key file is present.
func loadKeyFileIfExists(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read key file: %w", err)
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("invalid key file: expected 32 bytes, got %d", len(data))
	}
	return data, nil
}

// collectKitNames collects unique kit ref names from the legacy meta,
// normalising them to a simple single-segment name (last part of the path).
// Returns an error if any ref derives a name that is not a valid kit slug;
// the caller should refuse the migration in that case rather than write a
// broken slug into workspace.yaml.
func collectKitNames(meta *orchestrator.LegacyProjectMeta) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	add := func(ref, source string) error {
		name, err := kitRefToName(ref)
		if err != nil {
			return fmt.Errorf("%s: %w", source, err)
		}
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
		return nil
	}
	for _, r := range meta.Kits {
		if err := add(r.Ref, "kits"); err != nil {
			return nil, err
		}
	}
	for bname, b := range meta.TaskBehaviors {
		for _, r := range b.Kits {
			if err := add(r.Ref, fmt.Sprintf("task_behaviors.%s.kits", bname)); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// kitRefToName returns the simple name for a kit ref. For a full path like
// "github.com/novshi-tech/boid-kits/go" this returns "go". For a simple name
// like "go" this returns "go". Returns an error if the derived name is not a
// valid kit slug per orchestrator.ValidKitName — e.g. a bare "github.com"
// ref would otherwise produce a literal "github.com" kit slug (the dot is
// rejected by ValidKitName, and a kit dir at that path is not loadable).
func kitRefToName(ref string) (string, error) {
	parts := strings.Split(ref, "/")
	name := parts[len(parts)-1]
	if err := orchestrator.ValidKitName(name); err != nil {
		return "", fmt.Errorf("kit ref %q yields invalid kit slug %q: %w", ref, name, err)
	}
	return name, nil
}

// computeRemoveKeys returns the list of top-level project.yaml keys that should
// be removed during migration.
func computeRemoveKeys(meta *orchestrator.LegacyProjectMeta) []string {
	var keys []string
	if len(meta.Kits) > 0 {
		keys = append(keys, "kits")
	}
	if len(meta.Env) > 0 {
		keys = append(keys, "env")
	}
	if len(meta.HostCommands) > 0 {
		keys = append(keys, "host_commands")
	}
	if len(meta.AdditionalBindings) > 0 {
		keys = append(keys, "additional_bindings")
	}
	if meta.SecretNamespace != "" {
		keys = append(keys, "secret_namespace")
	}
	if meta.Capabilities.Docker != nil {
		keys = append(keys, "capabilities")
	}
	// Behavior-level kits are inside task_behaviors but we track them separately.
	return keys
}

// printMigratePlan prints a human-readable summary of what the migration will do.
func printMigratePlan(out io.Writer, plan *migratePlan, apply bool) {
	mode := "dry-run"
	if apply {
		mode = "apply"
	}
	fmt.Fprintf(out, "=== boid project migrate [%s] ===\n", mode)
	fmt.Fprintf(out, "Project:   %s (%s)\n", plan.meta.ID, plan.meta.Name)
	fmt.Fprintf(out, "Directory: %s\n", plan.dir)
	fmt.Fprintf(out, "Workspace: %s\n", plan.workspaceSlug)
	fmt.Fprintln(out)

	if !plan.meta.HasLegacyFields() && len(plan.removeKeys) == 0 {
		fmt.Fprintln(out, "Nothing to migrate: project.yaml is already in the new schema.")
		return
	}

	// Workspace changes.
	fmt.Fprintln(out, "--- workspace.yaml changes ---")
	wsData, _ := yaml.Marshal(plan.workspaceMeta)
	fmt.Fprintf(out, "workspace slug: %s\n", plan.workspaceSlug)
	fmt.Fprintln(out, string(wsData))

	// External kit refs (docs/plans/workspace-db-consolidation.md Phase 2.5
	// PR7, decision 12): these came from the legacy project.yaml's top-level
	// or behavior-level `kits:` lists but are no longer resolved into the
	// workspace automatically — the kit mechanism was retired in PR6. Shown
	// here purely so the operator knows to add any host_commands/env/
	// additional_bindings such a kit used to supply by hand, if still
	// needed.
	if len(plan.kitRefStrs) > 0 {
		fmt.Fprintln(out, "--- kit refs found (no longer auto-resolved; add manually if still needed) ---")
		for _, name := range plan.kitRefStrs {
			fmt.Fprintf(out, "  - %s\n", name)
		}
		fmt.Fprintln(out)
	}

	// Legacy kit.
	if plan.needsLegacyKit {
		fmt.Fprintln(out, "--- legacy kit ---")
		fmt.Fprintf(out, "kit directory: %s\n", plan.legacyKitDir)
		kitData, _ := yaml.Marshal(plan.legacyKitMeta)
		fmt.Fprintln(out, string(kitData))
	}

	// project.yaml keys to remove.
	if len(plan.removeKeys) > 0 {
		fmt.Fprintln(out, "--- project.yaml keys to remove ---")
		for _, k := range plan.removeKeys {
			fmt.Fprintf(out, "  - %s\n", k)
		}
		// Behavior-level kits.
		for bname, b := range plan.meta.TaskBehaviors {
			if len(b.Kits) > 0 {
				fmt.Fprintf(out, "  - task_behaviors.%s.kits\n", bname)
			}
		}
		fmt.Fprintln(out)
	}

	// Secret migration plan.
	if plan.oldNamespace != "" {
		fmt.Fprintln(out, "--- secret migration ---")
		fmt.Fprintf(out, "  old namespace: %s\n", plan.oldNamespace)
		fmt.Fprintf(out, "  new namespace: %s\n", plan.newNamespace)
		if plan.oldNamespace == plan.newNamespace {
			fmt.Fprintln(out, "  (namespaces match; no copy needed)")
		} else if len(plan.secretKeys) == 0 {
			fmt.Fprintln(out, "  (no secrets found in old namespace, or key file missing)")
		} else {
			sort.Strings(plan.secretKeys)
			for _, k := range plan.secretKeys {
				fmt.Fprintf(out, "  copy: %s\n", k)
			}
		}
		if len(plan.secretCollisions) > 0 {
			fmt.Fprintln(out, "  COLLISIONS (key already exists in new namespace):")
			for _, c := range plan.secretCollisions {
				match := "DIFFERENT"
				if c.OldHashSame {
					match = "same value"
				}
				fmt.Fprintf(out, "    ! %s  (%s)\n", c.Key, match)
			}
		}
		fmt.Fprintln(out)
	}
}

// applyMigratePlan executes the migration plan: writes workspace.yaml, legacy
// kit.yaml, updates project.yaml, and copies secrets. onCollision and out
// are forwarded to migrateSecrets when a secret namespace move is needed.
func applyMigratePlan(plan *migratePlan, boidDB *db.DB, keyFilePath, onCollision string, out io.Writer) error {
	wsStore := orchestrator.NewWorkspaceStore("")

	// 1. Write legacy kit.yaml *before* touching the daemon at all (step 2
	// below). plan.workspaceMeta.HostCommands/AdditionalBindings already
	// carry this migration's legacy-kit-derived content directly
	// (mergeLegacyFieldsIntoWorkspace folds meta's own fields in with no
	// kit-directory dependency, Phase 2.5 PR7) — what this kit.yaml file is
	// still needed for is ensureLegacyKitHostCommandsKnownToDaemon below,
	// which registers its host_commands *definitions* into the daemon's
	// aggregated ~/.config/boid/host_commands.yaml config so that
	// workspace.HostCommands' name references (e.g. "gh") actually resolve
	// to something at dispatch/hydration time. The daemon POST/PUT in
	// pushMigratedWorkspaceToDaemon below 400s on an unresolvable
	// host_commands reference unless that config is updated (and, if
	// online, reloaded) first — which is exactly what
	// ensureLegacyKitHostCommandsKnownToDaemon does, reading this file back
	// off disk.
	if plan.needsLegacyKit {
		if err := os.MkdirAll(plan.legacyKitDir, 0o755); err != nil {
			return fmt.Errorf("mkdir legacy kit dir: %w", err)
		}
		kitData, err := yaml.Marshal(plan.legacyKitMeta)
		if err != nil {
			return fmt.Errorf("marshal legacy kit: %w", err)
		}
		kitPath := filepath.Join(plan.legacyKitDir, "kit.yaml")
		if err := os.WriteFile(kitPath, kitData, 0o644); err != nil {
			return fmt.Errorf("write legacy kit.yaml: %w", err)
		}
	}

	// 2. Write workspace.yaml. Since Phase 2.5's one-shot yaml->DB migration
	// (workspace_db_consolidation) has cut over, this yaml file is a
	// read-only shadow the daemon never loads from again (see
	// WorkspaceStore.Load's doc comment) — it exists here only as a
	// reviewable artifact / a `--from-file` source for a manual `boid
	// workspace edit|import`, not as the thing that makes the migration
	// take effect. See pushMigratedWorkspaceToDaemon below for the actual
	// live-daemon write (codex review MAJOR 4, docs/plans/
	// workspace-db-consolidation.md).
	if err := wsStore.Save(plan.workspaceSlug, plan.workspaceMeta); err != nil {
		return fmt.Errorf("save workspace.yaml: %w", err)
	}
	if shadowPath, err := workspaceShadowPath(plan.workspaceSlug); err != nil {
		fmt.Fprintf(out, "warning: could not resolve workspace shadow path: %v\n", err)
	} else if err := pushMigratedWorkspaceToDaemon(plan, boidDB, shadowPath, out); err != nil {
		// MAJOR 2 (codex review, docs/plans/workspace-db-consolidation.md):
		// a host_commands definition conflict is not a best-effort
		// daemon-push warning like every other outcome below — it means
		// this migration's legacy kit would either silently redefine an
		// existing host_commands.yaml entry, or (previously) be silently
		// discarded behind it. Abort before project.yaml (or anything
		// else) is touched, the same way the secret-collision check above
		// aborts before Phase 4 ever starts, so the user can resolve the
		// conflict by hand and re-run.
		return fmt.Errorf("legacy kit host_commands: %w", err)
	}

	// 3. Rewrite project.yaml removing migrated keys.
	if err := rewriteProjectYAML(plan); err != nil {
		return fmt.Errorf("rewrite project.yaml: %w", err)
	}

	// 4. Update project_workspaces in DB.
	if plan.project != nil {
		if err := orchestrator.SetProjectWorkspace(boidDB.Conn, plan.meta.ID, plan.workspaceSlug); err != nil {
			return fmt.Errorf("set project workspace in db: %w", err)
		}
	}

	// 5. Copy secrets.
	if plan.oldNamespace != "" && plan.oldNamespace != plan.newNamespace && len(plan.secretKeys) > 0 {
		if err := migrateSecrets(plan, boidDB, keyFilePath, onCollision, out); err != nil {
			return fmt.Errorf("migrate secrets: %w", err)
		}
	}

	return nil
}

// workspaceShadowPath returns the path of the legacy per-slug workspace yaml
// file under DefaultWorkspaceDir() — the file wsStore.Save (yaml mode) just
// wrote in applyMigratePlan above. Used only to point the user at a concrete
// file in pushMigratedWorkspaceToDaemon's warning/note messages.
func workspaceShadowPath(slug string) (string, error) {
	dir, err := orchestrator.DefaultWorkspaceDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, slug+".yaml"), nil
}

// pushMigrateDaemonPingTimeout bounds pushMigratedWorkspaceToDaemon's initial
// liveness check (daemon.IsSocketAlive) — a short, local UNIX-socket dial
// that resolves immediately whether or not something is listening, so this
// only exists as a backstop against a socket that accepts but never
// responds.
const pushMigrateDaemonPingTimeout = 500 * time.Millisecond

// pushMigratedWorkspaceToDaemon applies the migrated workspace content to
// the `workspaces` DB table, closing the gap identified in codex review
// MAJOR 4 (docs/plans/workspace-db-consolidation.md): once Phase 2.5's
// one-shot yaml->DB migration (workspace_db_consolidation) has run, that
// table is the daemon's sole authority and the plain wsStore.Save above
// (yaml mode, no repository wired) only ever touches the now-inert shadow
// file — a running daemon never re-reads it, so `boid project migrate
// --apply` looked like it worked but silently never changed what a live
// daemon actually uses.
//
// Whether this happens over HTTP or directly against boidDB is decided by a
// single liveness check (daemon.IsSocketAlive against
// client.DefaultSocketPath()) up front — MAJOR 1 (codex review,
// docs/plans/workspace-db-consolidation.md). This matters because `boid
// start --auto-migrate` (cmd/start.go's runDaemonParent ->
// handleMigrationFailure -> runAutoMigrate) calls MigrateProject in-process
// specifically because the daemon it just tried to start FAILED to start —
// so for that caller the daemon is *never* reachable, and before this fix
// the HTTP-only push always took the "could not reach the boid daemon"
// warning branch there, silently no-op'ing the one thing (the `workspaces`
// row) --apply is supposed to guarantee gets applied.
//
// The outcome is one of:
//   - the legacy kit's own host_commands.yaml merge hit a genuine definition
//     conflict (ensureLegacyKitHostCommandsKnownToDaemon returned an error
//     wrapping errHostCommandsConflict): returned as an error, aborting the
//     whole migration before project.yaml is rewritten (MAJOR 2, codex
//     review) — this is the one outcome that is NOT merely reported and
//     swallowed, see below.
//   - the legacy kit's own host_commands could not otherwise be
//     synced/reloaded (some other ensureLegacyKitHostCommandsKnownToDaemon
//     failure): warned, nothing else is attempted.
//   - daemon unreachable: the merge runs directly against boidDB instead
//     (applyMigratedWorkspaceOffline) — slug with no DB row yet is created,
//     slug with an existing row has it loaded/merged/compare-and-swapped,
//     retrying on a lost race the same pushMigratedWorkspaceRetryLimit
//     number of times mergeMigratedWorkspaceIntoDaemon does for its own
//     412 retries.
//   - daemon reachable, slug has no DB row yet: created via POST
//     /api/workspaces (the same create-only request `boid workspace create`
//     builds).
//   - daemon reachable, slug already has a DB row: MAJOR 2 (codex review,
//     prior pass) — its current content is GET-fetched and merged with this
//     migration's fields (mergeMigratedWorkspaceIntoDaemon: existing content
//     is preserved, migration-sourced values win on a key collision,
//     matching the shadow-yaml merge's own precedence), then PUT back with
//     If-Match; a 412 (concurrent modification) re-fetches and retries.
//
// Every outcome except the host_commands conflict is reported on out but
// never fails the overall migration — project.yaml has already been
// rewritten by the time most of these run, and an apply that partially
// lands (project.yaml migrated, workspace content pending manual
// application) is far more recoverable than one that aborts outright.
func pushMigratedWorkspaceToDaemon(plan *migratePlan, boidDB *db.DB, shadowPath string, out io.Writer) error {
	socketPath := client.DefaultSocketPath()
	online := daemon.IsSocketAlive(socketPath, pushMigrateDaemonPingTimeout)
	c := client.NewUnixClient(socketPath)

	if err := ensureLegacyKitHostCommandsKnownToDaemon(c, plan, online); err != nil {
		if errors.Is(err, errHostCommandsConflict) {
			return err
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "warning: could not sync workspace %q's legacy kit host_commands with the daemon (%v).\n", plan.workspaceSlug, err)
		fmt.Fprintf(out, "  The migrated content was written only to the (daemon-unread) shadow file:\n    %s\n", shadowPath)
		fmt.Fprintf(out, "  Start the daemon and re-run this command, or apply it by hand:\n    boid workspace import %s --slug %s\n", shadowPath, plan.workspaceSlug)
		return nil
	}

	if !online {
		applyMigratedWorkspaceOffline(plan, boidDB, shadowPath, out)
		return nil
	}

	getStatus, getBody, err := c.GetRaw("/api/workspaces/" + plan.workspaceSlug)
	if err != nil {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "warning: could not reach the boid daemon to apply workspace %q (%v).\n", plan.workspaceSlug, err)
		fmt.Fprintf(out, "  The migrated content was written only to the (daemon-unread) shadow file:\n    %s\n", shadowPath)
		fmt.Fprintf(out, "  Start the daemon and re-run this command, or apply it by hand:\n    boid workspace import %s --slug %s\n", shadowPath, plan.workspaceSlug)
		return nil
	}

	switch getStatus {
	case http.StatusNotFound:
		createMigratedWorkspaceInDaemon(c, plan, shadowPath, out)
	case http.StatusOK:
		var current api.WorkspaceDetail
		if err := json.Unmarshal(getBody, &current); err != nil {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "warning: could not decode the daemon's current workspace %q (%v).\n", plan.workspaceSlug, err)
			fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
			return nil
		}
		mergeMigratedWorkspaceIntoDaemon(c, plan, current, shadowPath, out)
	default:
		fmt.Fprintln(out)
		fmt.Fprintf(out, "warning: could not fetch workspace %q from the daemon: %s\n", plan.workspaceSlug, formatWorkspaceAPIError(getStatus, getBody))
		fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
	}
	return nil
}

// applyMigratedWorkspaceOffline is pushMigratedWorkspaceToDaemon's fallback
// when the daemon is not reachable (MAJOR 1, codex review,
// docs/plans/workspace-db-consolidation.md). It mirrors
// mergeMigratedWorkspaceIntoDaemon's precedence (existing content wins
// except where the migration's own fields collide, in which case migration
// wins) but reads/writes boidDB directly via orchestrator.WorkspaceRepository
// instead of GET/PUT over HTTP:
//   - slug has no DB row yet: created outright (repo.Create), mirroring
//     createMigratedWorkspaceInDaemon's create-only POST. An os.ErrExist
//     race (a concurrent writer created the row between this attempt's own
//     failed LoadWithRevision and the Create call) falls back to the
//     load+merge branch on the next attempt, the same way a concurrent 409
//     does online.
//   - slug already has a DB row: loaded with its revision
//     (LoadWithRevision), merged (mergeLegacyFieldsIntoWorkspace), and
//     written back with UpdateIfRevisionMatches — a compare-and-swap that
//     plays the same role If-Match does for the online PUT. A lost race
//     (matched=false: some other writer updated the row between the load
//     and this call) retries with a fresh read, up to
//     pushMigratedWorkspaceRetryLimit times — there is no live daemon in
//     this path, but another concurrent `boid project migrate`/`boid start
//     --auto-migrate` invocation against the same DB file is still
//     possible.
//
// Every branch also refreshes the on-disk shadow file (updateWorkspaceShadowFile)
// with the actual merge that was attempted against the DB — MAJOR 3, codex
// review — so a failure here still leaves the user a shadow file that is
// safe to apply in full via `boid workspace edit --from-file`, not just the
// migration's own delta.
//
// Phase 2.5 PR7 (docs/plans/workspace-db-consolidation.md, decision 12): this
// used to also run a kit-materialization step (materializeWorkspaceMetaForOfflineWrite,
// removed) on an independent copy immediately before the repo.Create/
// UpdateIfRevisionMatches call below, mirroring what the online path did
// server-side inside CreateWorkspace/UpdateWorkspace. Neither side does that
// any more: WorkspaceMeta has no Kits field, and plan.workspaceMeta/merged
// already carry this migration's legacy-kit-derived host_commands/
// additional_bindings directly (mergeLegacyFieldsIntoWorkspace folds them in
// from meta's own fields, no kit-directory lookup needed) — so both branches
// below persist plan.workspaceMeta/merged as-is.
func applyMigratedWorkspaceOffline(plan *migratePlan, boidDB *db.DB, shadowPath string, out io.Writer) {
	repo := orchestrator.NewWorkspaceRepository(boidDB.Conn)
	legacyKitName := ""
	if plan.needsLegacyKit {
		legacyKitName = plan.legacyKitMeta.Meta.Name
	}

	for attempt := 1; attempt <= pushMigratedWorkspaceRetryLimit; attempt++ {
		base, revision, err := repo.LoadWithRevision(plan.workspaceSlug)
		if errors.Is(err, os.ErrNotExist) {
			// plan.workspaceMeta is already the migration's fields merged
			// against the local shadow yaml (computeMigratePlan) — the same
			// value createMigratedWorkspaceInDaemon POSTs verbatim when the
			// live daemon reports no existing row, so reuse it here too
			// rather than re-deriving a fresh merge against an empty base.
			if createErr := repo.Create(plan.workspaceSlug, plan.workspaceMeta); createErr != nil {
				if errors.Is(createErr, os.ErrExist) {
					continue // concurrent creator raced us; retry as a load+merge
				}
				fmt.Fprintln(out)
				fmt.Fprintf(out, "warning: could not create workspace %q directly in the database (%v).\n", plan.workspaceSlug, createErr)
				fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
				return
			}
			fmt.Fprintf(out, "workspace %q created directly in the database (daemon unreachable; see %s for the equivalent yaml).\n", plan.workspaceSlug, shadowPath)
			return
		}
		if err != nil {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "warning: could not read workspace %q from the database directly (%v).\n", plan.workspaceSlug, err)
			fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
			return
		}

		merged := mergeLegacyFieldsIntoWorkspace(base, legacyKitName, plan.meta)
		updateWorkspaceShadowFile(plan, merged, out)

		_, matched, updErr := repo.UpdateIfRevisionMatches(plan.workspaceSlug, revision, merged)
		if updErr != nil {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "warning: could not update workspace %q directly in the database (%v).\n", plan.workspaceSlug, updErr)
			fmt.Fprintf(out, "  The migrated content (merged with what was already there for %q) is available at:\n    %s\n", plan.workspaceSlug, shadowPath)
			return
		}
		if matched {
			fmt.Fprintf(out, "workspace %q updated directly in the database (daemon unreachable; existing content merged with the migrated fields; see %s for the full merged content).\n", plan.workspaceSlug, shadowPath)
			return
		}
		// Lost a compare-and-swap race against a concurrent writer; retry
		// with a fresh read.
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "warning: workspace %q kept changing concurrently in the database; gave up after %d attempts.\n", plan.workspaceSlug, pushMigratedWorkspaceRetryLimit)
	fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
}

// updateWorkspaceShadowFile overwrites the on-disk shadow yaml for
// plan.workspaceSlug with meta — MAJOR 3 (codex review,
// docs/plans/workspace-db-consolidation.md). Both mergeMigratedWorkspaceIntoDaemon
// (online) and applyMigratedWorkspaceOffline (daemon unreachable) call this
// right after computing a merge against the workspace's actual current
// content — live-fetched via GET or read directly from the DB — so that a
// shadow file a failure message points the user at always matches the
// fullest merge this run computed: safe for a full-content-replace `boid
// workspace edit --from-file`. Before this, the shadow file only ever held
// plan.workspaceMeta — the migration's own delta merged once, in
// computeMigratePlan, against the local (often stale/empty, post-cutover
// inert) shadow yaml — so `edit --from-file` against it after a PUT/update
// failure would silently drop every field the live workspace already had
// that this migration didn't itself touch.
func updateWorkspaceShadowFile(plan *migratePlan, meta *orchestrator.WorkspaceMeta, out io.Writer) {
	if err := orchestrator.NewWorkspaceStore("").Save(plan.workspaceSlug, meta); err != nil {
		fmt.Fprintf(out, "warning: could not refresh shadow file for workspace %q: %v\n", plan.workspaceSlug, err)
	}
}

// createMigratedWorkspaceInDaemon is pushMigratedWorkspaceToDaemon's
// brand-new-slug branch: plan.workspaceSlug has no DB row yet, so the
// migrated content is created outright (POST /api/workspaces, create-only).
// A 409 here means a concurrent creator raced us between the caller's GET
// (404) and this POST; rather than dropping that race on the floor, it
// falls back to the merge path so the outcome is still correct.
func createMigratedWorkspaceInDaemon(c *client.Client, plan *migratePlan, shadowPath string, out io.Writer) {
	wsData, err := yaml.Marshal(plan.workspaceMeta)
	if err != nil {
		fmt.Fprintf(out, "warning: could not marshal migrated workspace %q for the daemon: %v\n", plan.workspaceSlug, err)
		return
	}
	body, err := buildWorkspaceCreateBody(plan.workspaceSlug, wsData)
	if err != nil {
		fmt.Fprintf(out, "warning: could not build daemon create request for workspace %q: %v\n", plan.workspaceSlug, err)
		return
	}

	statusCode, respBody, err := c.PostRaw("/api/workspaces", "application/yaml", body)
	if err != nil {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "warning: could not reach the boid daemon to apply workspace %q (%v).\n", plan.workspaceSlug, err)
		fmt.Fprintf(out, "  The migrated content was written only to the (daemon-unread) shadow file:\n    %s\n", shadowPath)
		fmt.Fprintf(out, "  Start the daemon and re-run this command, or apply it by hand:\n    boid workspace import %s --slug %s\n", shadowPath, plan.workspaceSlug)
		return
	}

	switch statusCode {
	case http.StatusOK, http.StatusCreated:
		fmt.Fprintf(out, "workspace %q created in the daemon (see %s for the equivalent yaml).\n", plan.workspaceSlug, shadowPath)
	case http.StatusConflict:
		fmt.Fprintln(out)
		fmt.Fprintf(out, "note: workspace %q was created concurrently; retrying as a merge.\n", plan.workspaceSlug)
		getStatus, getBody, getErr := c.GetRaw("/api/workspaces/" + plan.workspaceSlug)
		if getErr != nil || getStatus != http.StatusOK {
			fmt.Fprintf(out, "warning: could not re-fetch workspace %q after the concurrent create: %v\n", plan.workspaceSlug, getErr)
			fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
			return
		}
		var current api.WorkspaceDetail
		if err := json.Unmarshal(getBody, &current); err != nil {
			fmt.Fprintf(out, "warning: could not decode workspace %q after the concurrent create: %v\n", plan.workspaceSlug, err)
			fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
			return
		}
		mergeMigratedWorkspaceIntoDaemon(c, plan, current, shadowPath, out)
	default:
		fmt.Fprintln(out)
		fmt.Fprintf(out, "warning: could not create workspace %q in the daemon: %s\n", plan.workspaceSlug, formatWorkspaceAPIError(statusCode, respBody))
		fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
	}
}

// pushMigratedWorkspaceRetryLimit bounds mergeMigratedWorkspaceIntoDaemon's
// retry loop on a 412 (stale If-Match / concurrent modification) — a
// migrate run should not spin forever contending with some other concurrent
// writer.
const pushMigratedWorkspaceRetryLimit = 3

// mergeMigratedWorkspaceIntoDaemon implements MAJOR 2 (codex review,
// docs/plans/workspace-db-consolidation.md): plan.workspaceSlug already has
// a DB row (first is its current content), so instead of leaving it
// untouched behind a 409-style warning, this merges the migration's fields
// into first.Meta (mergeLegacyFieldsIntoWorkspace — existing content is
// preserved, migration-sourced values win on a key collision, the same
// precedence the shadow-yaml merge already uses) and PUTs the result back
// with If-Match: first.Revision. A 412 (the workspace changed concurrently
// between the GET and this PUT) re-fetches the current content, re-merges,
// and retries, up to pushMigratedWorkspaceRetryLimit times.
func mergeMigratedWorkspaceIntoDaemon(c *client.Client, plan *migratePlan, first api.WorkspaceDetail, shadowPath string, out io.Writer) {
	current := first
	legacyKitName := ""
	if plan.needsLegacyKit {
		legacyKitName = plan.legacyKitMeta.Meta.Name
	}

	for attempt := 1; attempt <= pushMigratedWorkspaceRetryLimit; attempt++ {
		base := current.Meta
		if base == nil {
			base = &orchestrator.WorkspaceMeta{}
		}
		merged := mergeLegacyFieldsIntoWorkspace(base, legacyKitName, plan.meta)

		// MAJOR 3 (codex review, docs/plans/workspace-db-consolidation.md):
		// refresh the shadow file with this attempt's full merge *before*
		// the PUT below, so that whatever happens next — success, a 412
		// retry, or a terminal failure — the file on disk already reflects
		// the fullest, most current merge this run computed, not just the
		// migration's own delta. See updateWorkspaceShadowFile's doc
		// comment for why that distinction matters for `edit --from-file`.
		updateWorkspaceShadowFile(plan, merged, out)

		data, err := yaml.Marshal(merged)
		if err != nil {
			fmt.Fprintf(out, "warning: could not marshal merged workspace %q for the daemon: %v\n", plan.workspaceSlug, err)
			return
		}

		statusCode, body, err := c.PutRawWithIfMatch("/api/workspaces/"+plan.workspaceSlug, "application/yaml", data, current.Revision)
		if err != nil {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "warning: could not reach the boid daemon to update workspace %q (%v).\n", plan.workspaceSlug, err)
			fmt.Fprintf(out, "  The migrated content (merged with what was already there for %q) was written to the shadow file:\n    %s\n", plan.workspaceSlug, shadowPath)
			fmt.Fprintf(out, "  Start the daemon and re-run this command, or apply it by hand:\n    boid workspace edit %s --from-file %s\n", plan.workspaceSlug, shadowPath)
			return
		}

		switch statusCode {
		case http.StatusOK:
			fmt.Fprintf(out, "workspace %q updated in the daemon (existing content merged with the migrated fields; see %s for the full merged content).\n", plan.workspaceSlug, shadowPath)
			return
		case http.StatusPreconditionFailed:
			getStatus, getBody, getErr := c.GetRaw("/api/workspaces/" + plan.workspaceSlug)
			if getErr != nil || getStatus != http.StatusOK {
				fmt.Fprintln(out)
				fmt.Fprintf(out, "warning: workspace %q changed concurrently and could not be re-fetched to retry the merge.\n", plan.workspaceSlug)
				fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
				fmt.Fprintf(out, "  Review and apply by hand if wanted:\n    boid workspace edit %s --from-file %s\n", plan.workspaceSlug, shadowPath)
				return
			}
			if err := json.Unmarshal(getBody, &current); err != nil {
				fmt.Fprintf(out, "warning: could not decode workspace %q while retrying the merge: %v\n", plan.workspaceSlug, err)
				fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
				return
			}
			continue
		default:
			fmt.Fprintln(out)
			fmt.Fprintf(out, "warning: could not update workspace %q in the daemon: %s\n", plan.workspaceSlug, formatWorkspaceAPIError(statusCode, body))
			fmt.Fprintf(out, "  The migrated content (merged with what was already there for %q) is available at:\n    %s\n", plan.workspaceSlug, shadowPath)
			fmt.Fprintf(out, "  Review and apply by hand if wanted:\n    boid workspace edit %s --from-file %s\n", plan.workspaceSlug, shadowPath)
			return
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "warning: workspace %q kept changing concurrently; gave up after %d attempts.\n", plan.workspaceSlug, pushMigratedWorkspaceRetryLimit)
	fmt.Fprintf(out, "  The migrated content is available at:\n    %s\n", shadowPath)
	fmt.Fprintf(out, "  Review and apply by hand if wanted:\n    boid workspace edit %s --from-file %s\n", plan.workspaceSlug, shadowPath)
}

// errHostCommandsConflict marks an ensureLegacyKitHostCommandsKnownToDaemon
// error as a hard, migration-aborting conflict (MAJOR 2, codex review,
// docs/plans/workspace-db-consolidation.md): the legacy kit this migration
// generated declares a host_commands name that host_commands.yaml already
// defines *differently*. pushMigratedWorkspaceToDaemon uses errors.Is
// against this sentinel to tell that case apart from every other error this
// function can return (path/load/write/reload failures — all of which are
// daemon-reachability-shaped and only ever downgrade to a warning; the
// project.yaml rewrite still happens for those).
var errHostCommandsConflict = errors.New("host_commands definition conflict")

// ensureLegacyKitHostCommandsKnownToDaemon merges the legacy kit's own
// host_commands definitions (already written to disk by applyMigratePlan's
// kit.yaml write, which runs before this) into the daemon's aggregated
// ~/.config/boid/host_commands.yaml config, then — when a daemon is
// actually reachable (online) — asks it to reload that config. This closes
// codex review MAJOR 1 (docs/plans/workspace-db-consolidation.md):
// CreateWorkspace/UpdateWorkspace's validateHostCommandRefs check every
// meta.HostCommands reference (added once the kit's Kits ref materializes)
// against the daemon's live in-memory snapshot — and
// Server.ReloadHostCommands only ever re-reads the on-disk
// host_commands.yaml file itself, never re-scanning kitsDir directly (see
// orchestrator.LoadHostCommandsFromKits vs LoadHostCommandsConfig) — so even
// with the legacy kit.yaml already on disk, the daemon would still 400 on an
// unknown reference unless this config file is updated and reloaded first.
// When offline, the reload step is skipped entirely: there is no live
// daemon to reload, and the file (freshly written, or already current) is
// picked up automatically the next time the daemon starts
// (internal/server/wire.go's own host_commands preflight) — which for the
// `boid start --auto-migrate` caller is about to happen anyway.
//
// A no-op (nil, no file/daemon access at all) when the migration did not
// generate a legacy kit, or the legacy kit defines no host_commands (only
// additional_bindings, which needs no daemon-side registration). Otherwise,
// each of the legacy kit's host_commands names is one of (MAJOR 2, codex
// review — see mergeLegacyKitHostCommandsIntoConfig for the merge/conflict
// logic itself):
//   - missing from the on-disk config: added, and the file is rewritten.
//   - already present with an identical (post-normalization) definition:
//     left alone on disk ("dedupe") — but still counted as a name this
//     migration depends on being live-known, so the reload below still runs
//     for it when online, even though the file itself did not change. Before
//     this fix, an all-dedupe run skipped the reload/daemon-sync step
//     entirely, silently leaving a stale live-daemon snapshot as the only
//     explanation for a later 400 on an "already defined" reference.
//   - already present with a genuinely different definition: this function
//     returns an error wrapping errHostCommandsConflict *before* writing
//     anything to host_commands.yaml — silently keeping the existing
//     definition (the old behaviour) would leave whatever this migration's
//     project.yaml declared quietly discarded; silently overwriting it would
//     just as quietly change the contract every project already relying on
//     that name has. Neither is safe to do without a word, so the whole
//     migration aborts instead (pushMigratedWorkspaceToDaemon) and the user
//     resolves the conflict by hand.
func ensureLegacyKitHostCommandsKnownToDaemon(c *client.Client, plan *migratePlan, online bool) error {
	if !plan.needsLegacyKit || len(plan.legacyKitMeta.HostCommands) == 0 {
		return nil
	}
	hcPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		return fmt.Errorf("resolve host_commands.yaml path: %w", err)
	}

	merged, added, err := mergeLegacyKitHostCommandsIntoConfig(hcPath, plan.legacyKitMeta.HostCommands)
	if err != nil {
		return err
	}
	if len(added) > 0 {
		if err := orchestrator.WriteHostCommandsConfig(hcPath, merged); err != nil {
			return fmt.Errorf("write host_commands.yaml: %w", err)
		}
	}

	if !online {
		return nil
	}
	if err := c.Do("POST", "/api/host_commands/reload", nil, nil); err != nil {
		return fmt.Errorf("reload host_commands: %w", err)
	}
	return nil
}

// mergeLegacyKitHostCommandsIntoConfig merges want (a legacy kit's own
// host_commands definitions) into the aggregated config at hcPath, without
// writing anything to disk. Returns the merged map (existing plus any
// genuinely new names) and the subset of want's names that are new (nil
// when every name already existed).
//
// A name already present in the on-disk config with a definition that
// differs from want's — compared after normalizeHostCommandSpecForCompare,
// mirroring orchestrator's own (unexported) normalizeHostCommandSpec so two
// specs compare equal regardless of nil-vs-empty slice/map spelling — is a
// hard conflict: this returns an error wrapping errHostCommandsConflict
// rather than silently keeping the existing definition (MAJOR 2, codex
// review, docs/plans/workspace-db-consolidation.md — see
// ensureLegacyKitHostCommandsKnownToDaemon's doc comment for why silently
// keeping-or-discarding is unsafe either way).
func mergeLegacyKitHostCommandsIntoConfig(hcPath string, want orchestrator.HostCommands) (merged map[string]orchestrator.HostCommandSpec, added []string, err error) {
	existing, err := orchestrator.LoadHostCommandsConfig(hcPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load host_commands.yaml: %w", err)
	}

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)

	var conflicts []string
	for _, name := range names {
		cur, ok := existing[name]
		if !ok {
			continue
		}
		if !reflect.DeepEqual(normalizeHostCommandSpecForCompare(cur), normalizeHostCommandSpecForCompare(want[name])) {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) > 0 {
		return nil, nil, fmt.Errorf("%w: %s already defined differently in %s; align the definitions by hand or rename the conflicting command in project.yaml, then re-run migrate",
			errHostCommandsConflict, strings.Join(conflicts, ", "), hcPath)
	}

	merged = make(map[string]orchestrator.HostCommandSpec, len(existing)+len(names))
	for k, v := range existing {
		merged[k] = v
	}
	for _, name := range names {
		if _, ok := existing[name]; ok {
			continue
		}
		merged[name] = want[name]
		added = append(added, name)
	}
	return merged, added, nil
}

// normalizeHostCommandSpecForCompare mirrors orchestrator's unexported
// normalizeHostCommandSpec (internal/orchestrator/host_commands_config.go):
// it replaces any zero-length slice/map field with nil so two
// otherwise-identical specs compare equal under reflect.DeepEqual
// regardless of whether a field was omitted entirely (nil) or spelled out
// empty in YAML (e.g. `env: {}` vs no `env:` key at all). Duplicated here
// (rather than exporting orchestrator's copy) to keep this fix scoped to
// this file; keep the two in sync if HostCommandSpec's shape ever changes.
func normalizeHostCommandSpecForCompare(spec orchestrator.HostCommandSpec) orchestrator.HostCommandSpec {
	if len(spec.Allow) == 0 {
		spec.Allow = nil
	}
	if len(spec.Deny) == 0 {
		spec.Deny = nil
	}
	if len(spec.Env) == 0 {
		spec.Env = nil
	}
	if len(spec.Reject) == 0 {
		spec.Reject = nil
	}
	return spec
}

// rewriteProjectYAML removes migrated keys from project.yaml, preserving
// comments and field order where possible via the yaml.Node API.
func rewriteProjectYAML(plan *migratePlan) error {
	projectYAML := filepath.Join(plan.dir, ".boid", "project.yaml")
	data, err := os.ReadFile(projectYAML)
	if err != nil {
		return fmt.Errorf("read project.yaml: %w", err)
	}

	var rootNode yaml.Node
	if err := yaml.Unmarshal(data, &rootNode); err != nil {
		return fmt.Errorf("parse project.yaml: %w", err)
	}
	if rootNode.Kind == 0 || len(rootNode.Content) == 0 {
		return nil // empty file
	}
	docNode := &rootNode
	if docNode.Kind == yaml.DocumentNode {
		if len(docNode.Content) == 0 {
			return nil
		}
		docNode = docNode.Content[0]
	}
	if docNode.Kind != yaml.MappingNode {
		return fmt.Errorf("project.yaml: expected top-level mapping")
	}

	// Keys to remove from the top-level mapping.
	topRemove := stringSet(plan.removeKeys)

	// Additionally we need to remove behavior-level kits.
	behaviorKitsToStrip := make(map[string]bool)
	for bname, b := range plan.meta.TaskBehaviors {
		if len(b.Kits) > 0 {
			behaviorKitsToStrip[bname] = true
		}
	}

	// Walk the mapping node and rebuild Content without the removed keys.
	docNode.Content = filterMappingNode(docNode.Content, topRemove, behaviorKitsToStrip)

	out, err := yaml.Marshal(&rootNode)
	if err != nil {
		return fmt.Errorf("marshal project.yaml: %w", err)
	}

	// Atomic write via temp file.
	tmpPath := projectYAML + ".migrate.tmp"
	if err := os.WriteFile(tmpPath, out, 0o644); err != nil {
		return fmt.Errorf("write tmp project.yaml: %w", err)
	}
	if err := os.Rename(tmpPath, projectYAML); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename project.yaml: %w", err)
	}
	return nil
}

// filterMappingNode removes key-value pairs whose keys appear in removeKeys,
// and strips the "kits" sub-key from task_behaviors entries listed in
// behaviorKitsToStrip.
func filterMappingNode(content []*yaml.Node, removeKeys map[string]bool, behaviorKitsToStrip map[string]bool) []*yaml.Node {
	var result []*yaml.Node
	for i := 0; i+1 < len(content); i += 2 {
		keyNode := content[i]
		valNode := content[i+1]

		if removeKeys[keyNode.Value] {
			continue // drop this key-value pair
		}

		// For task_behaviors, strip the kits sub-key from each behavior entry.
		if keyNode.Value == "task_behaviors" && len(behaviorKitsToStrip) > 0 {
			if valNode.Kind == yaml.MappingNode {
				stripBehaviorKits(valNode, behaviorKitsToStrip)
			}
		}

		result = append(result, keyNode, valNode)
	}
	return result
}

// stripBehaviorKits removes the "kits" key from each behavior in the
// task_behaviors mapping node that appears in the strip set.
func stripBehaviorKits(taskBehaviorsNode *yaml.Node, strip map[string]bool) {
	for i := 0; i+1 < len(taskBehaviorsNode.Content); i += 2 {
		behaviorNameNode := taskBehaviorsNode.Content[i]
		behaviorValNode := taskBehaviorsNode.Content[i+1]
		if !strip[behaviorNameNode.Value] {
			continue
		}
		if behaviorValNode.Kind != yaml.MappingNode {
			continue
		}
		// Remove "kits" key from this behavior.
		var newContent []*yaml.Node
		for j := 0; j+1 < len(behaviorValNode.Content); j += 2 {
			if behaviorValNode.Content[j].Value == "kits" {
				continue
			}
			newContent = append(newContent, behaviorValNode.Content[j], behaviorValNode.Content[j+1])
		}
		behaviorValNode.Content = newContent
	}
}

// migrateSecrets copies secrets from the old namespace to the new namespace,
// respecting the onCollision policy. Progress output is written to out.
func migrateSecrets(plan *migratePlan, boidDB *db.DB, keyFilePath, onCollision string, out io.Writer) error {
	kfPath := keyFilePath
	if kfPath == "" {
		kfPath = defaultKeyFilePath()
	}
	key, err := loadKeyFileIfExists(kfPath)
	if err != nil {
		return fmt.Errorf("load key file: %w", err)
	}
	if key == nil {
		fmt.Fprintln(out, "warning: key file not found; skipping secret migration")
		return nil
	}

	store, err := dispatcher.NewSecretStore(boidDB.Conn, key)
	if err != nil {
		return fmt.Errorf("open secret store: %w", err)
	}

	// Build collision set.
	collisionSet := make(map[string]bool)
	for _, c := range plan.secretCollisions {
		collisionSet[c.Key] = true
	}

	for _, k := range plan.secretKeys {
		if collisionSet[k] {
			switch onCollision {
			case "skip":
				fmt.Fprintf(out, "  skip (collision): %s\n", k)
				continue
			case "overwrite":
				// Fall through to copy.
			case "refuse":
				// Should have been caught earlier, but be safe.
				return fmt.Errorf("collision on key %q and --on-collision=refuse", k)
			}
		}

		val, err := store.Get(plan.oldNamespace, k)
		if err != nil {
			return fmt.Errorf("get secret %q from %q: %w", k, plan.oldNamespace, err)
		}
		if err := store.Set(plan.newNamespace, k, val); err != nil {
			return fmt.Errorf("set secret %q in %q: %w", k, plan.newNamespace, err)
		}
		fmt.Fprintf(out, "  copied: %s\n", k)
	}
	return nil
}

// stringSet converts a string slice to a membership set.
func stringSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// legacyKitNameFor returns the kit-name to use for a project's auto-generated
// legacy kit. It prefers a slug of the human-readable project name (so the kit
// is easy to spot in `boid kit list` / on disk) and only falls back to the raw
// project ID when the name has no usable characters. When the chosen base
// collides on disk with a legacy kit produced from a *different* project, a
// short ID suffix is appended; a re-migration of the same project always lands
// on the same dir (idempotent).
//
// Output is always a valid kit name per orchestrator.ValidKitName: lowercase
// ASCII letters / digits / hyphens, 1–64 chars.
func legacyKitNameFor(projectName, projectID, kitsBaseDir string) string {
	// Reserve room for "legacy-" prefix (7) and the disambiguation suffix
	// "-xxxxxxxx" (9) so the final name still fits the 64-char kit-name limit
	// even when we have to append the suffix.
	const maxBase = 64 - 7 - 9

	slug := slugifyForKitName(projectName)
	if slug == "" {
		slug = idSuffix(projectID, 8)
	}
	if slug == "" {
		// Last-resort fallback: should be unreachable because ReadProjectMetaLegacy
		// rejects empty IDs, but keeps ValidKitName happy if invariants ever shift.
		slug = "project"
	}
	if len(slug) > maxBase {
		slug = strings.TrimRight(slug[:maxBase], "-")
		if slug == "" {
			slug = idSuffix(projectID, 8)
		}
	}

	base := "legacy-" + slug
	if !legacyKitDirBelongsToOtherProject(filepath.Join(kitsBaseDir, base), projectID) {
		return base
	}

	suffix := idSuffix(projectID, 8)
	if suffix == "" {
		suffix = "x"
	}
	return base + "-" + suffix
}

// slugifyForKitName converts an arbitrary project name to a kit-name-safe slug:
// lowercase, ASCII letters / digits / hyphens only, with non-conforming runes
// replaced by '-', runs of '-' collapsed, and leading/trailing '-' trimmed.
// Returns "" when the input collapses to nothing (e.g. all-CJK names) — callers
// fall back to an ID-derived suffix in that case.
func slugifyForKitName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

// idSuffix returns the first n hyphen-stripped characters of id, lowercased.
// Used as a deterministic short disambiguator derived from the project ID.
func idSuffix(id string, n int) string {
	s := strings.ToLower(strings.ReplaceAll(id, "-", ""))
	if len(s) > n {
		return s[:n]
	}
	return s
}

// legacyKitDirBelongsToOtherProject returns true when dir exists, contains a
// readable kit.yaml whose source_project_id is set to a value *different* from
// the given projectID. It returns false (= safe to (re)use the dir) when:
//   - the dir does not exist,
//   - kit.yaml is missing or unparseable (treat as unowned for forward-compat),
//   - source_project_id is empty (kits written by an older boid),
//   - source_project_id matches projectID (same project re-migrating).
func legacyKitDirBelongsToOtherProject(dir, projectID string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "kit.yaml"))
	if err != nil {
		return false
	}
	var existing legacyKitYAML
	if err := yaml.Unmarshal(data, &existing); err != nil {
		return false
	}
	if existing.Meta.SourceProjectID == "" {
		return false
	}
	return existing.Meta.SourceProjectID != projectID
}
