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

A metaproject is an ordinary boid project. What makes it a metaproject is four
declarations in its `.boid/project.yaml` plus one skill that holds the judgment.

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
service instance that connector reaches through. Ask `boid workspace services
list <workspace>` for what the workspace can reach, and read the pack's
`integration.yaml` for the connector's `configSchema`.

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
              --judge-skill /<判断スキル> --max-targets 8

        これが inbox を読み、識別子を解決し、篩って対象へ畳み、判断に回す signal を
        claim して、**自分の description をその対象一覧で書き換える**。

        実行し終わったら `boid task current --field description` で書き換わった
        description を読み直し、1 対象につき subagent を 1 枚 fork して判断する。

        **`boid task notify --done` を打つのはあなただけ。** subagent は打たない
        (打つと sweep task ごと終了し、走っている兄弟が道連れになる)。全対象の結果が
        返ってから、あなたが `--done` に対象数と結果の要点を書いて打つ。
```

`readonly: false` is required, and not for the reason people assume: the API
gateway would let a read-only job do GETs just fine, but the judgment subagent
writes its payload to a file in the project directory, and a read-only job's
project directory is mounted read-only.

`--judge-skill` and `--max-targets` are flags rather than a config file on
purpose — the runner image has no YAML parser, and the behavior instruction is
already the place that says "run this first", so putting the two knobs there
costs no new file.

### 4. Write the judgment skill

This is the part nobody else can write for you. It receives one target — a card
id, or a bare identity for something not yet captured — plus the event keys that
are new about it, and decides what to propose. Everything mechanical is already
handled, so the skill should be about judgment and nothing else.

Tell it:

- **what this workspace is trying to achieve.** A sweep that doesn't know what
  counts as progress produces tidy lists nobody acts on.
- **what a good proposal looks like here**, and that not proposing is a real
  answer. Cards with three similar suggestions are worse than cards with none.
- **when something is done.** There is no mechanical completion rule; this is a
  judgment and it has to be written down.
- **what to skip.** Low-signal sources need a stated bar, or every notification
  becomes a card.

Do not tell it: how to write records, how to number children, which verb a card's
current status allows, or how to ack signals. Those are enforced by the machinery
and repeating them here creates a second copy that will drift.

Read `references/verbs.md` for the record CLI's vocabulary — that reference is
what the judgment skill should point at rather than restate.

## Adding a source to an existing metaproject

Add the `signals.sources[]` entry, `git push`, then `boid project fetch
<project>` — `reload` does not pick up project.yaml. Then watch one round: `boid
signal list --workspace <ws>` should show the new pack's rows, and the sweep's
own report line says how many it read, claimed, acked, and deferred.

If the source produces more than the round can hold, the report's deferred count
stays above zero every round. That is the signal to raise `--max-targets` or
tighten the connector's own filter — not to widen the read, which is already
free.

## When the sweep isn't doing anything

Work down the chain; each step tells you whether to keep going.

1. **Is anything arriving?** `boid signal list --workspace <ws> --state all`.
   Empty means the connectors aren't producing — check their derived trigger jobs,
   not the sweep.
2. **Is the trigger firing?** `on: signals` deliberately does not fire on an empty
   inbox. If rows are pending and nothing fires, look for a round still in flight
   holding single-flight.
3. **Did the round read them?** The sweep task's `--done` report carries the
   counts. Read > 0 but targets 0 means the sieve dropped everything — that is a
   judgment-skill question, not a machinery one.
4. **Are signals dying?** `--state dead` shows rows claimed five times without
   ever being acked. A dead row means something was handed to a judgment repeatedly
   and the judgment never wrote anything for it.

## What this skill does not cover

- **Writing a connector** (fetching from an external API into the inbox). That
  lives in an Integration Pack, in the `boid-api-skills` repo, with its own
  contract and conformance tests.
- **The card lifecycle itself** — what `parked`/`working`/`done`/`dropped` mean
  and who may move between them. The `boid-task` skill has it.
- **The inbox's own semantics** — claim, ack, dead signals. The `boid-signal`
  skill has it, and the judgment skill should not restate it.
