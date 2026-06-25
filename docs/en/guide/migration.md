# Migrating from the old schema

## Removed fields

The following `project.yaml` fields have been removed in the new schema:

- Top-level: `kits` / `env` / `host_commands` / `additional_bindings` / `secret_namespace` / `capabilities`
- Behavior-level: `task_behaviors.<name>.kits`

These migrate to **workspace.yaml** (machine-local) or a **legacy kit**.

## Using `boid project migrate <dir>`

```bash
# Dry run (no files are changed)
boid project migrate ~/src/myproject --workspace dev

# Apply
boid project migrate ~/src/myproject --workspace dev --apply

# Handle secret collisions
boid project migrate ~/src/myproject --workspace dev --apply --on-collision skip
```

The migrate command:

1. Detects legacy fields in `project.yaml`.
2. Copies kits to `~/.local/share/boid/kits/` and adds kit refs to `workspace.yaml`.
3. Moves `env` / `host_commands` / `additional_bindings` to `workspace.yaml`.
4. Rewrites `project.yaml` in the new schema (dry run leaves files unchanged).

## `project.local.yaml` removal

`project.local.yaml` has been removed. Its contents move to `workspace.yaml`.
`boid project migrate` handles this automatically.

| Old field | New location |
|---|---|
| `env` | `workspace.yaml` `env` |
| `host_commands` | `workspace.yaml` `host_commands` |
| `additional_bindings` | `workspace.yaml` `additional_bindings` |
| `secret_namespace` | `workspace.yaml` `secret_namespace` |

## The new onboarding flow

Initial setup uses three commands instead of the removed `boid init`.
See `docs/en/guide/onboarding.md` for details.
