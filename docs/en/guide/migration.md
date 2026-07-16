# Migrating from the old schema

## Removed fields

The following `project.yaml` fields have been removed in the new schema:

- Top-level: `kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`
- Behavior-level: `task_behaviors.<name>.kits`

These are distributed to a **workspace** (machine-local; the database is authoritative, `~/.config/boid/workspaces/<slug>.yaml` is a shadow copy) or, in one case, a **legacy kit** generated as part of the migration. See "What `boid project migrate` converts" below for exactly where each field lands.

## Using `boid project migrate <dir>`

```bash
# Dry run (no files are changed)
boid project migrate ~/src/myproject --workspace dev

# Apply
boid project migrate ~/src/myproject --workspace dev --apply

# Handle secret collisions
boid project migrate ~/src/myproject --workspace dev --apply --on-collision skip
```

### What `boid project migrate` converts

1. Detects the fields being removed from `project.yaml` (`kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`, and behavior-level `task_behaviors.<name>.kits`).
2. **(Changed in Phase 2.5 PR7)** Existing `kits:` refs (e.g. `github.com/.../foo`) are only name-validated (`ValidKitName`) and shown as an informational note in migrate's dry-run/apply output. `WorkspaceMeta.Kits` itself was removed, so nothing is carried into the workspace any more — if such a kit used to supply host_commands/env/additional_bindings, add them to workspace.yaml by hand after migrating.
3. If `host_commands` and/or `additional_bindings` is non-empty, their contents are bundled into a **newly generated legacy kit**, written to `~/.local/share/boid/kits/legacy-<slug>/kit.yaml`. **(Changed in Phase 2.5 PR7)** That kit's host_commands names and additional_bindings are now folded **directly** into the workspace's own `host_commands:` / `additional_bindings:` fields, rather than via a kit reference (this data is `project.yaml`'s own fields, already fully known — no directory lookup is needed to resolve it). The legacy kit's `host_commands` definitions are also merged into the daemon-wide registry `~/.config/boid/host_commands.yaml` (so `workspace.host_commands`'s name references resolve), and a reachable daemon is told to reload it.
4. `env` is merged directly into the workspace's `env` (on a key collision, the new — i.e. `project.yaml`-sourced — value wins).
5. `capabilities.docker` is merged directly into the workspace's `capabilities.docker` (overwritten if `project.yaml` set it).
6. If `secret_namespace` was set, secrets are copied from the old namespace into the new namespace (= the workspace's own slug). **This does not create a `secret_namespace` field on the workspace** — a workspace was designed from the start to use its slug as the secret namespace; migration only copies the secret values.
7. Rewrites `project.yaml` in the new schema (dry run leaves files unchanged).

### Applying the change to a live workspace (when the daemon is running)

`--apply` does not only write the converted result to the local shadow yaml (`~/.config/boid/workspaces/<slug>.yaml`, a reviewable artifact the daemon never re-reads) — it also **attempts to apply it to the running daemon's database** (`pushMigratedWorkspaceToDaemon`):

- If the workspace slug has no row in the daemon yet: it is created with `POST /api/workspaces`.
- If the slug already exists: its current content is fetched with `GET /api/workspaces/<slug>`, merged with the fields this migration generated, and written back with `PUT /api/workspaces/<slug>` (`If-Match: <revision>`) (`mergeLegacyFieldsIntoWorkspace`). **The merge precedence favors the migration side** (the values derived from `project.yaml`): `env` entries from the migration overwrite the workspace's existing value on a matching key, and `capabilities.docker` is overwritten when `project.yaml` set it. When this migration generated a legacy kit, its `host_commands` (reference names) are unioned in (existing entries are never dropped), and its `additional_bindings` overwrite the workspace's existing entry on a matching Source. Every other existing field is carried over untouched.
- A `412 Precondition Failed` (revision mismatch — concurrent edit) re-fetches, re-merges, and retries, up to 3 times.
- If the daemon is unreachable, or the retries are exhausted without resolving, the change only lands in the shadow yaml. The command's output explains how to apply it by hand (`boid workspace import <file> --slug <slug>` or `boid workspace edit <slug> --from-file <file>`) — follow that guidance, since **`project.yaml` itself has already been rewritten regardless of whether the workspace push succeeded** (unless this was a dry run).

## `project.local.yaml` removal

`project.local.yaml` has been removed. Its contents move to a workspace.
`boid project migrate` handles this automatically.

| Old field | New location |
|---|---|
| `env` | Merged directly into the workspace's `env` |
| `host_commands` | Appended directly to the workspace's `host_commands:` (reference names) + merged into the daemon-wide registry `~/.config/boid/host_commands.yaml` (actual definitions), via a generated legacy kit when non-empty (Phase 2.5 PR7) |
| `additional_bindings` | Appended directly to the workspace's `additional_bindings:` (Phase 2.5 PR7 — no kit-directory lookup needed) |
| `secret_namespace` | Not a same-named field on the workspace — **the workspace's own slug becomes the new secret namespace**. Migration only copies secret values from the old namespace into the new one (= the workspace slug) |

## Workspace DB migration (Phase 2.5, automatic — no action needed)

Separate from the `project.yaml` schema migration this page documents (`boid project migrate`), Phase 2.5 (workspace DB consolidation) introduced a migration that moves a workspace's authority from yaml files to the database (the `workspaces` table). This one **runs automatically at daemon startup** — no manual step is needed:

- Reads existing `~/.config/boid/workspaces/<slug>.yaml` files and writes each into the `workspaces` table, once (`orchestrator.MigrateWorkspaceYAMLToDB`).
- Idempotent — a no-op on every subsequent daemon start, tracked as `workspace_db_consolidation` in the `schema_migrations` table.
- Crash-safe: if the daemon dies mid-migration, the next start either resumes (same inputs) or aborts with an error (inputs changed since the interrupted attempt — a deliberate fail-closed choice over silently reconciling).
- Creates the `default` workspace as part of the same pass if it doesn't already exist.

After this migration, the `workspaces` table is the sole authority; `~/.config/boid/workspaces/*.yaml` files remain only as a shadow copy for `boid workspace export`. See `docs/plans/workspace-db-consolidation.md` for details.

## On the retirement of the kit mechanism (Phase 2.5 PR6)

`boid kit init` (generating a machine-wide kit catalog), `boid workspace configure` (an LLM conversation that generated workspace configuration), and the surrounding commands (`boid kit list` / `boid kit remove`) were removed in Phase 2.5 PR6 (2026-07).

The `boid project migrate` conversion logic described above (generating a kit, wiring it into `workspace.yaml`) is unaffected by PR6 — what changed is that there is no longer a CLI to **inspect or remove** the generated `kit.yaml` afterward. Edit or delete `~/.local/share/boid/kits/<name>/kit.yaml` by hand instead.

To set up a workspace's contents from scratch, use `boid workspace create` / `edit` / `import` (yaml, passed directly) instead of the retired `boid workspace configure`. See [Onboarding](../guide/onboarding.md) for details.

## Final retirement of the kit mechanism (Phase 2.5 PR7)

The `WorkspaceMeta.Kits` field (workspace.yaml's `kits:`) was removed from the code outright in Phase 2.5 PR7 (2026-07). Consequences:

- `POST` / `PUT` / `import /api/workspaces` now reject a request body containing a `kits:` key with 400 (`unknown field kits`).
- `boid project migrate` still name-validates and displays a legacy project.yaml's `kits:` refs informationally, but no longer resolves them into the workspace at all (see "What `boid project migrate` converts" above). The legacy kit it generates from `host_commands`/`additional_bindings` is unaffected — its content is still folded in, just directly rather than via a kit reference.
- The one remaining caller that still honors a legacy `kits:` list is `boid workspace assign`'s auto-create convenience path (for a hand-authored or e2e-fixture workspace shadow yaml) — it resolves the reference client-side against the installed kits directory before submitting an already-materialized (kits-free) body.
- **(Correction)** The shadow yaml files kept for rollback (`~/.config/boid/workspaces/*.yaml`) and the `~/.local/share/boid/kits/` directory are *not* fully unread once the DB is authoritative — two dependencies remain, so the earlier "safe to delete any time, no effect on the daemon" guidance above is retracted:
  - Shadow yaml: `boid workspace assign`'s auto-create path (the bullet just above) reads `~/.config/boid/workspaces/<slug>.yaml` whenever the target slug has no DB row yet. If you still plan to `assign` a not-yet-assigned slug going forward, do not delete that slug's shadow yaml.
  - `~/.local/share/boid/kits/`: the daemon startup preflight (`buildProjectStore` in `internal/server/wire.go`) rebuilds the aggregated `~/.config/boid/host_commands.yaml` from the kit.yaml files under this directory as a self-healing fallback whenever that config is missing for any reason (`boid host-commands reload` itself does *not* do this rebuild — it only re-reads the existing file). If you're confident `host_commands.yaml` will never go missing, the impact is small, but it isn't guaranteed.
  - Only delete these once you've confirmed both conditions above: you will not use the auto-create path (`workspace assign` against an unassigned slug) again, and you don't anticipate `host_commands.yaml` ever being lost.

## On the new onboarding flow

Initial setup registers a project, then optionally configures a workspace — 2 steps instead of the removed `boid init` (effectively 1 step when the `default` workspace is good enough).
See `docs/en/guide/onboarding.md` for details.
