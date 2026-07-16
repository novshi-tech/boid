package orchestrator

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

// migrationTestEnv bundles a fresh migrated in-memory DB, a temp workspace
// yaml dir, and a temp kits dir for MigrateWorkspaceYAMLToDB tests.
type migrationTestEnv struct {
	conn         *db.DB
	projectRepo  *ProjectRepository
	workspaceDir string
	kitsDir      string
}

func newMigrationTestEnv(t *testing.T) *migrationTestEnv {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return &migrationTestEnv{
		conn:         d,
		projectRepo:  NewProjectRepository(d.Conn),
		workspaceDir: t.TempDir(),
		kitsDir:      t.TempDir(),
	}
}

func writeMigrationWorkspaceYAML(t *testing.T, dir, slug, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir workspace dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write workspace yaml %q: %v", slug, err)
	}
}

func writeMigrationKitYAML(t *testing.T, kitsDir, name, content string) {
	t.Helper()
	dir := filepath.Join(kitsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir kit dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kit.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write kit.yaml: %v", err)
	}
}

func schemaMigrationRow(t *testing.T, conn *db.DB, version string) (state, inputHash string, found bool) {
	t.Helper()
	err := conn.Conn.QueryRow(
		`SELECT state, input_hash FROM schema_migrations WHERE version = ?`, version,
	).Scan(&state, &inputHash)
	if err != nil {
		return "", "", false
	}
	return state, inputHash, true
}

// findBindMountBySource returns the first BindMount in mounts whose Source
// matches source, mirroring project_store_hydrate_test.go's
// findBindingBySource (that helper lives in the external orchestrator_test
// package and is not reachable from here).
func findBindMountBySource(mounts []BindMount, source string) (BindMount, bool) {
	for _, m := range mounts {
		if m.Source == source {
			return m, true
		}
	}
	return BindMount{}, false
}

func workspaceSlugs(t *testing.T, conn *db.DB) []string {
	t.Helper()
	rows, err := conn.Conn.Query(`SELECT slug FROM workspaces ORDER BY slug`)
	if err != nil {
		t.Fatalf("query workspaces: %v", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		slugs = append(slugs, s)
	}
	return slugs
}

// TestMigrateWorkspaceYAMLToDB_HappyPath covers the normal case: two
// workspace yaml files (one referencing a kit) plus one kit yaml migrate
// into the workspaces table, the aggregated host_commands.yaml is written,
// and schema_migrations records workspace_db_consolidation as committed.
func TestMigrateWorkspaceYAMLToDB_HappyPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr, issue]\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - gh-kit\nenv:\n  FOO: bar\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-b", "allowed_domains:\n  - example.com\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	state, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found {
		t.Fatal("expected schema_migrations row for workspace_db_consolidation")
	}
	if state != "committed" {
		t.Fatalf("state = %q, want committed", state)
	}

	slugs := workspaceSlugs(t, env.conn)
	sort.Strings(slugs)
	want := []string{DefaultWorkspaceSlug, "team-a", "team-b"}
	if len(slugs) != len(want) {
		t.Fatalf("workspace slugs = %v, want %v", slugs, want)
	}
	for i, s := range want {
		if slugs[i] != s {
			t.Errorf("workspace slugs = %v, want %v", slugs, want)
			break
		}
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamA, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a): %v", err)
	}
	if !equalStringSlice(teamA.HostCommands, []string{"gh"}) {
		t.Errorf("team-a.HostCommands = %v, want [gh] (unioned from kit gh-kit)", teamA.HostCommands)
	}
	if teamA.Env["FOO"] != "bar" {
		t.Errorf("team-a.Env[FOO] = %q, want bar", teamA.Env["FOO"])
	}

	teamB, err := repo.Load("team-b")
	if err != nil {
		t.Fatalf("Load(team-b): %v", err)
	}
	if !equalStringSlice(teamB.AllowedDomains, []string{"example.com"}) {
		t.Errorf("team-b.AllowedDomains = %v, want [example.com]", teamB.AllowedDomains)
	}

	hostCommandsPath, err := DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	written, err := LoadHostCommandsConfig(hostCommandsPath)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if _, ok := written["gh"]; !ok {
		t.Errorf("expected 'gh' host_command in aggregated config, got %v", written)
	}
}

// TestMigrateWorkspaceYAMLToDB_PreflightFailsOnBrokenYAML pins the
// "corrupt yaml -> abort, zero DB changes" e2e precondition
// (docs/plans/workspace-db-consolidation.md e2e 前提).
func TestMigrateWorkspaceYAMLToDB_PreflightFailsOnBrokenYAML(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "broken", "kits: [unclosed bracket\n")

	err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo)
	if err == nil {
		t.Fatal("expected error for broken workspace yaml")
	}

	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Errorf("expected zero workspace rows after preflight failure, got %v", slugs)
	}
	if _, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion); found {
		t.Error("expected no schema_migrations row after preflight failure")
	}
}

// TestMigrateWorkspaceYAMLToDB_PreflightFailsOnHostCommandCollision pins
// decision 9: two kits with the same host_command name but different
// definitions abort the migration, and no staging row is left behind.
func TestMigrateWorkspaceYAMLToDB_PreflightFailsOnHostCommandCollision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "kit-a", "host_commands:\n  gh:\n    allow: [pr]\n")
	writeMigrationKitYAML(t, env.kitsDir, "kit-b", "host_commands:\n  gh:\n    allow: [issue]\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - kit-a\n  - kit-b\n")

	err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo)
	if err == nil {
		t.Fatal("expected error for conflicting host_command definitions")
	}
	if !strings.Contains(err.Error(), "gh") {
		t.Errorf("expected error to mention the conflicting command name: %v", err)
	}

	if _, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion); found {
		t.Error("expected no schema_migrations row (not even staging) after collision preflight failure")
	}
	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Errorf("expected zero workspace rows after collision preflight failure, got %v", slugs)
	}
}

// TestPreflightWorkspaceMigration_FailsOnUnresolvedKit pins MAJOR 2 (codex
// review, 3rd pass): a workspace referencing a kit with no corresponding
// kit.yaml under kitsDir must abort the (one-time) cutover rather than
// warn-and-skip. Before this fix, materializeKitRuntimeIntoWorkspace logged
// a warning and silently dropped the unresolved kit's runtime — since this
// migration only ever commits once, a merely temporary kit-directory
// absence (e.g. a kit volume not yet mounted at the moment the daemon
// happens to start) would have permanently stranded that workspace's
// dispatch-time env/host_commands/bindings with no way to recover short of
// manually editing the workspaces table. Failing preflight instead leaves
// zero DB changes, so the operator can restore the kit directory (or edit
// the workspace's kits list) and simply restart the daemon.
func TestPreflightWorkspaceMigration_FailsOnUnresolvedKit(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-ghost", "kits:\n  - ghost-kit\n")

	err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo)
	if err == nil {
		t.Fatal("expected error for workspace referencing an unresolved kit")
	}
	if !strings.Contains(err.Error(), "team-ghost") {
		t.Errorf("expected error to mention the workspace slug %q: %v", "team-ghost", err)
	}
	if !strings.Contains(err.Error(), "ghost-kit") {
		t.Errorf("expected error to mention the unresolved kit name %q: %v", "ghost-kit", err)
	}
	wantKitDir := filepath.Join(env.kitsDir, "ghost-kit")
	if !strings.Contains(err.Error(), wantKitDir) {
		t.Errorf("expected error to mention the expected kit dir path %q: %v", wantKitDir, err)
	}

	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Errorf("expected zero workspace rows after unresolved-kit preflight failure, got %v", slugs)
	}
	if _, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion); found {
		t.Error("expected no schema_migrations row (not even staging) after unresolved-kit preflight failure")
	}
}

// TestMigrateWorkspaceYAMLToDB_CrashRecoveryMatchingHashRollsForward
// simulates a daemon crash between the staging upsert (committed) and the
// final commit: a pre-existing state=staging row whose input_hash matches
// the freshly recomputed preflight hash must be rolled forward to
// state=committed, and the workspace data must land in the DB exactly as a
// fresh run would produce.
func TestMigrateWorkspaceYAMLToDB_CrashRecoveryMatchingHashRollsForward(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "env:\n  FOO: bar\n")

	pre, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration: %v", err)
	}

	tx, err := env.conn.Conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := upsertMigrationRow(tx, workspaceDBConsolidationVersion, "staging", pre.inputHash); err != nil {
		t.Fatalf("upsertMigrationRow(staging): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Sanity: nothing else was written yet (matches the crashed-mid-flight
	// state this test simulates).
	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Fatalf("test setup: expected zero workspace rows before roll-forward, got %v", slugs)
	}

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB (roll-forward): %v", err)
	}

	state, inputHash, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found || state != "committed" {
		t.Fatalf("state after roll-forward = %q (found=%v), want committed", state, found)
	}
	if inputHash != pre.inputHash {
		t.Errorf("input_hash after roll-forward = %q, want %q (unchanged)", inputHash, pre.inputHash)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamA, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a) after roll-forward: %v", err)
	}
	if teamA.Env["FOO"] != "bar" {
		t.Errorf("team-a.Env[FOO] after roll-forward = %q, want bar", teamA.Env["FOO"])
	}
}

// TestMigrateWorkspaceYAMLToDB_CrashRecoveryMismatchedHashAborts covers the
// other crash-recovery branch: a state=staging row whose recorded
// input_hash no longer matches the freshly recomputed preflight hash (the
// on-disk inputs changed between the crash and this restart) must abort
// with an error rather than silently rolling forward against different
// data, and must leave the staging row untouched.
func TestMigrateWorkspaceYAMLToDB_CrashRecoveryMismatchedHashAborts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "env:\n  FOO: bar\n")

	tx, err := env.conn.Conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := upsertMigrationRow(tx, workspaceDBConsolidationVersion, "staging", "stale-bogus-hash"); err != nil {
		t.Fatalf("upsertMigrationRow(staging): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	err = MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo)
	if err == nil {
		t.Fatal("expected error for mismatched input_hash during crash recovery")
	}

	state, inputHash, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found {
		t.Fatal("expected the staging row to still exist after abort")
	}
	if state != "staging" || inputHash != "stale-bogus-hash" {
		t.Errorf("staging row was mutated on abort: state=%q input_hash=%q, want staging/stale-bogus-hash", state, inputHash)
	}
	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Errorf("expected zero workspace rows after mismatched-hash abort, got %v", slugs)
	}
}

// TestMigrateWorkspaceYAMLToDB_EmptyEnvironmentCreatesOnlyDefault covers the
// fresh-install e2e precondition: no workspace yaml, no kits, no projects ->
// only the default workspace is created.
func TestMigrateWorkspaceYAMLToDB_EmptyEnvironmentCreatesOnlyDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	// Neither workspaceDir nor kitsDir exist on disk at all.
	missingWorkspaceDir := filepath.Join(env.workspaceDir, "does-not-exist")
	missingKitsDir := filepath.Join(env.kitsDir, "does-not-exist")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, missingWorkspaceDir, missingKitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	slugs := workspaceSlugs(t, env.conn)
	if len(slugs) != 1 || slugs[0] != DefaultWorkspaceSlug {
		t.Fatalf("workspace slugs = %v, want only [%s]", slugs, DefaultWorkspaceSlug)
	}

	state, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found || state != "committed" {
		t.Fatalf("state = %q (found=%v), want committed", state, found)
	}
}

// TestMigrateWorkspaceYAMLToDB_Idempotent verifies that calling the
// migration a second time (after it has already committed) is a safe no-op:
// no error, and the workspace data is unchanged.
func TestMigrateWorkspaceYAMLToDB_Idempotent(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "env:\n  FOO: bar\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("first MigrateWorkspaceYAMLToDB: %v", err)
	}
	firstSlugs := workspaceSlugs(t, env.conn)

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("second MigrateWorkspaceYAMLToDB: %v", err)
	}
	secondSlugs := workspaceSlugs(t, env.conn)

	if !equalStringSlice(firstSlugs, secondSlugs) {
		t.Errorf("workspace slugs changed across idempotent re-run: %v -> %v", firstSlugs, secondSlugs)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamA, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a): %v", err)
	}
	if teamA.Env["FOO"] != "bar" {
		t.Errorf("team-a.Env[FOO] after idempotent re-run = %q, want bar", teamA.Env["FOO"])
	}
}

// TestMigrateWorkspaceYAMLToDB_BrokenProjectReferenceAborts pins the
// project -> workspace reference-resolution precondition: a project
// assigned to a workspace slug with no corresponding yaml file (and not
// "default") must abort the migration.
func TestMigrateWorkspaceYAMLToDB_BrokenProjectReferenceAborts(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	if err := CreateProject(env.conn.Conn, &Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(env.conn.Conn, "proj-1", "ghost-workspace"); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo)
	if err == nil {
		t.Fatal("expected error for broken project -> workspace reference")
	}
	if !strings.Contains(err.Error(), "ghost-workspace") {
		t.Errorf("expected error to mention the broken workspace reference: %v", err)
	}

	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Errorf("expected zero workspace rows after broken-reference abort, got %v", slugs)
	}
	if _, _, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion); found {
		t.Error("expected no schema_migrations row after broken-reference abort")
	}
}

// TestMigrateWorkspaceYAMLToDB_MaterializesKitEnvAndBindings pins BLOCKER 1
// (codex review): a workspace's Kits contribute more than just
// host_commands *names* — a kit's Env and AdditionalBindings must also be
// materialized into the DB-bound WorkspaceMeta, since the workspaces table
// has no column for Kits at all (WorkspaceRepository never persists it).
// Before this fix, only host_command names survived the cutover; a kit's
// env var or bind mount silently vanished the moment the DB became
// authoritative. Kits must also end up cleared (nil) on the persisted row,
// since it is now fully folded into the row's own fields.
func TestMigrateWorkspaceYAMLToDB_MaterializesKitEnvAndBindings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "toolkit", ""+
		"host_commands:\n  gh:\n    allow: [pr]\n"+
		"env:\n  KIT_VAR: from-kit\n"+
		"additional_bindings:\n  - source: /opt/kit-tool\n    target: /opt/kit-tool\n    mode: ro\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-x", "kits:\n  - toolkit\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamX, err := repo.Load("team-x")
	if err != nil {
		t.Fatalf("Load(team-x): %v", err)
	}
	if !equalStringSlice(teamX.HostCommands, []string{"gh"}) {
		t.Errorf("team-x.HostCommands = %v, want [gh]", teamX.HostCommands)
	}
	if teamX.Env["KIT_VAR"] != "from-kit" {
		t.Errorf("team-x.Env[KIT_VAR] = %q, want from-kit", teamX.Env["KIT_VAR"])
	}
	// Two entries expected: the kit's own explicit additional_bindings entry
	// (/opt/kit-tool) plus the kit root directory itself (BLOCKER 1 2nd-pass
	// fix — see TestMigrateWorkspaceYAMLToDB_MaterializesKitRootsAsAdditionalBindings
	// below for the dedicated coverage of the latter).
	if len(teamX.AdditionalBindings) != 2 {
		t.Fatalf("team-x.AdditionalBindings = %+v, want 2 entries (/opt/kit-tool + kit root)", teamX.AdditionalBindings)
	}
	if _, ok := findBindMountBySource(teamX.AdditionalBindings, "/opt/kit-tool"); !ok {
		t.Errorf("team-x.AdditionalBindings = %+v, want an entry for /opt/kit-tool", teamX.AdditionalBindings)
	}
	if _, ok := findBindMountBySource(teamX.AdditionalBindings, filepath.Join(env.kitsDir, "toolkit")); !ok {
		t.Errorf("team-x.AdditionalBindings = %+v, want an entry for the kit root dir", teamX.AdditionalBindings)
	}
	if len(teamX.Kits) != 0 {
		t.Errorf("team-x.Kits = %v, want empty (materialized then cleared)", teamX.Kits)
	}
}

// TestMigrateWorkspaceYAMLToDB_KitEnvDoesNotOverrideWorkspaceEnv pins
// BLOCKER 1's precedence rule: a workspace's own env (workspace-authored)
// wins over a same-named kit env var (kit-supplied default), while a
// non-conflicting kit env var still comes through.
func TestMigrateWorkspaceYAMLToDB_KitEnvDoesNotOverrideWorkspaceEnv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "toolkit2", "env:\n  SHARED_VAR: from-kit\n  KIT_ONLY: kit-value\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-y", "kits:\n  - toolkit2\nenv:\n  SHARED_VAR: from-workspace\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamY, err := repo.Load("team-y")
	if err != nil {
		t.Fatalf("Load(team-y): %v", err)
	}
	if teamY.Env["SHARED_VAR"] != "from-workspace" {
		t.Errorf("team-y.Env[SHARED_VAR] = %q, want from-workspace (workspace must win over kit)", teamY.Env["SHARED_VAR"])
	}
	if teamY.Env["KIT_ONLY"] != "kit-value" {
		t.Errorf("team-y.Env[KIT_ONLY] = %q, want kit-value (non-conflicting kit env still materialized)", teamY.Env["KIT_ONLY"])
	}
}

// TestPreflightWorkspaceMigration_UsesSingleKitYAMLSnapshot pins MAJOR 1
// (codex review): preflightWorkspaceMigration must read every kit.yaml
// exactly once and derive both the aggregated host_commands config and
// every workspace's kit-name union from that single snapshot, rather than
// each re-scanning kitsDir independently (the pre-fix TOCTOU: a kit.yaml
// edit racing between the two separate reads could make the aggregate and
// the union silently disagree). This is verified directly at the snapshot
// level: a kit.yaml is mutated *after* a snapshot is taken, and both
// aggregateHostCommandsFromSnapshot and materializeKitRuntimeIntoWorkspace
// must still reflect the pre-mutation content when fed that same snapshot
// — proving neither one re-reads the file from disk.
func TestPreflightWorkspaceMigration_UsesSingleKitYAMLSnapshot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "kit-a", "host_commands:\n  gh:\n    allow: [pr]\n")

	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs: %v", err)
	}

	// Simulate a kit.yaml edit racing the preflight after the snapshot was
	// taken.
	writeMigrationKitYAML(t, kitsDir, "kit-a", "host_commands:\n  gh:\n    allow: [pr, issue, admin]\n")

	aggregated, err := aggregateHostCommandsFromSnapshot(snap)
	if err != nil {
		t.Fatalf("aggregateHostCommandsFromSnapshot: %v", err)
	}
	if !equalStringSlice(aggregated["gh"].Allow, []string{"pr"}) {
		t.Errorf("aggregate must reflect the snapshot taken before the race, got %v", aggregated["gh"].Allow)
	}

	meta := &WorkspaceMeta{Kits: []string{"kit-a"}}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}
	if !equalStringSlice(meta.HostCommands, []string{"gh"}) {
		t.Errorf("union must reflect the snapshot taken before the race, got %v", meta.HostCommands)
	}

	// Consistency check: a *fresh* snapshot taken now (post-mutation) must
	// disagree with the stale one above — otherwise this test's setup would
	// not actually be exercising a race at all.
	freshSnap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs (fresh): %v", err)
	}
	freshAggregated, err := aggregateHostCommandsFromSnapshot(freshSnap)
	if err != nil {
		t.Fatalf("aggregateHostCommandsFromSnapshot (fresh): %v", err)
	}
	if equalStringSlice(freshAggregated["gh"].Allow, aggregated["gh"].Allow) {
		t.Fatalf("test setup invalid: a fresh snapshot should reflect the mutated file, got the same result as the stale snapshot: %v", freshAggregated["gh"].Allow)
	}
}

// TestMigrateWorkspaceYAMLToDB_ProjectReferencingDefaultIsFine is the
// regression guard alongside the broken-reference test above: a project
// already assigned to "default" (the common case — every project not
// explicitly assigned lands there per AssignDefaultWorkspaceToUnlinked) must
// not be treated as a broken reference even though no default.yaml file
// exists on disk.
func TestMigrateWorkspaceYAMLToDB_ProjectReferencingDefaultIsFine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	if err := CreateProject(env.conn.Conn, &Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := SetProjectWorkspace(env.conn.Conn, "proj-1", DefaultWorkspaceSlug); err != nil {
		t.Fatalf("SetProjectWorkspace: %v", err)
	}

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	slugs := workspaceSlugs(t, env.conn)
	if len(slugs) != 1 || slugs[0] != DefaultWorkspaceSlug {
		t.Fatalf("workspace slugs = %v, want only [%s]", slugs, DefaultWorkspaceSlug)
	}
}

// --- BLOCKER (codex review, 2nd pass): KitRoots materialization ---

// TestMigrateWorkspaceYAMLToDB_MaterializesKitRootsAsAdditionalBindings pins
// the 2nd-pass BLOCKER: MergeKitMetaIntoBehavior (spec_loader.go) normally
// bind-mounts every kit's own directory read-only into the sandbox
// (behavior.KitRoots), so shell hooks can read scripts/assets living next to
// kit.yaml. Once a workspace's Kits list is folded into DB-bound fields and
// cleared (meta.Kits = nil, see preflightWorkspaceMigration), that
// mechanism can never fire again for a DB-backed workspace — so the kit
// root directory itself must be materialized as a (read-only)
// AdditionalBindings entry instead, the same way BLOCKER 1 already
// materializes a kit's Env and its own explicit additional_bindings.
//
// Updated for MAJOR 3 (codex review, 3rd pass): the kit root binding is now
// materialized with an explicit Target (equal to Source) and Mode="rw"
// rather than an implicit-empty-Target read-only mount — see
// materializeKitRuntimeIntoWorkspace's doc comment on kitRootBindings for
// why (the earlier read-only, empty-Target form could be silently dropped
// when Source-keyed union/merge collided with an unrelated same-Source
// binding; explicit Target + rw is what lets it bypass
// additionalBindingMounts' self-mount skip guard once it is appended as its
// own dedicated entry).
func TestMigrateWorkspaceYAMLToDB_MaterializesKitRootsAsAdditionalBindings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "shellkit", "host_commands:\n  mytool:\n    path: /usr/bin/true\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-root", "kits:\n  - shellkit\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamRoot, err := repo.Load("team-root")
	if err != nil {
		t.Fatalf("Load(team-root): %v", err)
	}

	wantKitDir := filepath.Join(env.kitsDir, "shellkit")
	binding, ok := findBindMountBySource(teamRoot.AdditionalBindings, wantKitDir)
	if !ok {
		t.Fatalf("expected kit root %q to be materialized as an AdditionalBindings entry, got %+v", wantKitDir, teamRoot.AdditionalBindings)
	}
	// Target must stay empty: dispatcher/sandbox_builder.go's self-mount
	// skip guard is `explicitTarget && Source==Target && Mode != "rw"`,
	// and `explicitTarget = bm.Target != ""`. Leaving Target empty makes
	// explicitTarget false, so the guard is skipped entirely and Mode
	// stays read-only (the KitRoots contract: kit scripts/assets are
	// never writable from inside the sandbox). Dedupe safety is
	// preserved because kitRootBindings is appended unconditionally,
	// bypassing every Source-keyed merge path (codex 4th pass).
	if binding.Target != "" {
		t.Errorf("kit root binding Target = %q, want empty (implicit Source==Target, keeps read-only via skipped self-mount guard)", binding.Target)
	}
	if binding.Mode != "" {
		t.Errorf("kit root binding Mode = %q, want empty (default read-only; KitRoots must not be writable from inside the sandbox)", binding.Mode)
	}
}

// TestMigrateWorkspaceYAMLToDB_MultipleKitsAllRootsMaterialized extends the
// above to a workspace with two kits: both kit root directories must be
// materialized, not just the first/last one processed.
func TestMigrateWorkspaceYAMLToDB_MultipleKitsAllRootsMaterialized(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "kit-one", "host_commands:\n  tool-one:\n    path: /usr/bin/true\n")
	writeMigrationKitYAML(t, env.kitsDir, "kit-two", "host_commands:\n  tool-two:\n    path: /usr/bin/true\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-multi", "kits:\n  - kit-one\n  - kit-two\n")

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamMulti, err := repo.Load("team-multi")
	if err != nil {
		t.Fatalf("Load(team-multi): %v", err)
	}

	for _, kitName := range []string{"kit-one", "kit-two"} {
		wantDir := filepath.Join(env.kitsDir, kitName)
		if _, ok := findBindMountBySource(teamMulti.AdditionalBindings, wantDir); !ok {
			t.Errorf("expected kit root %q to be materialized, got %+v", wantDir, teamMulti.AdditionalBindings)
		}
	}
}

// --- MAJOR 1 (codex review, 2nd pass): bind mount merge precedence ---

// TestMaterializeKitRuntime_WorkspaceBindingsWinRO pins the MAJOR 1 fix: when
// a workspace explicitly declares an additional_bindings entry as read-only
// for the same Source a kit also binds (e.g. rw, for its own tooling), the
// workspace-authored ro must win — not get silently promoted to rw by
// unionBindMountSlices' mode-promotion behavior, which inverts the intended
// "workspace-authored beats kit-supplied default" precedence.
func TestMaterializeKitRuntime_WorkspaceBindingsWinRO(t *testing.T) {
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "kit-a", "additional_bindings:\n  - source: /opt/tool\n    mode: rw\n")

	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs: %v", err)
	}

	meta := &WorkspaceMeta{
		Kits: []string{"kit-a"},
		AdditionalBindings: []BindMount{
			{Source: "/opt/tool", Mode: ""}, // workspace-authored: explicit ro
		},
	}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}

	got, ok := findBindMountBySource(meta.AdditionalBindings, "/opt/tool")
	if !ok {
		t.Fatalf("expected /opt/tool binding, got %+v", meta.AdditionalBindings)
	}
	if got.Mode == "rw" {
		t.Errorf("workspace-authored ro binding must win over kit's rw for the same source, got mode=%q", got.Mode)
	}
}

// TestMaterializeKitRuntime_WorkspaceEnvWinsOnConflict is a regression guard
// (念のため) alongside the binding precedence test above, pinned directly at
// the materializeKitRuntimeIntoWorkspace level (TestMigrateWorkspaceYAMLToDB_
// KitEnvDoesNotOverrideWorkspaceEnv already covers this through the full
// MigrateWorkspaceYAMLToDB round trip).
func TestMaterializeKitRuntime_WorkspaceEnvWinsOnConflict(t *testing.T) {
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "kit-a", "env:\n  SHARED: from-kit\n")

	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs: %v", err)
	}

	meta := &WorkspaceMeta{
		Kits: []string{"kit-a"},
		Env:  map[string]string{"SHARED": "from-workspace"},
	}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}
	if meta.Env["SHARED"] != "from-workspace" {
		t.Errorf("meta.Env[SHARED] = %q, want from-workspace (workspace must win over kit)", meta.Env["SHARED"])
	}
}

// --- MAJOR 3 (codex review, 3rd pass): kit root binding must survive
// --- Source-keyed union/merge with an unrelated same-Source binding ---

// TestMaterializeKitRoot_SurvivesUnionWithOtherBindingWithSameSource pins the
// MAJOR 3 fix: mergeBindMounts/unionBindMountSlices key solely on Source and,
// on a match, either swallow the new entry outright (unionBindMountSlices)
// or wholesale-replace it (mergeBindMounts) — either way silently dropping
// the kit-root "expose this kit's directory to shell hooks" mount if any
// other binding (here: the workspace's own additional_bindings, declared for
// an unrelated purpose) happens to share the same Source. The old,
// independent KitRoots mechanism (behavior.KitRoots) never had this failure
// mode since it was never folded into AdditionalBindings at all; this test
// pins that the kit-root binding must keep surviving as its own entry now
// that it has been folded into the same field.
func TestMaterializeKitRoot_SurvivesUnionWithOtherBindingWithSameSource(t *testing.T) {
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "kit-a", "host_commands:\n  tool:\n    path: /usr/bin/true\n")
	kitDir := filepath.Join(kitsDir, "kit-a")

	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs: %v", err)
	}

	// Workspace-authored binding that happens to share the kit's own
	// directory as Source, but for an unrelated purpose/target.
	meta := &WorkspaceMeta{
		Kits: []string{"kit-a"},
		AdditionalBindings: []BindMount{
			{Source: kitDir, Target: "/opt/unrelated-target", Mode: "rw"},
		},
	}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}

	// The workspace-authored, unrelated-purpose binding must still be
	// present...
	if _, ok := findBindMountBySourceAndTarget(meta.AdditionalBindings, kitDir, "/opt/unrelated-target"); !ok {
		t.Errorf("workspace-authored binding (source=%s target=/opt/unrelated-target) missing after materialize, got %+v", kitDir, meta.AdditionalBindings)
	}
	// ...and the kit-root self-mount must ALSO still be present as its own
	// entry (Source==kitDir, Target=="" → implicit Source at dispatch,
	// read-only by default — codex 4th pass), not silently dropped by the
	// Source-keyed union/merge. Dedupe safety comes from appending
	// kitRootBindings unconditionally rather than from an explicit Target.
	if _, ok := findBindMountBySourceAndTarget(meta.AdditionalBindings, kitDir, ""); !ok {
		t.Fatalf("kit root binding (source=%s target=<empty>) missing after union with an unrelated same-Source binding, got %+v", kitDir, meta.AdditionalBindings)
	}
}

// TestMaterializeKitRoot_UsesImplicitTargetForReadOnly pins the other half
// of the MAJOR 3 fix's chosen approach after the codex 4th-pass correction:
// the kit-root binding must materialize with an implicit empty Target (not
// an explicit Source-equal Target) and empty Mode (default read-only), so
// dispatcher/sandbox_builder.go's self-mount skip guard
// (`explicitTarget && Source==Target && Mode != "rw"`) does not fire —
// leaving the mount in place *and* keeping the KitRoots contract that kit
// scripts/assets are not writable from inside the sandbox. Dedupe safety
// is provided by kitRootBindings being appended unconditionally (bypassing
// every Source-keyed merge path), not by an explicit Target.
func TestMaterializeKitRoot_UsesImplicitTargetForReadOnly(t *testing.T) {
	kitsDir := t.TempDir()
	writeMigrationKitYAML(t, kitsDir, "kit-a", "host_commands:\n  tool:\n    path: /usr/bin/true\n")
	kitDir := filepath.Join(kitsDir, "kit-a")

	snap, err := snapshotAllKitYAMLs(kitsDir)
	if err != nil {
		t.Fatalf("snapshotAllKitYAMLs: %v", err)
	}

	meta := &WorkspaceMeta{Kits: []string{"kit-a"}}
	if err := materializeKitRuntimeIntoWorkspace(snap, meta.Kits, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}

	got, ok := findBindMountBySource(meta.AdditionalBindings, kitDir)
	if !ok {
		t.Fatalf("expected kit root binding for %q, got %+v", kitDir, meta.AdditionalBindings)
	}
	// Target empty + Mode empty: implicit Source==Target via
	// additionalBindingMounts' default, read-only by contract. See the
	// analogous assertion in TestMigrateWorkspaceYAMLToDB_MaterializesKitRootsAsAdditionalBindings
	// for the full rationale (codex 4th pass).
	if got.Target != "" {
		t.Errorf("kit root binding Target = %q, want empty (implicit Source==Target)", got.Target)
	}
	if got.Mode != "" {
		t.Errorf("kit root binding Mode = %q, want empty (default read-only)", got.Mode)
	}
}

// findBindMountBySourceAndTarget is findBindMountBySource extended to also
// match on Target, needed when a test deliberately has two entries sharing
// the same Source (as TestMaterializeKitRoot_SurvivesUnionWithOtherBindingWithSameSource
// does) and must distinguish between them.
func findBindMountBySourceAndTarget(mounts []BindMount, source, target string) (BindMount, bool) {
	for _, m := range mounts {
		if m.Source == source && m.Target == target {
			return m, true
		}
	}
	return BindMount{}, false
}

// --- MAJOR 2 (codex review, 2nd pass): input_hash must cover kit runtime ---

// TestPreflightHash_ChangesWhenKitEnvChanges pins the MAJOR 2 fix: the
// preflight input_hash must change when a kit's env section changes, even
// though no workspace yaml, host_commands aggregate composition, or project
// reference changed. Before this fix, the hash input only covered the
// aggregated host_commands map (names + specs) plus the raw workspace metas
// (which do not carry kit env/bindings — those are only materialized after
// the hash is computed), so a kit yaml env/binding-only edit between a
// staged and a resumed migration attempt would go undetected and silently
// roll forward with the changed values.
func TestPreflightHash_ChangesWhenKitEnvChanges(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "kit-a", "env:\n  FOO: v1\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - kit-a\n")

	pre1, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration (before): %v", err)
	}

	// Change only the kit's env value; the workspace yaml, the aggregated
	// host_commands (kit-a defines none), and project references are all
	// untouched.
	writeMigrationKitYAML(t, env.kitsDir, "kit-a", "env:\n  FOO: v2\n")

	pre2, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration (after): %v", err)
	}

	if pre1.inputHash == pre2.inputHash {
		t.Errorf("input_hash unchanged after kit env value changed: %q", pre1.inputHash)
	}
}

// TestPreflightHash_ChangesWhenKitBindingsChange is the same case for a
// kit's additional_bindings section.
func TestPreflightHash_ChangesWhenKitBindingsChange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "kit-a", "additional_bindings:\n  - source: /opt/tool\n    mode: ro\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - kit-a\n")

	pre1, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration (before): %v", err)
	}

	writeMigrationKitYAML(t, env.kitsDir, "kit-a", "additional_bindings:\n  - source: /opt/tool\n    mode: rw\n")

	pre2, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration (after): %v", err)
	}

	if pre1.inputHash == pre2.inputHash {
		t.Errorf("input_hash unchanged after kit additional_bindings mode changed: %q", pre1.inputHash)
	}
}

// --- MAJOR 3 (codex review, 2nd pass): migration must not clobber an
// --- existing host_commands.yaml ---

// TestMigrateWorkspaceYAMLToDB_PreservesExistingHostCommandsConfig pins the
// 2nd-pass MAJOR 3 fix: MigrateWorkspaceYAMLToDB itself (not just wire.go's
// startup preflight, fixed in the 1st pass) must not unconditionally
// overwrite ~/.config/boid/host_commands.yaml. A PR2-generated or
// hand-edited config already on disk when the one-time cutover runs must
// survive untouched — before this fix, MigrateWorkspaceYAMLToDB called
// WriteHostCommandsConfig unconditionally, so its first (committed,
// one-time) run silently replaced any existing content with its own
// freshly-aggregated kit host_commands.
func TestMigrateWorkspaceYAMLToDB_PreservesExistingHostCommandsConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr, issue]\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - gh-kit\n")

	hostCommandsPath, err := DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	handEdited := map[string]HostCommandSpec{
		"custom-tool": {Allow: []string{"run"}},
	}
	if err := WriteHostCommandsConfig(hostCommandsPath, handEdited); err != nil {
		t.Fatalf("seed hand-edited host_commands.yaml: %v", err)
	}

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB: %v", err)
	}

	got, err := LoadHostCommandsConfig(hostCommandsPath)
	if err != nil {
		t.Fatalf("LoadHostCommandsConfig: %v", err)
	}
	if _, ok := got["custom-tool"]; !ok {
		t.Errorf("expected hand-edited 'custom-tool' entry to survive migration, got %v", got)
	}
	if _, ok := got["gh"]; ok {
		t.Errorf("migration must not merge/aggregate into an existing host_commands.yaml, got a 'gh' entry too: %v", got)
	}
}
