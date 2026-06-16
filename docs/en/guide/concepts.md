# Concepts

This page walks through the main concepts that make up `boid`. The rest of the docs assume you have read it.

## Task

The unit of work that `boid` tracks from request to completion. Every task carries the following fields:

- A **status** — what stage the task is in right now. Tasks move through `pending → executing → done`, and end at `aborted` if they fail. The meaning of each state and the transition rules between them are covered in [State machine](state-machine.md).
- A **payload** — a JSON document that accumulates information as the task progresses. Outputs that execution scripts leave behind are stored under predefined keys called *traits* (defined below).
- A **behavior** — either `supervisor` or `executor`. It says whether the task is the orchestrator or the implementer, and it also determines whether the sandbox is read-only and whether a worktree is allocated.
- The **project** the task belongs to.

Tasks are created with `boid task create` and observed with `boid task list`, `boid task show`, `boid task watch`, the TUI, or the Web UI.

## Project

A directory that contains a `.boid/project.yaml` file. The project file declares:

- An `id` (the unique identifier `boid` uses for the project) and a `name` (display name).
- An optional project-top `worktree: true` flag that gives each executor task its own git worktree.
- The list of **kits** the project uses (`kits:`).
- One or more **task_behaviors** — for `supervisor` and/or `executor`, an optional `default_instruction` template. Whether the sandbox is read-only / runs in a worktree is not set per behavior; it is derived from the behavior name combined with the project-top flag.

You register a project with `boid project add <path>`. Any number of projects can coexist; each task belongs to exactly one of them.

## Workspace

A label for grouping projects. You might bucket projects as "personal", "work", and "OSS" so the Web UI and TUI can filter the views by group. Workspaces are not declared in `project.yaml`; they are assigned with `boid workspace assign <project> <workspace-id>` (and removed with `boid workspace clear`). A project can belong to at most one workspace.

- `boid workspace list` lists the configured workspaces.
- `boid workspace show <id>` lists the projects in a workspace along with their recent tasks.

Workspaces are purely classification metadata — they do not affect sandbox configuration or hook execution.

## Behavior

A `task_behaviors` entry, naming one kind of task the project supports. When you create a task and pick a behavior name, `boid` decides the isolation level for the task and loads the hooks bound to it, then fires them while the task is in `executing`.

**Only two names are supported**:

- **`supervisor`** — readonly orchestrator. Reads a request, decides what child tasks are needed, creates them, monitors them, integrates results.
- **`executor`** — writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait).

`boid` runs a single state machine regardless of behavior. Different task shapes come from which hooks a behavior wires in, and from how failures are recovered: either by `reopen`ing the task with a new instruction, or by spawning a fresh task. The harness does not encode a verification loop — failure detection and the recovery plan live in the agent's instruction text.

## Payload and traits

The payload is a JSON document that grows as the task progresses. Only a fixed set of keys is allowed at the top level — these are called **traits**.

Today the only trait hooks can write is **`artifact`**: a free-form map where implementation-style tasks record what they produced (commits, PR URLs, changed files, and so on).

You may also see fields like `lifecycle.abort` on the payload, but those are virtual — `boid` derives them from task history at evaluation time and they are never actually stored. See the [Payload trait reference](../reference/traits.md) for details.

Instructions are not a payload trait. They live in the top-level `Task.Instructions` array on the task itself; the last element is the active one, and `boid task reopen <id> --message "..."` appends a new entry.

Scripts update the payload by emitting **payload patches** (JSON merge instructions). The daemon stores each patch in order, so the history of a task can be replayed for debugging.

## Hook

A **hook** is a script that runs while the task is in `executing`. Hooks do the substantive work: invoking an AI agent, editing code, running tests, opening a PR. They run inside the sandbox, and several hooks bound to the same behavior run in parallel.

Hooks communicate with `boid` over a fixed protocol: the task payload arrives on stdin, and a payload patch is expected on stdout (see the [hook script protocol](../reference/hook-contract.md) for details).

## Kit

A **kit** is the distribution unit that bundles whatever a project needs to run work inside the sandbox. A single kit may package any of:

- **hooks** — the scripts described above that run during `executing`.
- **commands** — named commands invokable through `boid exec` from inside the sandbox.
- **host_commands** — the allow-list of commands the sandbox may forward to the host.
- **additional_bindings** — extra paths to mount into the sandbox.
- **env** — environment variables set inside the sandbox.

On disk a kit is a directory holding a `kit.yaml` alongside the relevant scripts. Once installed, a kit can be referenced from any project's `kits:` field. Official packages live in the [boid-kits](https://github.com/novshi-tech/boid-kits) repository; see the [kit authoring overview](../kit-authoring/overview.md) for the on-disk layout and the full field reference.

## Job

A record of a single hook invocation. Each job carries its own status (`running` / `success` / `failed`) and an exit code. "Watching a task" really means watching the jobs attached to that task come and go.

`boid job list --task <id>` and `boid job show <id>` are the primary inspection commands.

## Sandbox

The isolated environment that hooks execute inside. Internally it is built from a Linux mount namespace plus a chroot, and applies these constraints:

- Reads and writes are confined to the worktree (or the project root, for tasks that do not get a worktree — supervisor tasks, and executor tasks in projects that do not set `worktree: true`).
- Outbound network connections are limited to a built-in allowlist (`defaultAllowedDomains` in `cmd/start.go`) merged with any extra entries in `sandbox.allowed_domains` from `~/.config/boid/config.yaml`. There is no per-kit domain declaration — the allowlist is global.
- Other parts of the host filesystem (your home directory, SSH keys, other projects) are not visible.

This means that even a runaway agent cannot leave the task's working area.

Some commands legitimately need to reach outside the sandbox (for example `git push`, `gh pr merge`, `boid task update`). They are allowed only if the kit explicitly declares them as **host commands**, in which case they run on the host instead of inside the sandbox.

## Worktree

For projects that opt in with project-top `worktree: true`, **executor and supervisor** tasks receive dedicated **git worktrees**. A worktree is a git feature that lets you check out multiple branches of the same repository into separate directories simultaneously, so changes stay isolated per task.

Worktree allocation varies by task kind:

| Task kind | HEAD branch | Fork point | Read-only |
|---|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | n/a | sup=true / exec=false |
| **child sup / child exec** | `boid/<task_id8>` | **parent task's HEAD branch** | sup=true / exec=false |

- **Root tasks** (no parent): if `base_branch` matches the project's current HEAD (case 1), no worktree is allocated and the task runs in the project root. If they differ (cases 2/3), a dedicated worktree is created with `base_branch` as its HEAD.
- **Child tasks** (have a parent): always receive a `boid/<task_id8>` branch worktree. The fork point is the **parent task's HEAD branch** — only the immediate parent is referenced (1 hop).
- `base_branch` propagates to all child tasks as the PR target and is passed to executors as the `BOID_BASE_BRANCH` environment variable.

The hook runs inside the worktree, its commits are pushed, and (if needed) a PR is created. Once the task is done, the worktree is cleaned up. Within the same project, tasks that share the same HEAD branch are serialised in FIFO order. See [`project.yaml` reference / HEAD branch lock](../reference/project-yaml.md#head-branch-lock-1-active-task-per-project--head-branch) for details.

## Action

A discrete event that triggers a manual state transition. Examples:

- `start` — advance the task from `pending` to `executing`.
- `reopen` — return a `done` task to `executing`, appending a new instruction to `Task.Instructions` (`--message "..."`).
- `abort` — force the task into `aborted` from any non-terminal state.

Send actions with `boid action send --task <id> --type <action>`, or issue them from the TUI / Web UI.

## Daemon

The long-running `boid` server process. It owns:

- A UNIX socket for the CLI and an HTTP listener for the Web UI.
- Exclusive access to the SQLite database.
- The dispatch loop that fires hooks in order.
- The lifecycle of worktrees and sandboxes (creation and cleanup).

Started with `boid start`, stopped with `boid stop`. Most subcommands launch the daemon automatically if it is not already running.

---

Next: [State machine](state-machine.md)
