# State machine

Every task in `boid` moves through the same state machine. There is one machine, not one per behavior — what differs between behaviors is which hooks run.

This page documents the states, the transitions, and the rules that fire them. For the broader vocabulary, read [Concepts](concepts.md) first.

## States

```
                 +--------+    abort / job_failed
                 |aborted | <--------------------+
                 +--------+                      |
                                                 |
   start                                         |
pending -----> executing -----> done             |
                  ^    ^                         |
                  |    | ask                     |
                  |    +------+                  |
                  |           v                  |
                  |       awaiting               |
                  |           |                  |
                  +-- answer -+                  |
```

| State | Meaning |
|---|---|
| `pending` | Created, not yet started |
| `executing` | Hooks are doing the main work |
| `awaiting` | Agent is waiting for a user reply (C2 Q&A mode) |
| `done` | Terminal success |
| `aborted` | Terminal failure (manual abort or job failure) |

## Manual transitions

Sent as actions by the user or by hooks (`boid action send --task <id> --type <action>`):

| Action | From | To | Notes |
|---|---|---|---|
| `start` | `pending` | `executing` | |
| `done` | `executing` | `done` | Force completion (usually let auto-transitions handle it). |
| `reopen` | `done` | `executing` | Appends a new instruction and restarts (`--message` to supply it). |
| `ask` | `executing` | `awaiting` | Issued by `boid task notify --ask`. Pauses the task while it waits for an `answer`. |
| `answer` | `awaiting` | `executing` | Issued by `boid task answer` or the Web UI. Restarts the hook. |
| `abort` | any non-terminal state | `aborted` | |
| `job_failed` (system) | any non-terminal state | `aborted` | |

## Auto transitions

Auto transitions fire on payload changes. After every payload update, the state machine evaluates all rules in priority order; the first match advances the task.

### From `executing`

- `lifecycle.executed` is `true` (the most recent hook exited cleanly via `boid job done`) → `done`.

`lifecycle.executed` is not a persisted trait; the state machine reads the hook completion event and re-evaluates. After a `done` transition the flag resets, so a `reopen` returns to `executing` and waits for the next hook completion.

## Reopen with a new instruction

`boid task reopen <id> --message "..."` returns a `done` task to `executing` and appends a new `Instruction` to `Task.Instructions`. The last element of the array is the active instruction; `agent`, `model`, and `interactive` are inherited from the previously active one.

```bash
# Send the task back through executing with a new ask
boid task reopen abc-123 --message "Resolve the merge conflict against origin/main and push again"
```

Each reopen appends to the array, so historical instructions are preserved and observable as `Task.Instructions[..]`.

## Hooks

- **hook**: the substantive work, runs in the sandbox. Hooks only fire while the task is `executing`. A clean exit (`boid job done`) sets `lifecycle.executed = true`, which drives the auto-transition.

## Modes of operation

Because there is one state machine, behavior shapes come from:

- which hooks are wired into the behavior, and
- whether failures are handled by `reopen` or by spawning a fresh task.

The harness does not encode a verification loop. Failure detection and the recovery plan live in the agent's instruction text.

## Reading from the CLI

```bash
boid task show <id>              # current status and payload
boid task watch <id>             # follow status changes in real time
boid job list --task <id>        # list every job ever run for this task
boid job show <id>               # one job's stdout, stderr, and exit code
```

The status and the payload tell you what is happening to the task; the jobs tell you what the extension packages' scripts actually did.

---

Next: [Web UI](web-ui.md)
