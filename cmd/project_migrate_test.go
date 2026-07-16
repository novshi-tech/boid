package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/server"
	"github.com/novshi-tech/boid/testutil"
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

	// MAJOR 4 (codex review, docs/plans/workspace-db-consolidation.md):
	// --apply now best-effort-pushes the migrated workspace to whatever
	// daemon client.DefaultSocketPath() resolves to. Without pinning this to
	// an isolated, guaranteed-empty path, these tests would silently dial
	// the machine's real running boid daemon (if any) over its real
	// $XDG_RUNTIME_DIR/boid.sock and create real workspace rows there.
	// opts.socketPath lets a test opt into a real (test) daemon instead —
	// see TestProjectMigrate_Apply_Push* below, which call MigrateProject
	// directly rather than through this helper.
	socketPath := opts.socketPath
	if socketPath == "" {
		socketPath = filepath.Join(t.TempDir(), "no-daemon-here.sock")
	}
	t.Setenv("BOID_SOCKET", socketPath)

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
	// socketPath overrides the BOID_SOCKET path invokeMigrate pins tests to
	// (see its doc comment). Empty uses an isolated, guaranteed-unreachable
	// path so the daemon-push added for MAJOR 4 deterministically takes the
	// "daemon unreachable" branch.
	socketPath string
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

	kitPath := filepath.Join(dataDir, "boid", "kits", "legacy-my-project", "kit.yaml")
	data, err := os.ReadFile(kitPath)
	if err != nil {
		t.Fatalf("legacy kit.yaml not found at %s: %v", kitPath, err)
	}
	kitContent := string(data)

	if !strings.Contains(kitContent, "legacy-my-project") {
		t.Errorf("kit.yaml missing kit name:\n%s", kitContent)
	}
	// source_project_id must be embedded so re-runs are idempotent.
	if !strings.Contains(kitContent, "source_project_id: proj-abc123") {
		t.Errorf("kit.yaml missing source_project_id:\n%s", kitContent)
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

	kitDir := filepath.Join(dataDir, "boid", "kits", "legacy-minimal-project")
	if _, err := os.Stat(kitDir); !os.IsNotExist(err) {
		t.Errorf("legacy kit dir unexpectedly created: %s", kitDir)
	}
}

// TestProjectMigrate_LegacyKitName_SlugifiesProjectName verifies that the
// auto-generated legacy kit dir is named after a slug of the project's
// human-readable name, not the (UUID) project ID. Real project IDs are 36-char
// UUIDs which produced unreadable "legacy-<uuid>" names before this change.
func TestProjectMigrate_LegacyKitName_SlugifiesProjectName(t *testing.T) {
	projectYAML := `id: 11111111-2222-3333-4444-555555555555
name: My Web App
host_commands:
  gh:
    allow:
      - pr
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	want := filepath.Join(dataDir, "boid", "kits", "legacy-my-web-app", "kit.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected slug-based legacy kit at %s: %v", want, err)
	}

	// The UUID-shaped fallback must NOT have been created.
	bad := filepath.Join(dataDir, "boid", "kits", "legacy-11111111-2222-3333-4444-555555555555")
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Errorf("UUID-shaped legacy kit dir was created (regression): %s", bad)
	}
}

// TestProjectMigrate_LegacyKitName_NonASCIIFallsBackToID verifies that a
// project name with no ASCII letters/digits (e.g. all CJK) falls back to an
// ID-derived suffix so we always produce a valid kit name.
func TestProjectMigrate_LegacyKitName_NonASCIIFallsBackToID(t *testing.T) {
	projectYAML := `id: deadbeef-1234-5678-9abc-def012345678
name: "日本語プロジェクト"
host_commands:
  gh:
    allow:
      - pr
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: unexpected error: %v", err)
	}

	// First 8 hyphen-stripped chars of the ID.
	want := filepath.Join(dataDir, "boid", "kits", "legacy-deadbeef", "kit.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected ID-fallback legacy kit at %s: %v", want, err)
	}
}

// TestProjectMigrate_LegacyKitName_Idempotent verifies that re-running migrate
// on the same project lands on the same legacy kit dir (matching
// source_project_id) instead of appending an ever-growing suffix.
func TestProjectMigrate_LegacyKitName_Idempotent(t *testing.T) {
	projectYAML := `id: 99999999-aaaa-bbbb-cccc-dddddddddddd
name: Reuse Project
host_commands:
  gh:
    allow:
      - pr
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	for i := 0; i < 2; i++ {
		err := invokeMigrate(t, dir, migrateOpts{
			workspace:     "ws",
			apply:         true,
			dbPath:        dbFile,
			xdgConfigHome: cfgDir,
			xdgDataHome:   dataDir,
		})
		if err != nil {
			t.Fatalf("apply iteration %d: %v", i, err)
		}
	}

	kitsDir := filepath.Join(dataDir, "boid", "kits")
	entries, err := os.ReadDir(kitsDir)
	if err != nil {
		t.Fatalf("read kits dir: %v", err)
	}
	var legacyDirs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "legacy-") {
			legacyDirs = append(legacyDirs, e.Name())
		}
	}
	if len(legacyDirs) != 1 || legacyDirs[0] != "legacy-reuse-project" {
		t.Fatalf("expected exactly one legacy kit dir 'legacy-reuse-project', got %v", legacyDirs)
	}
}

// TestProjectMigrate_LegacyKitName_CollisionAppendsIDSuffix verifies that when
// a *different* project already owns the slug-based dir, the new project gets
// an ID-suffixed name instead of clobbering the prior kit.
func TestProjectMigrate_LegacyKitName_CollisionAppendsIDSuffix(t *testing.T) {
	projectYAML := `id: 22222222-3333-4444-5555-666666666666
name: Shared Name
host_commands:
  gh:
    allow:
      - pr
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	// Pre-create a legacy kit owned by a different project at the slug path.
	preDir := filepath.Join(dataDir, "boid", "kits", "legacy-shared-name")
	if err := os.MkdirAll(preDir, 0o755); err != nil {
		t.Fatalf("pre mkdir: %v", err)
	}
	prior := []byte("meta:\n  name: legacy-shared-name\n  description: other\n  category: legacy\n  source_project_id: 00000000-0000-0000-0000-000000000000\n")
	if err := os.WriteFile(filepath.Join(preDir, "kit.yaml"), prior, 0o644); err != nil {
		t.Fatalf("pre write: %v", err)
	}

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Prior owner's kit.yaml must be untouched.
	priorYAML, _ := os.ReadFile(filepath.Join(preDir, "kit.yaml"))
	if !strings.Contains(string(priorYAML), "00000000-0000-0000-0000-000000000000") {
		t.Errorf("prior kit.yaml was overwritten:\n%s", priorYAML)
	}

	// New project gets a suffixed dir.
	want := filepath.Join(dataDir, "boid", "kits", "legacy-shared-name-22222222", "kit.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected suffixed legacy kit at %s: %v", want, err)
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

// TestProjectMigrate_InvalidKitRef_Refuses verifies that a legacy project.yaml
// whose kit ref derives a slug rejected by orchestrator.ValidKitName (e.g. a
// bare "github.com" that yields the literal "github.com" — `.` is invalid)
// fails the migration with a clear error instead of silently producing an
// unloadable kit slug in workspace.yaml. Older versions tolerated this and
// left invalid kit dirs that later tripped boid-kit-init cleanup.
func TestProjectMigrate_InvalidKitRef_Refuses(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		wantIn string // substring expected in the error
	}{
		{
			name: "top-level kits ref",
			yaml: `id: proj-bad
name: Bad Top
kits:
  - github.com
`,
			wantIn: "kits",
		},
		{
			name: "behavior kits ref",
			yaml: `id: proj-bad-b
name: Bad Behavior
task_behaviors:
  dev:
    traits:
      - artifact
    kits:
      - github.com
`,
			wantIn: "task_behaviors.dev.kits",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := setupMigrateProject(t, tc.yaml)
			dbFile := setupMigrateDBFile(t)
			cfgDir := t.TempDir()
			dataDir := t.TempDir()

			err := invokeMigrate(t, dir, migrateOpts{
				workspace:     "bad-ws",
				apply:         true,
				dbPath:        dbFile,
				xdgConfigHome: cfgDir,
				xdgDataHome:   dataDir,
			})
			if err == nil {
				t.Fatalf("expected migrate to refuse invalid ref, got nil")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.wantIn) || !strings.Contains(msg, "github.com") {
				t.Errorf("error should name source (%q) and ref (%q), got: %v", tc.wantIn, "github.com", err)
			}

			// On refusal nothing should be written: no workspace.yaml created.
			wsPath := filepath.Join(cfgDir, "boid", "workspaces", "bad-ws.yaml")
			if _, statErr := os.Stat(wsPath); !os.IsNotExist(statErr) {
				t.Errorf("workspace.yaml should not be written on refusal, got stat err: %v", statErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// MAJOR 4 (codex review, docs/plans/workspace-db-consolidation.md): --apply
// best-effort-pushes the migrated workspace to a live daemon (POST
// /api/workspaces, create-only), since the plain wsStore.Save (yaml mode)
// exercised by every test above only ever writes the now-inert shadow file.
// These use testutil.NewTestServer for a real (test) daemon to push against;
// invokeMigrate's tests above run with BOID_SOCKET pinned to a guaranteed-
// unreachable path (see invokeMigrate's doc comment) and so never exercise
// this branch.
// ---------------------------------------------------------------------------

// TestProjectMigrate_Apply_PushesNewWorkspaceToLiveDaemon verifies that a
// brand-new workspace slug (no DB row yet) is created directly in a
// reachable daemon by --apply, not just written to the shadow yaml.
func TestProjectMigrate_Apply_PushesNewWorkspaceToLiveDaemon(t *testing.T) {
	minimalYAML := `id: proj-push-new
name: Push New
env:
  FOO: bar
`
	dir := setupMigrateProject(t, minimalYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	var out bytes.Buffer
	err := MigrateProject(MigrateProjectOptions{
		Dir:       dir,
		Workspace: "push-new-ws",
		Apply:     true,
		DBPath:    dbFile,
		Out:       &out,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/push-new-ws", nil, &detail); err != nil {
		t.Fatalf("workspace should have been created in the live daemon: %v", err)
	}
	if detail.Meta == nil || detail.Meta.Env["FOO"] != "bar" {
		t.Errorf("workspace env = %+v, want FOO=bar", detail.Meta)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("expected no warning when pushing a brand-new workspace, got: %s", out.String())
	}
}

// TestProjectMigrate_Apply_ExistingLiveWorkspace_MergesAndUpdates verifies
// that when the workspace slug already has a DB row in the live daemon,
// --apply merges the migrated fields into it (GET current content, merge,
// PUT with If-Match) instead of leaving it untouched behind a 409-style
// warning (MAJOR 2, codex review, docs/plans/workspace-db-consolidation.md
// — a create-only POST used to just 409 and never actually apply the
// migration to an already-existing workspace, the common case for the
// `default` workspace).
func TestProjectMigrate_Apply_ExistingLiveWorkspace_MergesAndUpdates(t *testing.T) {
	minimalYAML := `id: proj-push-existing
name: Push Existing
env:
  FOO: bar
`
	dir := setupMigrateProject(t, minimalYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	// Pre-seed the workspace with content distinguishable from what migrate
	// would compute, so we can tell a real merge from a blind overwrite.
	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	if err := repo.Save("push-existing-ws", &orchestrator.WorkspaceMeta{
		Env: map[string]string{"PRE_EXISTING": "marker"},
	}); err != nil {
		t.Fatalf("seed existing workspace: %v", err)
	}

	var out bytes.Buffer
	err := MigrateProject(MigrateProjectOptions{
		Dir:       dir,
		Workspace: "push-existing-ws",
		Apply:     true,
		DBPath:    dbFile,
		Out:       &out,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("expected no warning for a successful merge, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "updated in the daemon") {
		t.Errorf("expected output to report the workspace was updated, got: %s", out.String())
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/push-existing-ws", nil, &detail); err != nil {
		t.Fatalf("get existing workspace: %v", err)
	}
	if detail.Meta == nil || detail.Meta.Env["PRE_EXISTING"] != "marker" {
		t.Errorf("existing workspace content was lost: %+v", detail.Meta)
	}
	if detail.Meta == nil || detail.Meta.Env["FOO"] != "bar" {
		t.Errorf("migrated env was not merged into the live already-existing workspace: %+v", detail.Meta)
	}
}

// TestProjectMigrate_PreservesExistingWorkspaceFields is the exact scenario
// from the codex MAJOR 2 review: an existing workspace already has its own
// env entry (EXISTING: yes, authored some other way — hand edit, a prior
// migration, `boid workspace edit`, ...) that this migration's project.yaml
// says nothing about. --apply must keep it, in addition to adding the
// fields this migration actually carries (NEW: yes) — a union, not a
// replace.
func TestProjectMigrate_PreservesExistingWorkspaceFields(t *testing.T) {
	projectYAML := `id: proj-preserve
name: Preserve Project
env:
  NEW: "yes"
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	if err := repo.Save("preserve-ws", &orchestrator.WorkspaceMeta{
		Env: map[string]string{"EXISTING": "yes"},
	}); err != nil {
		t.Fatalf("seed existing workspace: %v", err)
	}

	var out bytes.Buffer
	err := MigrateProject(MigrateProjectOptions{
		Dir:       dir,
		Workspace: "preserve-ws",
		Apply:     true,
		DBPath:    dbFile,
		Out:       &out,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/preserve-ws", nil, &detail); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if detail.Meta == nil {
		t.Fatalf("workspace meta is nil")
	}
	if detail.Meta.Env["EXISTING"] != "yes" {
		t.Errorf("pre-existing env EXISTING lost: %+v", detail.Meta.Env)
	}
	if detail.Meta.Env["NEW"] != "yes" {
		t.Errorf("migrated env NEW not merged in: %+v", detail.Meta.Env)
	}
}

// TestProjectMigrate_Apply_DaemonUnreachable_WarnsButSucceeds verifies that
// --apply still completes successfully (project.yaml rewritten, shadow yaml
// written) when no daemon is reachable at all — the daemon push is
// best-effort and must never turn into a hard failure of the whole command.
func TestProjectMigrate_Apply_DaemonUnreachable_WarnsButSucceeds(t *testing.T) {
	minimalYAML := `id: proj-push-unreachable
name: Push Unreachable
env:
  FOO: bar
`
	dir := setupMigrateProject(t, minimalYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "push-unreachable-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	wsStore := orchestrator.NewWorkspaceStore(filepath.Join(cfgDir, "boid", "workspaces"))
	if _, err := wsStore.Load("push-unreachable-ws"); err != nil {
		t.Fatalf("shadow workspace.yaml not written: %v", err)
	}
}

// TestProjectMigrate_WithHostCommandsAndBindings is MAJOR 1's regression
// test (codex review, docs/plans/workspace-db-consolidation.md): a legacy
// project.yaml with host_commands + additional_bindings generates a legacy
// kit whose name is folded into workspace.Kits. Before the fix, the daemon
// push (POST /api/workspaces) ran *before* the legacy kit.yaml was written
// to disk and before the daemon's aggregated host_commands.yaml/live cache
// knew about the kit's host_commands names, so
// CreateWorkspace/UpdateWorkspace's post-materialization
// validateHostCommandRefs check 400'd on "unknown host_commands
// reference(s)" — project.yaml still got rewritten (silently "succeeding")
// but the DB never actually gained the migrated workspace content. This
// exercises the whole path end-to-end against a real daemon: kit.yaml must
// land on disk, host_commands.yaml must be synced + reloaded, and *then*
// the workspace create must succeed with host_commands/additional_bindings
// already resolved.
func TestProjectMigrate_WithHostCommandsAndBindings(t *testing.T) {
	projectYAML := `id: proj-hostcmd
name: Host Cmd Project
host_commands:
  gh:
    allow:
      - pr
      - issue
additional_bindings:
  - source: /var/data
    mode: ro
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	kitsDir := filepath.Join(dataDir, "boid", "kits")

	// MUST match cmd/start.go's defaultKitsDir() resolution exactly, so the
	// migrate CLI (writing the legacy kit.yaml) and the daemon (reading it
	// back via KitsDir) agree on where kits live — testutil.NewTestServer
	// does not accept a custom KitsDir, so a bespoke server is constructed
	// here instead (mirrors internal/server/wire_host_commands_test.go's own
	// pattern for the same reason).
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	sockPath := filepath.Join(t.TempDir(), "boid.sock")
	// MigrateProject's daemon push resolves the socket via
	// client.DefaultSocketPath(), which falls back to the real
	// $XDG_RUNTIME_DIR/boid.sock (or /run/user/<uid>/boid.sock) when
	// $BOID_SOCKET is unset — pin it to this test's isolated server so the
	// push never reaches a real daemon that happens to be running on the
	// host (see invokeMigrate's own doc comment for the same concern).
	t.Setenv("BOID_SOCKET", sockPath)

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    kitsDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	c := client.NewUnixClient(sockPath)

	var out bytes.Buffer
	err = MigrateProject(MigrateProjectOptions{
		Dir:       dir,
		Workspace: "hostcmd-ws",
		Apply:     true,
		DBPath:    dbFile,
		Out:       &out,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("expected no warning, got: %s", out.String())
	}

	var detail api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/hostcmd-ws", nil, &detail); err != nil {
		t.Fatalf("workspace should exist in the live daemon: %v", err)
	}
	if detail.Meta == nil {
		t.Fatalf("workspace meta is nil")
	}

	hcSet := stringSetFromSlice(detail.Meta.HostCommands)
	if !hcSet["gh"] {
		t.Errorf("workspace host_commands missing 'gh' (kit materialization/host_commands sync did not run before the daemon push): %v", detail.Meta.HostCommands)
	}

	foundBinding := false
	for _, b := range detail.Meta.AdditionalBindings {
		if b.Source == "/var/data" {
			foundBinding = true
		}
	}
	if !foundBinding {
		t.Errorf("workspace additional_bindings missing /var/data: %v", detail.Meta.AdditionalBindings)
	}
}

// ---------------------------------------------------------------------------
// MAJOR 1 (codex review, 4th pass, docs/plans/workspace-db-consolidation.md):
// `boid start --auto-migrate` (cmd/start.go's runDaemonParent ->
// handleMigrationFailure -> runAutoMigrate) calls MigrateProject in-process
// specifically because the daemon it just tried to start FAILED to start —
// so the daemon push is never reachable at that call site. Before this fix,
// pushMigratedWorkspaceToDaemon's HTTP-only push always took the "could not
// reach the boid daemon" warning branch there and left the `workspaces` DB
// row completely untouched: auto-migrate looked like it succeeded but never
// actually applied anything a subsequent successful daemon start would read
// from. These tests exercise the offline fallback
// (applyMigratedWorkspaceOffline) through MigrateProject/invokeMigrate with
// no daemon reachable at all — invokeMigrate's default BOID_SOCKET already
// points at a guaranteed-empty path (see its own doc comment), matching the
// auto-migrate call site exactly.
// ---------------------------------------------------------------------------

// TestProjectMigrate_AutoMigrate_OfflineDaemonMode verifies that --apply,
// with no daemon reachable, writes the migrated workspace fields (env,
// host_commands, additional_bindings) directly into the `workspaces` DB row
// for a brand-new slug, instead of only reaching the (daemon-unread) shadow
// yaml file.
func TestProjectMigrate_AutoMigrate_OfflineDaemonMode(t *testing.T) {
	projectYAML := `id: proj-automigrate-offline
name: Auto Migrate Offline
env:
  FOO: bar
host_commands:
  gh:
    allow:
      - pr
additional_bindings:
  - source: /var/data
    mode: ro
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "automigrate-offline-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	boidDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer boidDB.Close()

	meta, err := orchestrator.NewWorkspaceRepository(boidDB.Conn).Load("automigrate-offline-ws")
	if err != nil {
		t.Fatalf("workspace row should have been created directly in the database: %v", err)
	}
	if meta.Env["FOO"] != "bar" {
		t.Errorf("workspace env = %+v, want FOO=bar", meta.Env)
	}
	foundBinding := false
	for _, b := range meta.AdditionalBindings {
		if b.Source == "/var/data" {
			foundBinding = true
		}
	}
	if !foundBinding {
		t.Errorf("workspace additional_bindings missing /var/data: %v", meta.AdditionalBindings)
	}
	hcSet := stringSetFromSlice(meta.HostCommands)
	if !hcSet["gh"] {
		t.Errorf("workspace host_commands missing 'gh': %v", meta.HostCommands)
	}
}

// TestProjectMigrate_OfflineDaemonMode_MergesExistingWorkspace is the offline
// counterpart of TestProjectMigrate_PreservesExistingWorkspaceFields: a
// workspace slug that already has a DB row (the common case — most projects
// migrate onto an already-existing `default` workspace) must have its
// existing content preserved, not overwritten, by the offline
// compare-and-swap merge (MAJOR 1).
func TestProjectMigrate_OfflineDaemonMode_MergesExistingWorkspace(t *testing.T) {
	projectYAML := `id: proj-automigrate-merge
name: Auto Migrate Merge
env:
  NEW: "yes"
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()

	seedDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := orchestrator.NewWorkspaceRepository(seedDB.Conn).Save("automigrate-merge-ws", &orchestrator.WorkspaceMeta{
		Env: map[string]string{"EXISTING": "yes"},
	}); err != nil {
		t.Fatalf("seed existing workspace: %v", err)
	}
	seedDB.Close()

	err = invokeMigrate(t, dir, migrateOpts{
		workspace:     "automigrate-merge-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	boidDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("re-open db: %v", err)
	}
	defer boidDB.Close()
	meta, err := orchestrator.NewWorkspaceRepository(boidDB.Conn).Load("automigrate-merge-ws")
	if err != nil {
		t.Fatalf("load workspace: %v", err)
	}
	if meta.Env["EXISTING"] != "yes" {
		t.Errorf("pre-existing env EXISTING lost: %+v", meta.Env)
	}
	if meta.Env["NEW"] != "yes" {
		t.Errorf("migrated env NEW not merged in: %+v", meta.Env)
	}
}

// ---------------------------------------------------------------------------
// MAJOR 2 (codex review, 4th pass, docs/plans/workspace-db-consolidation.md):
// when the legacy kit this migration generates declares a host_commands name
// that host_commands.yaml already defines, the outcome must depend on
// whether the two definitions actually match (post-normalization) — an
// identical definition dedupes (and still resyncs the daemon's live
// snapshot), a different one aborts the whole migration rather than
// silently keeping (or silently discarding) either side.
// ---------------------------------------------------------------------------

// TestProjectMigrate_HostCommandsSameNameSameDefinition_Dedupes verifies
// that a host_commands name already present in host_commands.yaml with the
// same (post-normalization) definition this migration's legacy kit would
// write is deduped: migrate succeeds and the file's existing entry is left
// as-is.
func TestProjectMigrate_HostCommandsSameNameSameDefinition_Dedupes(t *testing.T) {
	projectYAML := `id: proj-hc-dedupe
name: Host Cmd Dedupe
host_commands:
  gh:
    allow:
      - pr
      - issue
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	hcPath := filepath.Join(cfgDir, "boid", "host_commands.yaml")
	if err := orchestrator.WriteHostCommandsConfig(hcPath, map[string]orchestrator.HostCommandSpec{
		"gh": {Allow: []string{"pr", "issue"}},
	}); err != nil {
		t.Fatalf("seed host_commands.yaml: %v", err)
	}

	err := invokeMigrate(t, dir, migrateOpts{
		workspace:     "hc-dedupe-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}

	got, err := orchestrator.LoadHostCommandsConfig(hcPath)
	if err != nil {
		t.Fatalf("reload host_commands.yaml: %v", err)
	}
	spec, ok := got["gh"]
	if !ok {
		t.Fatalf("host_commands.yaml lost the 'gh' entry: %+v", got)
	}
	if len(spec.Allow) != 2 || spec.Allow[0] != "pr" || spec.Allow[1] != "issue" {
		t.Errorf("host_commands.yaml 'gh' entry changed unexpectedly: %+v", spec)
	}
}

// TestProjectMigrate_HostCommandsSameNameDifferentDefinition_FailsMigration
// verifies that a host_commands name already present in host_commands.yaml
// with a *different* definition than this migration's legacy kit would
// write aborts the whole migration (error returned, host_commands.yaml left
// untouched, project.yaml NOT rewritten) instead of silently keeping the
// existing definition and reporting success.
func TestProjectMigrate_HostCommandsSameNameDifferentDefinition_FailsMigration(t *testing.T) {
	projectYAML := `id: proj-hc-conflict
name: Host Cmd Conflict
host_commands:
  gh:
    allow:
      - pr
      - issue
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)
	cfgDir := t.TempDir()
	dataDir := t.TempDir()

	hcPath := filepath.Join(cfgDir, "boid", "host_commands.yaml")
	if err := orchestrator.WriteHostCommandsConfig(hcPath, map[string]orchestrator.HostCommandSpec{
		"gh": {Allow: []string{"pr"}}, // different from project.yaml's [pr, issue]
	}); err != nil {
		t.Fatalf("seed host_commands.yaml: %v", err)
	}
	before, err := os.ReadFile(hcPath)
	if err != nil {
		t.Fatalf("read seeded host_commands.yaml: %v", err)
	}

	err = invokeMigrate(t, dir, migrateOpts{
		workspace:     "hc-conflict-ws",
		apply:         true,
		dbPath:        dbFile,
		xdgConfigHome: cfgDir,
		xdgDataHome:   dataDir,
	})
	if err == nil {
		t.Fatalf("expected migrate apply to fail on a host_commands definition conflict")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("error should name the conflicting command 'gh': %v", err)
	}

	after, err := os.ReadFile(hcPath)
	if err != nil {
		t.Fatalf("read host_commands.yaml after failed migrate: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("host_commands.yaml was modified despite the conflict:\nbefore: %s\nafter:  %s", before, after)
	}

	// project.yaml must not have been rewritten either — the whole
	// migration aborts before "source rewrite" (MAJOR 2).
	updatedYAML := readProjectYAMLContent(t, dir)
	if !strings.Contains(updatedYAML, "host_commands:") {
		t.Errorf("project.yaml was rewritten despite the migration aborting:\n%s", updatedYAML)
	}

	// The workspace must not have been created in the DB either — nothing
	// downstream of the conflict runs.
	boidDB, err := db.Open(dbFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer boidDB.Close()
	if _, err := orchestrator.NewWorkspaceRepository(boidDB.Conn).Load("hc-conflict-ws"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace %q should not have been created in the database, load returned: %v", "hc-conflict-ws", err)
	}
}

// TestProjectMigrate_HostCommandsDedupeStillReloads verifies that even when
// every legacy-kit host_commands name already exists in host_commands.yaml
// with a matching definition (so the file itself is not rewritten), the
// daemon is still asked to reload — otherwise a live daemon whose
// in-memory snapshot predates a hand-edit to the file would 400 on
// "unknown host_commands reference" even though the file is objectively
// correct and unchanged (MAJOR 2).
func TestProjectMigrate_HostCommandsDedupeStillReloads(t *testing.T) {
	projectYAML := `id: proj-hc-reload
name: Host Cmd Reload
host_commands:
  gh:
    allow:
      - pr
      - issue
`
	dir := setupMigrateProject(t, projectYAML)
	dbFile := setupMigrateDBFile(t)

	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	kitsDir := filepath.Join(dataDir, "boid", "kits")
	hcPath := filepath.Join(cfgDir, "boid", "host_commands.yaml")

	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	// Seed host_commands.yaml WITHOUT 'gh' so the daemon's startup preflight
	// (internal/server/wire.go) loads a live snapshot that does not yet
	// know about it.
	if err := orchestrator.WriteHostCommandsConfig(hcPath, map[string]orchestrator.HostCommandSpec{
		"unrelated": {Allow: []string{"x"}},
	}); err != nil {
		t.Fatalf("seed host_commands.yaml: %v", err)
	}

	sockPath := filepath.Join(t.TempDir(), "boid.sock")
	t.Setenv("BOID_SOCKET", sockPath)

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
		KitsDir:    kitsDir,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// Hand-edit host_commands.yaml to add 'gh' with the SAME definition
	// project.yaml's legacy kit will produce, *after* the daemon already
	// started — so its live in-memory snapshot is stale relative to this
	// file. This is the exact "dedupe" precondition MAJOR 2 targets: the
	// name is already in the file, but the daemon doesn't know it yet.
	if err := orchestrator.WriteHostCommandsConfig(hcPath, map[string]orchestrator.HostCommandSpec{
		"unrelated": {Allow: []string{"x"}},
		"gh":        {Allow: []string{"pr", "issue"}},
	}); err != nil {
		t.Fatalf("hand-edit host_commands.yaml: %v", err)
	}

	c := client.NewUnixClient(sockPath)

	var out bytes.Buffer
	err = MigrateProject(MigrateProjectOptions{
		Dir:       dir,
		Workspace: "hc-reload-ws",
		Apply:     true,
		DBPath:    dbFile,
		Out:       &out,
	})
	if err != nil {
		t.Fatalf("migrate apply: unexpected error: %v", err)
	}
	if strings.Contains(out.String(), "warning:") {
		t.Errorf("expected no warning, got: %s", out.String())
	}

	var detail api.WorkspaceDetail
	if err := c.Do("GET", "/api/workspaces/hc-reload-ws", nil, &detail); err != nil {
		t.Fatalf("workspace should exist in the live daemon: %v", err)
	}
	if detail.Meta == nil {
		t.Fatalf("workspace meta is nil")
	}
	hcSet := stringSetFromSlice(detail.Meta.HostCommands)
	if !hcSet["gh"] {
		t.Errorf("workspace host_commands missing 'gh' (dedupe path did not reload the daemon's live snapshot): %v", detail.Meta.HostCommands)
	}
}

// ---------------------------------------------------------------------------
// MAJOR 3 (codex review, 4th pass, docs/plans/workspace-db-consolidation.md):
// when the PUT that merges a migration into an already-existing live
// workspace fails, the shadow file the failure message points the user at
// must be the full merge (existing content + migrated fields), not just the
// migration's own delta — `boid workspace edit --from-file` is a
// full-content replace, so a delta-only file would silently drop every
// field the migration didn't itself touch.
// ---------------------------------------------------------------------------

// TestProjectMigrate_PutFailure_ShadowFileIsMergedComplete verifies that
// mergeMigratedWorkspaceIntoDaemon refreshes the on-disk shadow file with
// the full merge (existing content + migrated fields) before/regardless of
// the PUT's own outcome, so a PUT failure still leaves a shadow file that is
// safe to apply via `boid workspace edit --from-file`.
func TestProjectMigrate_PutFailure_ShadowFileIsMergedComplete(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	const slug = "put-fail-ws"

	sockPath := filepath.Join(t.TempDir(), "always-500.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fakeSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})}
	go func() { _ = fakeSrv.Serve(ln) }()
	t.Cleanup(func() { _ = fakeSrv.Close() })

	c := client.NewUnixClient(sockPath)

	plan := &migratePlan{
		workspaceSlug: slug,
		meta: &orchestrator.LegacyProjectMeta{
			Env: map[string]string{"NEW": "yes"},
		},
	}
	first := api.WorkspaceDetail{
		Meta: &orchestrator.WorkspaceMeta{
			Env: map[string]string{"EXISTING": "yes"},
		},
		Revision: "2026-01-01T00:00:00.000000000Z",
	}

	shadowPath, err := workspaceShadowPath(slug)
	if err != nil {
		t.Fatalf("workspaceShadowPath: %v", err)
	}

	var out bytes.Buffer
	mergeMigratedWorkspaceIntoDaemon(c, plan, first, shadowPath, &out)

	if !strings.Contains(out.String(), "warning:") {
		t.Fatalf("expected a warning when the PUT fails, got: %s", out.String())
	}

	shadow, err := orchestrator.NewWorkspaceStore("").Load(slug)
	if err != nil {
		t.Fatalf("load shadow file: %v", err)
	}
	if shadow.Env["EXISTING"] != "yes" {
		t.Errorf("shadow file lost the pre-existing field on PUT failure: %+v", shadow.Env)
	}
	if shadow.Env["NEW"] != "yes" {
		t.Errorf("shadow file missing the migrated field on PUT failure: %+v", shadow.Env)
	}
}
