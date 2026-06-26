package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/spf13/cobra"
)

// ---- helpers ----------------------------------------------------------------

// setupMigrateProject creates a temporary project directory with .boid/project.yaml.
func setupMigrateProject(t *testing.T, projectYAML string) string {
	t.Helper()
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return dir
}

// setupMigrateDB creates a temp SQLite file with boid schema applied.
func setupMigrateDBFile(t *testing.T) string {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "boid.db")
	boidDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(boidDB.Conn); err != nil {
		boidDB.Close()
		t.Fatalf("migrate db: %v", err)
	}
	boidDB.Close()
	return dbFile
}

// setupSecretStoreOnFile opens the DB at dbFile, pre-populates secrets, and
// returns the key so callers can verify after the migration.
func setupSecretsOnFile(t *testing.T, dbFile string, namespace string, kvs map[string]string) []byte {
	t.Helper()
	boidDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer boidDB.Close()
	key := dispatcher.GenerateKey()
	store, err := dispatcher.NewSecretStore(boidDB.Conn, key)
	if err != nil {
		t.Fatalf("new secret store: %v", err)
	}
	for k, v := range kvs {
		if err := store.Set(namespace, k, v); err != nil {
			t.Fatalf("set secret %q: %v", k, err)
		}
	}
	return key
}

// writeKeyFileForMigrate writes the given key to a temp file and returns its path.
func writeKeyFileForMigrate(t *testing.T, key []byte) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(p, key, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return p
}

// invokeMigrate calls runProjectMigrate via a fresh cobra.Command, setting the
// package-level flag variables directly (mirrors how cobra would set them).
func invokeMigrate(t *testing.T, dir string, opts migrateOpts) error {
	t.Helper()

	// Set global flag vars.
	migrateWorkspace = opts.workspace
	migrateApply = opts.apply
	migrateOnCollision = "refuse"
	if opts.onCollision != "" {
		migrateOnCollision = opts.onCollision
	}
	migrateDBPath = opts.dbPath
	migrateKeyFilePath = opts.keyPath

	// Set XDG dirs for workspace store and kits dir.
	if opts.xdgConfigHome != "" {
		t.Setenv("XDG_CONFIG_HOME", opts.xdgConfigHome)
	}
	if opts.xdgDataHome != "" {
		t.Setenv("XDG_DATA_HOME", opts.xdgDataHome)
	}

	cmd := &cobra.Command{Use: "migrate"}
	cmd.Flags().StringVar(&migrateWorkspace, "workspace", migrateWorkspace, "")
	cmd.Flags().BoolVar(&migrateApply, "apply", migrateApply, "")
	cmd.Flags().StringVar(&migrateOnCollision, "on-collision", migrateOnCollision, "")
	cmd.Flags().StringVar(&migrateDBPath, "db-path", migrateDBPath, "")
	cmd.Flags().StringVar(&migrateKeyFilePath, "key-file-path", migrateKeyFilePath, "")

	return runProjectMigrate(cmd, []string{dir})
}

type migrateOpts struct {
	workspace     string
	apply         bool
	onCollision   string
	dbPath        string
	keyPath       string
	xdgConfigHome string
	xdgDataHome   string
}

// readProjectYAMLContent reads .boid/project.yaml as a string.
func readProjectYAMLContent(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".boid", "project.yaml"))
	if err != nil {
		t.Fatalf("read project.yaml: %v", err)
	}
	return string(data)
}

// stringSetFromSlice converts a string slice to a membership set.
func stringSetFromSlice(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// ---- Tests ------------------------------------------------------------------

const testLegacyProjectYAML = `id: proj-abc123
name: My Project
kits:
  - github.com/novshi-tech/boid-kits/go
env:
  GOPATH: /home/user/go
  DEBUG: "1"
host_commands:
  gh:
    allow:
      - pr
      - issue
additional_bindings:
  - source: /var/data
    mode: ro
secret_namespace: myproject
capabilities:
  docker: {}
task_behaviors:
  dev:
    traits:
      - artifact
    kits:
      - github.com/novshi-tech/boid-kits/node
`

// TestProjectMigrate_NoWorkspaceFlag_FallsBackToDefault verifies that running
// migrate without --workspace and without an existing project_workspaces row
// falls back to the implicit default workspace slug instead of erroring.
func TestProjectMigrate_NoWorkspaceFlag_FallsBackToDefault(t *testing.T) {
	dir := setupMigrateProject(t, testLegacyProjectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		// workspace is intentionally empty.
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply with default fallback: unexpected error: %v", err)
	}

	wsPath := filepath.Join(cfgDir, "boid", "workspaces")
	store := orchestrator.NewWorkspaceStore(wsPath)
	_, err = store.Load(orchestrator.DefaultWorkspaceSlug)
	if err != nil {
		t.Fatalf("default workspace.yaml not produced: %v", err)
	}
}

// TestProjectMigrate_DryRun verifies that dry-run mode does not write any files.
func TestProjectMigrate_DryRun(t *testing.T) {
	dir := setupMigrateProject(t, testLegacyProjectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	origYAML := readProjectYAMLContent(t, dir)

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "my-ws",
		apply:         false, // dry-run
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
	})
	if err != nil {
		t.Fatalf("dry-run: unexpected error: %v", err)
	}

	// project.yaml must be unchanged.
	if got := readProjectYAMLContent(t, dir); got != origYAML {
		t.Errorf("dry-run modified project.yaml:\ngot:\n%s\nwant:\n%s", got, origYAML)
	}

	// workspace.yaml must not have been created.
	wsPath := filepath.Join(cfgDir, "boid", "workspaces", "my-ws.yaml")
	if _, err := os.Stat(wsPath); !os.IsNotExist(err) {
		t.Errorf("dry-run created workspace.yaml unexpectedly")
	}
}

// TestProjectMigrate_Apply_WritesWorkspaceYAML verifies --apply creates workspace.yaml
// with the migrated fields.
func TestProjectMigrate_Apply_WritesWorkspaceYAML(t *testing.T) {
	dir := setupMigrateProject(t, testLegacyProjectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "my-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	wsStore := orchestrator.NewWorkspaceStore(filepath.Join(cfgDir, "boid", "workspaces"))
	wsMeta, err := wsStore.Load("my-ws")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	// Kit refs normalised to last segment name.
	kitSet := stringSetFromSlice(wsMeta.Kits)
	if !kitSet["go"] {
		t.Errorf("workspace kits missing 'go': %v", wsMeta.Kits)
	}
	if !kitSet["node"] {
		t.Errorf("workspace kits missing 'node': %v", wsMeta.Kits)
	}

	if wsMeta.Env["GOPATH"] != "/home/user/go" {
		t.Errorf("workspace env GOPATH = %q, want /home/user/go", wsMeta.Env["GOPATH"])
	}
	if wsMeta.Capabilities.Docker == nil {
		t.Errorf("workspace capabilities.docker should be set")
	}
}

// TestProjectMigrate_Apply_RemovesProjectKeys verifies --apply removes migrated
// keys from project.yaml.
func TestProjectMigrate_Apply_RemovesProjectKeys(t *testing.T) {
	dir := setupMigrateProject(t, testLegacyProjectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "my-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	updatedYAML := readProjectYAMLContent(t, dir)

	for _, key := range []string{"kits:", "env:", "host_commands:", "additional_bindings:", "secret_namespace:", "capabilities:"} {
		if strings.Contains(updatedYAML, key) {
			t.Errorf("project.yaml still contains %q after migration:\n%s", key, updatedYAML)
		}
	}

	// Core fields must be retained.
	if !strings.Contains(updatedYAML, "id:") {
		t.Errorf("project.yaml lost 'id' field")
	}
	if !strings.Contains(updatedYAML, "task_behaviors:") {
		t.Errorf("project.yaml lost 'task_behaviors'")
	}
}

// TestProjectMigrate_Apply_LegacyKit verifies that a legacy kit.yaml is created
// for projects with host_commands or additional_bindings.
func TestProjectMigrate_Apply_LegacyKit(t *testing.T) {
	dir := setupMigrateProject(t, testLegacyProjectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "my-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	kitPath := filepath.Join(dataDir, "boid", "kits", "legacy-proj-abc123", "kit.yaml")
	data, err := os.ReadFile(kitPath)
	if err != nil {
		t.Fatalf("legacy kit.yaml not found at %s: %v", kitPath, err)
	}
	kitContent := string(data)

	if !strings.Contains(kitContent, "legacy-proj-abc123") {
		t.Errorf("kit.yaml missing kit name:\n%s", kitContent)
	}
	if !strings.Contains(kitContent, "gh") {
		t.Errorf("kit.yaml missing host_commands 'gh':\n%s", kitContent)
	}
	if !strings.Contains(kitContent, "/var/data") {
		t.Errorf("kit.yaml missing additional_bindings source:\n%s", kitContent)
	}

	// Env must NOT appear in the legacy kit; per the plan's transformation
	// table, env migrates only to workspace.yaml, not to the legacy kit.
	if strings.Contains(kitContent, "\nenv:") {
		t.Errorf("legacy kit.yaml must not contain top-level env: (env should live in workspace.yaml only):\n%s", kitContent)
	}
}

// TestProjectMigrate_NoLegacyKit verifies no legacy kit is created when
// host_commands and additional_bindings are absent.
func TestProjectMigrate_NoLegacyKit(t *testing.T) {
	minimalYAML := `id: proj-minimal
name: Minimal Project
kits:
  - mykit
env:
  FOO: bar
`
	dir := setupMigrateProject(t, minimalYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "test-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	kitDir := filepath.Join(dataDir, "boid", "kits", "legacy-proj-minimal")
	if _, err := os.Stat(kitDir); !os.IsNotExist(err) {
		t.Errorf("legacy kit dir unexpectedly created: %s", kitDir)
	}
}

// TestProjectMigrate_SecretCopyNoCollision verifies secrets are copied from old
// to new namespace when there are no collisions.
func TestProjectMigrate_SecretCopyNoCollision(t *testing.T) {
	projectYAML := `id: proj-secret
name: Secret Project
secret_namespace: myproject
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	key := setupSecretsOnFile(t, dbFile, "myproject", map[string]string{
		"GH_TOKEN": "ghp_abc",
	})
	keyPath := writeKeyFileForMigrate(t, key)

	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "my-ws",
		apply:         true,
		dbPath:        dbFile,
		keyPath:       keyPath,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	// Verify secret was copied to new namespace.
	boidDB2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer boidDB2.Close()
	store2, err := dispatcher.NewSecretStore(boidDB2.Conn, key)
	if err != nil {
		t.Fatalf("new store2: %v", err)
	}

	val, err := store2.Get("my-ws", "GH_TOKEN")
	if err != nil {
		t.Fatalf("get secret from new namespace: %v", err)
	}
	if val != "ghp_abc" {
		t.Errorf("secret in new namespace = %q, want ghp_abc", val)
	}

	// Old namespace secret must still exist (not deleted).
	oldVal, err := store2.Get("myproject", "GH_TOKEN")
	if err != nil {
		t.Fatalf("get secret from old namespace: %v", err)
	}
	if oldVal != "ghp_abc" {
		t.Errorf("secret in old namespace = %q, want ghp_abc", oldVal)
	}
}

// TestProjectMigrate_SecretCollision_Refuse verifies that collisions cause an
// error under the default --on-collision=refuse policy.
func TestProjectMigrate_SecretCollision_Refuse(t *testing.T) {
	projectYAML := `id: proj-coll
name: Collision Project
secret_namespace: oldns
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	key := setupSecretsOnFile(t, dbFile, "oldns", map[string]string{
		"MY_KEY": "old-value",
	})
	// Also set the same key in the new namespace (target = workspace slug).
	{
		boidDB, _ := db.Open(dbFile)
		store, _ := dispatcher.NewSecretStore(boidDB.Conn, key)
		store.Set("new-ws", "MY_KEY", "different-value")
		boidDB.Close()
	}

	keyPath := writeKeyFileForMigrate(t, key)
	cfgDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "new-ws",
		apply:         true,
		onCollision:   "refuse",
		dbPath:        dbFile,
		keyPath:       keyPath,
		xdgConfigHome: cfgDir,
	})
	if err == nil {
		t.Fatal("expected error due to collision, got nil")
	}
	if !strings.Contains(err.Error(), "collision") {
		t.Errorf("error does not mention collision: %v", err)
	}
}

// TestProjectMigrate_SecretCollision_Skip verifies --on-collision=skip leaves
// colliding keys alone and copies non-colliding keys.
func TestProjectMigrate_SecretCollision_Skip(t *testing.T) {
	projectYAML := `id: proj-skip
name: Skip Project
secret_namespace: oldns
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	key := setupSecretsOnFile(t, dbFile, "oldns", map[string]string{
		"KEY_A": "old-a", // collision
		"KEY_B": "old-b", // no collision
	})
	{
		boidDB, _ := db.Open(dbFile)
		store, _ := dispatcher.NewSecretStore(boidDB.Conn, key)
		store.Set("skip-ws", "KEY_A", "existing-a")
		boidDB.Close()
	}

	keyPath := writeKeyFileForMigrate(t, key)
	cfgDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "skip-ws",
		apply:         true,
		onCollision:   "skip",
		dbPath:        dbFile,
		keyPath:       keyPath,
		xdgConfigHome: cfgDir,
	})
	if err != nil {
		t.Fatalf("apply with skip: unexpected error: %v", err)
	}

	boidDB2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer boidDB2.Close()
	store2, err := dispatcher.NewSecretStore(boidDB2.Conn, key)
	if err != nil {
		t.Fatalf("new store2: %v", err)
	}

	// KEY_A should remain unchanged (skip).
	valA, err := store2.Get("skip-ws", "KEY_A")
	if err != nil {
		t.Fatalf("get KEY_A: %v", err)
	}
	if valA != "existing-a" {
		t.Errorf("KEY_A after skip = %q, want existing-a", valA)
	}

	// KEY_B should be copied.
	valB, err := store2.Get("skip-ws", "KEY_B")
	if err != nil {
		t.Fatalf("get KEY_B: %v", err)
	}
	if valB != "old-b" {
		t.Errorf("KEY_B after skip = %q, want old-b", valB)
	}
}

// TestProjectMigrate_SecretCollision_Overwrite verifies --on-collision=overwrite
// overwrites colliding keys.
func TestProjectMigrate_SecretCollision_Overwrite(t *testing.T) {
	projectYAML := `id: proj-ow
name: Overwrite Project
secret_namespace: oldns
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	key := setupSecretsOnFile(t, dbFile, "oldns", map[string]string{
		"KEY_A": "new-value",
	})
	{
		boidDB, _ := db.Open(dbFile)
		store, _ := dispatcher.NewSecretStore(boidDB.Conn, key)
		store.Set("ow-ws", "KEY_A", "old-value")
		boidDB.Close()
	}

	keyPath := writeKeyFileForMigrate(t, key)
	cfgDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "ow-ws",
		apply:         true,
		onCollision:   "overwrite",
		dbPath:        dbFile,
		keyPath:       keyPath,
		xdgConfigHome: cfgDir,
	})
	if err != nil {
		t.Fatalf("apply with overwrite: unexpected error: %v", err)
	}

	boidDB2, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db2: %v", err)
	}
	defer boidDB2.Close()
	store2, err := dispatcher.NewSecretStore(boidDB2.Conn, key)
	if err != nil {
		t.Fatalf("new store2: %v", err)
	}

	val, err := store2.Get("ow-ws", "KEY_A")
	if err != nil {
		t.Fatalf("get KEY_A: %v", err)
	}
	if val != "new-value" {
		t.Errorf("KEY_A after overwrite = %q, want new-value", val)
	}
}

// TestProjectMigrate_ExistingWorkspaceYAML_Merge verifies existing workspace.yaml
// is merged: kits are appended without duplicates, env is merged.
func TestProjectMigrate_ExistingWorkspaceYAML_Merge(t *testing.T) {
	projectYAML := `id: proj-merge
name: Merge Project
kits:
  - mykit
env:
  NEW_VAR: "hello"
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	// Pre-populate workspace.yaml.
	wsPath := filepath.Join(cfgDir, "boid", "workspaces")
	os.MkdirAll(wsPath, 0o755)
	preStore := orchestrator.NewWorkspaceStore(wsPath)
	preMeta := &orchestrator.WorkspaceMeta{
		Kits: []string{"existing-kit", "mykit"}, // mykit should not be duplicated
		Env:  map[string]string{"EXISTING": "yes"},
	}
	if err := preStore.Save("merge-ws", preMeta); err != nil {
		t.Fatalf("pre-save workspace: %v", err)
	}

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "merge-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	postStore := orchestrator.NewWorkspaceStore(wsPath)
	meta, err := postStore.Load("merge-ws")
	if err != nil {
		t.Fatalf("load workspace after merge: %v", err)
	}

	kitSet := stringSetFromSlice(meta.Kits)
	if !kitSet["existing-kit"] {
		t.Errorf("merged workspace lost 'existing-kit': %v", meta.Kits)
	}
	if !kitSet["mykit"] {
		t.Errorf("merged workspace lost 'mykit': %v", meta.Kits)
	}

	// mykit must not be duplicated.
	count := 0
	for _, k := range meta.Kits {
		if k == "mykit" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mykit appears %d times (want 1): %v", count, meta.Kits)
	}

	if meta.Env["EXISTING"] != "yes" {
		t.Errorf("env EXISTING lost: %v", meta.Env)
	}
	if meta.Env["NEW_VAR"] != "hello" {
		t.Errorf("env NEW_VAR not merged: %v", meta.Env)
	}
}

// TestProjectMigrate_BehaviorKitsAggregated verifies that behavior-level kits
// are moved to workspace kits and cleared from project.yaml.
func TestProjectMigrate_BehaviorKitsAggregated(t *testing.T) {
	projectYAML := `id: proj-bkits
name: Behavior Kits Project
task_behaviors:
  dev:
    traits:
      - artifact
    kits:
      - github.com/novshi-tech/boid-kits/node
  plan:
    traits:
      - artifact
    kits:
      - github.com/novshi-tech/boid-kits/node
      - localkit
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "bk-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	wsPath := filepath.Join(cfgDir, "boid", "workspaces")
	wsStore := orchestrator.NewWorkspaceStore(wsPath)
	meta, err := wsStore.Load("bk-ws")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}

	kitSet := stringSetFromSlice(meta.Kits)
	if !kitSet["node"] {
		t.Errorf("workspace kits missing 'node': %v", meta.Kits)
	}
	if !kitSet["localkit"] {
		t.Errorf("workspace kits missing 'localkit': %v", meta.Kits)
	}

	// project.yaml must not have behavior-level kits.
	updatedYAML := readProjectYAMLContent(t, dir)
	// The kits key would appear indented under a behavior if still present.
	if strings.Contains(updatedYAML, "    kits:") {
		t.Errorf("project.yaml still has behavior-level kits:\n%s", updatedYAML)
	}
}
