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
| `worktree` | bool | `false` | If `true`, allocates a dedicated git worktree for executor and supervisor tasks. **Root tasks** (`parent_id == ""`) use `base_branch` directly as the worktree HEAD; case 1 (`base_branch` matches the project HEAD) runs in the project root with no worktree. **Child tasks** always get a `boid/<task_id8>` branch worktree. See [Task kinds and worktree HEAD](#task-kinds-and-worktree-head). |
| `base_branch` | string | (see below) | The PR target branch, resolved at task creation and stored in the row. **When omitted**: root tasks expand to the daemon's current HEAD branch (`${current_branch}` equivalent) at creation time — a detached-HEAD repository returns 400. Child tasks inherit the parent's `base_branch`. Supports `${TASK_REMOTE_ID}` and `${current_branch}` expansion (see [Dynamic base_branch](#dynamic-base_branch)). |
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
| `worktree` | Project-top `worktree:` combined with the 3-case `base_branch` classification. Executor gets a worktree when project-top `worktree: true`. Supervisor: no worktree in case 1 (`base_branch` = HEAD or omitted); readonly worktree in cases 2 and 3. |
| `base_branch` | Project-top `base_branch:`. |
| `branch_prefix` | Not configurable. Worktree branches are always created under `boid/`. |
| `default_payload` | Removed. Provide payload at task creation time instead. |

Setting any of these inside `task_behaviors.<name>` is a load-time error that points at the new location.

### Dynamic `base_branch`

`base_branch` supports two interpolation tokens:

- `${TASK_REMOTE_ID}` — the remote identifier (e.g. a GitHub PR number) the parent supervisor recorded for this task. Resolved for both supervisor and executor. Used in the "1 Supervisor 1 PR" workflow ([Workflow 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) to give each supervisor session its own integration branch.
- `${current_branch}` — resolved to the daemon's current HEAD branch in the project repository at task creation time.

**Resolution priority when omitted:**

1. **Child task** (`parent_id` is set): inherits the parent's `base_branch` verbatim. No template expansion is performed.
2. **Root task, base_branch omitted** (`parent_id` is empty): the value is expanded to `${current_branch}` and saved into the task row at creation time. If the repository is in detached-HEAD state, the request returns 400.
3. **Root task, base_branch provided**: template tokens (`${TASK_REMOTE_ID}` / `${current_branch}`) are expanded normally.

See [docs/workflows.md](../../workflows.md) for end-to-end examples (Workflow 3 is the canonical example of a dynamic supervisor `base_branch`).

For how `worktree: true` behaves, see [Concepts / Worktree](../guide/concepts.md#worktree).

### Task kinds and worktree HEAD

When `worktree: true` is set, the HEAD branch and fork point differ by task kind:

| Task kind | HEAD branch | Fork point | Read-only |
|---|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | n/a | sup=true / exec=false |
| **child sup / child exec** | `boid/<task_id8>` | **parent task's HEAD branch** | sup=true / exec=false |

- **Root tasks** (`parent_id == ""`): placed directly on `base_branch`. When `base_branch` matches the project HEAD (case 1), no worktree is created and the task runs in the project root. When they differ (cases 2/3), a dedicated worktree is created with `base_branch` as its HEAD.
- **Child tasks** (have a parent): always get a `boid/<task_id8>` branch worktree. The fork point is the **parent task's HEAD branch** (the parent's `base_branch` if the parent is a root task; `boid/<parent_id8>` if the parent is itself a child task). Only the immediate parent is referenced (1 hop).
- `task.BaseBranch` propagates to all child tasks as the PR target and is passed to executors via the `BOID_BASE_BRANCH` environment variable.

### HEAD branch lock (1 active task per project × HEAD branch)

To prevent two tasks from sharing the same working copy simultaneously, `boid` holds a **`<projectID>:<HEAD branch>`** lock for every executing task:

| Task kind | HEAD branch | Lock key |
|---|---|---|
| root sup / root exec | `task.BaseBranch` | `<projectID>:<baseBranch>` |
| child sup / child exec | `boid/<task_id8>` | `<projectID>:boid/<task_id8>` |

- **Serialised**: two root tasks in the same project with the same `base_branch` queue in FIFO order — the second waits until the first reaches a terminal state.
- **Parallel-safe**: root tasks with different `base_branch` values, root + child combinations, and any two child tasks are all allowed to run simultaneously.
- The lock is held for the full executing lifetime, including while the task is in `awaiting`. It is released only on a terminal transition.
- No validation at task-creation time — the lock is acquired when the task transitions to `executing`.

### Base synchronisation and merge responsibility

`boid` core does not control child task dispatch order or base synchronisation. A sub-supervisor orchestrates its children:

```
A (executor) done → A's PR is merged into base
                         ↓
            sub-sup: git fetch && merge → updates own branch (boid/<subid8>)
                         ↓
            sub-sup dispatches B → B's worktree forks from the updated boid/<subid8>
```

The merge command, timing, and target are the **responsibility of the project instruction**, not of any skill or boid core component. Core's only contribution is passing `BOID_BASE_BRANCH` and `BOID_PARENT_BRANCH` environment variables to executors.

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

# Project-top worktree flag: allocates worktrees by task kind.
# Root task HEAD = base_branch (case 1 → project root, cases 2/3 → worktree).
# Child tasks always get a boid/<id8> worktree.
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
