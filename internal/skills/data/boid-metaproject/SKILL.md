---
name: boid-metaproject
description: Stand up or extend a boid METAPROJECT — the project in a workspace
  that watches the signal inbox and turns incoming events into judged, actionable
  cards. Use this whenever someone wants a workspace to notice things on its own:
  "make boid watch my Jira/GitHub/mail", "set up a task collector for this
  workspace", "I want signals to become cards", "add a source to the sweep", "why
  isn't my sweep firing", or when they mention a sweep task, a judgment task, a
  signal trigger, or a card queue that nobody is filling. Also use it before
  writing ANY Python inside a metaproject — the detection and record machinery
  already ships here as runnable scripts, and copying it into the workspace is
  the mistake this skill exists to prevent.
---

# boid metaproject

A **metaproject** is the one project in a workspace whose job is not to build
anything. It watches the workspace's signal inbox, decides which incoming events
deserve a human's attention, and turns them into cards a person can act on with
one click. Every other project in the workspace does work; this one decides what
work there is.

Two things make a metaproject, and only one of them is yours to write.

| | who writes it | where it lives |
|---|---|---|
| **the machinery** — read the inbox, group by identity, screen, claim, assemble targets, write judgments back to boid | already written, ships with this skill | `~/.claude/skills/boid-metaproject/scripts/` |
| **the judgment** — which events matter, what to propose, when something is done | **you** | the metaproject's own repo |

## The judgment runs in two stages

They are separate jobs, started by different things, and each needs its own
skill in the metaproject's repo.

| stage | started by | scope |
|---|---|---|
| **intake** | a sweep round, one subagent per target | is this worth a card at all, and which card is it |
| **judgment** | a card command the daemon starts per card | what this card now means and what to propose next |

Intake owns what happens *before or outside* a live card: screening a candidate
away, capturing a new one, joining a follow-up to an existing one. Judgment owns
everything *on* a live card: the summary, the child specs, the suggestion.

What joins them is the action log. `capture`, `link` and `note` each write a card
action (`created`, `identity_linked`, `noted`), and those action types are on the
daemon's card-event allowlist — so a captured, joined or updated card starts its
own judgment without the sweep round doing anything further. The same is true of a person
editing a card in the web UI, a work child finishing, a wake condition coming
due, and an answered suggestion. **The sweep is not the only thing that can
cause a card to be re-judged, and that is the point of splitting them.**

Do not have intake write summaries or child specs "while it is already there".
That is not a shortcut — it is a second judgment racing the one the daemon just
started on the same card.

## Do not copy the machinery into the metaproject

The scripts here are baked into the runner image and symlinked into every
sandbox, so a metaproject reaches them by absolute path — it never vendors them.
That is the whole point of this skill.

The reason is not tidiness. The machinery encodes boid's own card state machine,
its action vocabulary, and the inbox contract. A copy is a second place those
have to stay true, and copies rot silently: the first metaproject
(khi-task-collector) ended up with the card transition table written out in four
places, and a missing entry in its copy of the payload-consuming action list made
one whole verb fail without a word in production for weeks. If you find yourself
writing Python in a metaproject that talks to `boid`, stop — either the script
you need is here, or it belongs here.

## Standing up a new metaproject

A metaproject is an ordinary boid project. What makes it a metaproject is five
declarations in its `.boid/project.yaml` plus the two skills that hold the
judgment.

### 1. Declare where signals come from

```yaml
signals:
  sources:
    - connector: jira-cloud/assigned-issues
      service: jira-cloud
      every: 10m
      config:
        initial_window_days: 1
```

Each entry becomes a derived trigger the daemon runs on its own. `connector` is
`<pack>/<connector>` from an installed Integration Pack; `service` is the gateway
service instance that connector reaches through. **From a terminal on the host**
(these are host-side commands — the sandbox shim has no `workspace` subcommand
at all), `boid workspace services list <workspace>` says what the workspace can
reach; the pack's own `integration.yaml` gives the connector's `configSchema`.

**Give a first run a small window.** A connector with no bookmark yet reads
"everything since the beginning" unless its config bounds it, and the first
sweep then faces a year of history at once.

### 2. Declare the round

```yaml
triggers:
  - name: sweep
    on: signals
    every: 2m
    timeout: 20m
    run: |
      id=$(printf '%s\n' 'title: "[sweep]"' 'behavior: sweep' 'auto_start: true' | boid task create | awk '{print $3}')
      [ -n "$id" ] || exit 1
      boid task wait "$id"
```

This shape matters more than it looks. `boid task wait` keeps the trigger's job
alive for exactly as long as the judgment task it started, so the trigger's own
machinery measures the *work* rather than the launcher: single-flight stops a
second round starting on top of the first, a round that ends badly reaches the
failure-streak notification as an ordinary non-zero exit, and `timeout:` bounds
the round from a place the daemon can see. Workspaces that instead launch and
exit have to rebuild all three by hand out of the action log — and get them
subtly wrong.

`on: signals` fires only while the inbox has something unacked, so `every` means
"how often to look", not "how often to run". A round that takes 6 minutes is
followed immediately by the next one if work remains, and falls back to the
`every` cadence when the inbox drains.

**One sweep trigger, not several.** Single-flight is per `(project, trigger
name)`, so two triggers both fire on the same tick and two agents write to the
same card.

### 3. Point the round at the machinery

```yaml
task_behaviors:
  sweep:
    readonly: false
    default_instruction:
      agent: claude-code
      model: sonnet
      message: |
        **最初の一手として次を実行すること。**

            python3 ~/.claude/skills/boid-metaproject/scripts/sweep_targets.py \
              --intake-skill /<仕分けスキル> --max-targets 8

        これが inbox を読み、識別子を解決し、篩って対象へ畳み、仕分けに回す signal を
        claim して、**自分の description をその対象一覧で書き換える**。

        実行し終わったら `boid task current --field description` で書き換わった
        description を読み直し、1 対象につき subagent を 1 枚 fork して仕分ける。

        **`boid task notify --done` を打つのはあなただけ。** subagent は打たない
        (打つと sweep task ごと終了し、走っている兄弟が道連れになる)。全対象の結果が
        返ってから、あなたが `--done` を打つ。**最初の一手が出した件数の行
        (`signal N 件を読み、…`) をそのまま含めること** —— 溢れや篩い落としは他に
        人の目に触れる場所が無い。
```

**`readonly: false` is required, and leaving it out fails silently.** A behavior
that does not say otherwise is read-only — that is the daemon's fail-safe
default for every name except `executor`. The trap is what a read-only sweep
does: `write.py` asks the daemon `boid task current --field readonly` on every
invocation and, unless the answer is exactly `false`, forces `--report` mode,
where it prints what it *would* have written and writes nothing. So a
metaproject that forgets this line reads its inbox, claims signals, forks
judgment subagents, and records none of it — exit code 0 throughout. Five rounds
later the claimed signals are dead, the inbox has nothing pending, and
`on: signals` stops firing. Nothing anywhere reports an error.

That check is deliberate (it stops a prompt-injected subagent from dropping
`--report` to escape a read-only shadow run), which is why it fails closed. It
just means the behavior's `readonly` is load-bearing rather than cosmetic.

It is *not* about the filesystem. Under the clone model a job's project
directory is always mounted read-write and `readonly` is enforced at the gateway
instead (transport-RO), so "I moved the payload to /tmp" does not make a
read-only sweep work.

**This `readonly`-forces-report check only applies to `write.py` invocations
with no card context** — a plain Sweep task, or any other job `boid card
context` reports nothing for. A card-command continuation (a task or session
`boid task create` / `boid agent start` dispatched for a project.yaml
`card_commands.<key>` entry, docs/plans/card-next-step-and-timeline.md §4.5)
is gated by that entry's own `card_write: true/false` instead — an
independent axis from this behavior's `readonly`, since it has no
`BOID_TASK_ID` to ask `boid task current` about in the session case. `write.py`
tells the two paths apart itself (`boid card context`'s presence), so nothing
here changes for a plain Sweep behavior.

`--intake-skill` and `--max-targets` are flags rather than a config file on
purpose — the runner image has no YAML parser, and the behavior instruction is
already the place that says "run this first", so putting the two knobs there
costs no new file. `--judge-skill` is the old name for `--intake-skill` and
still works; it names the intake skill now, so prefer the new name.

### 4. Declare the card command and arm it

```yaml
card_commands:
  judge:
    label: Judge
    card_write: true
    run: |
      id=$(printf '%s\n' 'title: "[judge]"' 'behavior: judge' 'auto_start: true' | boid task create | awk '{print $3}')
      [ -n "$id" ] || exit 1

card_events:
  command: judge
```

`card_commands` declares what a person can run against one card from the web UI.
`card_events` names the one of them the daemon starts on its own when something
happens to a card. Without `card_events` the commands still exist as buttons and
nothing is automatic — which is the right state while you are still cutting a
metaproject over, and the switch to flip when intake has stopped judging.

The launcher is short-lived on purpose: it creates the continuation and exits,
with no `boid task wait`. The card's execution slot is held by the continuation
rather than by the launcher, so a second event arriving mid-judgment waits for
the slot instead of starting a second judgment. `card_events.command` must name
a **task** command — an unattended session has nobody to end it, so the daemon
refuses to auto-start one.

`card_write: true` is what lets the continuation write to the card. It is an
independent axis from the behavior's `readonly` (see §3), so the judgment
behavior does not need `readonly: false` the way the sweep behavior does.
**Anything under `card_commands.<key>` is not strictly decoded** — a misspelt
`card_write` silently becomes false, so confirm it with
`boid card context --field card_write` rather than by reading the YAML.

### 5. Write the two skills

This is the part nobody else can write for you. Everything mechanical is already
handled, so both skills should be about judgment and nothing else.

**The intake skill** receives one target — a card id, or a bare identity for
something not yet captured — plus the event keys that are new about it. Tell it:

- **what to skip.** Low-signal sources need a stated bar, or every notification
  becomes a card. A skip is a real answer and it has to be recorded with a
  reason, or the next round re-decides the same candidate.
- **what deserves a card at all.** The usual bar is whether a card takes work off
  the person's hands, not whether the event is interesting.
- **how to tell a follow-up from a new thing.** That a mail thread, an issue and
  an existing card are the same matter is not mechanically derivable; it is the
  one read only a judgment can do.
- **what is new about a card it already knows.** A target that arrives as a card
  id is handed on with a `note` saying what happened. This is the steady state,
  not the edge case, and it is the only outlet that carries a follow-up forward:
  intake has no "read it, wrote nothing" verb, precisely because a follow-up that
  writes nothing to the card starts no judgment.

For linked resources, pass the Pack signal's external `url` with its matching
`identity` to `capture` or `link`. A follow-up `note` can also include `identity`
and `url` to fill in links on existing cards. `display_name` is optional. Omit
unknown metadata; an explicit empty string clears the saved value. See
[verbs.md](references/verbs.md) for the input contract.

**The judgment skill** receives one card and works out what it now means. Tell
it:

- **what this workspace is trying to achieve.** A judgment that doesn't know what
  counts as progress produces tidy lists nobody acts on.
- **what a good proposal looks like here**, and that not proposing is a real
  answer. Cards with three similar suggestions are worse than cards with none.
- **when something is done.** There is no mechanical completion rule; this is a
  judgment and it has to be written down.

Do not tell either: how to write records, how to number children, which verb a
card's current status allows, how to ack signals, or which of them owns which
verb. Those are enforced by the machinery — the last one is written into the
round's own instruction — and repeating them here creates a second copy that
will drift.

Read `references/verbs.md` for the record CLI's vocabulary — that reference is
what both skills should point at rather than restate.

## Adding a source to an existing metaproject

Add the `signals.sources[]` entry, `git push`, then — **from the host** —
`boid project fetch <project>`. `reload` does not pick up project.yaml, and
`fetch` does not exist in the sandbox shim. Then watch one round: `boid signal
list --workspace <ws>` (host-side; inside a job the workspace is fixed by the
job's own token and there is no `--workspace` flag) should show the new pack's
rows, and the sweep's own report line says how many it read, claimed, acked, and
deferred.

If the source produces more than the round can hold, the report's deferred count
stays above zero every round. That is the signal to raise `--max-targets` or
tighten the connector's own filter — not to widen the read, which is already
free.

## When the sweep isn't doing anything

Work down the chain; each step tells you whether to keep going. **These are
host-side commands.** The sandbox shim has no `workspace` subcommand and its
`signal list` takes no `--workspace` (the job's own token fixes it), so run
these from a terminal on the machine the daemon runs on — a job cannot diagnose
its own workspace.

1. **Is anything arriving?** `boid signal list --workspace <ws> --state all`.
   Empty means the connectors aren't producing — check their derived trigger jobs,
   not the sweep.
2. **Is any of it still alive?** `--state pending` versus `--state dead`. This
   comes before "is the trigger firing", because a dead row looks exactly like a
   live one in the `all` listing and yet `on: signals` counts only pending ones —
   an inbox whose unacked rows have all died is silent with nothing in flight and
   nothing wrong upstream. A row dies after being claimed five times without ever
   being acked, so a wall of dead rows means judgments were started and wrote
   nothing: check `readonly` on the sweep behavior first (see §3 — a read-only
   sweep produces exactly this), then the judgment skill.
3. **Is the trigger firing?** With pending rows and no firing, look for a round
   still in flight holding single-flight (`boid job list`, or a `[sweep]` task not
   in a terminal status).
4. **Did the round read them?** `boid job log <the sweep task's job>` — the first
   move prints `signal N 件を読み、対象 M 件 (claim …、ack …、次巡送り K 件)`. Read
   > 0 with targets 0 means the sieve dropped everything, which is a
   judgment-skill question rather than a machinery one. A `次巡送り` that stays
   above zero every round means the source outruns `--max-targets`.

## What this skill does not cover

- **Writing a connector** (fetching from an external API into the inbox). That
  lives in an Integration Pack, in the `boid-api-skills` repo, with its own
  contract and conformance tests.
- **The card lifecycle itself** — what `parked`/`working`/`done`/`dropped` mean
  and who may move between them. The `boid-task` skill has it.
- **The inbox's own semantics** — claim, ack, dead signals. The `boid-signal`
  skill has it, and the judgment skill should not restate it.
