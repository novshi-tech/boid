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
| `done` | `awaiting` | `done` | Force completion from the awaiting state. |
| `fail` | `executing` | `aborted` | Force abort from executing. |
| `reopen` | `done` | `executing` | Appends a new instruction and restarts (`--message` to supply it). |
| `reopen` | `aborted` | `executing` | Return an aborted task to executing. |
| `ask` | `executing` | `awaiting` | Issued by `boid task ask` (blocking RPC) or `boid task notify --ask`. Places the task in `awaiting`. |
| `answer` | `awaiting` | `executing` | Issued by `boid task answer` or the Web UI. Only the `boid task ask` flow can be resolved (the answer is handed to the parked broker connection). `notify --ask` awaitings are rejected with 409 — the agent already exited and the daemon no longer dispatches a resume hook. |
| `abort` | any non-terminal state | `aborted` | |
| `job_failed` (system) | any non-terminal state | `aborted` | |

### Non-transitioning actions (timeline record only)

These actions record an entry in the task timeline but do **not** change the task status:

| Action | Purpose |
|---|---|
| `progress` | Progress report from the agent (informational). |
| `done_request` | Recorded by `boid task notify --done`; triggers auto-advance after the runtime exits. |
| `fail_request` | Recorded by `boid task notify --fail`; triggers auto-advance after the runtime exits. |

`notify --done` and `notify --fail` do **not** transition the task immediately. They record a `done_request` / `fail_request` entry and the daemon advances the state automatically once the runtime process exits.

## Auto transitions

Auto transitions fire on payload changes. After every payload update, the state machine evaluates all rules in priority order; the first match advances the task.

### From `executing`

Three rules are evaluated in order:

1. `lifecycle.executed` is set **and** `lifecycle.fail` is set → `aborted`.
2. `lifecycle.executed` is set **and** `lifecycle.done` is set → `done`.
3. `lifecycle.executed` is set (legacy hook path, no explicit done/fail signal) → `done`.

`lifecycle.executed` is not a persisted trait; the state machine reads the hook completion event and re-evaluates. After a `done` transition the flag resets, so a `reopen` returns to `executing` and waits for the next hook completion.

### Project lock and `awaiting`

While a task is in `awaiting`, the project lock is **released**. Other tasks that share the same project (and same HEAD branch) may proceed. The lock is reacquired when the task transitions back to `executing` via an `answer` action.

## Reopen with a new instruction

`boid task reopen <id> --message "..."` returns a `done` or `aborted` task to `executing` and appends a new `Instruction` to `Task.Instructions`. The last element of the array is the active instruction; `agent` and `model` are inherited from the previously active one.

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
