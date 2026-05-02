# State machine

Every task in `boid` moves through the same state machine. There is one machine, not one per behavior — what differs between behaviors is which hooks and gates run.

This page documents the states, the transitions, and the rules that fire them. For the broader vocabulary, read [Concepts](concepts.md) first.

## States

```
                 +--------+    abort / job_failed
                 |aborted | <--------------------+
                 +--------+                      |
                                                 |
   start                                         |
pending -----> executing -----> done             |
                  ^               |              |
                  |  reopen       |              |
                  +---------------+              |
```

| State | Meaning |
|---|---|
| `pending` | Created, not yet started |
| `executing` | Hooks are doing the main work |
| `done` | Terminal success |
| `aborted` | Terminal failure (manual abort or job failure) |

## Manual transitions

Sent as actions by the user or by handlers (`boid action send --task <id> --type <action>`):

| Action | From | To | Notes |
|---|---|---|---|
| `start` | `pending` | `executing` | |
| `done` | `executing` | `done` | Force completion (still runs the entry gate; usually let auto-transitions handle it). |
| `reopen` | `done` | `executing` | Appends a new instruction and restarts (`--message` to supply it). |
| `abort` | any non-terminal state | `aborted` | |
| `job_failed` (system) | any non-terminal state | `aborted` | |

## Auto transitions

Auto transitions fire on payload changes. After every payload update, the state machine evaluates all rules in priority order; the first match advances the task.

### From `executing`

- `lifecycle.executed` is `true` (the most recent hook exited cleanly via `boid job done`) → `done`.

`lifecycle.executed` is not a persisted trait; the state machine reads the hook completion event and re-evaluates. After a `done` transition the flag resets, so a `reopen` returns to `executing` and waits for the next hook completion.

### Entry gate before `done`

If an entry gate is registered for `done` (gates run on the host), it fires immediately before the transition. A non-zero exit blocks the transition and keeps the task in `executing`.

## Reopen with a new instruction

`boid task reopen <id> --message "..."` returns a `done` task to `executing` and appends a new `Instruction` to `Task.Instructions`. The last element of the array is the active instruction; `agent`, `model`, and `interactive` are inherited from the previously active one.

```bash
# Send the task back through executing with a new ask
boid task reopen abc-123 --message "Resolve the merge conflict against origin/main and push again"
```

Each reopen appends to the array, so historical instructions are preserved and observable as `Task.Instructions[..]`.

## Hooks and gates

- **hook**: the substantive work, runs in the sandbox. Hooks only fire while the task is `executing`. A clean exit (`boid job done`) sets `lifecycle.executed = true`, which drives the auto-transition.
- **gate**: optional host-side scripts. Only `phase: entry` (just before `pending → executing`) and `phase: exit` (just before `executing → done`) are valid. Use them for actions that must touch the host: opening PRs, calling `gh pr merge`, restarting services, and so on.

The old `on:` field on hooks and gates has been removed. Hooks always run in `executing`; gates are controlled solely by `phase`.

## Modes of operation

Because there is one state machine, behavior shapes come from:

- which hooks and gates are wired into the behavior, and
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
