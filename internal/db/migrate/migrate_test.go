package migrate

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestApplyFreshDatabase(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0001_initial")
	assertVersionRecorded(t, d.Conn, "0002_add_jobs_handler_id")
	assertVersionRecorded(t, d.Conn, "0003_add_jobs_role")
	assertVersionRecorded(t, d.Conn, "0004_add_jobs_runtime_metadata")

	hasColumn, err := columnExists(d.Conn, "jobs", "handler_id")
	if err != nil {
		t.Fatalf("check jobs.handler_id: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.handler_id to exist")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "role")
	if err != nil {
		t.Fatalf("check jobs.role: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.role to exist")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "runtime_id")
	if err != nil {
		t.Fatalf("check jobs.runtime_id: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.runtime_id to exist")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "interactive")
	if err != nil {
		t.Fatalf("check jobs.interactive: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.interactive to exist")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "tty")
	if err != nil {
		t.Fatalf("check jobs.tty: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.tty to exist")
	}
	hasColumn, err = columnExists(d.Conn, "tasks", "instructions")
	if err != nil {
		t.Fatalf("check tasks.instructions: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected tasks.instructions to exist")
	}
}

func TestApplyLegacyDatabaseAddsHandlerID(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if _, err := d.Conn.Exec(`
		CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			work_dir TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			behavior TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE actions (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id),
			type TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE secrets (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL UNIQUE,
			value_encrypted BLOB NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE jobs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id),
			project_id TEXT NOT NULL REFERENCES projects(id),
			status TEXT NOT NULL DEFAULT 'running',
			exit_code INTEGER,
			output TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE worktrees (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
			project_id TEXT NOT NULL REFERENCES projects(id),
			path TEXT NOT NULL,
			branch TEXT NOT NULL,
			base_branch TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			cleaned_at DATETIME
		);
	`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations to legacy db: %v", err)
	}

	hasColumn, err := columnExists(d.Conn, "jobs", "handler_id")
	if err != nil {
		t.Fatalf("check jobs.handler_id: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.handler_id to be added")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "role")
	if err != nil {
		t.Fatalf("check jobs.role: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.role to be added")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "runtime_id")
	if err != nil {
		t.Fatalf("check jobs.runtime_id: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.runtime_id to be added")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "interactive")
	if err != nil {
		t.Fatalf("check jobs.interactive: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.interactive to be added")
	}
	hasColumn, err = columnExists(d.Conn, "jobs", "tty")
	if err != nil {
		t.Fatalf("check jobs.tty: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected jobs.tty to be added")
	}

	assertVersionRecorded(t, d.Conn, "0001_initial")
	assertVersionRecorded(t, d.Conn, "0002_add_jobs_handler_id")
	assertVersionRecorded(t, d.Conn, "0003_add_jobs_role")
	assertVersionRecorded(t, d.Conn, "0004_add_jobs_runtime_metadata")
}

func TestApplyIsIdempotent(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	var firstCount int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatalf("count after first apply: %v", err)
	}
	if firstCount == 0 {
		t.Fatal("expected at least one migration to be applied on first run")
	}

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	var secondCount int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatalf("count after second apply: %v", err)
	}
	if secondCount != firstCount {
		t.Fatalf("schema_migrations count differs after idempotent apply: first=%d, second=%d", firstCount, secondCount)
	}
}

func TestMigration0023_RenamesConsumerToAgent(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// Apply all migrations up to but not including 0023.
	// We do this by applying all migrations first to get the schema, then
	// manually inserting a task with the old "consumer" key format.
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	if _, err = d.Conn.Exec(
		`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`,
	); err != nil {
		t.Fatalf("insert project: %v", err)
	}

	// Insert a task with old-format instructions containing "consumer":
	oldInstructions := `[{"type":"execution","consumer":"claude-code","message":"do it"}]`
	_, err = d.Conn.Exec(
		`INSERT INTO tasks (id, project_id, title, status, behavior, payload, instructions)
		 VALUES ('t1', 'p1', 'test', 'pending', 'dev', '{}', ?)`,
		oldInstructions,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}

	// Re-run the migration (should apply it to the newly inserted row).
	// Since the migration was already recorded, we need to run it directly.
	if _, err := d.Conn.Exec(
		`UPDATE tasks SET instructions = replace(instructions, '"consumer":', '"agent":') WHERE instructions LIKE '%"consumer":%'`,
	); err != nil {
		t.Fatalf("run migration SQL: %v", err)
	}

	// Verify the instructions now use "agent" key.
	var got string
	if err := d.Conn.QueryRow(`SELECT instructions FROM tasks WHERE id = 't1'`).Scan(&got); err != nil {
		t.Fatalf("query instructions: %v", err)
	}
	expected := `[{"type":"execution","agent":"claude-code","message":"do it"}]`
	if got != expected {
		t.Errorf("instructions after migration = %q, want %q", got, expected)
	}
}

// TestMigration0028_AddsProjectsUpstreamURL covers PR2 of
// docs/plans/git-gateway-cutover.md: the projects.upstream_url column must
// exist after migration and default to NULL (nullable — existing rows are
// backfilled separately, not by the migration itself).
func TestMigration0028_AddsProjectsUpstreamURL(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0028_add_projects_upstream_url")

	hasColumn, err := columnExists(d.Conn, "projects", "upstream_url")
	if err != nil {
		t.Fatalf("check projects.upstream_url: %v", err)
	}
	if !hasColumn {
		t.Fatal("expected projects.upstream_url to exist")
	}

	if _, err := d.Conn.Exec(`INSERT INTO projects (id, work_dir) VALUES ('p1', '/tmp/p1')`); err != nil {
		t.Fatalf("insert project without upstream_url: %v", err)
	}
	var upstreamURL sql.NullString
	if err := d.Conn.QueryRow(`SELECT upstream_url FROM projects WHERE id = 'p1'`).Scan(&upstreamURL); err != nil {
		t.Fatalf("query upstream_url: %v", err)
	}
	if upstreamURL.Valid {
		t.Errorf("expected upstream_url to default to NULL, got %q", upstreamURL.String)
	}
}

// TestMigration0029_DropsWorktreesTable covers PR8 of
// docs/plans/git-gateway-cutover.md: the worktrees table is retired (host git
// worktree allocation was replaced by sandbox-internal clone in PR6) and must
// be gone after migration, both for a fresh database and for a legacy
// database that still has the table from before this migration existed.
func TestMigration0029_DropsWorktreesTable(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0029_drop_worktrees_table")

	exists, err := tableExists(d.Conn, "worktrees")
	if err != nil {
		t.Fatalf("check worktrees table: %v", err)
	}
	if exists {
		t.Fatal("expected worktrees table to be dropped")
	}
}

// TestMigration0029_IdempotentOnAlreadyDroppedTable verifies the migration is
// safe to run again (e.g. a fresh 0001_initial database created after this
// migration was added never has a worktrees table to begin with).
func TestMigration0029_IdempotentOnAlreadyDroppedTable(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("second apply (idempotent): %v", err)
	}
	assertVersionRecorded(t, d.Conn, "0029_drop_worktrees_table")
}

// TestMigration0031_AddsSchemaMigrationsStateAndInputHash covers PR3 of
// docs/plans/workspace-db-consolidation.md: schema_migrations must gain
// `state` and `input_hash` columns, and existing rows (every migration
// version applied earlier in this same Apply() run) must read back with the
// backward-compatible defaults ('committed' / '').
func TestMigration0031_AddsSchemaMigrationsStateAndInputHash(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0031_add_schema_migrations_state")

	hasState, err := columnExists(d.Conn, "schema_migrations", "state")
	if err != nil {
		t.Fatalf("check schema_migrations.state: %v", err)
	}
	if !hasState {
		t.Fatal("expected schema_migrations.state to exist")
	}
	hasInputHash, err := columnExists(d.Conn, "schema_migrations", "input_hash")
	if err != nil {
		t.Fatalf("check schema_migrations.input_hash: %v", err)
	}
	if !hasInputHash {
		t.Fatal("expected schema_migrations.input_hash to exist")
	}

	// A migration recorded earlier in the same Apply() run (before 0031 added
	// these columns) must read back with the backward-compatible defaults.
	var state, inputHash string
	if err := d.Conn.QueryRow(
		`SELECT state, input_hash FROM schema_migrations WHERE version = ?`, "0001_initial",
	).Scan(&state, &inputHash); err != nil {
		t.Fatalf("query 0001_initial state/input_hash: %v", err)
	}
	if state != "committed" {
		t.Errorf("0001_initial state = %q, want %q", state, "committed")
	}
	if inputHash != "" {
		t.Errorf("0001_initial input_hash = %q, want empty", inputHash)
	}
}

// TestMigration0032_AddsWebDevicesTokenColumns covers Phase 3 PR0 of
// docs/plans/cli-remote-connection.md: web_devices must gain nullable
// token_hash/token_created_at columns, cookie_hash must become nullable (so
// a Bearer-only device row can omit it), and a Bearer-only device insert
// (cookie_hash left NULL) must succeed.
func TestMigration0032_AddsWebDevicesTokenColumns(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0032_add_web_devices_token")

	hasTokenHash, err := columnExists(d.Conn, "web_devices", "token_hash")
	if err != nil {
		t.Fatalf("check web_devices.token_hash: %v", err)
	}
	if !hasTokenHash {
		t.Fatal("expected web_devices.token_hash to exist")
	}
	hasTokenCreatedAt, err := columnExists(d.Conn, "web_devices", "token_created_at")
	if err != nil {
		t.Fatalf("check web_devices.token_created_at: %v", err)
	}
	if !hasTokenCreatedAt {
		t.Fatal("expected web_devices.token_created_at to exist")
	}
	cookieHashNullable, err := columnIsNullable(d.Conn, "web_devices", "cookie_hash")
	if err != nil {
		t.Fatalf("check web_devices.cookie_hash nullability: %v", err)
	}
	if !cookieHashNullable {
		t.Fatal("expected web_devices.cookie_hash to be nullable")
	}

	// A Bearer-only device row (no cookie_hash) must be insertable now that
	// the NOT NULL constraint is gone.
	if _, err := d.Conn.Exec(
		`INSERT INTO web_devices (id, label, token_hash, token_created_at, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"dev-bearer-only", "cli", []byte("tokenhash"), "2026-07-17T00:00:00Z", "2026-07-17T00:00:00Z", "2026-07-17T00:00:00Z",
	); err != nil {
		t.Fatalf("insert bearer-only device (no cookie_hash): %v", err)
	}

	var cookieHash sql.NullString
	if err := d.Conn.QueryRow(`SELECT cookie_hash FROM web_devices WHERE id = ?`, "dev-bearer-only").Scan(&cookieHash); err != nil {
		t.Fatalf("query bearer-only device: %v", err)
	}
	if cookieHash.Valid {
		t.Errorf("cookie_hash = %q, want NULL", cookieHash.String)
	}
}

// TestMigration0032_LegacyDatabasePreservesExistingCookieDevice verifies a
// pre-existing cookie-authenticated device row survives the table
// recreation (0021_jobs_nullable_task_id.sql's pattern) with cookie_hash
// intact and token_hash/token_created_at left NULL.
func TestMigration0032_LegacyDatabasePreservesExistingCookieDevice(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	// Full pre-0001 base schema (mirrors TestApplyLegacyDatabaseAddsHandlerID)
	// plus a web_devices table in its pre-0032 shape (cookie_hash NOT NULL,
	// no token columns — i.e. exactly what a real installation upgrading
	// straight from before this migration has on disk) with one seeded
	// cookie-authenticated device row.
	if _, err := d.Conn.Exec(`
		CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			work_dir TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id),
			title TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			behavior TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE actions (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id),
			type TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE secrets (
			id TEXT PRIMARY KEY,
			key TEXT NOT NULL UNIQUE,
			value_encrypted BLOB NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE jobs (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES tasks(id),
			project_id TEXT NOT NULL REFERENCES projects(id),
			status TEXT NOT NULL DEFAULT 'running',
			exit_code INTEGER,
			output TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE worktrees (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id),
			project_id TEXT NOT NULL REFERENCES projects(id),
			path TEXT NOT NULL,
			branch TEXT NOT NULL,
			base_branch TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			cleaned_at DATETIME
		);
		CREATE TABLE web_devices (
			id           TEXT PRIMARY KEY,
			label        TEXT,
			cookie_hash  BLOB NOT NULL,
			created_at   TIMESTAMP NOT NULL,
			last_seen_at TIMESTAMP NOT NULL,
			revoked_at   TIMESTAMP
		);
		INSERT INTO web_devices (id, label, cookie_hash, created_at, last_seen_at)
		VALUES ('dev-legacy', 'old-laptop', X'cafe', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
	`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	assertVersionRecorded(t, d.Conn, "0032_add_web_devices_token")

	var label string
	var cookieHash []byte
	var tokenHash sql.NullString
	if err := d.Conn.QueryRow(
		`SELECT label, cookie_hash, token_hash FROM web_devices WHERE id = ?`, "dev-legacy",
	).Scan(&label, &cookieHash, &tokenHash); err != nil {
		t.Fatalf("query legacy device: %v", err)
	}
	if label != "old-laptop" {
		t.Errorf("label = %q, want %q", label, "old-laptop")
	}
	if string(cookieHash) != "\xca\xfe" {
		t.Errorf("cookie_hash = %x, want cafe", cookieHash)
	}
	if tokenHash.Valid {
		t.Errorf("token_hash = %q, want NULL", tokenHash.String)
	}
}

// TestRecordMigrationState_UpsertTransitionsState pins the UPSERT behavior
// recordMigrationState needs for workspace_db_consolidation's staging →
// committed state transition (docs/plans/workspace-db-consolidation.md
// マイグレーション節): calling it twice for the same version must update the
// existing row in place (not insert a duplicate), and the final read must
// reflect the latest state/input_hash.
func TestRecordMigrationState_UpsertTransitionsState(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	tx, err := d.Conn.Begin()
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := recordMigrationState(tx, "workspace_db_consolidation", "staging", "hash1"); err != nil {
		t.Fatalf("recordMigrationState (staging): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var state, inputHash string
	if err := d.Conn.QueryRow(
		`SELECT state, input_hash FROM schema_migrations WHERE version = ?`, "workspace_db_consolidation",
	).Scan(&state, &inputHash); err != nil {
		t.Fatalf("query after staging: %v", err)
	}
	if state != "staging" || inputHash != "hash1" {
		t.Fatalf("after staging: state=%q input_hash=%q, want staging/hash1", state, inputHash)
	}

	tx2, err := d.Conn.Begin()
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	if err := recordMigrationState(tx2, "workspace_db_consolidation", "committed", "hash1"); err != nil {
		t.Fatalf("recordMigrationState (committed): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("commit tx2: %v", err)
	}

	var count int
	if err := d.Conn.QueryRow(
		`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, "workspace_db_consolidation",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected a single upserted row, got %d", count)
	}

	if err := d.Conn.QueryRow(
		`SELECT state, input_hash FROM schema_migrations WHERE version = ?`, "workspace_db_consolidation",
	).Scan(&state, &inputHash); err != nil {
		t.Fatalf("query after committed: %v", err)
	}
	if state != "committed" || inputHash != "hash1" {
		t.Fatalf("after committed: state=%q input_hash=%q, want committed/hash1", state, inputHash)
	}
}

// TestApply_RefusesUnknownFutureMigrationVersion covers the §決定4 schema
// ceiling check (docs/plans/phase6-container-backend.md §PR6): a database
// already migrated by a newer binary (a schema_migrations row this
// binary's own migrations slice has never heard of, with a numeric prefix
// past its newest known version) must make Apply refuse to start rather
// than silently proceed as if nothing were different.
func TestApply_RefusesUnknownFutureMigrationVersion(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	// Simulate a future binary having already migrated this database past
	// what the current binary (this test process) knows about.
	if _, err := d.Conn.Exec(
		`INSERT INTO schema_migrations (version, state, input_hash) VALUES (?, 'committed', '')`,
		"9999_some_future_migration",
	); err != nil {
		t.Fatalf("seed future migration row: %v", err)
	}

	err = Apply(d.Conn)
	if err == nil {
		t.Fatal("expected Apply to refuse to start against a database with an unknown future migration version, got nil error")
	}
	if !strings.Contains(err.Error(), "9999_some_future_migration") {
		t.Errorf("error = %q, want it to name the offending version", err.Error())
	}
}

// TestApply_IgnoresNonNumberedSchemaMigrationsRows verifies the ceiling
// check does not misfire on schema_migrations rows that were never a
// numbered migrations/*.sql file to begin with — in particular
// "workspace_db_consolidation" (internal/orchestrator/workspace_migration.go
// records this directly via recordMigrationState, not through this
// package's migrations slice). A non-numeric-prefixed version string must
// never trip the "future migration" refusal.
func TestApply_IgnoresNonNumberedSchemaMigrationsRows(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("initial apply: %v", err)
	}

	if _, err := d.Conn.Exec(
		`INSERT INTO schema_migrations (version, state, input_hash) VALUES (?, 'committed', '')`,
		"workspace_db_consolidation",
	); err != nil {
		t.Fatalf("seed workspace_db_consolidation row: %v", err)
	}

	if err := Apply(d.Conn); err != nil {
		t.Fatalf("Apply should not refuse on a non-numbered schema_migrations row: %v", err)
	}
}

func assertVersionRecorded(t *testing.T, conn *sql.DB, version string) {
	t.Helper()

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
		t.Fatalf("query schema_migrations for %s: %v", version, err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations[%s] count = %d, want 1", version, count)
	}
}
