# State Machine

## Contents

- [Status Overview](#status-overview)
- [Unified Flow](#unified-flow)
- [Per-Status Guide](#per-status-guide)
- [Auto-Transition](#auto-transition)

## Status Overview

| status | Role | FS |
|--------|------|-----|
| executing | Implement according to instructions | RW |

Agents are not started in pending, done, or aborted states.

## Unified Flow

All tasks run on a single state machine.
States are three: `pending → executing → done`, with `aborted` as the failure terminal.

```
pending → executing → done
              ↑
              │ can return from done via reopen
              │
done ─────────┘
```

## Per-Status Guide

### executing

Work according to the instructions.

- Follow the instructions, and if you exit normally (exit 0), the hook trap fires `boid job done` and the state machine advances
- When you want to close the session early (e.g., after a plan agent has confirmed all children completed), you can call it explicitly:
  `boid job done "$BOID_JOB_ID" --exit-code 0`
  This causes the daemon to send SIGTERM to the process and the session ends.
  The bash EXIT trap then fires `boid job done` again, but the daemon absorbs the double-fire.
- If you encounter an unrecoverable error, abort:
  `boid task abort <task_id> --code <reason> --message "<summary>"`

When returned to executing via reopen, the last element of the `Task.Instructions` array becomes the new active instruction. Past instructions remain at the front of the array and can be referenced as context.

## Auto-Transition

State transitions are determined automatically by the system based on hook exit events.
Agents do not need to explicitly signal transitions.

| Condition | Transition |
|------|------|
| hook exits with exit 0 (`boid job done` fires) | executing → done |
| exit gate returns exit 0 just before entering `done` | executing → done confirmed |
| exit gate returns non-zero just before entering `done` | transition blocked (stays in executing) |
| `boid task abort` called from any state | * → aborted |
