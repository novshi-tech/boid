package cmd

import (
	"crypto/sha256"
	"errors"
	"fmt"
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
type legacyKitYAML struct {
	Meta               legacyKitMeta              `yaml:"meta"`
	HostCommands       orchestrator.HostCommands   `yaml:"host_commands,omitempty"`
	AdditionalBindings []orchestrator.BindMount    `yaml:"additional_bindings,omitempty"`
	Env                map[string]string           `yaml:"env,omitempty"`
}

type legacyKitMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Category    string `yaml:"category"`
}

func runProjectMigrate(cmd *cobra.Command, args []string) error {
	dir, err := filepath.Abs(args[0])
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}

	// Validate --on-collision flag.
	switch migrateOnCollision {
	case "refuse", "skip", "overwrite":
	default:
		return fmt.Errorf("--on-collision must be one of: refuse, skip, overwrite (got %q)", migrateOnCollision)
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
	dbPath := migrateDBPath
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

	workspaceSlug := migrateWorkspace
	if workspaceSlug == "" && project != nil && project.WorkspaceID != "" {
		workspaceSlug = project.WorkspaceID
	}
	if workspaceSlug == "" {
		// List available workspaces as guidance.
		wsStore := orchestrator.NewWorkspaceStore("")
		slugs, _ := wsStore.List()
		msg := "migrate: workspace slug could not be resolved.\n" +
			"  Use --workspace <slug> to specify the workspace.\n"
		if len(slugs) > 0 {
			msg += "  Available workspaces: " + strings.Join(slugs, ", ")
		} else {
			msg += "  No workspaces are configured yet."
		}
		return fmt.Errorf("%s", msg)
	}
	if err := orchestrator.ValidWorkspaceSlug(workspaceSlug); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	plan.workspaceSlug = workspaceSlug

	// ---- Compute what changes will be made -----------------------------------
	if err := computeMigratePlan(plan, boidDB, migrateKeyFilePath); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// ---- Phase 3: Print plan (dry-run output) --------------------------------
	printMigratePlan(cmd, plan)

	// Check for collisions before apply.
	if len(plan.secretCollisions) > 0 && migrateOnCollision == "refuse" {
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "ERROR: secret collisions detected. Migration aborted.")
		fmt.Fprintln(cmd.OutOrStdout(), "  Use --on-collision skip  to copy non-conflicting keys")
		fmt.Fprintln(cmd.OutOrStdout(), "  Use --on-collision overwrite  to overwrite all (DANGEROUS)")
		return fmt.Errorf("secret collisions require --on-collision flag")
	}

	// ---- Phase 4: Apply (only when --apply is set) --------------------------
	if !migrateApply {
		fmt.Fprintln(cmd.OutOrStdout())
		fmt.Fprintln(cmd.OutOrStdout(), "(dry-run) Pass --apply to execute the migration.")
		return nil
	}

	if err := applyMigratePlan(plan, boidDB, migrateKeyFilePath); err != nil {
		return fmt.Errorf("migrate: apply: %w", err)
	}

	fmt.Fprintln(cmd.OutOrStdout(), "Migration complete.")
	return nil
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
	// Normalize to simple name form (last segment).
	kitRefStrs := collectKitNames(meta)
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
		kitName := "legacy-" + meta.ID
		legacyKitDir := filepath.Join(kitsBaseDir, kitName)

		kitMeta := &legacyKitYAML{
			Meta: legacyKitMeta{
				Name:        kitName,
				Description: "Auto-migrated from project.yaml of " + meta.Name,
				Category:    "legacy",
			},
			HostCommands:       meta.HostCommands,
			AdditionalBindings: meta.AdditionalBindings,
			Env:                meta.Env,
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
func collectKitNames(meta *orchestrator.LegacyProjectMeta) []string {
	seen := make(map[string]bool)
	var result []string
	add := func(ref string) {
		name := kitRefToName(ref)
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}
	for _, r := range meta.Kits {
		add(r.Ref)
	}
	for _, b := range meta.TaskBehaviors {
		for _, r := range b.Kits {
			add(r.Ref)
		}
	}
	return result
}

// kitRefToName returns the simple name for a kit ref. For a full path like
// "github.com/novshi-tech/boid-kits/go" this returns "go". For a simple name
// like "go" this returns "go".
func kitRefToName(ref string) string {
	parts := strings.Split(ref, "/")
	return parts[len(parts)-1]
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
func printMigratePlan(cmd *cobra.Command, plan *migratePlan) {
	out := cmd.OutOrStdout()
	mode := "dry-run"
	if migrateApply {
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
// kit.yaml, updates project.yaml, and copies secrets.
func applyMigratePlan(plan *migratePlan, boidDB *db.DB, keyFilePath string) error {
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
		if err := migrateSecrets(plan, boidDB, keyFilePath); err != nil {
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
// respecting the --on-collision policy.
func migrateSecrets(plan *migratePlan, boidDB *db.DB, keyFilePath string) error {
	kfPath := keyFilePath
	if kfPath == "" {
		kfPath = defaultKeyFilePath()
	}
	key, err := loadKeyFileIfExists(kfPath)
	if err != nil {
		return fmt.Errorf("load key file: %w", err)
	}
	if key == nil {
		fmt.Println("warning: key file not found; skipping secret migration")
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
			switch migrateOnCollision {
			case "skip":
				fmt.Printf("  skip (collision): %s\n", k)
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
		fmt.Printf("  copied: %s\n", k)
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
