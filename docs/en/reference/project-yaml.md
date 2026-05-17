# `project.yaml` reference

Every field of `.boid/project.yaml`, the file that lives at the root of a `boid` project.

This page is the schema reference. For the meaning of the underlying terms, see [Concepts](../guide/concepts.md). For walkthroughs, see [Getting started](../getting-started/).

## Role and location

- Path: `.boid/project.yaml` directly under the project root.
- Role: registers the directory as a `boid` project, declares the kinds of tasks (behaviors) it supports, and the extension packages (kits) the project loads.
- Registration: `boid project add <project-root>` reads the file into `boid`'s database.
- Reload: after editing, run `boid project reload`.

## Minimal example

```yaml
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
```

## Top-level fields

| Key | Type | Required | Role |
|---|---|---|---|
| `id` | string | yes | Unique identifier for this project inside `boid`. Tasks reference it via `project_id`. |
| `name` | string | yes | Display name shown in UIs. |
| `worktree` | bool | `false` | If `true`, **executor** tasks in this project run in their own git worktree on a fresh branch. Supervisor tasks always run readonly in the project root regardless of this flag. |
| `base_branch` | string | repository default | Branch used as the base for executor worktrees. Supports `${TASK_REMOTE_ID}` and `${current_branch}` expansion (see [Dynamic base_branch](#dynamic-base_branch)). |
| `kits` | list of KitRef | no | Kits loaded for this project. |
| `task_behaviors` | map (string → TaskBehavior) | yes | The kinds of tasks this project can produce. |
| `commands` | map (string → CommandSpec) | no | Named commands the sandbox can invoke through `boid exec`. |
| `host_commands` | HostCommands | no | External commands the sandbox is allowed to forward to the host. |
| `additional_bindings` | list of BindMount | no | Extra paths to mount into the sandbox. |
| `env` | map (string → string) | no | Environment variables to set inside the sandbox. |
| `secret_namespace` | string | no | Namespace under which this project's secrets are resolved. |

## `task_behaviors.<name>`

The map key is the behavior's identifier and is what `boid task create` references via `behavior:`. **Only two names are supported**:

| Name | Role |
|---|---|
| `supervisor` | Readonly orchestrator. Reads the request, triages it, creates child executor tasks, monitors them. Never edits files. |
| `executor` | Writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait). |

Each behavior entry has very few knobs:

| Key | Type | Default | Role |
|---|---|---|---|
| `name` | string | the map key | Display label (optional). |
| `traits` | list of string | (empty) | Top-level payload trait names this behavior is allowed to use (e.g. `[artifact]`). |
| `default_instruction` | Instruction | (empty) | A single Instruction template appended to `Task.Instructions` when a task is created. |

### Removed behavior-level fields

The fields below used to live under `task_behaviors.<name>.*`. They have been moved to the project top level (or derived from the behavior name) so that any one project pins one workflow shape.

| Removed field | Where it lives now |
|---|---|
| `readonly` | Derived from the behavior name: `supervisor` ⇒ `true`, `executor` ⇒ `false`. |
| `worktree` | Project-top `worktree:` combined with the behavior name. Supervisor never gets a worktree; executor gets one when project-top `worktree: true`. |
| `base_branch` | Project-top `base_branch:`. |
| `branch_prefix` | Not configurable. Worktree branches are always created under `boid/`. |
| `default_payload` | Removed. Provide payload at task creation time instead. |

Setting any of these inside `task_behaviors.<name>` is a load-time error that points at the new location.

### Dynamic `base_branch`

`base_branch` accepts two interpolation tokens that are resolved per task at dispatch time:

- `${TASK_REMOTE_ID}` — the remote identifier (e.g. a GitHub PR number) the parent supervisor recorded for this task. Used in the "1 Supervisor 1 PR" workflow to give each supervisor session its own integration branch.
- `${current_branch}` — the daemon's current HEAD branch in the project repository at the moment the executor worktree is created.

If `base_branch` is omitted, executor worktrees branch from the daemon's current HEAD branch (i.e. the same behaviour as `${current_branch}`). See [docs/workflows.md](../../workflows.md) for end-to-end examples.

For how `worktree: true` behaves, see [Concepts / Worktree](../guide/concepts.md#worktree).

### `default_instruction`

A single Instruction object. At task creation it is appended to `Task.Instructions` and becomes the active instruction the first time the task enters `executing`.

A `boid task reopen <id> --message "..."` call appends a new Instruction at the end of the array; the last element is what the agent sees, and `agent` / `model` / `interactive` are inherited from the previously active one.

## Shared building blocks

### KitRef

Each entry in a `kits` list is either:

- A string of the form `github.com/<owner>/<repo>/<sub-path>` (e.g. `github.com/novshi-tech/boid-kits/claude-code`), or
- A map:
  ```yaml
  kits:
    - ref: github.com/novshi-tech/boid-kits/claude-code
      as: agent
  ```
  `as` assigns an alias, useful when two kits would otherwise collide on agent name.

`<sub-path>` is optional — if the kit lives at the repository root, omit it.

### HostCommands

By default the sandbox cannot invoke commands on the host. `host_commands` declares an allow-list of what to forward. Two forms are supported.

List form (allow with no further restrictions):

```yaml
host_commands:
  - gh
  - aws
```

Map form (with per-command restrictions):

```yaml
host_commands:
  gh:
    allow: [pr, issue, run]
    deny: ["* delete*"]
    stdin: false
  aws:
    path: /usr/local/bin/aws
    env:
      AWS_REGION: ap-northeast-1
```

Each entry (a `HostCommandSpec`) has:

| Key | Type | Role |
|---|---|---|
| `allow` | list of string | Allowed subcommands or glob patterns (entries containing `*` or `?` are treated as patterns). |
| `deny` | list of string | Patterns that override `allow`. |
| `stdin` | bool | Whether stdin may be passed to this command. |
| `path` | string | Absolute path of the binary, overriding `$PATH` lookup. |
| `env` | map (string → string) | Extra environment variables to set when invoking this command. |

A specialised use: setting `path` to a relative path inside the project or a kit forwards only that exact path to the host (for example `path: e2e/run.sh`).

### BindMount

Each `additional_bindings` entry mounts a path from the host into the sandbox.

```yaml
additional_bindings:
  - source: ${HOME}/.local/share/some-tool
  - source: ${HOME}/.config/some-tool
    mode: rw
  - source: ${HOME}/.netrc
    is_file: true
    optional: true
```

| Key | Type | Default | Role |
|---|---|---|---|
| `source` | string | (required) | Host path. Supports `${VAR}` expansion. |
| `target` | string | same as `source` | Mount path inside the sandbox. |
| `mode` | string | `""` (read-only) | `rw` for read-write; empty string is read-only. |
| `is_file` | bool | `false` | Set to `true` if `source` is a single file. |
| `optional` | bool | `false` | If `true`, skip silently when `source` does not exist on the host. |

### Instruction

The shape of the `default_instruction` value.

```yaml
default_instruction:
  type: execution
  agent: claude-code
  model: sonnet
  message: |
    ...
```

| Key | Type | Role |
|---|---|---|
| `type` | enum | `execution` (the old `rework` / `verification` values are gone). |
| `agent` | string | The kit identifier expected to receive this instruction (e.g. `claude-code`). |
| `name` | string | Optional sub-identifier when several instructions go to the same agent. |
| `message` | string | The instruction text given to the agent. |
| `interactive` | bool | `true` to start the agent in interactive mode (if the kit supports it). |
| `model` | string | Model selector the kit will pass through (e.g. `opus`, `sonnet`). |

### CommandSpec

Entries under `commands` declare named commands the sandbox can call via `boid exec <name>`.

```yaml
commands:
  shell:
    command: [bash]
  test:
    command: [go, test, "./..."]
    readonly: false
```

| Key | Type | Role |
|---|---|---|
| `command` | list of string | The argv to execute. `${VAR}` references are expanded at load time. |
| `readonly` | bool | `true` to force the sandbox into read-only mode for this command alone. |

## Project-local overrides (`.boid/project.local.yaml`)

Alongside `project.yaml`, you can place `.boid/project.local.yaml` to override certain fields locally. The intent is for this file to be `gitignore`d and to hold configuration that should not be shared (a personal `host_commands` extension, for instance).

Supported fields:

```yaml
version: 1
host_commands:
  ...
additional_bindings:
  ...
env:
  ...
secret_namespace: ...
```

`task_behaviors` and `kits` cannot be overridden here.

## Example: a real project

An excerpt from `.boid/project.yaml` in the `boid` repository itself, showing the two behaviors (`supervisor`, `executor`) with `worktree: true` declared at the project top level so each executor task runs in its own git worktree.

```yaml
id: boid
name: boid

# Project-top worktree flag: executor tasks get a per-task worktree.
# Supervisor tasks ignore this flag — they always run readonly in the
# project root.
worktree: true

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/go-dev
  - github.com/novshi-tech/boid-kits/github-cli

host_commands:
  playwright-cli:
    allow: ['*']
  run-e2e:
    path: e2e/run.sh

commands:
  shell:
    command: [bash]

task_behaviors:
  executor:
    name: executor
    default_instruction: { ... }
  supervisor:
    name: Supervisor
    default_instruction: { ... }
```

For a fuller example — and three different workflow shapes built on top of this schema — see [Workflows](../../workflows.md).
