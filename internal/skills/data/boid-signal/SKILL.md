---
name: boid-signal
description: Read and decide on the boid signal inbox from inside a sandboxed
  task. Explains `boid signal list`/`boid signal claim`/`boid signal ack`, what
  "claim" means (I am handing this to a judgment) and what "ack" means (this
  Signal has a written judgment) — for a scan-loop task whose job is to scan a
  workspace's inbox and make a judgment call per item. Not a contract
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
boid signal list                     # read pending Signals (no side effect)
  ... decide which of them you are actually going to judge this round ...
boid signal claim <id> [<id> ...]    # "these are the ones I'm handing to a judgment"
  ... make a judgment about each one (create/update a task, notify, etc.) ...
boid signal ack <id> [<id> ...]      # mark them decided
```

**Reading is free; claiming is the declaration that costs something.** `list`
never changes anything, so read as widely as you like — more than you intend
to judge is fine and normal (several Signals often collapse onto one task, so
you cannot tell how many you need until you have looked at them). `claim`
charges one attempt against exactly the ids you name, which is what
eventually retires a Signal nobody can ever handle (see "Dead Signals").

**Claim what you hand to a judgment, including the ones you expect to fail.**
If you looked at a Signal and could not resolve what it refers to, claiming it
is correct — you tried, and five such rounds should give up. What you must NOT
claim is a Signal you never looked at (one you deferred to the next round
because this round was already full): charging that one retires a Signal
nobody ever judged.

## `boid signal list`

```
boid signal list [--source <pack>/<connector>] [--state <state>] [--limit N]
```

- No flags: the caller's own workspace's **pending** (unacked, not yet dead)
  Signals, oldest `occurred_at` first, JSON to stdout. Pure read — call it as
  often and as widely as you want.
- `--source <pack>/<connector>`: narrow to one connector's Signals (e.g.
  `--source slack/mentions`).
- `--state pending|dead|acked|all`: look at a different slice than the
  default (`pending`).
- `--limit N`: cap the batch size (default is generous; set this explicitly
  in a scan loop so one run never tries to read an unbounded batch).
- `--claim`: **deprecated.** Selects pending Signals AND increments `attempts`
  on every row it returns, in one call. It cannot tell "I read this" apart
  from "I handed this to a judgment", so a loop that reads wider than it
  judges retires Signals it never judged. Use `list` + `claim` instead. Still
  accepted so a workspace can switch at its own pace.

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

## `boid signal claim <id>...`

```
boid signal claim <id> [<id> ...]
```

Charges one attempt against exactly these Signals. Say it once per round, for
the Signals you are about to judge — the counter it moves is what makes a
Signal eventually "dead" (below) if nothing ever acks it, so it is the
inbox's only defense against a loop that keeps picking up the same stuck
Signal forever without making progress.

Same typo guard as ack: an id that doesn't exist in your workspace fails the
whole call and charges nothing. Claiming an already-acked id is harmless — it
is not an error, and nothing is charged (there is nothing left to judge).

**Why this is separate from `list`.** `attempts` is a give-up counter, so what
it counts has to be "handed to a judgment". A single read-and-charge call can
only count "returned by the read", and those are not the same number for any
loop that reads more rows than it processes in a round. Splitting them makes
reading free and leaves the counter measuring the thing it is named after.

**Why not a timeout instead.** A clock cannot tell "this Signal is
unprocessable" from "nothing was running to process it" — a workspace down
for a week would retire every Signal that arrived meanwhile. `attempts` has
the property a clock does not: no consumer, no decay. (boid's own GC encodes
the same ruling: it refuses to delete a pending Signal on age alone.)

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
exactly the ids you claimed.

## Dead Signals

A Signal that's been claimed enough times without ever being acked becomes
**dead** — a `pending` listing stops returning it, so a broken judgment loop
can't spin on it forever. Dead Signals are not deleted; a human (or a
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
