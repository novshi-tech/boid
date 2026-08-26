---
name: boid-signal
description: Read and decide on the boid signal inbox from inside a sandboxed
  task. Explains `boid signal list`/`boid signal ack` and what "ack" means
  (this Signal has a written judgment) — for a scan-loop task whose job is to
  scan a workspace's inbox and make a judgment call per item. Not a contract
  or a connector guide — connectors never run judgment code and never use
  this skill.
---

# boid Signal Inbox

A **Signal** is one row in the workspace's inbox: an external event a connector
already fetched and normalized (a Slack mention, a Jira issue update, a
Bitbucket PR comment, ...) and handed to boid via `boid signal ingest`. You
never call `ingest` yourself — that command only exists inside a connector's
own job, is not part of your policy, and will be rejected if you try it. Your
job, from inside a scan/judgment task, is simpler: **read the inbox, decide
what each Signal means, and ack the ones you've handled.**

This is knowledge about how the inbox behaves, not a contract you must
enforce — there is no required response shape or mandatory action per Signal.
What you do with a Signal (open a task, post a reply, ignore it) is entirely
up to your own instructions/behavior.

## The loop

```
boid signal list --claim   # read + claim a batch of pending Signals
  ... make a judgment about each one (create/update a task, notify, etc.) ...
boid signal ack <id> [<id> ...]   # mark them decided
```

`--claim` is the mode a scan loop normally wants: it selects pending Signals
**and** increments each one's attempts counter in the same call (see "Claim
vs. plain list" below). Without `--claim`, `list` is a pure read with no side
effect — useful for a one-off look, but a repeated scan loop should use
`--claim` so a Signal nobody ever acks eventually stops being handed out
(see "Dead Signals" below).

## `boid signal list`

```
boid signal list [--claim] [--source <pack>/<connector>] [--state <state>] [--limit N]
```

- No flags: the caller's own workspace's **pending** (unacked, not yet dead)
  Signals, oldest `occurred_at` first, JSON to stdout.
- `--claim`: same selection, but each returned Signal's `attempts` is
  incremented as part of the same call — use this in a scan loop, not a
  one-off `list --state all` for debugging.
- `--source <pack>/<connector>`: narrow to one connector's Signals (e.g.
  `--source slack/mentions`). Not compatible with `--claim` — claim always
  operates workspace-wide; combine them and the call is rejected.
- `--state pending|dead|acked|all`: look at a different slice than the
  default (`pending`). Not compatible with `--claim` except the default
  `pending` — same reason as `--source` above.
- `--limit N`: cap the batch size (default is generous; set this explicitly
  in a scan loop so one run never tries to process an unbounded batch).

The reply is a JSON object with a `signals` array — the SAME envelope shape
the host-side `GET /api/signals` API uses, so the two are interchangeable
if you're ever comparing output from a sandboxed task against `boid signal
list` run from outside a job:

```json
{
  "signals": [
    {
      "id": "1699999999.000100",
      "occurred_at": "2026-08-26T01:23:45Z",
      "source": {
        "pack": "slack",
        "connector": "mentions",
        "service": "slack-api"
      },
      "identity": "slack:C0123:1699999999.000100",
      "url": "https://...",
      "author": "U0ABC123",
      "title": "someone mentioned you in #general",
      "received_at": "2026-08-26T01:24:01Z",
      "attempts": 1,
      "acked_at": null
    }
  ]
}
```

`id` is the connector's own stable event key — unique per (service,
connector), **not** unique on its own across the whole workspace (see ack's
matching rule below). `source.pack`/`source.connector` are the two halves of
what `--source` filters on together as `<pack>/<connector>` (e.g.
`--source slack/mentions` means `source.pack == "slack" && source.connector
== "mentions"`). `identity` is an opaque string from the connector; if
you need to correlate a Signal with an existing boid task, that's your own
judgment logic, not something this command resolves for you.

## `boid signal ack <id>...`

```
boid signal ack <id> [<id> ...]
```

**Ack means "I have written a judgment for this Signal" — nothing more.** It
does not imply any particular outcome (you may have decided the Signal
warranted no action at all and still ack it, so it stops coming back). Ack
one or many ids in a single call.

Acking is **idempotent**: acking an id that's already acked is a no-op
success, not an error — so a scan loop that crashes after acking some ids and
retries from the top will not fail on the ones it already handled. Acking an
id that doesn't exist in your workspace at all IS an error (typo guard) — the
whole call fails and nothing is acked, so fix the id and retry rather than
assuming partial success.

Ack matches by id alone, not by (service, connector, id) — if the very same
id string happens to appear under two different connectors (unusual, but
possible), acking it acks both rows. In practice this only matters if you're
deliberately cross-referencing ids across connectors; normally you ack
exactly the ids `list --claim` just handed you.

## Claim vs. plain list

- `list` (no `--claim`): read-only, safe to call repeatedly, does not affect
  what a later `--claim` call selects.
- `list --claim`: increments `attempts` on every Signal it returns. This is
  what makes a Signal eventually "dead" (see below) if nothing ever acks it —
  it is the inbox's only defense against a scan loop that keeps claiming the
  same stuck Signal forever without making progress.

## Dead Signals

A Signal that's been claimed enough times without ever being acked becomes
**dead** — `list --claim` stops returning it, so a broken judgment loop can't
spin on it forever. Dead Signals are not deleted; a human (or a
`--state dead` listing) can still see and manually ack them. You don't need
to do anything special about this in normal operation — it's a safety net,
not something your scan loop needs to check for on every run.

## What this skill does NOT cover

- `boid signal ingest` / `boid signal cursor` — connector-only commands that
  write to and read the inbox's per-source bookmark. They are not in your
  policy from a judgment task; if you're implementing a connector, that's a
  different job (a Pack's `connectors/` code), not this skill.
- Host-side `boid signal list`/`ack` (outside a sandbox, e.g. from your own
  terminal) — same commands, but that's a human operating the daemon
  directly, not a dispatched task.
