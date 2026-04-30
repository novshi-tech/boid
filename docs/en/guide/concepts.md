# Concepts

This page walks through the main concepts that make up `boid`. The rest of the docs assume you have read it.

## Task

The unit of work that `boid` tracks from request to completion. Every task carries the following fields:

- A **status** — what stage the task is in right now. Tasks move through `pending → executing → verifying → reworking → done`, and end at `aborted` if they fail. The meaning of each state and the transition rules between them are covered in [State machine](state-machine.md).
- A **payload** — a JSON document that accumulates information as the task progresses. The original request, generated artifacts, review findings, and so on are stored under predefined keys called *traits* (defined below).
- A **behavior** — a label such as `dev` or `plan` that says what kind of work this task is. The project's configuration maps each label to a set of extension packages (*kits*), so picking a behavior selects which scripts will fire.
- The **project** the task belongs to.

Tasks are created with `boid task create` and observed with `boid task list`, `boid task show`, `boid task watch`, the TUI, or the Web UI.

## Project

A directory that contains a `.boid/project.yaml` file. The project file declares:

- An `id` (the unique identifier `boid` uses for the project) and a `name` (display name).
- One or more **task_behaviors** — for each behavior label, settings like whether the sandbox should be read-only or run inside a git worktree, and the list of extension packages (kits) to load.
- Optional configuration values passed through to each kit.

You register a project with `boid project add <path>`. Any number of projects can coexist; each task belongs to exactly one of them.

## Behavior

A named entry in the project's `task_behaviors` map representing a kind of task. When you create a task and pick a behavior name (e.g. `dev`, `plan`), `boid` loads the extension packages bound to that behavior and fires their scripts as the task changes state.

`boid` runs a single state machine regardless of behavior. Phrases like "one-shot" and "feedback-loop" are not two different machines; they describe how the handlers wired to a behavior interact with that single machine.

- **One-shot-style** — no verifying-state handler, or one that never writes findings. The task passes through `executing → verifying → done` once. Suited to short, one-off jobs.
- **Feedback-loop-style** — a verifying-state handler that may write findings. When it does, the task drops into `reworking` and a rework-style handler keeps cycling until every finding is resolved. Suited to changes that go through PR review or CI.

## Payload and traits

The payload is a JSON document that grows as the task progresses. Only a fixed set of keys is allowed at the top level — these are called **traits** — and each trait specifies who is allowed to write it and what writing it triggers.

| Trait | Written by | What writing it does |
|---|---|---|
| `instructions` | task creator / extension package | Holds work instructions for downstream scripts. |
| `artifact` | execution scripts | Signals that the work in `executing` is done. Writing it advances the task to `verifying`. |
| `tasks` | plan-style scripts | Plays the same role as `artifact` for planning-style tasks. |
| `verification.findings` | review-style scripts | The list of issues the reviewer found. Open findings push the task into `reworking`; once they are resolved the task returns to `verifying`. |
| `lifecycle` | `boid` itself | Auto-derived counters and flags such as the rework count and an "already executed" flag. |

Scripts update the payload by emitting **payload patches** (JSON merge instructions). The daemon stores each patch in order, so the history of a task can be replayed for debugging.

## Hook, gate, kit, handler

`boid` divides task-related scripts into two kinds — **hook** and **gate** — and uses **handler** as the umbrella term for both. The packaged unit that bundles a set of handlers for reuse is a **kit**.

- **Hook** — a script that runs while the task is in a particular state (e.g. `executing`). Hooks do the substantive work: invoking an AI agent, editing code, running tests. They run inside the sandbox, and several hooks bound to the same state run in parallel.
- **Gate** — a script that fires at a state transition (entry or exit). Gates do work on the host machine itself: opening a PR, calling `gh pr merge`, restarting a service. Gates do not go through the sandbox and act as a checkpoint at the boundary between two states.
- **Kit** — a directory holding a `kit.yaml` together with hook and gate scripts and any supporting assets. Once installed, a kit can be referenced from any project's `task_behaviors`. Official packages live in the [boid-kits](https://github.com/novshi-tech/boid-kits) repository.

Handlers communicate with `boid` over a fixed protocol: the task payload arrives on stdin, and a payload patch is expected on stdout.

## Job

A record of a single handler invocation. Each job carries its own status (`running` / `success` / `failed`) and an exit code. "Watching a task" really means watching the jobs attached to that task come and go.

`boid job list --task <id>` and `boid job show <id>` are the primary inspection commands.

## Sandbox

The isolated environment that hooks execute inside. Internally it is built from a Linux mount namespace plus a chroot, and applies these constraints:

- Reads and writes are confined to the worktree (or the project root, for behaviors that do not use a worktree).
- Outbound network connections are limited to the domains the kit declares.
- Other parts of the host filesystem (your home directory, SSH keys, other projects) are not visible.

This means that even a runaway agent cannot leave the task's working area.

Some commands legitimately need to reach outside the sandbox (for example `git push`, `gh pr merge`, `boid task update`). They are allowed only if the kit explicitly declares them as **host commands**, in which case they run on the host instead of inside the sandbox.

## Worktree

For behaviors that change a git repository (typically `feedback-loop`), `boid` creates a fresh **git worktree** on a new branch. A worktree is a git feature that lets you check out multiple branches of the same repository into separate directories simultaneously, so the task's edits stay in their own directory and do not collide with other tasks. The hook runs inside that worktree, its commits are pushed, and (if needed) a PR is created. Once the PR is merged, the worktree is cleaned up.

## Verification finding

An object inside `verification.findings` representing one issue a review-style script wants fixed. Each finding has:

- `state` — which state wrote it (`executing` / `verifying` / `reworking`).
- `status` — `open` if still unresolved, `resolved` if addressed.
- `severity` — `info` (default), `warning`, `error`, or `fatal`. A task with any open `fatal` finding aborts immediately.
- `message` — free-form text the rework script reads.

The auto-transitions `verifying → reworking` and the exit out of the rework loop are decided entirely from the state of these findings.

## Action

A discrete event that triggers a manual state transition. Examples:

- `start` — advance the task from `pending` to `executing`.
- `done` — force the task into `done` from any of the working states.
- `abort` — force the task into `aborted` from any of the working states.

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
