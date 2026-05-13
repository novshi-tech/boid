---
name: boid-executor
description: Runs an executor task (writable implementation) for the boid orchestrator.
  Reads task.yaml title/description, implements the requested change, and either
  calls a project-side release skill (e.g. /dev-pr-flow) or just commits and pushes
  before exiting. This is the canonical writable counterpart to /boid-supervisor.
---

# boid Executor

An executor task **implements** what the parent supervisor asked for. It runs inside a writable worktree (the project-top `worktree: true` setting in `.boid/project.yaml` provisions one automatically), edits files, runs tests, and finishes with a commit.

This is the canonical behavior counterpart to the old `dev` behavior. Implementation work happens here; the parent `/boid-supervisor` only watches and integrates results.

## Context

| File | Contents |
|---------|------|
| `~/.boid/context/task.yaml` | Task ID, title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions addressed to you (array) |
| `~/.boid/context/payload.yaml` | Full payload (existing artifacts, verification results) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (RO/RW, network, tools) |

Start by reading `task.yaml` and `instructions.yaml` to understand the task. Past instructions are kept at the front of the `instructions.yaml` array (when the task has been reopened); the **last element** is the currently active instruction. Use earlier elements as context only.

## Workflow

1. **Read the task.** Title + description in `task.yaml`, current instruction at the tail of `instructions.yaml`. Inspect `environment.yaml` to confirm the worktree path and writability.
2. **Implement.** Make the code / test / doc changes the task asks for. Stay within the executor's worktree — do **not** edit files outside it.
3. **Verify locally.** Run the project's quick verification (typically tests + lint) before committing. If the project ships a verification skill / script, use it.
4. **Release.** Pick one of two paths depending on the project conventions (see "Release Step" below).
5. **Exit.** Exit 0; the hook EXIT trap fires `boid job done` and the daemon transitions you to `done`. The supervisor sees the transition and runs its integration step.

If the work cannot proceed (missing fact, ambiguous spec, unrecoverable environment problem), call:

```bash
boid task notify "$BOID_TASK_ID" --ask "<what you need to know>"
```

This pauses the task in `awaiting` and notifies the user. When the user replies, the kit resumes you with `$BOID_USER_ANSWER` set. If the situation is unrecoverable, call `boid task abort "$BOID_TASK_ID" --code <reason> --message "<summary>"`.

## Release Step

The release step ships your committed work to the place the supervisor expects to find it. Two models are common:

### Default: Local Merge Model

`commit + push` is enough. The supervisor will fast-forward / merge the branch locally into the base branch as part of its supervise loop.

```bash
git add -A
git commit -m "<conventional commit message>"
git push origin HEAD   # only if the project pushes branches; otherwise skip
```

If the project does **not** push child branches to a remote (purely local supervisor merge), skip the `push` — the supervisor will read the branch from the worktree directly.

### Override: PR Model

If a project-side release skill exists (typically `/dev-pr-flow`), **call it** instead of the manual commit + push above. Example for the boid repo:

```
/dev-pr-flow
```

The skill handles the project's release conventions: running the right verifications, picking the commit / PR title from `task.yaml`, opening the PR, watching CI, and exiting with the right status. Use the skill exactly as the project documents it; do not reimplement the flow inline.

To know which model the current project uses, read the executor behavior's `default_instruction.message` in `.boid/project.yaml`. Projects that want PR mode will explicitly mention `/dev-pr-flow` (or a similar skill) in the instruction; otherwise default to local commit + push.

## Progress Reporting

During long-running work you can leave a progress note on the task timeline at any time:

```
boid task notify <task_id> --progress "<message>"
```

- **No state transition** — executing stays executing
- **No notify hook** — `notify.command` is not invoked
- **Timeline entry** — shown on Web UI / TUI task detail

Use this for milestone signals during multi-hour implementations. For decision branches use `--ask`; for FYI use the bare form (which fires the notify hook).

## Rules

- Only edit files inside your worktree (the path in `environment.yaml`). The worktree is removed when the task transitions to `done`, so anything outside it would be lost anyway.
- Do not write to `instructions` in the task payload — it is delivered as a separate read-only file (`instructions.yaml`).
- Follow constraints in `environment.yaml` (`network.restricted`, `tools`).
- Always commit before exiting. Uncommitted changes vanish with the worktree.
- Do **not** spawn child tasks unless the project's executor instruction tells you to. Triage and decomposition are the supervisor's job; executors implement.

## State Machine

The executor only runs in the `executing` state. State transitions are automatic:

| Condition | Transition |
|------|------|
| Exit 0 (`boid job done` fires via the EXIT trap) | executing → done |
| Exit non-zero / `boid task abort` | * → aborted |
| Supervisor calls `boid task reopen <id> -m "<new instruction>"` | done → executing (with the new instruction appended to `instructions.yaml`) |

When reopened, read the **last element** of `instructions.yaml` as the new active instruction; earlier elements are prior context.
