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
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	var count int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 15 {
		t.Fatalf("schema_migrations count = %d, want 15", count)
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
