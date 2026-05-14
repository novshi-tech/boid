# Child Task State Machine

All boid tasks share the unified state machine defined in `internal/orchestrator/machine.go`. The supervisor's job is to observe child task state and react.

## Contents

- [Statuses](#statuses)
- [Manual Transitions](#manual-transitions)
- [Event-Driven Transitions](#event-driven-transitions)
- [Auto Transitions](#auto-transitions)
- [How the Supervisor Reacts](#how-the-supervisor-reacts)
- [Reopen Semantics](#reopen-semantics)

## Statuses

| Status | Meaning for the supervisor |
|---|---|
| `pending` | Created but not yet started. With `auto_start: true` this is transient — the dispatch loop picks it up almost immediately. |
| `executing` | Child agent is running. Keep polling. |
| `awaiting` | Child agent called `notify --ask` and is paused for a user reply. Keep polling — the reply returns it to `executing`. |
| `done` | Terminal (success). Read the artifact and run the integration step from the active instruction. |
| `aborted` | Terminal (failure). Read `lifecycle.abort.message`; diagnose with `boid job list/show/log`. |

The supervisor itself runs as `executing` and transitions to `awaiting` when it calls `notify --ask`, then back to `executing` when the user replies.

## Manual Transitions

Triggered by explicit CLI calls (`boid task ...` or `boid action send --type ...`).

| Action | From | To | Triggered by |
|---|---|---|---|
| `start` | pending | executing | dispatch loop (auto_start) or `boid action send --type start` |
| `done` | executing | done | `boid job done "$BOID_JOB_ID" --exit-code 0` from the agent (the EXIT trap of the harness fires the same call, which CompleteJob absorbs idempotently) |
| `reopen` | done | executing | `boid task reopen <id> -m "<msg>"` |
| `ask` | executing | awaiting | `boid task notify --ask` |
| `answer` | awaiting | executing | `boid task answer` (or the Web UI Q&A reply) |
| `abort` | * | aborted | `boid action send --type abort` |

## Event-Driven Transitions

Triggered automatically by hook lifecycle events; not exposed as a manual action.

| Trigger | Transition |
|---|---|
| Hook exits non-zero | * → aborted (recorded as `job_failed`) |

## Auto Transitions

Triggered by condition rules evaluated after each dispatch step.

| Condition | Transition |
|---|---|
| `lifecycle.executed` becomes true (the hook ran to completion and `task.exit` gates passed) | executing → done |

`lifecycle.executed` is a transient signal injected by the coordinator; it is not persisted to the payload. A `task.exit` gate that returns non-zero blocks this transition and routes the task to `aborted` via the `job_failed` event-driven path instead.

## How the Supervisor Reacts

| Observed status | Supervisor action |
|---|---|
| `pending` (lingering) | Verify `auto_start: true` was set; rarely seen otherwise. |
| `executing` | Sleep and re-poll. |
| `awaiting` | Sleep and re-poll. The child returns to `executing` on user reply. |
| `done` | Read artifacts (`boid task get <id> --field artifact.<key>`), run the integration step from the active instruction, then either spawn the next child or move to exit handling. |
| `aborted` | Read `lifecycle.abort.message` (`boid task get <id> --field lifecycle.abort.message`) and the job log (`boid job list/show/log`). Decide between: retry via `boid task reopen` (only valid from `done`, so unavailable here — see "Reopen Semantics"), creating a fresh child with a revised description, or escalating via `notify --ask`. |

## Reopen Semantics

`boid task reopen` transitions `done → executing` with the new `-m "<msg>"` **appended** to `instructions.yaml`; the last element becomes the active instruction. Earlier elements remain as context. The same task ID and worktree are reused — no new branch is cut.

`reopen` is only valid from `done`. Aborted tasks cannot be reopened — either:

- Create a fresh child task with a revised description, or
- Use `boid task rerun <id> [--auto-start]` from the host CLI to reset a done/aborted task back to `pending` for re-execution with the same ID. (Note: `rerun` is not in the in-sandbox shim; it must be invoked from the host or scripted via the user.)
