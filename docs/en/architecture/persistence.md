# Persistence layer

What lives in `boid`'s SQLite database, and which table is responsible for what. Companion to the [Architecture overview](overview.md).

Aimed at contributors who touch the database — adding migrations, indexing for new queries, debugging schema-shaped bugs.

## Overview

- Implementation: [`modernc.org/sqlite`](https://gitlab.com/cznic/sqlite) (a pure-Go SQLite, no cgo).
- Database file: `~/.local/share/boid/boid.db` (under `XDG_DATA_HOME`).
- Open / close: `internal/server.New` opens it and the daemon holds it for its lifetime; closed when the daemon exits.
- Concurrency: the `boid` daemon owns the database exclusively. Multiple daemons are not supported (the UNIX socket is single-bind).

## Tables at a glance

The full schema lives in [`internal/db/migrate/migrations/`](https://github.com/novshi-tech/boid/tree/main/internal/db/migrate/migrations).

| Table | Role | One row = |
|---|---|---|
| `projects` | Registered projects | One project |
| `project_workspaces` | Project ↔ workspace association | One row per project |
| `tasks` | The task itself (the largest table) | One task |
| `actions` | Audit log of state transitions | One action |
| `jobs` | Handler execution records | One handler run |
| `worktrees` | Git worktrees created for tasks | One worktree |
| `secrets` | Encrypted secret values | One namespace × key |
| `task_dependencies` | Edges between dependent tasks | One dependency |
| `web_devices` | Paired Web UI devices | One device |
| `web_pairing_codes` | Issued pairing codes | One code |

## `tasks`

The main table. The persisted form of the task domain object discussed in [Concepts](../guide/concepts.md#task).

Key columns:

| Column | Type | Role |
|---|---|---|
| `id` | TEXT PK | Task ID (UUID). |
| `project_id` | TEXT FK → projects.id | Owning project. |
| `remote_id` | TEXT | Mapping to an external issue tracker (optional). |
| `title` / `description` | TEXT | Display fields. |
| `status` | TEXT | `pending` / `executing` / `done` / `aborted` (legacy `verifying` / `reworking` rows were force-aborted by migration 0022). |
| `behavior` | TEXT | The behavior name. |
| `payload` | TEXT (JSON) | The current full payload. |
| `instructions` | TEXT (JSON) | An array of `Instruction`s; the last element is the active one and `reopen` appends to it. |
| `auto_start` | BOOLEAN | Whether to start automatically on create. |
| `traits` | TEXT (JSON array) | Trait names declared by the behavior. |
| `readonly` / `worktree` | BOOLEAN | Sandbox mode flags. |
| `branch_prefix` / `base_branch` | TEXT | Worktree settings. |
| `depends_on_payload` | TEXT | Condition on a `depends_on` task's payload (JSON-Path-ish). |
| `ref` / `parent_id` | TEXT | Optional parent reference. |
| `created_at` / `updated_at` | DATETIME | Timestamps. |

**JSON columns**:

- `payload` — A JSON document containing the traits (`artifact`, ...). See [Payload trait reference](../reference/traits.md).
- `instructions` — An array of `Instruction` objects. The last element is the active one; `reopen` appends to it.
- `traits` — A JSON array of trait names this task uses, derived from the behavior.

A partial index on `remote_id` is present. There is also a partial index on `parent_id` and a unique partial index on `(parent_id, ref)` to prevent collisions in the parent-child reference scheme.

## `actions`

An append-only audit log of actions (`start` / `done` / `abort` / ...) and the resulting state transitions.

| Column | Type | Role |
|---|---|---|
| `id` | TEXT PK | Action ID. |
| `task_id` | TEXT FK → tasks.id | Target task. |
| `type` | TEXT | Action type. |
| `payload` | TEXT (JSON) | Parameters. |
| `from_status` / `to_status` | TEXT | The status before and after the transition. |
| `created_at` | DATETIME | When the action was issued. |

`actions` is append-only; we do not update or delete rows. It is the source data for `boid task show`'s timeline and for the history views in the TUI / Web UI.

## `jobs`

The execution record of a handler (hook or gate) run.

| Column | Type | Role |
|---|---|---|
| `id` | TEXT PK | Job ID. |
| `task_id` | TEXT FK → tasks.id (NULLABLE) | Related task; NULL for standalone runs through `boid exec`. |
| `project_id` | TEXT FK → projects.id | Project. |
| `handler_id` | TEXT | Hook / gate ID. |
| `role` | TEXT | `hook` or `gate`. |
| `runtime_id` | TEXT | The runtime ID assigned by the dispatcher. |
| `interactive` / `tty` | INTEGER (bool) | PTY connection flags. |
| `status` | TEXT | `running` / `success` / `failed`. |
| `exit_code` | INTEGER | The process exit code. |
| `output` | TEXT | Full stderr (the log). |
| `execution_state` | TEXT | Auxiliary runtime state. |
| `created_at` / `updated_at` | DATETIME | Timestamps. |

The `output` column holds the handler's full stderr; stdout is consumed by the payload-patch parser and is not appended here. Handlers that log heavily can grow the database, so prefer to throttle on the handler side.

`task_id` is nullable because runs initiated via `boid exec` (commands not bound to a task) are also recorded here.

## `worktrees`

Metadata for the per-task git worktrees created for executor tasks when the project has `worktree: true` at the top level.

| Column | Type | Role |
|---|---|---|
| `id` | TEXT PK | Worktree ID. |
| `task_id` | TEXT FK → tasks.id (UNIQUE) | One worktree per task. |
| `project_id` | TEXT FK → projects.id | Project. |
| `path` | TEXT | Path on the host. |
| `branch` | TEXT | Branch name. |
| `base_branch` | TEXT | Base branch. |
| `created_at` / `cleaned_at` | DATETIME | Created and (when removed) cleaned-up timestamps. |

`cleaned_at IS NULL` means the worktree is currently live. We retain the row after cleanup so the audit trail stays intact.

## `secrets`

Encrypted storage for API tokens and similar values. The encryption key lives at `~/.local/share/boid/secret.key` and is loaded by the daemon at startup.

| Column | Type | Role |
|---|---|---|
| `id` | TEXT PK | Secret ID. |
| `namespace` | TEXT | Namespace (e.g. per-project), default `default`. |
| `key` | TEXT | Secret name. |
| `value_encrypted` | BLOB | Encrypted value. |
| `created_at` / `updated_at` | DATETIME | Timestamps. |

`(namespace, key)` is unique. `boid secret set` / `boid secret get` are the front-door commands.

## `task_dependencies`

Many-to-many edges between dependent tasks.

| Column | Type | Role |
|---|---|---|
| `task_id` | TEXT FK → tasks.id | The waiter. |
| `depends_on` | TEXT FK → tasks.id | The waited-for. |

PK is `(task_id, depends_on)`. Combined with the `task.depends_on_payload` JSON column, the daemon checks "the prerequisite task is `done` AND its payload satisfies this condition".

## `web_devices` / `web_pairing_codes`

Tables for Web UI device authentication (see [Web UI](../guide/web-ui.md)).

```sql
web_devices(id, label, cookie_hash, created_at, last_seen_at, revoked_at)
web_pairing_codes(code_hash, label, created_at, expires_at, consumed_at)
```

Cookie values and pairing codes are stored as hashes; the plaintext never lands in the database.

## Migrations

Numbered SQL files live under `internal/db/migrate/migrations/`:

```
migrations/
├── 0001_initial.sql
├── 0002_add_jobs_handler_id.sql
├── ...
├── 0021_jobs_nullable_task_id.sql
└── 0022_drop_verifying_reworking.sql
```

Notes:

- There is **no** `schema_migrations` table. Each migration's `skip` function inspects the schema directly with helpers like `columnExists` and `legacySchemaPresent`; if the change is already applied, it is skipped.
- Migrations run at daemon startup (`server.New` → `migrate.Apply`). Each runs in its own transaction.
- SQLite cannot drop arbitrary columns through `ALTER TABLE` in older versions, so destructive changes use the `<table>_new` pattern: create the new table, `INSERT ... SELECT`, drop the old one, `RENAME`. Examples: `0005_add_secrets_namespace.sql`, `0021_jobs_nullable_task_id.sql`.
- `ALTER TABLE ... DROP COLUMN` is supported on SQLite 3.35+ and works with the bundled pure-Go SQLite (e.g. `0011_drop_tasks_start_gate.sql`).

Conventions for adding a migration:

1. Name the file `NNNN_short_description.sql` (4-digit serial).
2. Use plain `ALTER TABLE ... ADD COLUMN` for additive changes; for drops or type changes, fall back to the `_new` pattern.
3. Register it in the `migrations` list in `migrate.go` with a `skip` function that detects whether the change is already applied.
4. Use `NOT NULL DEFAULT ''` (or similar) for new columns so existing rows survive the migration.

## Related documents

- [Architecture overview](overview.md) — how `internal/server` wires the database in.
- [Payload trait reference](../reference/traits.md) — what each trait stored in the `tasks` table's `payload` column means.
- [`project.yaml` reference](../reference/project-yaml.md) — where the `tasks` table's `behavior` / `traits` columns come from.
- [Concepts / Daemon](../guide/concepts.md#daemon) — why the daemon owns the database.
