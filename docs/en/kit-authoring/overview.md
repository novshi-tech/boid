# Kit authoring overview

A minimal guide to the on-disk `kit.yaml` file format.

> **Retirement notice (Phase 2.5 PR6/PR7, 2026-07)**: the kit *mechanism* has been retired. `boid kit init` / `list` / `remove` and `boid workspace configure` are gone — there is no CLI left to discover, install, or manage a kit. PR7 went further and removed the `WorkspaceMeta.Kits` field from the code outright — `boid workspace create`/`edit`/`import` (`POST`/`PUT`/`import /api/workspaces`) now reject a body containing `kits:` with a 400. What this page documents is the `kit.yaml` **file format only**, and there are exactly two places left that still read it: (1) `boid workspace assign`'s auto-create convenience path (only when a hand-authored or e2e-fixture workspace shadow yaml still has a legacy `kits: [...]` list — resolved client-side before the workspace is created), and (2) the one-time DB migration at daemon startup (`MigrateWorkspaceYAMLToDB`, for pre-cutover workspace yaml). **For any new configuration, skip authoring a kit and set `host_commands` / `env` / `additional_bindings` directly on a workspace** via `boid workspace create`/`edit`/`import` — see [Onboarding](../guide/onboarding.md). This page (and hand-authoring a `kit.yaml` at all) only matters if you are maintaining an existing kit-referencing workspace via one of those two paths, or reading one someone else wrote.
>
> **Kits do not provide hooks or task behaviors.** That was true even before PR6 — hooks have always been a `project.yaml` concern (`task_behaviors.<name>.hooks`), never something a kit supplies. If you are looking at an old kit.yaml with a `hooks:`, `commands:`, `detect:`, `requires:`, or `provides_agent:` key, see [Fields no longer read](#fields-no-longer-read-pre-kit-init-retirement) below — the current loader ignores all of them.

The definition of "kit" lives in [Concepts](../guide/concepts.md#kit).

## On-disk layout

A kit is a directory that contains a `kit.yaml`. The loader reads nothing else in the directory:

```
my-kit/
└── kit.yaml
```

To ship multiple kits in one repository, place each kit in its own subdirectory at the repo root (the official [boid-kits](https://github.com/novshi-tech/boid-kits) follows this layout, though most of its content predates PR6 and demonstrates fields that are no longer read — see the note below).

## `kit.yaml` fields the current loader reads

```yaml
meta:
  name: My kit
  description: One-line description shown in UIs
  category: workflow            # language / vcs / ci / agent / workflow / utility

host_commands:                  # (optional) commands forwarded out of the sandbox to the host
  gh:
    allow: [pr, issue]
    env:
      GH_REPO: ${boid:repo_slug}  # host commands run in a neutral cwd, so pass repo context via env
    reject:
      - match: "*--body-file*"    # sandbox file paths are not visible on the host
        reason: 'Sandbox file paths are not visible on the host. Use --body "$(cat <file>)" instead.'

additional_bindings:            # (optional) extra mounts into the sandbox
  - source: ${HOME}/.config/my-tool

env:                            # (optional) env vars set in the sandbox
  MY_TOOL_FLAG: "1"
```

`meta` / `host_commands` / `additional_bindings` / `env` are the **only** four top-level keys the current loader (`orchestrator.KitMeta`) understands. Any other key is silently ignored — it is not an error, it just never reaches the sandbox.

The shared building blocks (`HostCommands`, `BindMount`) are exactly the same shape used in `project.yaml` — see the [`project.yaml` reference](../reference/project-yaml.md) for their detailed schema.

### `meta`

The label `boid` UIs use to identify the kit. By convention, `category` is one of `language`, `vcs`, `ci`, `agent`, `workflow`, or `utility`.

### `host_commands` / `additional_bindings` / `env`

Merged into the referencing workspace at materialization time (kit values are defaults; the workspace's own values win on conflict). See [`project.yaml` reference / HostCommands](../reference/project-yaml.md#hostcommands) and [BindMount](../reference/project-yaml.md#bindmount) for the field-level schema.

## Fields no longer read (pre-kit-init-retirement)

These keys appear in kits written before Phase 2.5 PR6 (including most of the reference kits in `boid-kits` today). They are harmless to leave in an existing `kit.yaml` — the loader just ignores them — but do not add them to a new one:

| Field | What it used to do |
|---|---|
| `detect.script` | A POSIX sh script `boid kit init`'s interactive flow ran to decide whether to auto-select this kit for a project. No selection flow exists any more. |
| `requires.commands` | Host commands the kit needed on `PATH`, checked during `boid kit init`. |
| `provides_agent` | Declared which agent name's instructions this kit's hook handled. Moot once kits stopped providing hooks/instruction routing. |
| `hooks` | A hook definition (`id` / `kind: agent` / `agent` / `traits`). Kits never actually own hook *dispatch* any more — hooks are defined in `project.yaml`'s `task_behaviors.<name>.hooks`, which is authoritative; see the [`project.yaml` reference](../reference/project-yaml.md) and the [Hook script protocol reference](../reference/hook-contract.md) for the current (project.yaml-level) hook contract. |
| `commands` | Named commands callable via `boid exec`. Retired in Phase 3-d; use `boid exec -p <project> -- <argv...>` instead. |

## Distribution

There is no `boid kit install` (and never was one — the only kit-facing commands that ever existed, `boid kit init` / `list` / `remove`, are also gone). Place a kit directory containing `kit.yaml` under `~/.local/share/boid/kits/<name>/` by hand (e.g. `git clone` it there, or copy it), then reference `<name>` in a legacy `kits: [...]` list on a hand-authored or e2e-fixture workspace shadow yaml — it is resolved client-side, once, the next time `boid workspace assign` runs its auto-create step against that slug (not read live on every dispatch). Passing `kits:` directly to `boid workspace create/edit/import` was removed in Phase 2.5 PR7.

Conventions if you maintain a kit repository anyway:

- The README should state what the kit does and which host commands it requires.
- If you ship multiple kits in one repo, give each subdirectory its own README.
- Set `meta.category` to match the kit's actual role.

## Related docs

- [Concepts](../guide/concepts.md) — for the meaning of hook / kit / trait.
- [`project.yaml` reference](../reference/project-yaml.md) — the current, authoritative hooks schema (`task_behaviors.<name>.hooks`) and the shared `HostCommands`/`BindMount` building blocks.
- [Onboarding / On the retirement of the kit mechanism](../guide/onboarding.md#on-the-retirement-of-the-kit-mechanism) — what replaced `boid kit init` / `boid workspace configure`.
- [State machine](../guide/state-machine.md) — when hooks fire.
