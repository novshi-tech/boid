# `project.yaml` reference

Every field of `.boid/project.yaml`, the file that lives at the root of a `boid` project.

This page is the schema reference. For the meaning of the underlying terms, see [Concepts](../guide/concepts.md). For walkthroughs, see [Getting started](../getting-started/).

## Role and location

- Path: `.boid/project.yaml` directly under the project root.
- Role: registers the directory as a `boid` project, declares the kinds of tasks (behaviors) it supports, and the extension packages (kits) each behavior loads.
- Registration: `boid project add <project-root>` reads the file into `boid`'s database.
- Reload: after editing, run `boid project reload`.

## Minimal example

```yaml
id: demo
name: Demo
task_behaviors:
  hello:
    name: Hello
    readonly: true
```

## Top-level fields

| Key | Type | Required | Role |
|---|---|---|---|
| `id` | string | yes | Unique identifier for this project inside `boid`. Tasks reference it via `project_id`. |
| `name` | string | yes | Display name shown in UIs. |
| `kits` | list of KitRef | no | Kits loaded for the whole project; available to every behavior. |
| `task_behaviors` | map (string → TaskBehavior) | yes | The kinds of tasks this project can produce. |
| `commands` | map (string → CommandSpec) | no | Named commands the sandbox can invoke through `boid exec`. |
| `host_commands` | HostCommands | no | External commands the sandbox is allowed to forward to the host. |
| `additional_bindings` | list of BindMount | no | Extra paths to mount into the sandbox. |
| `env` | map (string → string) | no | Environment variables to set inside the sandbox. |
| `secret_namespace` | string | no | Namespace under which this project's secrets are resolved. |

## `task_behaviors.<name>`

The map key is the behavior's identifier (e.g. `dev`, `plan`) — what `boid task create` references via `behavior:`. The value carries:

| Key | Type | Default | Role |
|---|---|---|---|
| `name` | string | the map key | Display label (optional). |
| `traits` | list of string | (empty) | Top-level payload trait names this behavior is allowed to use (e.g. `[tasks]`). |
| `readonly` | bool | `false` | If `true`, the sandbox is mounted read-only. |
| `worktree` | bool | `false` | If `true`, each task gets its own git worktree on a fresh branch. |
| `branch_prefix` | string | `boid/` | Prefix used when generating the worktree branch name. |
| `base_branch` | string | repository default | Branch used as the base for the worktree. |
| `default_instruction` | Instruction | (empty) | A single Instruction template appended to `Task.Instructions` when a task is created. |
| `default_payload` | YAML/JSON | (empty) | Initial payload applied to tasks created with this behavior. |
| `kits` | list of KitRef | (empty) | Additional kits loaded only for this behavior. |

For how `worktree: true` behaves, see [Concepts / Worktree](../guide/concepts.md#worktree).

### `default_instruction`

A single Instruction object. At task creation it is appended to `Task.Instructions` and becomes the active instruction the first time the task enters `executing`.

A `boid task reopen <id> --message "..."` call appends a new Instruction at the end of the array; the last element is what the agent sees, and `consumer` / `model` / `interactive` are inherited from the previously active one.

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
  `as` assigns an alias, useful when two kits would otherwise collide on consumer name.

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
  consumer: claude-code
  model: sonnet
  message: |
    ...
```

| Key | Type | Role |
|---|---|---|
| `type` | enum | `execution` (the old `rework` / `verification` values are gone). |
| `consumer` | string | The kit identifier expected to receive this instruction (e.g. `claude-code`). |
| `name` | string | Optional sub-identifier when several instructions go to the same consumer. |
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

An excerpt from `.boid/project.yaml` in the `boid` repository itself, showing three behaviors (`dev`, `plan`, `auto_plan`) with the `dev` behavior using `worktree: true` to run AI-driven development tasks.

```yaml
id: boid
name: boid

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
  claude:
    command: [claude, --permission-mode, bypassPermissions, ...]
  shell:
    command: [bash]

task_behaviors:
  dev:
    name: dev
    worktree: true
    kits:
      - github.com/novshi-tech/boid-kits/github-auto-merge
    default_instruction: { ... }
  plan:
    name: Plan
    readonly: true
    traits: [tasks]
    kits:
      - github.com/novshi-tech/boid-kits/boid-tasks
    default_instruction: { ... }
```

For a fuller example see [4. The GitHub PR-driven dev workflow](../getting-started/04-dev-workflow.md).
