# Concepts

This page walks through the main concepts that make up `boid`. The rest of the docs assume you have read it.

## Task

The unit of work that `boid` tracks from request to completion. Every task carries the following fields:

- A **status** — what stage the task is in right now. Tasks move through `pending → executing → done`, and end at `aborted` if they fail. The meaning of each state and the transition rules between them are covered in [State machine](state-machine.md).
- A **payload** — a JSON document that accumulates information as the task progresses. Generated artifacts and similar outputs are stored under predefined keys called *traits* (defined below).
- A **behavior** — a label such as `supervisor` (formerly `plan`) or `executor` (formerly `dev`) that says what kind of work this task is. The project's configuration maps each label to a set of extension packages (*kits*), so picking a behavior selects which scripts will fire.
- The **project** the task belongs to.

Tasks are created with `boid task create` and observed with `boid task list`, `boid task show`, `boid task watch`, the TUI, or the Web UI.

## Project

A directory that contains a `.boid/project.yaml` file. The project file declares:

- An `id` (the unique identifier `boid` uses for the project) and a `name` (display name).
- An optional project-top `worktree: true` flag that gives each executor task its own git worktree.
- One or more **task_behaviors** — for each behavior label (typically `supervisor` and `executor`), the list of extension packages (kits) to load and an optional `default_instruction` template. Whether the sandbox is read-only / runs in a worktree is no longer set per behavior; it is derived from the canonical name combined with the project-top flag.
- Optional configuration values passed through to each kit.

You register a project with `boid project add <path>`. Any number of projects can coexist; each task belongs to exactly one of them.

## Behavior

A named entry in the project's `task_behaviors` map representing a kind of task. When you create a task and pick a behavior name, `boid` loads the extension packages bound to that behavior and fires their scripts as the task changes state.

There are **two canonical behavior names**:

- **`supervisor`** (legacy alias: `plan`) — readonly orchestrator. Reads a request, decides what child tasks are needed, creates them, monitors them, integrates results.
- **`executor`** (legacy alias: `dev`) — writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait).

The aliases are translated at load time, so existing `project.yaml` files written before the rename keep working. New projects should use the canonical names.

`boid` runs a single state machine regardless of behavior. Different task shapes come from which hooks and gates a behavior wires in, and from how failures are recovered: either by `reopen`ing the task with a new instruction, or by spawning a fresh task. The harness does not encode a verification loop — failure detection and the recovery plan live in the agent's instruction text.

## Payload and traits

The payload is a JSON document that grows as the task progresses. Only a fixed set of keys is allowed at the top level — these are called **traits** — and each trait specifies who is allowed to write it and what writing it triggers.

| Trait | Written by | What writing it does |
|---|---|---|
| `artifact` | execution scripts | Free-form record of what the task produced (commit, PR URL, changed files, ...). |
| `lifecycle.abort` | `boid` itself | Auto-derived `code` / `message` for an aborted task. |

Subtask creation (the main job of supervisor-style behaviors) is no longer expressed through a payload trait. Hooks and gates call the `boid task create` builtin directly — see the [`/boid-supervisor` SKILL](../../../internal/skills/data/boid-supervisor/SKILL.md) for the typical shape.

Instructions are not a payload trait. They live in the top-level `Task.Instructions` array on the task itself; the last element is the active one, and `boid task reopen <id> --message "..."` appends a new entry.

Scripts update the payload by emitting **payload patches** (JSON merge instructions). The daemon stores each patch in order, so the history of a task can be replayed for debugging.

## Hook, gate, kit, handler

`boid` divides task-related scripts into two kinds — **hook** and **gate** — and uses **handler** as the umbrella term for both. The packaged unit that bundles a set of handlers for reuse is a **kit**.

- **Hook** — a script that runs while the task is in `executing`. Hooks do the substantive work: invoking an AI agent, editing code, running tests. They run inside the sandbox; several hooks bound to the same behavior run in parallel. Hooks only ever run in `executing`.
- **Gate** — a script that fires at a state transition. Use `phase: entry` (just before entering the next state) or `phase: exit` (just before leaving the current one). Gates do host-side work — opening a PR, calling `gh pr merge`, restarting a service — and act as a checkpoint at the boundary.
- **Kit** — a directory holding a `kit.yaml` together with hook and gate scripts and any supporting assets. Once installed, a kit can be referenced from any project's `task_behaviors`. Official packages live in the [boid-kits](https://github.com/novshi-tech/boid-kits) repository.

Handlers communicate with `boid` over a fixed protocol: the task payload arrives on stdin, and a payload patch is expected on stdout.

## Job

A record of a single handler invocation. Each job carries its own status (`running` / `success` / `failed`) and an exit code. "Watching a task" really means watching the jobs attached to that task come and go.

`boid job list --task <id>` and `boid job show <id>` are the primary inspection commands.

## Sandbox

The isolated environment that hooks execute inside. Internally it is built from a Linux mount namespace plus a chroot, and applies these constraints:

- Reads and writes are confined to the worktree (or the project root, for tasks that do not get a worktree — supervisor tasks, and executor tasks in projects that do not set `worktree: true`).
- Outbound network connections are limited to the domains the kit declares.
- Other parts of the host filesystem (your home directory, SSH keys, other projects) are not visible.

This means that even a runaway agent cannot leave the task's working area.

Some commands legitimately need to reach outside the sandbox (for example `git push`, `gh pr merge`, `boid task update`). They are allowed only if the kit explicitly declares them as **host commands**, in which case they run on the host instead of inside the sandbox.

## Worktree

For projects that opt in with project-top `worktree: true`, each **executor** task runs inside a fresh **git worktree** on a new branch. A worktree is a git feature that lets you check out multiple branches of the same repository into separate directories simultaneously, so the task's edits stay in their own directory and do not collide with other tasks. The hook runs inside that worktree, its commits are pushed, and (if needed) a PR is created. Once the task is done, the worktree is cleaned up.

Supervisor tasks never get a worktree — they are readonly and run in the project root regardless of the `worktree:` flag.

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
- The dispatch loop that fires handlers in order.
- The lifecycle of worktrees and sandboxes (creation and cleanup).

Started with `boid start`, stopped with `boid stop`. Most subcommands launch the daemon automatically if it is not already running.

---

Next: [State machine](state-machine.md)
