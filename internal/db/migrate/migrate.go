package migrate

import (
	"database/sql"
	"embed"
	"fmt"
)

//go:embed migrations/*.sql
var schemaFS embed.FS

func Apply(conn *sql.DB) error {
	migrations := []migration{
		{
			version: "0001_initial",
			path:    "migrations/0001_initial.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return legacySchemaPresent(tx)
			},
		},
		{
			version: "0002_add_jobs_handler_id",
			path:    "migrations/0002_add_jobs_handler_id.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "jobs", "handler_id")
			},
		},
		{
			version: "0003_add_jobs_role",
			path:    "migrations/0003_add_jobs_role.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "jobs", "role")
			},
		},
		{
			version: "0004_add_jobs_runtime_metadata",
			path:    "migrations/0004_add_jobs_runtime_metadata.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "jobs", "runtime_id")
			},
		},
		{
			version: "0005_add_secrets_namespace",
			path:    "migrations/0005_add_secrets_namespace.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "secrets", "namespace")
			},
		},
		{
			version: "0006_add_tasks_auto_start",
			path:    "migrations/0006_add_tasks_auto_start.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "auto_start")
			},
		},
		{
			version: "0007_embed_behavior_fields",
			path:    "migrations/0007_embed_behavior_fields.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "transition")
			},
		},
		{
			version: "0008_add_tasks_start_gate",
			path:    "migrations/0008_add_tasks_start_gate.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "start_gate")
			},
		},
		{
			version: "0009_add_task_dependencies",
			path:    "migrations/0009_add_task_dependencies.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "task_dependencies")
			},
		},
		{
			version: "0010_add_task_ref_parent",
			path:    "migrations/0010_add_task_ref_parent.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "ref")
			},
		},
		{
			version: "0011_drop_tasks_start_gate",
			path:    "migrations/0011_drop_tasks_start_gate.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := columnExists(tx, "tasks", "start_gate")
				return !exists, err
			},
		},
		{
			version: "0012_add_tasks_depends_on_payload",
			path:    "migrations/0012_add_tasks_depends_on_payload.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "depends_on_payload")
			},
		},
		{
			version: "0013_add_tasks_ephemeral",
			path:    "migrations/0013_add_tasks_ephemeral.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "ephemeral")
			},
		},
		{
			version: "0014_drop_tasks_transition",
			path:    "migrations/0014_drop_tasks_transition.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := columnExists(tx, "tasks", "transition")
				return !exists, err
			},
		},
		{
			version: "0015_drop_task_ephemeral",
			path:    "migrations/0015_drop_task_ephemeral.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := columnExists(tx, "tasks", "ephemeral")
				return !exists, err
			},
		},
		{
			version: "0016_add_actions_status_transition",
			path:    "migrations/0016_add_actions_status_transition.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "actions", "from_status")
			},
		},
		{
			version: "0018_add_tasks_instructions",
			path:    "migrations/0018_add_tasks_instructions.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "instructions")
			},
		},
		{
			version: "0019_add_jobs_execution_state",
			path:    "migrations/0019_add_jobs_execution_state.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "jobs", "execution_state")
			},
		},
		{
			version: "0020_web_auth",
			path:    "migrations/0020_web_auth.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				ok, err := tableExists(tx, "web_devices")
				if err != nil || !ok {
					return ok, err
				}
				return tableExists(tx, "web_pairing_codes")
			},
		},
		{
			version: "0021_jobs_nullable_task_id",
			path:    "migrations/0021_jobs_nullable_task_id.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnIsNullable(tx, "jobs", "task_id")
			},
		},
		{
			version: "0022_drop_verifying_reworking",
			path:    "migrations/0022_drop_verifying_reworking.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				// idempotent: skip when no tasks remain in verifying/reworking.
				var count int
				if err := tx.QueryRow(
					`SELECT COUNT(*) FROM tasks WHERE status IN ('verifying','reworking')`,
				).Scan(&count); err != nil {
					return false, err
				}
				return count == 0, nil
			},
		},
		{
			version: "0023_rename_instruction_consumer_to_agent",
			path:    "migrations/0023_rename_instruction_consumer_to_agent.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				// idempotent: skip when no tasks still use the old "consumer": key.
				var count int
				if err := tx.QueryRow(
					`SELECT COUNT(*) FROM tasks WHERE instructions LIKE '%"consumer":%'`,
				).Scan(&count); err != nil {
					return false, err
				}
				return count == 0, nil
			},
		},
		{
			version: "0024_drop_tasks_remote_unique",
			path:    "migrations/0024_drop_tasks_remote_unique.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return indexNotExists(tx, "idx_tasks_remote")
			},
		},
		{
			version: "0025_drop_tasks_depends_on",
			path:    "migrations/0025_drop_tasks_depends_on.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				tableGone, err := func() (bool, error) {
					ok, e := tableExists(tx, "task_dependencies")
					return !ok, e
				}()
				if err != nil {
					return false, err
				}
				colGone, err := func() (bool, error) {
					ok, e := columnExists(tx, "tasks", "depends_on_payload")
					return !ok, e
				}()
				if err != nil {
					return false, err
				}
				return tableGone && colGone, nil
			},
		},
	}

	if err := ensureSchemaMigrationsTable(conn); err != nil {
		return err
	}

	applied, err := appliedVersions(conn)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if err := applyMigration(conn, m); err != nil {
			return err
		}
	}
	return nil
}

type migration struct {
	version string
	path    string
	skip    func(*sql.Tx) (bool, error)
}

func ensureSchemaMigrationsTable(conn *sql.DB) error {
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(conn *sql.DB) (map[string]struct{}, error) {
	rows, err := conn.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("list schema_migrations: %w", err)
	}
	defer rows.Close()

	versions := make(map[string]struct{})
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		versions[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}
	return versions, nil
}

func applyMigration(conn *sql.DB, m migration) error {
	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.version, err)
	}
	defer tx.Rollback()

	if m.skip != nil {
		skip, err := m.skip(tx)
		if err != nil {
			return fmt.Errorf("preflight migration %s: %w", m.version, err)
		}
		if skip {
			if err := recordMigration(tx, m.version); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("commit skipped migration %s: %w", m.version, err)
			}
			return nil
		}
	}

	sqlBytes, err := schemaFS.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.version, err)
	}
	if _, err := tx.Exec(string(sqlBytes)); err != nil {
		return fmt.Errorf("exec migration %s: %w", m.version, err)
	}
	if err := recordMigration(tx, m.version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.version, err)
	}
	return nil
}

func recordMigration(tx *sql.Tx, version string) error {
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
		return fmt.Errorf("record migration %s: %w", version, err)
	}
	return nil
}

func columnIsNullable(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, table, column string) (bool, error) {
	rows, err := q.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("table info for %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			typeName  string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("scan table info for %s: %w", table, err)
		}
		if name == column {
			return notNull == 0, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info for %s: %w", table, err)
	}
	return false, nil
}

func columnExists(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, table, column string) (bool, error) {
	rows, err := q.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, fmt.Errorf("table info for %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			typeName  string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &typeName, &notNull, &dfltValue, &pk); err != nil {
			return false, fmt.Errorf("scan table info for %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info for %s: %w", table, err)
	}
	return false, nil
}

func legacySchemaPresent(q interface {
	QueryRow(string, ...any) *sql.Row
}) (bool, error) {
	for _, table := range []string{"projects", "tasks", "jobs"} {
		ok, err := tableExists(q, table)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func tableExists(q interface {
	QueryRow(string, ...any) *sql.Row
}, table string) (bool, error) {
	var count int
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`,
		table,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check table %s: %w", table, err)
	}
	return count > 0, nil
}

func indexNotExists(q interface {
	QueryRow(string, ...any) *sql.Row
}, index string) (bool, error) {
	var count int
	if err := q.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		index,
	).Scan(&count); err != nil {
		return false, fmt.Errorf("check index %s: %w", index, err)
	}
	return count == 0, nil
}
