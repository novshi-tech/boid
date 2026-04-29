# State machine

Every task in `boid` moves through the same state machine. There is one machine, not one per behavior — what differs between behaviors is which hooks and gates run in each state.

This page documents the states, the transitions, and the rules that fire them. For the broader vocabulary, read [Concepts](concepts.md) first.

## States

```
                 +--------+    abort / job_failed / fatal finding
                 |aborted | <-----------------------------------+
                 +--------+                                     |
                                                                |
   start                                                        |
pending -----> executing -----> verifying -----> done           |
                  ^   |             |  ^                        |
                  |   |             |  |                        |
                  |   v             v  |                        |
                  +- reworking <----+  |                        |
                       |               |                        |
                       +---------------+                        |
                                                                |
                                       reopen                   |
                                       <-----                   |
                                       (done -> reworking)      |
```

| State | Meaning |
|---|---|
| `pending` | Created, not yet started |
| `executing` | Hooks are doing the main work |
| `verifying` | Reviewer hooks/gates are checking the result |
| `reworking` | Findings need to be fixed; the executing-side hook re-runs |
| `done` | Terminal success |
| `aborted` | Terminal failure (manual abort, fatal finding, exceeded rework limit, or job failure) |

## Manual transitions

Sent as actions by the user or by handlers (`boid action send --task <id> --type <action>`):

| Action | From | To |
|---|---|---|
| `start` | `pending` | `executing` |
| `done` | `executing` / `verifying` / `reworking` | `done` |
| `reopen` | `done` | `reworking` |
| `abort` | any non-terminal state | `aborted` |
| `job_failed` (system) | any non-terminal state | `aborted` |

## Auto transitions

Auto transitions fire on payload changes. After every payload update, the state machine evaluates all rules in priority order; the first match advances the task.

### Abort (highest priority)

Auto-fires from any state.

- Any finding has `severity=fatal` and `status=open` → `aborted`
- `lifecycle.rework_count` exceeds the configured limit while in `reworking` → `aborted` (configurable via `state_machine.rework_limit` in `~/.config/boid/config.yaml`, default 5)

### From `executing`

Two inputs decide the transition:

- whether `artifact` or `tasks` has been written to the payload — either one means "the deliverables expected from `executing` are in place"
- whether any open findings written during `executing` are still around

Combining them gives:

- deliverables present + open findings from `executing` → `reworking`
- deliverables present + no open findings → `verifying`
- no deliverables but `lifecycle.executed` is true → `done` (the work was carried out but produced nothing that needs verifying)

`artifact` and `tasks` play symmetric roles: plan-style tasks write `tasks`, dev-style tasks write `artifact`. Different trait names, same meaning — "`executing` is finished, move on to verification".

### From `verifying`

- Open findings sourced from `verifying` → `reworking`
- No open findings → `done` (pass-through if no verification gate exists)

### From `reworking`

- All findings sourced from `reworking` resolved → `verifying` (re-enter verification)
- Some findings sourced from `reworking` still open → stays in `reworking` (self-loop until resolved)

The reworking exit checks only findings sourced from `reworking`. Findings sourced from `verifying` (such as `mergeable-check`) do not block reworking exit — they get re-evaluated when the task re-enters `verifying`.

## How findings drive the loop

Findings are objects in `verification.findings`. Each one carries:

- `state` — which state generated it (`executing`, `verifying`, `reworking`)
- `status` — `open` or `resolved`
- `severity` — `info` (default), `warning`, `error`, `fatal` (any open `fatal` aborts immediately)
- `message` — free-form text the rework hook can read

A review-style hook or gate writes findings via a payload patch. The daemon re-evaluates the auto-transition rules on the resulting payload change, so any qualifying transition fires immediately afterwards.

## Rework limit and abort

`reworking → aborted` triggers if `lifecycle.rework_count` exceeds the configured limit. Default is 5; override in `~/.config/boid/config.yaml`:

```yaml
state_machine:
  rework_limit: 10
```

This guards against runaway rework loops. The aborted task carries `code=rework_limit_exceeded` in its abort reason, so you can tell rework-limit aborts apart from other failures.

## Mode of operation: one-shot vs feedback-loop

The `transition` field on a behavior selects how aggressively rework is used:

- **one-shot** — runs `executing → verifying → done`. If the verifier writes findings, the task returns to `reworking` once and tries again. Suitable for "do this thing".
- **feedback-loop** — same machine, but expected to cycle through `reworking ↔ verifying` multiple times. Suitable for code changes that go through PR review and CI.

The state machine itself is identical; the difference is which kits and handlers each behavior wires up.

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
