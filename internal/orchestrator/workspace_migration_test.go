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

// TestMigrateWorkspaceYAMLToDB_UpgradeFromPR6StagingHash_RollsForward pins
// MAJOR 4 (codex review round 1, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): a state=staging row recorded by a PR6 binary (hashed with
// the pre-PR7 shape — no WorkspaceKitRefs field) must still roll forward on
// a restart with the PR7 binary, as long as the on-disk workspace/kit inputs
// have not actually changed since. Before this fix, PR7's input_hash always
// includes WorkspaceKitRefs, so comparing a PR6-recorded hash only against
// the current (PR7) shape would treat every such upgrade-in-place as "the
// inputs changed" and abort demanding manual intervention — even though
// nothing on disk actually changed. Seeding the staging row with
// pre.legacyInputHashPR6 (the same helper preflightWorkspaceMigration itself
// uses) simulates exactly what a real PR6 binary would have recorded.
func TestMigrateWorkspaceYAMLToDB_UpgradeFromPR6StagingHash_RollsForward(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "env:\n  FOO: bar\n")

	pre, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration: %v", err)
	}
	if pre.legacyInputHashPR6 == pre.inputHash {
		t.Fatalf("legacyInputHashPR6 (%q) unexpectedly equals the PR7 inputHash — the two hash shapes should differ", pre.legacyInputHashPR6)
	}

	// Seed the staging row with the *legacy* (pre-PR7) hash shape, simulating
	// a PR6 binary's interrupted attempt — not pre.inputHash, which is what
	// TestMigrateWorkspaceYAMLToDB_CrashRecoveryMatchingHashRollsForward
	// above already covers for the same-binary-version case.
	tx, err := env.conn.Conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := upsertMigrationRow(tx, workspaceDBConsolidationVersion, "staging", pre.legacyInputHashPR6); err != nil {
		t.Fatalf("upsertMigrationRow(staging, legacy hash): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Fatalf("test setup: expected zero workspace rows before roll-forward, got %v", slugs)
	}

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB (upgrade roll-forward): %v", err)
	}

	state, inputHash, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found || state != "committed" {
		t.Fatalf("state after upgrade roll-forward = %q (found=%v), want committed", state, found)
	}
	// The committed row is re-hashed with the current (PR7) shape, not left
	// at the legacy hash it was seeded with.
	if inputHash != pre.inputHash {
		t.Errorf("input_hash after upgrade roll-forward = %q, want %q (current PR7 shape)", inputHash, pre.inputHash)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamA, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a) after upgrade roll-forward: %v", err)
	}
	if teamA.Env["FOO"] != "bar" {
		t.Errorf("team-a.Env[FOO] after upgrade roll-forward = %q, want bar", teamA.Env["FOO"])
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

// TestMigrateWorkspaceYAMLToDB_MaterializesKitEnv pins BLOCKER 1 (codex
// review): a workspace's legacy `kits:` list contributes more than just
// host_commands *names* — a kit's Env must also be materialized into the
// DB-bound WorkspaceMeta, since the workspaces table has no column for Kits
// at all (WorkspaceRepository never persists it, and Phase 2.5 PR7 removed
// the field from the type outright). Before this fix, only host_command
// names survived the cutover; a kit's env var silently vanished the moment
// the DB became authoritative. (A kit's additional_bindings used to
// materialize here too — retired outright in docs/plans/home-workspace-volume.md
// Phase 4 PR4, along with the WorkspaceMeta.AdditionalBindings field it fed;
// the kit.yaml fixture below still declares one, to pin that its presence no
// longer causes an error or gets materialized anywhere.)
func TestMigrateWorkspaceYAMLToDB_MaterializesKitEnv(t *testing.T) {
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

	meta := &WorkspaceMeta{}
	if err := materializeKitRuntimeIntoWorkspace(snap, []string{"kit-a"}, meta); err != nil {
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

// --- MAJOR 1 (codex review, 2nd pass): bind mount merge precedence ---
// (TestMaterializeKitRuntime_WorkspaceBindingsWinRO, the additional_bindings
// half of this precedence, was removed in docs/plans/home-workspace-volume.md
// Phase 4 PR4 along with the mechanism it pinned — see
// materializeKitRuntimeIntoWorkspace's doc comment.)

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
		Env: map[string]string{"SHARED": "from-workspace"},
	}
	if err := materializeKitRuntimeIntoWorkspace(snap, []string{"kit-a"}, meta); err != nil {
		t.Fatalf("materializeKitRuntimeIntoWorkspace: %v", err)
	}
	if meta.Env["SHARED"] != "from-workspace" {
		t.Errorf("meta.Env[SHARED] = %q, want from-workspace (workspace must win over kit)", meta.Env["SHARED"])
	}
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

// TestReadWorkspaceYAMLSnapshot_SingleReadAvoidsTOCTOU pins MAJOR 5 (codex
// review round 1, docs/plans/workspace-db-consolidation.md): meta and
// kitRefs must both be decoded from a single byte snapshot of the workspace
// yaml file, not from two independent reads of the same path (the old
// yamlStore.Load + legacyWorkspaceYAMLKits pair) that an atomic rename could
// race between.
//
// workspaceYAMLReadFile is swapped out for the duration of this test to
// simulate exactly that race: right after the (only expected) read returns
// versionA's bytes, the swapped-in function overwrites the file on disk with
// versionB — so a second read, which the old two-read implementation would
// have performed to recover the kits: list, would observe a different file
// than the first read did (a "meta from versionA + kits from versionB"
// hybrid that never existed on disk at any single instant). Asserting
// callCount == 1 and that both the decoded meta and kitRefs come from
// versionA proves readWorkspaceYAMLSnapshot never performs that second,
// racy read.
func TestReadWorkspaceYAMLSnapshot_SingleReadAvoidsTOCTOU(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team-a.yaml")
	versionA := []byte("kits:\n  - kit-a\nenv:\n  MARK: A\n")
	versionB := []byte("kits:\n  - kit-b\nenv:\n  MARK: B\n")
	if err := os.WriteFile(path, versionA, 0o644); err != nil {
		t.Fatalf("write versionA: %v", err)
	}

	callCount := 0
	orig := workspaceYAMLReadFile
	t.Cleanup(func() { workspaceYAMLReadFile = orig })
	workspaceYAMLReadFile = func(p string) ([]byte, error) {
		callCount++
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if writeErr := os.WriteFile(p, versionB, 0o644); writeErr != nil {
			t.Fatalf("swap to versionB: %v", writeErr)
		}
		return data, nil
	}

	meta, kitRefs, _, err := readWorkspaceYAMLSnapshot(dir, "team-a")
	if err != nil {
		t.Fatalf("readWorkspaceYAMLSnapshot: %v", err)
	}
	if callCount != 1 {
		t.Fatalf("workspaceYAMLReadFile called %d times, want exactly 1 (a second read risks a TOCTOU hybrid of versionA/versionB)", callCount)
	}
	if len(kitRefs) != 1 || kitRefs[0] != "kit-a" {
		t.Errorf("kitRefs = %v, want [kit-a] (from the single versionA read, not the swapped-in versionB)", kitRefs)
	}
	if meta.Env["MARK"] != "A" {
		t.Errorf("meta.Env[MARK] = %q, want %q (from the single versionA read, not the swapped-in versionB)", meta.Env["MARK"], "A")
	}
}

// --- MAJOR 1 (codex review round 2): PR6 legacy hash reconstruction must
// --- actually include each workspace's legacy Kits reference list ---

// TestComputeWorkspaceMigrationInputHashPR6Shape_KitsIncluded_MatchesGolden
// pins MAJOR 1 (codex review round 2, docs/plans/workspace-db-consolidation.md
// Phase 2.5 PR7): computeWorkspaceMigrationInputHashPR6Shape must reproduce
// the *exact* byte shape (and therefore sha256 hash) a genuine PR6 binary
// computed for the same logical inputs. PR6's WorkspaceMeta had a
// `Kits []string` field (removed outright by PR7, decision 12); PR6's own
// computeWorkspaceMigrationInputHash relied on that field being part of each
// Workspaces entry's JSON encoding. Before this fix,
// computeWorkspaceMigrationInputHashPR6Shape reused the CURRENT (Kits-less)
// WorkspaceMeta to approximate the PR6 shape — which can never carry a Kits
// value at all, since the field does not exist on that type any more — so a
// workspace referencing a kit hashed differently from what a real PR6
// binary computed for the identical on-disk inputs, defeating the entire
// point of the crash-recovery upgrade check (MAJOR 4, round 1) specifically
// for the workspaces it matters most for.
//
// The two golden hash literals below were computed independently: by
// checking out the actual PR6 commit (fb1f222, "feat: Phase 2.5 PR6 (kit
// 機構退役)") into a throwaway git worktree and calling ITS OWN (unmodified)
// computeWorkspaceMigrationInputHash — a 4-argument function, since
// WorkspaceKitRefs did not exist yet, with Workspaces keyed by PR6's own
// Kits-having WorkspaceMeta — against the fixture data reproduced exactly
// below. Neither literal is derived from any code in this repository's
// current tree, so this test cannot pass by tautology/self-reference: it
// only passes if pr6WorkspaceMeta's field layout truly matches PR6's
// WorkspaceMeta field-for-field, tag-for-tag, order-for-order.
func TestComputeWorkspaceMigrationInputHashPR6Shape_KitsIncluded_MatchesGolden(t *testing.T) {
	// "with-kit" exercises the actual regression (a workspace whose legacy
	// `kits:` list must be reflected in the hash); "no-kit" pins the other
	// half the task calls for — a workspace with no kit reference at all
	// must still round-trip through pr6WorkspaceMeta unchanged.
	workspaces := map[string]*WorkspaceMeta{
		"with-kit": {
			Env:            map[string]string{"FOO": "bar"},
			HostCommands:   []string{"gh"},
			AllowedDomains: []string{"example.com"},
		},
		"no-kit": {
			Env: map[string]string{"BAZ": "qux"},
		},
	}
	hostCommands := map[string]HostCommandSpec{
		"gh": {Allow: []string{"pr", "issue"}},
	}
	projectRefs := []*WorkspaceSummary{
		{ID: "with-kit", ProjectCount: 2},
		{ID: "no-kit", ProjectCount: 1},
	}
	kitRuntime := map[string]kitRuntimeRaw{
		"docker-proxy-test": {
			HostCommands: HostCommands{
				"docker-proxy-test.sh": HostCommandSpec{Allow: []string{"run"}},
			},
			Env: map[string]string{"DOCKER_PROXY_TEST_ROOT": "${E2E_WORKSPACE_DIR}"},
			AdditionalBindings: []BindMount{
				{Source: "/host/docker-proxy-test"},
			},
		},
	}
	workspaceKitRefs := map[string][]string{
		"with-kit": {"docker-proxy-test"},
	}

	// workspaceAdditionalBindings (Phase 4 PR4, docs/plans/home-workspace-volume.md)
	// is left nil in both calls below: none of this fixture's workspaces set
	// WorkspaceMeta.AdditionalBindings before that field was retired either
	// (see the workspaces map literal above), so pr6WorkspaceMeta.AdditionalBindings
	// was already nil for both golden hashes — passing nil here reproduces
	// the identical byte shape, keeping both golden literals valid.
	const wantHashWithKits = "d3b637ed5cda1f902a60fff81e578ffa8084cfd798e1389fc88ebd7d35685ccb"
	got, err := computeWorkspaceMigrationInputHashPR6Shape(workspaces, hostCommands, projectRefs, kitRuntime, workspaceKitRefs, nil)
	if err != nil {
		t.Fatalf("computeWorkspaceMigrationInputHashPR6Shape: %v", err)
	}
	if got != wantHashWithKits {
		t.Errorf("hash = %q, want golden PR6 hash %q (pr6WorkspaceMeta is not reproducing PR6's byte shape for a workspace with kits)", got, wantHashWithKits)
	}

	// Same fixture, but "with-kit"'s legacy kits: list is empty (the "kit
	// 無し workspace" half) — an independently-computed golden value again,
	// from the same PR6 worktree/commit with the workspace's Kits field left
	// unset entirely.
	const wantHashKitsStripped = "14164174fde4844121d14d6e4c1c10306c878c34daa9c4c9d24249e7804a03de"
	gotStripped, err := computeWorkspaceMigrationInputHashPR6Shape(workspaces, hostCommands, projectRefs, kitRuntime, map[string][]string{}, nil)
	if err != nil {
		t.Fatalf("computeWorkspaceMigrationInputHashPR6Shape (kits stripped): %v", err)
	}
	if gotStripped != wantHashKitsStripped {
		t.Errorf("hash (kits stripped) = %q, want golden PR6 hash %q", gotStripped, wantHashKitsStripped)
	}
	if got == gotStripped {
		t.Fatal("hash must differ when a workspace's Kits ref list differs — pr6WorkspaceMeta.Kits is not affecting the hash at all")
	}
}

// TestMigrateWorkspaceYAMLToDB_UpgradeFromPR6StagingHash_WithKits_RollsForward
// is the integration counterpart of the golden test above: a real
// MigrateWorkspaceYAMLToDB crash-recovery roll-forward, end to end, for a
// workspace that references a kit — exercising the actual wiring
// (preflightWorkspaceMigration -> rawKitRefs -> pr6-shape hash computation)
// rather than hand-constructed Go values, and confirming the kit's
// host_commands are still correctly resolved after the roll-forward.
// Complements, rather than replaces, the golden test's byte-accuracy pin —
// see TestMigrateWorkspaceYAMLToDB_UpgradeFromPR6StagingHash_RollsForward
// above for the equivalent no-kits case.
func TestMigrateWorkspaceYAMLToDB_UpgradeFromPR6StagingHash_WithKits_RollsForward(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	env := newMigrationTestEnv(t)

	writeMigrationKitYAML(t, env.kitsDir, "gh-kit", "host_commands:\n  gh:\n    allow: [pr, issue]\n")
	writeMigrationWorkspaceYAML(t, env.workspaceDir, "team-a", "kits:\n  - gh-kit\nenv:\n  FOO: bar\n")

	pre, err := preflightWorkspaceMigration(env.workspaceDir, env.kitsDir, env.projectRepo)
	if err != nil {
		t.Fatalf("preflightWorkspaceMigration: %v", err)
	}
	if pre.legacyInputHashPR6 == pre.inputHash {
		t.Fatalf("legacyInputHashPR6 (%q) unexpectedly equals the PR7 inputHash — the two hash shapes should differ", pre.legacyInputHashPR6)
	}

	// Seed the staging row with the legacy (pre-PR7) hash shape, simulating
	// a PR6 binary's interrupted attempt for a workspace that references a
	// kit.
	tx, err := env.conn.Conn.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := upsertMigrationRow(tx, workspaceDBConsolidationVersion, "staging", pre.legacyInputHashPR6); err != nil {
		t.Fatalf("upsertMigrationRow(staging, legacy hash): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if slugs := workspaceSlugs(t, env.conn); len(slugs) != 0 {
		t.Fatalf("test setup: expected zero workspace rows before roll-forward, got %v", slugs)
	}

	if err := MigrateWorkspaceYAMLToDB(env.conn.Conn, env.workspaceDir, env.kitsDir, env.projectRepo); err != nil {
		t.Fatalf("MigrateWorkspaceYAMLToDB (upgrade roll-forward, with kits): %v", err)
	}

	state, inputHash, found := schemaMigrationRow(t, env.conn, workspaceDBConsolidationVersion)
	if !found || state != "committed" {
		t.Fatalf("state after upgrade roll-forward = %q (found=%v), want committed", state, found)
	}
	if inputHash != pre.inputHash {
		t.Errorf("input_hash after upgrade roll-forward = %q, want %q (current PR7 shape)", inputHash, pre.inputHash)
	}

	repo := NewWorkspaceRepository(env.conn.Conn)
	teamA, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("Load(team-a) after upgrade roll-forward: %v", err)
	}
	if !equalStringSlice(teamA.HostCommands, []string{"gh"}) {
		t.Errorf("team-a.HostCommands after upgrade roll-forward = %v, want [gh] (kit still resolved)", teamA.HostCommands)
	}
	if teamA.Env["FOO"] != "bar" {
		t.Errorf("team-a.Env[FOO] after upgrade roll-forward = %q, want bar", teamA.Env["FOO"])
	}
}
