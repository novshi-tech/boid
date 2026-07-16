# Concepts

This page walks through the main concepts that make up `boid`. The rest of the docs assume you have read it.

## Task

The unit of work that `boid` tracks from request to completion. Every task carries the following fields:

- A **status** — what stage the task is in right now. Tasks move through `pending → executing → done`, and end at `aborted` if they fail. The meaning of each state and the transition rules between them are covered in [State machine](state-machine.md).
- A **payload** — a JSON document that accumulates information as the task progresses. Outputs that execution scripts leave behind are stored under predefined keys called *traits* (defined below).
- A **behavior** — the name that identifies what kind of task this is. The behavior determines which hooks fire and whether the sandbox is read-only.
- The **project** the task belongs to.

Tasks are created with `boid task create` and observed with `boid task list`, `boid task show`, `boid task watch`, or the Web UI.

## Project

A directory that contains a `.boid/project.yaml` file. The project file declares:

- An `id` (the unique identifier `boid` uses for the project) and a `name` (display name).
- The list of **kits** the project uses (`kits:`).
- One or more **task_behaviors** — a map of behavior names to `default_instruction` templates. Names are free-form (free naming). Each behavior can set `readonly`; the default when omitted is `true` (fail-safe).

You register a project with `boid project add <path>`. Any number of projects can coexist; each task belongs to exactly one of them.

## Workspace

A label for grouping projects. You might bucket projects as "personal", "work", and "OSS" so the Web UI can filter the views by group. Workspaces are not declared in `project.yaml`; they are assigned with `boid workspace assign <project> <workspace-id>` (and removed with `boid workspace clear`). A project can belong to at most one workspace.

- `boid workspace list` lists the configured workspaces.
- `boid workspace show <id>` lists the projects in a workspace along with their recent tasks.

Workspaces are purely classification metadata — they do not affect sandbox configuration or hook execution.

## Behavior

A `task_behaviors` entry, naming one kind of task the project supports. When you create a task and pick a behavior name, `boid` decides the isolation level for the task and loads the hooks bound to it, then fires them while the task is in `executing`.

**Since Track A2, behavior names are free** — any name is valid. You can use `review` / `lint` / `release-mgr` or anything else that fits your project's vocabulary.

- `readonly` defaults to **`true`** (fail-safe). Set `readonly: false` explicitly for a writable sandbox.
- `default_task_behavior` is a new top-level key that sets the behavior used by `boid task create` when `--behavior` is omitted.
- `supervisor` and `executor` are the **legacy canonical names**, now **deprecated** in favor of project-specific names. The daemon emits a warning on startup when they are detected. (The names `plan` and `dev` are reserved as `BehaviorAliases` mapping to `supervisor` / `executor` respectively, so they are unavailable as free-naming choices.)

`boid` runs a single state machine regardless of behavior. Different task shapes come from which hooks a behavior wires in, and from how failures are recovered: either by `reopen`ing the task with a new instruction, or by spawning a fresh task. The harness does not encode a verification loop — failure detection and the recovery plan live in the agent's instruction text.

For the full migration procedure and a `readonly` default table, see the [task_behaviors migration guide](../../ja/reference/task-behavior-migration.md) (Japanese).

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

## Session

A **session** is an interactive job that is not tied to any task. You start one with `boid agent <harness>`, which attaches a PTY to your terminal. Valid harness values are `claude`, `codex`, and `opencode`. To just open an interactive shell inside a project sandbox, use `boid exec -p <project> -- bash` (the former `boid agent shell` variant was retired after the git gateway cutover).

```bash
boid agent claude   -p <project>                        # start a Claude Code session
boid exec           -p <project> -- bash                # open a shell inside the sandbox
boid agent claude   -p <project> --resume <session-id> # reattach to an existing session
```

### Sessions vs. tasks

| | Task | Session |
|---|---|---|
| Start | `boid task create` | `boid agent <harness>` |
| Tracking | status / payload / instructions | none |
| State machine | `pending → executing → done` | none (running only) |
| Config | behavior (hooks / kits / readonly …) | project-level traits only |
| Use case | autonomous long-running work | interactive or exploratory work |

A session inherits the project's `env`, `host_commands`, `additional_bindings`, and `secret_namespace` traits. It does not use any behavior definition.

To stop a session, exit the agent or run `boid agent stop <job-id>`. Closing the browser does not kill the session process — you can reattach from the Web UI.

## Sandbox

The isolated environment that hooks execute inside. Internally it is built from a Linux mount namespace plus a chroot, and applies these constraints:

- For project-visible jobs, reads and writes are confined to the sandbox-internal copy of the project cloned through the git gateway (see [Worktree](#worktree)).
- Outbound network connections are limited to a built-in allowlist (`defaultAllowedDomains` in `cmd/start.go`) merged with any extra entries in `sandbox.allowed_domains` from `~/.config/boid/config.yaml`. There is no per-kit domain declaration — the allowlist is global.
- Other parts of the host filesystem (your home directory, SSH keys, other projects) are not visible.

This means that even a runaway agent cannot leave the task's working area.

Some commands legitimately need to reach outside the sandbox (for example `git push`, `gh pr merge`, `boid task update`). They are allowed only if the kit explicitly declares them as **host commands**, in which case they run on the host instead of inside the sandbox.

## Worktree

> **History**: `worktree: true` was a project-top boolean field in `project.yaml` that used to allocate a dedicated, isolated branch (`boid/<id8>`) to child tasks. [The git gateway cutover (2026-07)](../../plans/git-gateway-cutover.md) already dropped host-side `git worktree` usage; [branch-policy-simplification Phase 1 (v0.0.11)](../../plans/branch-policy-simplification.md) retired the per-task branch and fork-point concepts; **Phase 2 (v0.0.12)** removed the `worktree` field itself.

Current behaviour: every project-visible job freshly clones the project into the sandbox through the git gateway and checks out `task.BaseBranch` directly, regardless of task kind. No per-job worktree directory is created on the host, and nothing is written to the host repo's `.git`; results become visible to other sessions only after a commit is pushed. Existing `worktree:` lines in `project.yaml` are silently ignored for BC. See [`project.yaml` reference / git gateway](../reference/project-yaml.md#git-gateway--in-sandbox-clone) for the full contract.

The per-task `boid/<id8>` branch and fork-point concepts existed to let a child continue from a parent's in-progress work on a shared `.git` — a worktree-era mechanism. Once every job got its own independent fresh clone (each job's clone is now the isolation unit), that mechanism became both impossible (a fresh clone only ever sees origin's already-pushed refs) and unnecessary (two clones can check out the same branch name without conflict), so it was retired. See docs/plans/branch-policy-simplification.md for the full rationale.

Branch allocation is now uniform across task kinds:

| Task kind | HEAD branch | Read-only |
|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | sup=true / exec=false |
| **child sup / child exec** | `task.BaseBranch` | sup=true / exec=false |

- **Root tasks** (no parent): the clone checks out `base_branch` directly (no new branch is created). If `base_branch` does not exist on origin yet (case 3), it is created locally from the resolved `fork_point`.
- **Child tasks** (have a parent): handled identically to root tasks. When `base_branch` is omitted, the child inherits the parent's `base_branch` verbatim, so parent and child check out the same branch unless one is explicitly overridden.
- `base_branch` propagates to all child tasks as the PR target and is passed to executors as the `BOID_BASE_BRANCH` environment variable.

Sibling executors running in parallel against the same `base_branch` can still collide on push — that is an executor-side rebase/retry contract, unchanged by this refactor. To isolate parallel children, assign each one a distinct `base_branch`.

Hooks run inside the sandbox-internal clone, their commits are pushed, and (if needed) a PR is created. When the task ends, the clone is cleaned up together with the sandbox runtime directory by the regular runtime GC — there is no dedicated worktree cleanup path anymore. The former FIFO serialisation of tasks sharing the same HEAD branch is also retired: multiple tasks targeting the same branch dispatch in parallel now. Concurrent pushes are resolved by the normal git workflow (non-fast-forward reject → fetch + merge/rebase → re-push).

## Action

A discrete event that triggers a manual state transition. Examples:

- `start` — advance the task from `pending` to `executing`.
- `reopen` — return a `done` task to `executing`, appending a new instruction to `Task.Instructions` (`--message "..."`).
- `abort` — force the task into `aborted` from any non-terminal state.

Send actions with `boid action send --task <id> --type <action>`, or issue them from the Web UI.

## Daemon

The long-running `boid` server process. It owns:

- A UNIX socket for the CLI and an HTTP listener for the Web UI.
- Exclusive access to the SQLite database.
- The dispatch loop that fires hooks in order.
- The lifecycle of sandboxes (including the in-sandbox project clone for project-visible jobs) — creation and cleanup.

Started with `boid start`, stopped with `boid stop`. Most subcommands launch the daemon automatically if it is not already running.

---

Next: [State machine](state-machine.md)
