package migrate

import (
	"database/sql"
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
