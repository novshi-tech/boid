# Concepts

This page is the vocabulary reference. Read it once before going deep into other docs; everything else assumes you know these terms.

## Task

A unit of work that `boid` tracks from request to completion. Every task has:

- A **status** that moves through a state machine: `pending → executing → verifying → reworking → done`, with `aborted` as the terminal failure state. See [State machine](state-machine.md) for the full transition rules.
- A **payload**, a JSON document that carries the task's evolving state — instructions, generated artifacts, verification findings, and so on.
- A **behavior**, a label like `dev` or `plan` that selects which kits and handlers participate.
- A **project** it belongs to.

Tasks are created with `boid task create` and observed with `boid task list`, `boid task show`, `boid task watch`, the TUI, or the Web UI.

## Project

A directory with a `.boid/project.yaml` file. The project file declares:

- An `id` and `name`.
- One or more **task behaviors**, each binding a transition mode and a list of kits.
- Optional kit-level configuration.

A project is registered with `boid project add <path>`. You can have many projects; tasks always belong to exactly one.

## Behavior

A named slot in the project's `task_behaviors` map. When you create a task, you pick a behavior — `boid` then loads the kits associated with that behavior and runs their handlers as the task moves through states.

The most common transition modes are:

- **one-shot** — execute once, verify, done. Suitable for "do this single thing".
- **feedback-loop** — execute, verify, accept rework cycles until findings are resolved. Suitable for code changes that need review.

## Payload and traits

The task payload is a JSON document. Top-level keys are called **traits**, each with a defined meaning:

| Trait | Written by | Drives |
|---|---|---|
| `instructions` | task creator / kit | What handlers should do |
| `artifact` | execution hooks | Marks "executing complete"; triggers transition to `verifying` |
| `tasks` | plan hooks | Same role as `artifact`, used by planning behaviors |
| `verification.findings` | reviewer hooks/gates | Drives transitions into `reworking` and back to `verifying` |
| `lifecycle` | core | Tracks rework count, executed flag, and other computed state |

Handlers update the payload by emitting **payload patches** — JSON merge instructions that get applied to the persisted payload. This keeps the conversation between core and kits structured and replayable.

## Hook, gate, and kit

Three concepts that show up everywhere:

- **Hook** — a script that runs **inside the sandbox** during a state. Hooks do the actual work: invoking an AI agent, writing code, running tests. Multiple hooks can run in parallel for the same state.
- **Gate** — a script that fires **on the host** at a state transition (entry or exit). Gates do environment-side work: creating a PR, calling `gh pr merge`, restarting a service. Gates run in parallel and have no sandbox lock.
- **Kit** — a directory containing a `kit.yaml` plus the hook/gate scripts and any shared assets. Kits are reusable packages — you install them once and reference them from any project that needs them. Official kits live in [boid-kits](https://github.com/novshi-tech/boid-kits).

Hook and gate handlers communicate with `boid` through stdin (the task payload) and stdout (a payload patch).

## Job

A single execution of a hook or gate. Jobs have their own status (`running`, `success`, `failed`) and exit code. When you watch a task, you are really watching its jobs unfold.

`boid job list --task <id>` and `boid job show <id>` are the main introspection commands.

## Sandbox

A Linux mount-namespace + chroot environment that hooks run inside. Reads and writes are confined to the worktree (or the project root for non-worktree behaviors), the network is restricted to declared domains, and the host filesystem is hidden. Hooks therefore cannot touch your home directory, ssh keys, or other projects unless explicitly allowed.

A small set of **host commands** (declared per-kit) can cross the boundary — for example `git push`, `gh pr merge`, or `boid task update`. Everything else stays inside.

## Worktree

For behaviors that involve git changes (typically `feedback-loop`), `boid` creates a dedicated git worktree on a fresh branch. The hook runs inside that worktree, the resulting commits get pushed, and a PR is created. After the PR is merged, the worktree is torn down. Worktrees let several dev tasks run in parallel without stepping on each other.

## Verification finding

An object inside `verification.findings` that records something a reviewer wants fixed. Findings have a `state` source (which state generated them: `executing` / `verifying` / `reworking`), a `status` (`open` / `resolved`), an optional `severity` (`fatal` immediately aborts), and a free-form message. Findings are how `verifying → reworking` transitions get triggered, and how rework loops eventually exit.

## Action

A user- or system-issued event that drives a manual transition. Examples: `start` (move pending → executing), `done` (force a state to done), `abort`. Sent with `boid action send --task <id> --type <action>` or via the TUI/Web UI.

## Daemon

The long-lived `boid` server. It listens on a UNIX socket (and an HTTP socket for the Web UI), holds the SQLite database, runs the dispatch loop that fires hooks/gates, and manages worktrees and sandboxes. Started with `boid start`, stopped with `boid stop`. Most CLI commands auto-start the daemon if it is not already running.

---

Next: [State machine](state-machine.md)
