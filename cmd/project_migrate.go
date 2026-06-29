package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// to be running — all I/O is direct file/DB access.
var projectMigrateCmd = &cobra.Command{
	Use:   "migrate <dir>",
	Short: "Migrate a project from the old project.yaml schema to the new workspace+kit schema (daemon-free)",
	Long: `Migrate reads the project.yaml under <dir>/.boid/ using the legacy schema,
generates a workspace.yaml update and optionally a legacy kit.yaml, and rewrites
project.yaml to remove the fields that have moved.

By default the command runs in dry-run mode: it prints what it would do without
writing anything. Pass --apply to actually perform the migration.

Secret migration copies secrets from the old namespace (secret_namespace field)
to the workspace namespace. Use --on-collision to control behaviour when a
key already exists in the new namespace (default: refuse and list collisions).`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{annotationSkipAutostart: "skip"},
	RunE:        runProjectMigrate,
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

	// Load existing workspace.yaml (may not exist yet).
	existingWS, err := wsStore.Load(plan.workspaceSlug)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("load workspace %q: %w", plan.workspaceSlug, err)
	}
	if existingWS == nil {
		existingWS = &orchestrator.WorkspaceMeta{}
	}

	// Build new workspace meta by merging legacy fields.
	newWS := *existingWS

	// Collect all kit refs from project top level and all behaviors.
	// Normalize to simple name form (last segment). Invalid refs (whose
	// derived name fails ValidKitName, e.g. a bare "github.com") fail the
	// migration outright rather than silently producing an unloadable kit
	// slug in workspace.yaml — past versions did the latter and left
	// invalid kit dirs that later tripped boid-kit-init cleanup.
	kitRefStrs, err := collectKitNames(meta)
	if err != nil {
		return fmt.Errorf("collect kit names: %w", err)
	}
	existingKitSet := stringSet(existingWS.Kits)
	for _, name := range kitRefStrs {
		if !existingKitSet[name] {
			newWS.Kits = append(newWS.Kits, name)
			existingKitSet[name] = true
		}
	}

	// Env: merge, new values take precedence (match spec: "merge で新値が優先")
	if len(meta.Env) > 0 {
		if newWS.Env == nil {
			newWS.Env = make(map[string]string)
		}
		for k, v := range meta.Env {
			newWS.Env[k] = v
		}
	}

	// Capabilities.
	if meta.Capabilities.Docker != nil {
		newWS.Capabilities.Docker = meta.Capabilities.Docker
	}

	// Legacy kit (host_commands + additional_bindings).
	needsLegacyKit := len(meta.HostCommands) > 0 || len(meta.AdditionalBindings) > 0
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

		// Add the legacy kit to workspace.Kits if not already present.
		if !existingKitSet[kitName] {
			newWS.Kits = append(newWS.Kits, kitName)
			existingKitSet[kitName] = true
		}

		plan.needsLegacyKit = true
		plan.legacyKitDir = legacyKitDir
		plan.legacyKitMeta = kitMeta
	}

	plan.workspaceMeta = &newWS

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

	// 1. Write workspace.yaml.
	if err := wsStore.Save(plan.workspaceSlug, plan.workspaceMeta); err != nil {
		return fmt.Errorf("save workspace.yaml: %w", err)
	}

	// 2. Write legacy kit.yaml.
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
