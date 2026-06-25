# Onboarding

boid setup uses **3 steps**.

## The three commands

| Step | Command | Role |
|---|---|---|
| 1 | `boid kit init` | Generate the kit catalog for this machine |
| 2a | `boid project init [dir]` | Scaffold a new project and register it with the daemon |
| 2b | `boid project add <dir>` | Register an existing project with the daemon |
| 3 | `boid workspace configure <slug>` | Generate workspace configuration (select kits, env, host_commands) |

## Scenarios

### New machine + new project (all 3 steps)

```bash
boid kit init
boid project init ~/src/myproject --workspace dev
boid workspace configure dev
```

### New machine + existing project (all 3 steps)

```bash
boid kit init
boid project add ~/src/myproject --workspace dev
boid workspace configure dev
```

### Existing machine + new project (2 steps)

```bash
boid project init ~/src/newproject --workspace dev
boid workspace configure dev   # may be skippable if workspace already exists
```

### Existing machine + existing project (2 steps)

```bash
boid project add ~/src/myproject --workspace dev
boid workspace configure dev
```

### Adding a project to an existing workspace (1 step)

```bash
boid project add ~/src/another --workspace dev
# kit / env configuration stays the same as the existing workspace
```

## Concepts

- **project**: Work patterns (portable, checked into git). Defined in `.boid/project.yaml`.
- **workspace**: Environment matching (machine-local). `workspace.yaml` selects which kits and env vars to use.
- **kit**: Tool supply (globally shared). Provides `host_commands`, `env`, and `additional_bindings`.

## Migrating from old `boid init`

The old `boid init` has been removed. Use the 3-step flow above instead.

If your `project.yaml` contains legacy fields (`kits`, `env`, `host_commands`, `capabilities`, etc.),
run `boid project migrate <dir>` to convert automatically.
See `docs/en/guide/migration.md` for details.
