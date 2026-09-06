package migrate

import (
	"database/sql"
	"embed"
	"fmt"
	"regexp"
	"strconv"
)

//go:embed migrations/*.sql
var schemaFS embed.FS

func Apply(conn *sql.DB) error {
	migrations := allMigrations()

	if err := ensureSchemaMigrationsTable(conn); err != nil {
		return err
	}

	applied, err := appliedVersions(conn)
	if err != nil {
		return err
	}

	// Refuse to start against a database a NEWER binary has already
	// migrated past this binary's own newest known migration, rather than
	// silently ignoring the gap and running against an untested schema
	// shape. Checked before the apply loop so an old binary refuses
	// immediately.
	if err := checkSchemaCeiling(migrations, applied); err != nil {
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

// allMigrations returns the full, ordered migration slice Apply() applies.
// Separated out so tests can apply a PREFIX of the chain (see applyThrough
// in migrate_0045_card_sti_test.go) to set up pre-migration fixture data —
// something Apply() itself, which always runs the whole chain, cannot do.
func allMigrations() []migration {
	return []migration{
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
			version: "0025_drop_tasks_datasource_id",
			path:    "migrations/0025_drop_tasks_datasource_id.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := columnExists(tx, "tasks", "datasource_id")
				return !exists, err
			},
		},
		{
			version: "0026_drop_tasks_depends_on",
			path:    "migrations/0026_drop_tasks_depends_on.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := columnExists(tx, "tasks", "depends_on_payload")
				return !exists, err
			},
		},
		{
			version: "0027_add_jobs_display_name",
			path:    "migrations/0027_add_jobs_display_name.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "jobs", "display_name")
			},
		},
		{
			version: "0028_add_projects_upstream_url",
			path:    "migrations/0028_add_projects_upstream_url.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "projects", "upstream_url")
			},
		},
		{
			version: "0029_drop_worktrees_table",
			path:    "migrations/0029_drop_worktrees_table.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				exists, err := tableExists(tx, "worktrees")
				return !exists, err
			},
		},
		{
			version: "0030_add_workspaces_table",
			path:    "migrations/0030_add_workspaces_table.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "workspaces")
			},
		},
		{
			version: "0031_add_schema_migrations_state",
			path:    "migrations/0031_add_schema_migrations_state.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "schema_migrations", "state")
			},
		},
		{
			version: "0032_add_web_devices_token",
			path:    "migrations/0032_add_web_devices_token.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				nullable, err := columnIsNullable(tx, "web_devices", "cookie_hash")
				if err != nil || !nullable {
					return false, err
				}
				return columnExists(tx, "web_devices", "token_hash")
			},
		},
		{
			version: "0033_add_workspace_default_project_fields",
			path:    "migrations/0033_add_workspace_default_project_fields.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "workspaces", "task_behaviors")
			},
		},
		{
			version: "0034_add_workspaces_services",
			path:    "migrations/0034_add_workspaces_services.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "workspaces", "services")
			},
		},
		{
			version: "0035_add_task_triage",
			path:    "migrations/0035_add_task_triage.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "task_triage")
			},
		},
		{
			version: "0036_add_workspace_watchdog",
			path:    "migrations/0036_add_workspace_watchdog.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "workspace_watchdog")
			},
		},
		{
			version: "0037_scope_task_ref_dedup_by_project",
			path:    "migrations/0037_scope_task_ref_dedup_by_project.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				// Skip ONLY when the new index exists AND the legacy one is
				// gone: skipping just because the new index exists would
				// leave a stale, stricter uniqueness constraint alongside it
				// in a schema-drift/bootstrap scenario, rejecting the very
				// cross-project inserts this migration exists to allow.
				newIndexAbsent, err := indexNotExists(tx, "idx_tasks_ref_parent_project")
				if err != nil {
					return false, err
				}
				oldIndexAbsent, err := indexNotExists(tx, "idx_tasks_ref_parent")
				if err != nil {
					return false, err
				}
				return !newIndexAbsent && oldIndexAbsent, nil
			},
		},
		{
			version: "0038_add_actions_actor",
			path:    "migrations/0038_add_actions_actor.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "actions", "actor")
			},
		},
		{
			version: "0039_add_workspace_egress_port",
			path:    "migrations/0039_add_workspace_egress_port.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "workspace_egress_port")
			},
		},
		{
			// No skip func: the statement is itself idempotent (its NOT EXISTS
			// clause makes a re-run a no-op), and a data migration like this
			// has no schema object whose presence could stand in for "already
			// ran". The normal schema_migrations version guard already
			// prevents a second run on an up-to-date DB.
			version: "0040_backfill_task_triage_rows",
			path:    "migrations/0040_backfill_task_triage_rows.sql",
		},
		{
			version: "0041_add_task_identities",
			path:    "migrations/0041_add_task_identities.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return tableExists(tx, "task_identities")
			},
		},
		{
			// No skip function: this index never existed under any prior
			// code path, so there is no pre-migration-system state to guard
			// against.
			version: "0042_add_actions_created_at_id_index",
			path:    "migrations/0042_add_actions_created_at_id_index.sql",
		},
		{
			// No skip function: a brand-new table with no pre-migration-
			// system state to guard against.
			version: "0043_add_trigger_runs",
			path:    "migrations/0043_add_trigger_runs.sql",
		},
		{
			// No skip function: an ADD COLUMN + backfill UPDATE, same shape
			// as 0040.
			version: "0044_add_task_triage_suggestion_verb",
			path:    "migrations/0044_add_task_triage_suggestion_verb.sql",
		},
		{
			// Rebuilds tasks as a Single Table Inheritance table (a `type`
			// discriminator column plus card-only/execution-only column
			// groups, each NULL on the "other" type's rows, enforced by CHECK
			// constraints); see the SQL file's own header comment for the
			// full step-by-step. No skip function, same as other table-
			// rebuild migrations (0021/0032) — there is no single "already
			// migrated" check safe to probe mid-rebuild.
			version:            "0045_card_sti_migration",
			path:               "migrations/0045_card_sti_migration.sql",
			disableForeignKeys: true,
		},
		{
			// Unlike 0042/0043 (brand-new tables, no skip needed), this DOES
			// probe before re-applying, guarding against a partial-apply-
			// then-rerun (e.g. a daemon crash between the file's two CREATE
			// TABLE statements). Both tables are probed since either could
			// be the one left dangling by a partial apply.
			version: "0046_add_signal_inbox",
			path:    "migrations/0046_add_signal_inbox.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				signalsExist, err := tableExists(tx, "signals")
				if err != nil || !signalsExist {
					return signalsExist, err
				}
				return tableExists(tx, "signal_cursors")
			},
		},
		{
			// Persists `boid task create --idempotency-key` + a partial
			// unique index; see the SQL file's own doc comment for invariants.
			version: "0047_add_tasks_idempotency_key",
			path:    "migrations/0047_add_tasks_idempotency_key.sql",
			skip: func(tx *sql.Tx) (bool, error) {
				return columnExists(tx, "tasks", "idempotency_key")
			},
		},
		{
			// No skip function: a pure data backfill (UPDATE only, no schema
			// change) — the migration runner's own version tracking is what
			// keeps this from re-running, and the UPDATEs are idempotent by
			// construction (a second run matches zero rows) regardless.
			version: "0048_card_verb_rename",
			path:    "migrations/0048_card_verb_rename.sql",
		},
	}
}

type migration struct {
	version string
	path    string
	skip    func(*sql.Tx) (bool, error)
	// disableForeignKeys, when true, toggles `PRAGMA foreign_keys` OFF then
	// back ON around this migration's transaction (see applyMigration).
	// Needed when a migration DROPs a table still referenced by a live FK
	// (modernc.org/sqlite enforces FK constraints even on DROP TABLE of a
	// referenced parent). Must run outside the transaction boundary since
	// `PRAGMA foreign_keys` is a no-op once a transaction is already open.
	disableForeignKeys bool
}

// versionNumberPattern matches the leading zero-padded numeric prefix of a
// versioned schema migration's version string (e.g.
// "0032_add_web_devices_token" -> "0032"). Not every row this package's own
// recordMigrationState writes to schema_migrations has this shape —
// internal/orchestrator/workspace_migration.go directly records a
// non-file, workflow-state marker under the literal version string
// "workspace_db_consolidation" — so parseMigrationVersion below treats a
// non-match as "not a numbered schema migration" rather than an error.
var versionNumberPattern = regexp.MustCompile(`^(\d{4})_`)

// parseMigrationVersion extracts the leading 4-digit numeric prefix from a
// schema_migrations version string. ok is false for a version with no such
// prefix (e.g. "workspace_db_consolidation") — those rows are invisible to
// checkSchemaCeiling by design (see versionNumberPattern's doc comment).
func parseMigrationVersion(version string) (n int, ok bool) {
	m := versionNumberPattern.FindStringSubmatch(version)
	if m == nil {
		return 0, false
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return v, true
}

// checkSchemaCeiling refuses to start if applied (schema_migrations, as
// already recorded on disk) contains any numbered migration version newer
// than the newest version this binary's own migrations slice knows about
// — i.e. the database was migrated by a newer binary.
func checkSchemaCeiling(migrations []migration, applied map[string]struct{}) error {
	var maxKnown int
	for _, m := range migrations {
		if v, ok := parseMigrationVersion(m.version); ok && v > maxKnown {
			maxKnown = v
		}
	}
	for version := range applied {
		v, ok := parseMigrationVersion(version)
		if !ok {
			continue
		}
		if v > maxKnown {
			return fmt.Errorf(
				"database schema is newer than this boid binary knows how to handle "+
					"(found applied migration %q, this binary's newest known migration is %04d_*); "+
					"refusing to start — upgrade the boid binary before opening this database "+
					"(docs/plans/phase6-container-backend.md §PR6/§決定4 schema ceiling check)",
				version, maxKnown)
		}
	}
	return nil
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
	if m.disableForeignKeys {
		// Must run BEFORE conn.Begin() below: `PRAGMA foreign_keys` is a
		// documented no-op once a transaction is already open, so it cannot
		// live inside the migration's own SQL file. Restored unconditionally
		// via defer (including on error) — a failed migration must not leave
		// FK enforcement silently disabled for whatever runs next on this
		// connection (db.go's SetMaxOpenConns(1) makes this the ONE
		// connection every subsequent query on this *db.DB uses).
		if _, err := conn.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
			return fmt.Errorf("disable foreign_keys for migration %s: %w", m.version, err)
		}
		defer func() {
			_, _ = conn.Exec(`PRAGMA foreign_keys=ON`)
		}()
	}

	tx, err := conn.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.version, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed

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

// recordMigration records version as applied with the default terminal
// state ("committed") and an empty input_hash — the shape every ordinary
// file-based migration in the slice above uses. See recordMigrationState for
// the staging/committed two-phase form workspace_db_consolidation needs.
func recordMigration(tx *sql.Tx, version string) error {
	return recordMigrationState(tx, version, "committed", "")
}

// recordMigrationState upserts version's row in schema_migrations with the
// given state and input_hash: INSERT if the version has no row yet, or
// UPDATE state/input_hash/applied_at in place if it does — calling it
// twice for the same version never creates a duplicate row.
//
// Bootstrapping note: this function is also used (via recordMigration) by
// every migration from 0001 through 0030, and on a completely fresh
// database all of those run — in order — before migration 0031 has added
// the state/input_hash columns to schema_migrations. columnExists is
// checked on every call so those early calls fall back to the legacy
// version-only INSERT against the pre-0031 schema, while 0031's own
// recordMigration call (and everything after it) sees the columns already
// added by its own ALTER TABLE (executed earlier in the same transaction)
// and takes the UPSERT path.
func recordMigrationState(tx *sql.Tx, version, state, inputHash string) error {
	hasState, err := columnExists(tx, "schema_migrations", "state")
	if err != nil {
		return fmt.Errorf("record migration %s: check state column: %w", version, err)
	}
	if !hasState {
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		return nil
	}
	if _, err := tx.Exec(`
		INSERT INTO schema_migrations (version, state, input_hash) VALUES (?, ?, ?)
		ON CONFLICT(version) DO UPDATE SET
			state      = excluded.state,
			input_hash = excluded.input_hash,
			applied_at = datetime('now')
	`, version, state, inputHash); err != nil {
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
